// DotAgent - Ultra-lightweight personal AI agent
// Inspired by and based on nanobot: https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 DotAgent contributors

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/dotsetgreg/dotagent/pkg/bus"
	"github.com/dotsetgreg/dotagent/pkg/channels"
	"github.com/dotsetgreg/dotagent/pkg/config"
	"github.com/dotsetgreg/dotagent/pkg/constants"
	"github.com/dotsetgreg/dotagent/pkg/logger"
	"github.com/dotsetgreg/dotagent/pkg/memory"
	"github.com/dotsetgreg/dotagent/pkg/providers"
	"github.com/dotsetgreg/dotagent/pkg/state"
	"github.com/dotsetgreg/dotagent/pkg/toolpacks"
	"github.com/dotsetgreg/dotagent/pkg/tools"
	"github.com/dotsetgreg/dotagent/pkg/utils"
	"github.com/google/uuid"
)

type AgentLoop struct {
	bus                    *bus.MessageBus
	provider               providers.LLMProvider
	providerName           string
	workspace              string
	workspaceID            string
	model                  string
	temperature            float64
	completionMax          int
	contextWindow          int // Maximum context window size in tokens
	contextPruningMode     string
	contextPruningKeepLast int
	loopDetectionCfg       tools.ToolLoopDetectionConfig
	maxIterations          int
	maxConcurrent          int
	memory                 *memory.Service
	state                  *state.Manager
	contextBuilder         *ContextBuilder
	tools                  *tools.ToolRegistry
	toolpacks              *toolpacks.Manager
	scheduler              *sessionScheduler
	sessionLocks           *sessionLockManager
	inboundDedupeMu        sync.Mutex
	recentInbound          map[string]int64
	inboundDedupeTTL       time.Duration
	promptBaselineMu       sync.Mutex
	sessionPromptHash      map[string]string
	personaSyncTimeout     time.Duration
	running                atomic.Bool
	channelManager         *channels.Manager
}

// processOptions configures how a message is processed
type processOptions struct {
	SessionKey      string // Session identifier for history/context
	Channel         string // Target channel for tool execution
	ChatID          string // Target chat ID for tool execution
	UserID          string // User identifier for memory namespace
	UserMessage     string // User message content (may include prefix)
	DefaultResponse string // Response when LLM returns empty
	EnableSummary   bool   // Whether to trigger summarization
	SendResponse    bool   // Whether to send response via bus
	StreamResponse  bool   // Whether to stream partial LLM output via bus
	NoHistory       bool   // If true, don't load session history (for heartbeat)
}

// createToolRegistry creates a tool registry with common tools.
// This is shared between main agent and subagents.
func createToolRegistry(workspace string, restrict bool, cfg *config.Config, msgBus *bus.MessageBus) (*tools.ToolRegistry, error) {
	registry := tools.NewToolRegistry()
	register := func(tool tools.Tool) error {
		if err := registry.Register(tool); err != nil {
			return fmt.Errorf("register tool %q: %w", tool.Name(), err)
		}
		return nil
	}

	// File system tools
	if err := register(tools.NewReadFileTool(workspace, restrict)); err != nil {
		return nil, err
	}
	if err := register(tools.NewWriteFileTool(workspace, restrict)); err != nil {
		return nil, err
	}
	if err := register(tools.NewListDirTool(workspace, restrict)); err != nil {
		return nil, err
	}
	if err := register(tools.NewEditFileTool(workspace, restrict)); err != nil {
		return nil, err
	}
	if err := register(tools.NewAppendFileTool(workspace, restrict)); err != nil {
		return nil, err
	}

	// Shell execution
	if err := register(tools.NewExecTool(workspace, restrict)); err != nil {
		return nil, err
	}
	if err := register(tools.NewProcessTool(workspace, restrict)); err != nil {
		return nil, err
	}

	if searchTool := tools.NewWebSearchTool(tools.WebSearchToolOptions{
		BraveAPIKey:          cfg.Tools.Web.Brave.APIKey,
		BraveMaxResults:      cfg.Tools.Web.Brave.MaxResults,
		BraveEnabled:         cfg.Tools.Web.Brave.Enabled,
		DuckDuckGoMaxResults: cfg.Tools.Web.DuckDuckGo.MaxResults,
		DuckDuckGoEnabled:    cfg.Tools.Web.DuckDuckGo.Enabled,
	}); searchTool != nil {
		if err := register(searchTool); err != nil {
			return nil, err
		}
	}
	if err := register(tools.NewWebFetchTool(50000)); err != nil {
		return nil, err
	}

	// Message tool - available to both agent and subagent
	// Subagent uses it to communicate directly with user
	messageTool := tools.NewMessageTool()
	messageTool.SetSendCallback(func(channel, chatID, content string) error {
		if msgBus == nil {
			return fmt.Errorf("message bus not configured")
		}
		err := msgBus.PublishOutbound(bus.OutboundMessage{
			Channel: channel,
			ChatID:  chatID,
			Content: content,
		})
		if err != nil {
			logger.WarnCF("agent", "Message tool publish failed", map[string]interface{}{
				"channel": channel,
				"chat_id": chatID,
				"error":   err.Error(),
			})
		}
		return err
	})
	if err := register(messageTool); err != nil {
		return nil, err
	}

	if cfg != nil && cfg.Admin.ConfigApply.Enabled {
		configPath := strings.TrimSpace(os.Getenv("DOTAGENT_CONFIG"))
		if configPath == "" {
			instanceRoot := filepath.Dir(cfg.RuntimePath())
			configPath = filepath.Join(instanceRoot, "config", "config.json")
		}
		historyDir := filepath.Join(filepath.Dir(configPath), "history")
		adminOpts := tools.ConfigApplyOptions{
			ConfigPath:         configPath,
			HistoryDir:         historyDir,
			AuditLogPath:       filepath.Join(cfg.RuntimePath(), "config_audit.log"),
			RequestsPath:       filepath.Join(cfg.RuntimePath(), "config_requests.json"),
			PendingRestartPath: filepath.Join(cfg.RuntimePath(), "config_restart_pending.json"),
			RequireApproval:    cfg.Admin.ConfigApply.RequireApproval,
			MutableKeys:        append([]string(nil), cfg.Admin.ConfigApply.MutableKeys...),
			OnRestartRequest: func(context.Context) error {
				proc, err := os.FindProcess(os.Getpid())
				if err != nil {
					return err
				}
				return proc.Signal(syscall.SIGTERM)
			},
			PostApplyTimeout: 20 * time.Second,
			PostApplyCheck: func(context.Context) error {
				nextCfg, err := config.LoadConfig(configPath)
				if err != nil {
					return fmt.Errorf("reload config: %w", err)
				}
				if err := providers.ValidateProviderConfig(nextCfg); err != nil {
					return fmt.Errorf("provider validation failed: %w", err)
				}
				paths := []string{
					nextCfg.WorkspacePath(),
					nextCfg.DataPath(),
					nextCfg.LogsPath(),
					nextCfg.RuntimePath(),
				}
				for _, p := range paths {
					if err := os.MkdirAll(p, 0o755); err != nil {
						return fmt.Errorf("ensure path %s: %w", p, err)
					}
				}
				stateDir := filepath.Join(nextCfg.DataPath(), "state")
				if err := os.MkdirAll(stateDir, 0o755); err != nil {
					return fmt.Errorf("ensure state dir: %w", err)
				}
				healthProbe := filepath.Join(stateDir, ".config-apply-probe")
				if err := os.WriteFile(healthProbe, []byte("ok"), 0o600); err != nil {
					return fmt.Errorf("state dir not writable: %w", err)
				}
				_ = os.Remove(healthProbe)
				return nil
			},
		}
		if err := register(tools.NewConfigRequestTool(adminOpts)); err != nil {
			return nil, err
		}
		if err := register(tools.NewConfigApplyTool(adminOpts)); err != nil {
			return nil, err
		}
	}

	return registry, nil
}

func NewAgentLoop(cfg *config.Config, msgBus *bus.MessageBus, provider providers.LLMProvider) (*AgentLoop, error) {
	workspace := cfg.WorkspacePath()
	dataRoot := cfg.DataPath()
	_ = os.MkdirAll(dataRoot, 0755)
	os.MkdirAll(workspace, 0755)

	restrict := cfg.Agents.Defaults.RestrictToWorkspace

	// Create tool registry for main agent
	toolsRegistry, err := createToolRegistry(workspace, restrict, cfg, msgBus)
	if err != nil {
		return nil, fmt.Errorf("create main tool registry: %w", err)
	}
	packManager := toolpacks.NewManager(workspace, restrict)
	packTools, err := packManager.LoadEnabledTools()
	for _, t := range packTools {
		if regErr := toolsRegistry.Register(t); regErr != nil {
			return nil, fmt.Errorf("register toolpack tool %q: %w", t.Name(), regErr)
		}
	}
	if err != nil {
		logger.WarnCF("agent", "Failed loading toolpacks", map[string]interface{}{"error": err.Error()})
	}

	// Create subagent manager with its own tool registry
	subagentManager := tools.NewSubagentManager(provider, cfg.Agents.Defaults.Model, workspace, dataRoot, msgBus)
	subagentTools, err := createToolRegistry(workspace, restrict, cfg, msgBus)
	if err != nil {
		return nil, fmt.Errorf("create subagent tool registry: %w", err)
	}
	// Subagent doesn't need spawn/subagent tools to avoid recursion
	subagentManager.SetTools(subagentTools)

	// Register spawn tool (for main agent)
	spawnTool := tools.NewSpawnTool(subagentManager)
	if err := toolsRegistry.Register(spawnTool); err != nil {
		return nil, fmt.Errorf("register spawn tool: %w", err)
	}

	// Register subagent tool (synchronous execution)
	subagentTool := tools.NewSubagentTool(subagentManager)
	if err := toolsRegistry.Register(subagentTool); err != nil {
		return nil, fmt.Errorf("register subagent tool: %w", err)
	}

	// Create state manager for atomic state persistence
	stateManager := state.NewManager(dataRoot)

	// Create context builder and set tools registry
	contextBuilder := NewContextBuilder(workspace)
	contextBuilder.SetToolsRegistry(toolsRegistry)
	subagentWorkspaceContext := strings.TrimSpace(contextBuilder.getIdentity())
	if subagentWorkspaceContext != "" {
		subagentManager.SetWorkspaceContext(subagentWorkspaceContext)
	}

	summarizeFn := func(ctx context.Context, existingSummary, transcript string) (string, error) {
		prompt := "Update the durable conversation summary.\n" +
			"Preserve user preferences, constraints, commitments, unresolved tasks, and key technical context.\n" +
			"Keep it compact and factual.\n\n" +
			"EXISTING SUMMARY:\n" + existingSummary + "\n\n" +
			"NEW TRANSCRIPT SEGMENT:\n" + transcript + "\n\n" +
			"Return only the updated summary."
		resp, err := provider.Chat(ctx, []providers.Message{{Role: "user", Content: prompt}}, nil, cfg.Agents.Defaults.Model, map[string]interface{}{
			"max_tokens":  900,
			"temperature": 0.2,
		})
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(resp.Content), nil
	}

	personaExtractFn := func(ctx context.Context, req memory.PersonaExtractionRequest) ([]memory.PersonaUpdateCandidate, error) {
		existingRaw, _ := json.Marshal(req.ExistingProfile)
		prompt := strings.TrimSpace(`Extract persona update candidates from this conversation turn.

Return strict JSON only. No prose.
Allowed schema:
{
	  "candidates": [
	    {
	      "field_path": "user.name | user.timezone | user.location | user.language | user.communication_style | user.session_intent | user.goals | user.preferences.<key> | user.attributes.<key> | identity.agent_name | identity.role | identity.purpose | identity.goals | identity.boundaries | identity.attributes.<key> | soul.voice | soul.communication_style | soul.values | soul.behavioral_rules | soul.attributes.<key>",
	      "operation": "set | append | delete",
	      "value": "string (empty for delete)",
	      "confidence": 0.0,
	      "evidence": "short quote/paraphrase from turn"
    }
  ]
}

Rules:
- Include only durable user personalization and stable behavior instructions.
- Capture explicit second-person directives when the user sets assistant identity/style.
- Ignore secrets, credentials, or sensitive tokens.
- Prefer fewer high-confidence candidates over many weak ones.
- Do not emit duplicates.

EXISTING PROFILE JSON:
` + string(existingRaw) + `

TURN TRANSCRIPT:
` + req.Transcript)

		resp, err := provider.Chat(ctx, []providers.Message{
			{Role: "user", Content: prompt},
		}, nil, cfg.Agents.Defaults.Model, map[string]interface{}{
			"max_tokens":  900,
			"temperature": 0.1,
		})
		if err != nil {
			return nil, err
		}
		candidates, parsed := parsePersonaCandidatesResponse(resp.Content)
		if !parsed && strings.TrimSpace(resp.Content) != "" {
			return nil, memory.ErrPersonaExtractorParse
		}
		return candidates, nil
	}

	resolvedContextWindow := resolveRuntimeContextWindow(provider, cfg.Agents.Defaults.Model, cfg.Agents.Defaults.MaxTokens)
	subagentRetryCfg := providers.DefaultRetryConfig()
	subagentRetryCfg.MaxAttempts = 3
	subagentRetryCfg.MinDelay = 1500 * time.Millisecond
	subagentRetryCfg.MaxDelay = 8 * time.Second
	subagentManager.ConfigureLoopRuntime(tools.SubagentLoopRuntimeOptions{
		ContextWindowTokens:    resolvedContextWindow,
		ContextPruningMode:     strings.TrimSpace(cfg.Memory.ContextPruningMode),
		ContextPruningKeepLast: cfg.Memory.ContextPruningKeepLastToolResults,
		MaxOverflowCompactions: 3,
		Retry:                  subagentRetryCfg,
		LoopDetection: tools.ToolLoopDetectionConfig{
			Enabled:                     cfg.Memory.ToolLoopDetectionEnabled,
			WarningsEnabled:             cfg.Memory.ToolLoopWarningsEnabled,
			SignatureWarnThreshold:      cfg.Memory.ToolLoopSignatureWarnThreshold,
			SignatureCriticalThreshold:  cfg.Memory.ToolLoopSignatureCriticalThreshold,
			DriftWarnThreshold:          cfg.Memory.ToolLoopDriftWarnThreshold,
			DriftCriticalThreshold:      cfg.Memory.ToolLoopDriftCriticalThreshold,
			PollingWarnThreshold:        cfg.Memory.ToolLoopPollingWarnThreshold,
			PollingCriticalThreshold:    cfg.Memory.ToolLoopPollingCriticalThreshold,
			NoProgressWarnThreshold:     cfg.Memory.ToolLoopNoProgressWarnThreshold,
			NoProgressCriticalThreshold: cfg.Memory.ToolLoopNoProgressCriticalThreshold,
			PingPongWarnThreshold:       cfg.Memory.ToolLoopPingPongWarnThreshold,
			PingPongCriticalThreshold:   cfg.Memory.ToolLoopPingPongCriticalThreshold,
			GlobalCircuitThreshold:      cfg.Memory.ToolLoopGlobalCircuitThreshold,
		},
	})
	compactionHooks := memory.CompactionHooks{
		Before: func(_ context.Context, payload memory.CompactionHookPayload) {
			if msgBus == nil {
				return
			}
			_ = msgBus.PublishEvent(bus.EventMessage{
				Type:       "before_compaction",
				SessionKey: payload.SessionKey,
				Metadata: map[string]string{
					"user_id":        payload.UserID,
					"agent_id":       payload.AgentID,
					"compaction_id":  payload.CompactionID,
					"status":         payload.Status,
					"stage":          payload.Stage,
					"source_count":   strconv.Itoa(payload.SourceCount),
					"retained_count": strconv.Itoa(payload.RetainedCount),
				},
			})
		},
		After: func(_ context.Context, payload memory.CompactionHookPayload) {
			if msgBus == nil {
				return
			}
			_ = msgBus.PublishEvent(bus.EventMessage{
				Type:       "after_compaction",
				SessionKey: payload.SessionKey,
				Metadata: map[string]string{
					"user_id":        payload.UserID,
					"agent_id":       payload.AgentID,
					"compaction_id":  payload.CompactionID,
					"status":         payload.Status,
					"stage":          payload.Stage,
					"source_count":   strconv.Itoa(payload.SourceCount),
					"retained_count": strconv.Itoa(payload.RetainedCount),
					"archived_count": strconv.Itoa(payload.ArchivedCount),
					"summary_length": strconv.Itoa(payload.SummaryLength),
					"recovery_mode":  payload.RecoveryMode,
					"error":          payload.Error,
				},
			})
		},
	}

	memSvc, err := memory.NewService(memory.Config{
		Workspace:               workspace,
		DataDir:                 dataRoot,
		AgentID:                 "dotagent",
		ContextModel:            cfg.Agents.Defaults.Model,
		EmbeddingModel:          cfg.Memory.EmbeddingModel,
		EmbeddingFallbackModels: append([]string(nil), cfg.Memory.EmbeddingFallbackModels...),
		EmbeddingOpenAIToken: firstNonEmpty(
			strings.TrimSpace(cfg.Providers.OpenAI.APIKey),
			strings.TrimSpace(cfg.Providers.OpenAI.OAuthAccessToken),
		),
		EmbeddingOpenAIAPIBase:       strings.TrimSpace(cfg.Providers.OpenAI.APIBase),
		EmbeddingOpenRouterKey:       strings.TrimSpace(cfg.Providers.OpenRouter.APIKey),
		EmbeddingOpenRouterBase:      strings.TrimSpace(cfg.Providers.OpenRouter.APIBase),
		EmbeddingOllamaAPIBase:       strings.TrimSpace(cfg.Memory.EmbeddingOllamaAPIBase),
		EmbeddingBatchSize:           cfg.Memory.EmbeddingBatchSize,
		EmbeddingConcurrency:         cfg.Memory.EmbeddingConcurrency,
		MaxContextTokens:             resolvedContextWindow,
		MaxRecallItems:               cfg.Memory.MaxRecallItems,
		CandidateLimit:               cfg.Memory.CandidateLimit,
		RetrievalCache:               time.Duration(cfg.Memory.RetrievalCacheSeconds) * time.Second,
		WorkerLease:                  time.Duration(cfg.Memory.WorkerLeaseSeconds) * time.Second,
		WorkerPoll:                   time.Duration(cfg.Memory.WorkerPollMS) * time.Millisecond,
		EventRetention:               time.Duration(cfg.Memory.EventRetentionDays) * 24 * time.Hour,
		AuditRetention:               time.Duration(cfg.Memory.AuditRetentionDays) * 24 * time.Hour,
		PersonaCardTokens:            480,
		PersonaExtractor:             personaExtractFn,
		PersonaSyncApply:             cfg.Memory.PersonaSyncApply,
		PersonaFileSync:              memory.NormalizePersonaFileSyncMode(cfg.Memory.PersonaFileSyncMode),
		PersonaPolicyMode:            cfg.Memory.PersonaPolicyMode,
		PersonaMinConfidence:         cfg.Memory.PersonaMinConfidence,
		CompactionSummaryTimeout:     time.Duration(cfg.Memory.CompactionSummaryTimeoutSeconds) * time.Second,
		CompactionChunkChars:         cfg.Memory.CompactionChunkChars,
		CompactionMaxTranscriptChars: cfg.Memory.CompactionMaxTranscriptChars,
		CompactionPartialSkipChars:   cfg.Memory.CompactionPartialSkipChars,
		CompactionHooks:              compactionHooks,
		FileMemoryEnabled:            cfg.Memory.FileMemoryEnabled,
		FileMemoryDir:                strings.TrimSpace(cfg.Memory.FileMemoryDir),
		FileMemoryPoll:               time.Duration(cfg.Memory.FileMemoryPollSeconds) * time.Second,
		FileMemoryWatchEnabled:       cfg.Memory.FileMemoryWatchEnabled,
		FileMemoryWatchDebounce:      time.Duration(cfg.Memory.FileMemoryWatchDebounceMS) * time.Millisecond,
		FileMemoryMaxFileBytes:       cfg.Memory.FileMemoryMaxFileBytes,
	}, summarizeFn)
	if err != nil {
		return nil, fmt.Errorf("initialize memory service: %w", err)
	}

	completionMax := cfg.Agents.Defaults.MaxTokens
	if completionMax <= 0 {
		completionMax = 16384
	}
	temperature := cfg.Agents.Defaults.Temperature
	if temperature < 0 {
		temperature = 0
	}

	agentLoop := &AgentLoop{
		bus:                    msgBus,
		provider:               provider,
		providerName:           providers.ActiveProviderName(cfg),
		workspace:              workspace,
		workspaceID:            workspaceNamespace(workspace),
		model:                  cfg.Agents.Defaults.Model,
		temperature:            temperature,
		completionMax:          completionMax,
		contextWindow:          resolvedContextWindow,
		contextPruningMode:     strings.TrimSpace(cfg.Memory.ContextPruningMode),
		contextPruningKeepLast: cfg.Memory.ContextPruningKeepLastToolResults,
		loopDetectionCfg: tools.ToolLoopDetectionConfig{
			Enabled:                     cfg.Memory.ToolLoopDetectionEnabled,
			WarningsEnabled:             cfg.Memory.ToolLoopWarningsEnabled,
			SignatureWarnThreshold:      cfg.Memory.ToolLoopSignatureWarnThreshold,
			SignatureCriticalThreshold:  cfg.Memory.ToolLoopSignatureCriticalThreshold,
			DriftWarnThreshold:          cfg.Memory.ToolLoopDriftWarnThreshold,
			DriftCriticalThreshold:      cfg.Memory.ToolLoopDriftCriticalThreshold,
			PollingWarnThreshold:        cfg.Memory.ToolLoopPollingWarnThreshold,
			PollingCriticalThreshold:    cfg.Memory.ToolLoopPollingCriticalThreshold,
			NoProgressWarnThreshold:     cfg.Memory.ToolLoopNoProgressWarnThreshold,
			NoProgressCriticalThreshold: cfg.Memory.ToolLoopNoProgressCriticalThreshold,
			PingPongWarnThreshold:       cfg.Memory.ToolLoopPingPongWarnThreshold,
			PingPongCriticalThreshold:   cfg.Memory.ToolLoopPingPongCriticalThreshold,
			GlobalCircuitThreshold:      cfg.Memory.ToolLoopGlobalCircuitThreshold,
		},
		maxIterations:  cfg.Agents.Defaults.MaxToolIterations,
		maxConcurrent:  cfg.Agents.Defaults.MaxConcurrentRuns,
		memory:         memSvc,
		state:          stateManager,
		contextBuilder: contextBuilder,
		tools:          toolsRegistry,
		toolpacks:      packManager,
		sessionLocks: newSessionLockManager(sessionLockOptions{
			WorkspaceRoot:   dataRoot,
			FileLockEnabled: cfg.Agents.Defaults.SessionFileLockEnabled,
			LockTimeout:     time.Duration(cfg.Agents.Defaults.SessionLockTimeoutMS) * time.Millisecond,
			StaleAfter:      time.Duration(cfg.Agents.Defaults.SessionLockStaleSeconds) * time.Second,
			MaxHoldDuration: time.Duration(cfg.Agents.Defaults.SessionLockMaxHoldSeconds) * time.Second,
		}),
		recentInbound:      map[string]int64{},
		inboundDedupeTTL:   30 * time.Second,
		sessionPromptHash:  map[string]string{},
		personaSyncTimeout: time.Duration(cfg.Memory.PersonaSyncTimeoutMS) * time.Millisecond,
	}

	sessionTool := tools.NewSessionTool(
		agentLoop.memory,
		func(channel, chatID, userID string) (string, error) {
			return resolveSessionKey("", agentLoop.workspaceID, channel, chatID, valueOr(userID, "local-user"))
		},
		agentLoop.ProcessDirectWithChannel,
	)
	if err := toolsRegistry.Register(sessionTool); err != nil {
		return nil, fmt.Errorf("register session tool: %w", err)
	}
	if agentLoop.maxIterations <= 0 {
		agentLoop.maxIterations = 50
	}
	if agentLoop.maxConcurrent <= 0 {
		agentLoop.maxConcurrent = 4
	}
	if agentLoop.personaSyncTimeout <= 0 {
		agentLoop.personaSyncTimeout = 2200 * time.Millisecond
	}

	return agentLoop, nil
}

func (al *AgentLoop) Run(ctx context.Context) error {
	al.running.Store(true)
	scheduler := newSessionScheduler(al.maxConcurrent)
	al.scheduler = scheduler
	defer func() {
		scheduler.Stop()
		if !scheduler.Wait(5 * time.Second) {
			logger.WarnCF("agent", "Timed out waiting for in-flight session tasks during shutdown", map[string]interface{}{
				"timeout_ms": 5000,
			})
		}
		al.scheduler = nil
		al.closeResources()
	}()

	for al.running.Load() {
		select {
		case <-ctx.Done():
			return nil
		default:
			msg, ok := al.bus.ConsumeInbound(ctx)
			if !ok {
				continue
			}
			incoming := msg
			if al.isDuplicateInbound(incoming) {
				logger.WarnCF("agent", "Skipping duplicate inbound message", map[string]interface{}{
					"channel":    incoming.Channel,
					"chat_id":    incoming.ChatID,
					"sender_id":  incoming.SenderID,
					"message_id": incoming.MessageID,
				})
				continue
			}
			laneKey := al.resolveLaneKey(incoming)
			runTask := func() {
				roundState := tools.NewExecutionRoundState()
				roundCtx := tools.WithExecutionRoundState(ctx, roundState)

				response, err := al.processMessage(roundCtx, incoming)
				if err != nil {
					response = fmt.Sprintf("Error processing message: %v", err)
				}

				if response != "" && !roundState.MessageSent() {
					al.publishOutbound(bus.OutboundMessage{
						Channel: incoming.Channel,
						ChatID:  incoming.ChatID,
						Content: response,
					}, "run_loop_response")
				}
			}
			if submitErr := scheduler.Submit(laneKey, runTask); submitErr != nil {
				logger.ErrorCF("agent", "Failed to submit session task", map[string]interface{}{
					"session_key": incoming.SessionKey,
					"lane_key":    laneKey,
					"error":       submitErr.Error(),
				})
				if errors.Is(submitErr, ErrSchedulerLaneFull) {
					if !constants.IsInternalChannel(incoming.Channel) {
						al.publishOutbound(bus.OutboundMessage{
							Channel: incoming.Channel,
							ChatID:  incoming.ChatID,
							Content: "I am currently busy in this chat and could not queue that message yet. Please retry in a moment.",
						}, "scheduler_lane_full")
					}
					continue
				}
				if retryErr := scheduler.Submit(laneKey, runTask); retryErr != nil {
					logger.ErrorCF("agent", "Retry submit failed; notifying user", map[string]interface{}{
						"session_key": incoming.SessionKey,
						"lane_key":    laneKey,
						"error":       retryErr.Error(),
					})
					if !constants.IsInternalChannel(incoming.Channel) {
						al.publishOutbound(bus.OutboundMessage{
							Channel: incoming.Channel,
							ChatID:  incoming.ChatID,
							Content: "I hit an internal scheduling issue and could not process that message. Please try again.",
						}, "scheduler_submit_failed")
					}
				}
			}
		}
	}

	return nil
}

func (al *AgentLoop) isDuplicateInbound(msg bus.InboundMessage) bool {
	key := inboundDedupeKey(msg)
	if key == "" {
		return false
	}
	now := time.Now().UnixMilli()
	ttl := al.inboundDedupeTTL
	if ttl <= 0 {
		ttl = 90 * time.Second
	}
	cutoff := now - ttl.Milliseconds()
	al.inboundDedupeMu.Lock()
	defer al.inboundDedupeMu.Unlock()
	for k, seenAt := range al.recentInbound {
		if seenAt < cutoff {
			delete(al.recentInbound, k)
		}
	}
	if seenAt, ok := al.recentInbound[key]; ok {
		if seenAt >= cutoff {
			return true
		}
	}
	al.recentInbound[key] = now
	return false
}

func inboundDedupeKey(msg bus.InboundMessage) string {
	channel := strings.TrimSpace(msg.Channel)
	chatID := strings.TrimSpace(msg.ChatID)
	sender := strings.TrimSpace(msg.SenderID)
	if mid := strings.TrimSpace(msg.MessageID); mid != "" {
		return strings.ToLower(channel + "|" + chatID + "|" + sender + "|" + mid)
	}
	if msg.Metadata != nil {
		if dedupeID := strings.TrimSpace(msg.Metadata["dedupe_id"]); dedupeID != "" {
			return strings.ToLower(channel + "|" + chatID + "|" + sender + "|d:" + dedupeID)
		}
	}
	// Avoid suppressing legitimate repeated user messages from channels that do
	// not provide stable message IDs.
	if !constants.IsInternalChannel(channel) && !strings.HasPrefix(strings.ToLower(sender), "cron:") {
		return ""
	}
	content := strings.TrimSpace(strings.ToLower(msg.Content))
	if content == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(content))
	return strings.ToLower(channel + "|" + chatID + "|" + sender + "|h:" + hex.EncodeToString(sum[:16]))
}

func (al *AgentLoop) detectPromptBaselineChange(sessionKey, promptHash string) bool {
	sessionKey = strings.TrimSpace(sessionKey)
	promptHash = strings.TrimSpace(promptHash)
	if sessionKey == "" || promptHash == "" {
		return false
	}
	al.promptBaselineMu.Lock()
	defer al.promptBaselineMu.Unlock()
	prev := strings.TrimSpace(al.sessionPromptHash[sessionKey])
	al.sessionPromptHash[sessionKey] = promptHash
	return prev != "" && prev != promptHash
}

func (al *AgentLoop) Stop() {
	al.running.Store(false)
	if al.scheduler != nil {
		al.scheduler.Stop()
		return
	}
	al.closeResources()
}

func (al *AgentLoop) closeResources() {
	if al.tools != nil {
		if err := al.tools.Close(); err != nil {
			logger.WarnCF("agent", "Tool teardown reported errors", map[string]interface{}{"error": err.Error()})
		}
	}
	if al.memory != nil {
		_ = al.memory.Close()
	}
	if al.sessionLocks != nil {
		al.sessionLocks.Close()
	}
}

func (al *AgentLoop) resolveLaneKey(msg bus.InboundMessage) string {
	actorID := valueOr(msg.SenderID, "local-user")
	if laneKey, err := resolveSessionKey(msg.SessionKey, al.workspaceID, msg.Channel, msg.ChatID, actorID); err == nil {
		return laneKey
	}
	if strings.TrimSpace(msg.SessionKey) != "" {
		return strings.TrimSpace(msg.SessionKey)
	}
	return fmt.Sprintf("%s|%s|%s", valueOr(msg.Channel, "unknown"), valueOr(msg.ChatID, "unknown"), actorID)
}

func (al *AgentLoop) RegisterTool(tool tools.Tool) {
	if tool == nil {
		logger.ErrorCF("agent", "Failed to register tool", map[string]interface{}{
			"error": "tool is nil",
		})
		return
	}
	if err := al.tools.Register(tool); err != nil {
		logger.ErrorCF("agent", "Failed to register tool", map[string]interface{}{
			"tool":  tool.Name(),
			"error": err.Error(),
		})
	}
}

func (al *AgentLoop) SetChannelManager(cm *channels.Manager) {
	al.channelManager = cm
}

// RecordLastChannel records the last active channel for this workspace.
// This uses the atomic state save mechanism to prevent data loss on crash.
func (al *AgentLoop) RecordLastChannel(channel string) error {
	return al.state.SetLastChannel(channel)
}

// RecordLastChatID records the last active chat ID for this workspace.
// This uses the atomic state save mechanism to prevent data loss on crash.
func (al *AgentLoop) RecordLastChatID(chatID string) error {
	return al.state.SetLastChatID(chatID)
}

func (al *AgentLoop) ProcessDirect(ctx context.Context, content, sessionKey string) (string, error) {
	return al.ProcessDirectWithChannel(ctx, content, sessionKey, "cli", "direct")
}

func (al *AgentLoop) ProcessDirectWithChannel(ctx context.Context, content, sessionKey, channel, chatID string) (string, error) {
	msg := bus.InboundMessage{
		Channel:    channel,
		SenderID:   "local-user",
		ChatID:     chatID,
		Content:    content,
		SessionKey: sessionKey,
	}

	return al.processMessage(ctx, msg)
}

// ProcessHeartbeat processes a heartbeat request without session history.
// Each heartbeat is independent and doesn't accumulate context.
func (al *AgentLoop) ProcessHeartbeat(ctx context.Context, content, channel, chatID string) (string, error) {
	return al.runAgentLoop(ctx, processOptions{
		SessionKey:      "heartbeat",
		Channel:         channel,
		ChatID:          chatID,
		UserID:          "heartbeat",
		UserMessage:     content,
		DefaultResponse: "I've completed processing but have no response to give.",
		EnableSummary:   false,
		SendResponse:    false,
		StreamResponse:  false,
		NoHistory:       true, // Don't load session history for heartbeat
	})
}

func (al *AgentLoop) processMessage(ctx context.Context, msg bus.InboundMessage) (string, error) {
	// Add message preview to log (show full content for error messages)
	var logContent string
	if strings.Contains(msg.Content, "Error:") || strings.Contains(msg.Content, "error") {
		logContent = msg.Content // Full content for errors
	} else {
		logContent = utils.Truncate(msg.Content, 80)
	}
	logger.InfoCF("agent", fmt.Sprintf("Processing message from %s:%s: %s", msg.Channel, msg.SenderID, logContent),
		map[string]interface{}{
			"channel":     msg.Channel,
			"chat_id":     msg.ChatID,
			"sender_id":   msg.SenderID,
			"session_key": msg.SessionKey,
		})

	// Route system messages to processSystemMessage
	if msg.Channel == "system" {
		return al.processSystemMessage(ctx, msg)
	}

	// Check for commands
	if response, handled := al.handleCommand(ctx, msg); handled {
		return response, nil
	}

	// Process as user message
	return al.runAgentLoop(ctx, processOptions{
		SessionKey:      msg.SessionKey,
		Channel:         msg.Channel,
		ChatID:          msg.ChatID,
		UserID:          msg.SenderID,
		UserMessage:     msg.Content,
		DefaultResponse: "I've completed processing but have no response to give.",
		EnableSummary:   true,
		SendResponse:    false,
		StreamResponse:  true,
	})
}

func (al *AgentLoop) processSystemMessage(ctx context.Context, msg bus.InboundMessage) (string, error) {
	// Verify this is a system message
	if msg.Channel != "system" {
		return "", fmt.Errorf("processSystemMessage called with non-system message channel: %s", msg.Channel)
	}

	logger.InfoCF("agent", "Processing system message",
		map[string]interface{}{
			"sender_id": msg.SenderID,
			"chat_id":   msg.ChatID,
		})

	// Parse origin channel from chat_id (format: "channel:chat_id")
	var originChannel string
	var originChatID string
	if idx := strings.Index(msg.ChatID, ":"); idx > 0 {
		originChannel = msg.ChatID[:idx]
		originChatID = msg.ChatID[idx+1:]
	} else {
		// Fallback
		originChannel = "cli"
		originChatID = msg.ChatID
	}

	// Extract subagent result from message content
	// Format: "Task 'label' completed.\n\nResult:\n<actual content>"
	content := msg.Content
	if idx := strings.Index(content, "Result:\n"); idx >= 0 {
		content = content[idx+8:] // Extract just the result part
	}

	// Skip internal channels - only log, don't send to user
	if constants.IsInternalChannel(originChannel) {
		logger.InfoCF("agent", "Subagent completed (internal channel)",
			map[string]interface{}{
				"sender_id":   msg.SenderID,
				"content_len": len(content),
				"channel":     originChannel,
			})
		return "", nil
	}

	// Agent acts as dispatcher only - subagent handles user interaction via message tool
	// Don't forward result here, subagent should use message tool to communicate with user
	logger.InfoCF("agent", "Subagent completed",
		map[string]interface{}{
			"sender_id":   msg.SenderID,
			"channel":     originChannel,
			"content_len": len(content),
		})

	if strings.TrimSpace(content) != "" && !constants.IsInternalChannel(originChannel) {
		al.publishOutbound(bus.OutboundMessage{
			Channel: originChannel,
			ChatID:  originChatID,
			Content: strings.TrimSpace(content),
		}, "subagent_completion")
	}

	return "", nil
}

// runAgentLoop is the core message processing logic.
// It handles context building, LLM calls, tool execution, and response handling.
func (al *AgentLoop) runAgentLoop(ctx context.Context, opts processOptions) (string, error) {
	// 0. Record last channel for heartbeat notifications (skip internal channels)
	if opts.Channel != "" && opts.ChatID != "" {
		// Don't record internal channels (cli, system, subagent)
		if !constants.IsInternalChannel(opts.Channel) {
			channelKey := fmt.Sprintf("%s:%s", opts.Channel, opts.ChatID)
			if err := al.RecordLastChannel(channelKey); err != nil {
				logger.WarnCF("agent", "Failed to record last channel: %v", map[string]interface{}{"error": err.Error()})
			}
		}
	}

	if !opts.NoHistory {
		normalizedSessionKey, skErr := resolveSessionKey(opts.SessionKey, al.workspaceID, opts.Channel, opts.ChatID, opts.UserID)
		if skErr != nil {
			_ = al.memory.AddMetric(ctx, "memory.session_key.missing", 1, map[string]string{
				"channel": opts.Channel,
				"chat_id": opts.ChatID,
				"user_id": opts.UserID,
			})
			return "", skErr
		}
		opts.SessionKey = normalizedSessionKey
	} else if strings.TrimSpace(opts.SessionKey) == "" {
		opts.SessionKey = "ephemeral:no_history"
	}
	if al.sessionLocks != nil {
		unlock, lockErr := al.sessionLocks.Acquire(ctx, opts.SessionKey)
		if lockErr != nil {
			return "", fmt.Errorf("acquire session lock: %w", lockErr)
		}
		defer unlock()
	}

	// 1. Ensure memory session exists
	if !opts.NoHistory {
		if err := al.memory.EnsureSession(ctx, opts.SessionKey, opts.Channel, opts.ChatID, opts.UserID); err != nil {
			logger.WarnCF("agent", "Failed to ensure memory session", map[string]interface{}{"error": err.Error(), "session_key": opts.SessionKey})
		}
	}

	// 2. Persist user event immediately (before prompt assembly) so same-turn
	// persona directives can be applied synchronously and reflected in the next response.
	turnID := "turn-" + uuid.NewString()
	seq := 1
	recordedUserTurn := false
	var syncPersonaReport memory.PersonaApplyReport
	if !opts.NoHistory {
		if _, _, err := al.memory.RecordUserTurn(ctx, memory.Event{
			SessionKey: opts.SessionKey,
			TurnID:     turnID,
			Seq:        seq,
			Role:       "user",
			Content:    opts.UserMessage,
			Metadata: map[string]string{
				"channel": opts.Channel,
				"chat_id": opts.ChatID,
				"user_id": opts.UserID,
			},
		}, opts.UserID); err != nil {
			logger.ErrorCF("agent", "Failed to record user turn", map[string]interface{}{
				"error":       err.Error(),
				"session_key": opts.SessionKey,
				"turn_id":     turnID,
			})
		} else {
			recordedUserTurn = true
		}
		seq++

		syncPersonaOutcome := "llm_empty"
		if recordedUserTurn {
			if shouldApplyPersonaSyncFastPath(opts.UserMessage) {
				personaCtx := ctx
				cancel := func() {}
				if al.personaSyncTimeout > 0 {
					personaCtx, cancel = context.WithTimeout(ctx, al.personaSyncTimeout)
				}
				report, applyErr := al.memory.ApplyPersonaDirectivesSync(personaCtx, opts.SessionKey, turnID, opts.UserID)
				cancel()
				if applyErr != nil {
					switch {
					case errors.Is(applyErr, context.DeadlineExceeded), errors.Is(applyErr, context.Canceled):
						syncPersonaOutcome = "llm_timeout"
					case errors.Is(applyErr, memory.ErrPersonaExtractorParse):
						syncPersonaOutcome = "llm_parse_error"
					default:
						syncPersonaOutcome = "llm_error"
					}
					logger.WarnCF("agent", "Synchronous persona apply failed", map[string]interface{}{
						"error":       applyErr.Error(),
						"session_key": opts.SessionKey,
						"turn_id":     turnID,
					})
				} else {
					syncPersonaReport = report
					if len(report.Decisions) > 0 {
						syncPersonaOutcome = "llm_ok"
					}
				}
			} else {
				syncPersonaOutcome = "heuristic_skip"
				_ = al.memory.AddMetric(ctx, "memory.persona.apply_sync.skipped", 1, map[string]string{
					"session_key": opts.SessionKey,
					"user_id":     opts.UserID,
					"reason":      "no_directive_signal",
				})
				logger.DebugCF("agent", "Skipping synchronous persona apply for non-directive turn", map[string]interface{}{
					"session_key": opts.SessionKey,
					"turn_id":     turnID,
				})
			}
		}
		_ = al.memory.AddMetric(ctx, "memory.persona.apply_sync.outcome", 1, map[string]string{
			"session_key": opts.SessionKey,
			"user_id":     opts.UserID,
			"turn_id":     turnID,
			"outcome":     syncPersonaOutcome,
		})
	}

	// 3. Build messages (skip history for heartbeat)
	var history []providers.Message
	var summary string
	var recall string
	continuityNotes := []string{}
	if !opts.NoHistory {
		promptCtx, err := al.memory.BuildPromptContext(ctx, opts.SessionKey, opts.UserID, opts.UserMessage, al.contextWindow)
		if err != nil {
			logger.WarnCF("agent", "Failed to build memory prompt context", map[string]interface{}{"error": err.Error(), "session_key": opts.SessionKey})
		} else {
			history = toProviderMessages(promptCtx.History)
			summary = promptCtx.Summary
			recall = promptCtx.RecallPrompt
			hasContinuityArtifacts := promptCtx.Continuity.HasHistory || promptCtx.Continuity.HasSummary || promptCtx.Continuity.HasRecall
			if promptCtx.Continuity.Degraded && promptCtx.Continuity.HasPriorTurns && !hasContinuityArtifacts {
				continuityNotes = append(continuityNotes, buildDegradedContinuitySystemNote(promptCtx.Continuity.DegradedBy))
			}
			if note := buildCompactionContinuationSystemNote(promptCtx.Continuity.ContinuationNotes); note != "" {
				continuityNotes = append(continuityNotes, note)
			}
		}
	}
	currentUserPrompt := opts.UserMessage
	if !opts.NoHistory && recordedUserTurn && historyEndsWithUserMessage(history, opts.UserMessage) {
		// Current user turn is already in persisted history; avoid duplicate copy.
		currentUserPrompt = ""
	}
	systemPrompt, promptMeta := al.contextBuilder.BuildSystemPromptWithMetadata()
	messages := al.contextBuilder.BuildMessagesWithSystemPrompt(
		systemPrompt,
		history,
		summary,
		recall,
		currentUserPrompt,
		nil,
		opts.Channel,
		opts.ChatID,
	)
	personaNote := buildPersonaDecisionSystemNote(syncPersonaReport)
	for _, note := range continuityNotes {
		if note == "" {
			continue
		}
		messages = injectSystemNote(messages, note)
	}
	if personaNote != "" {
		messages = injectSystemNote(messages, personaNote)
	}
	if !opts.NoHistory && al.detectPromptBaselineChange(opts.SessionKey, promptMeta.Hash) {
		messages = injectSystemNote(messages,
			"System capabilities/bootstrap instructions changed since the previous turn. Use the latest tool list and identity constraints for this response.")
	}

	// 4. Run shared LLM+tool iteration loop
	providerStateID := ""
	if !opts.NoHistory {
		if sid, err := al.memory.GetProviderState(ctx, opts.SessionKey, al.providerName); err == nil {
			providerStateID = strings.TrimSpace(sid)
		}
	}
	retryCfg := providers.DefaultRetryConfig()
	retryCfg.MaxAttempts = 3
	retryCfg.MinDelay = 1500 * time.Millisecond
	retryCfg.MaxDelay = 8 * time.Second
	if strings.EqualFold(al.providerName, providers.ProviderOllama) {
		retryCfg.MaxAttempts = 2
		retryCfg.MinDelay = 600 * time.Millisecond
		retryCfg.MaxDelay = 2500 * time.Millisecond
	}
	streamID := turnID
	streamForwarder := newLLMStreamForwarder(func(chunk string) {
		if chunk == "" || constants.IsInternalChannel(opts.Channel) {
			return
		}
		al.publishOutbound(bus.OutboundMessage{
			Channel:  opts.Channel,
			ChatID:   opts.ChatID,
			Content:  chunk,
			Stream:   true,
			StreamID: streamID,
		}, "stream_delta")
	})
	if !opts.StreamResponse || opts.NoHistory || constants.IsInternalChannel(opts.Channel) || strings.TrimSpace(opts.ChatID) == "" {
		streamForwarder = nil
	}
	overflowNoticeSent := false
	toolLoopCtx := tools.WithToolExecutionActor(ctx, opts.UserID)
	loopResult, err := tools.RunToolLoop(toolLoopCtx, tools.ToolLoopConfig{
		Provider:               al.provider,
		Model:                  al.model,
		Tools:                  al.tools,
		MaxIterations:          al.maxIterations,
		LLMOptions:             map[string]any{"max_tokens": al.completionMax, "temperature": al.temperature},
		ContextWindowTokens:    al.contextWindow,
		Retry:                  retryCfg,
		MaxOverflowCompactions: 3,
		ContextPruningMode:     al.contextPruningMode,
		ContextPruningKeepLast: al.contextPruningKeepLast,
		LoopDetection:          al.loopDetectionCfg,
		CallLLM: func(callCtx context.Context, loopMessages []providers.Message, toolDefs []providers.ToolDefinition, model string, callOpts map[string]interface{}) (*providers.LLMResponse, error) {
			effectiveOpts := cloneLLMCallOptions(callOpts)
			if streamForwarder != nil {
				effectiveOpts["stream"] = true
				effectiveOpts["stream_callback"] = func(delta string) {
					streamForwarder.Push(delta)
				}
			}
			if stateful, ok := al.provider.(providers.StatefulLLMProvider); ok && !opts.NoHistory {
				stateID := strings.TrimSpace(providerStateID)
				response, newState, callErr := stateful.ChatWithState(callCtx, stateID, loopMessages, toolDefs, model, effectiveOpts)
				if callErr == nil {
					newState = strings.TrimSpace(newState)
					if newState != "" && newState != stateID {
						providerStateID = newState
						_ = al.memory.SetProviderState(callCtx, opts.SessionKey, al.providerName, newState)
					}
				}
				return response, callErr
			}
			return al.provider.Chat(callCtx, loopMessages, toolDefs, model, effectiveOpts)
		},
		RebuildContext: func(rebuildCtx context.Context) ([]providers.Message, error) {
			if compactErr := al.memory.ForceCompact(rebuildCtx, opts.SessionKey, opts.UserID, al.contextWindow); compactErr != nil {
				logger.WarnCF("agent", "Memory force compaction failed", map[string]interface{}{
					"error":       compactErr.Error(),
					"session_key": opts.SessionKey,
				})
				return nil, compactErr
			}
			rebuilt, rebuildErr := al.memory.BuildPromptContext(rebuildCtx, opts.SessionKey, opts.UserID, opts.UserMessage, al.contextWindow)
			if rebuildErr != nil {
				return nil, rebuildErr
			}
			rebuiltSystemPrompt, rebuiltMeta := al.contextBuilder.BuildSystemPromptWithMetadata()
			rebuiltMessages := al.contextBuilder.BuildMessagesWithSystemPrompt(
				rebuiltSystemPrompt,
				toProviderMessages(rebuilt.History),
				rebuilt.Summary,
				rebuilt.RecallPrompt,
				currentUserPrompt,
				nil,
				opts.Channel,
				opts.ChatID,
			)
			for _, note := range continuityNotes {
				if note == "" {
					continue
				}
				rebuiltMessages = injectSystemNote(rebuiltMessages, note)
			}
			if personaNote != "" {
				rebuiltMessages = injectSystemNote(rebuiltMessages, personaNote)
			}
			if !opts.NoHistory && al.detectPromptBaselineChange(opts.SessionKey, rebuiltMeta.Hash) {
				rebuiltMessages = injectSystemNote(rebuiltMessages,
					"System capabilities/bootstrap instructions changed since the previous turn. Use the latest tool list and identity constraints for this response.")
			}
			return rebuiltMessages, nil
		},
		Callbacks: tools.LoopCallbacks{
			OnTransientRetry: func(_ context.Context, info providers.RetryInfo) {
				logger.WarnCF("agent", "Transient LLM error detected, retrying", map[string]interface{}{
					"error":      info.Err.Error(),
					"retry":      info.Attempt,
					"backoff_ms": info.Delay.Milliseconds(),
				})
				if info.Attempt == 1 && !constants.IsInternalChannel(opts.Channel) && opts.SendResponse {
					al.publishOutbound(bus.OutboundMessage{
						Channel: opts.Channel,
						ChatID:  opts.ChatID,
						Content: "⚠️ Provider timeout/error detected. Retrying...",
					}, "provider_retry_notice")
				}
			},
			OnOverflowStage: func(_ context.Context, stage string, attempt int, maxAttempts int, stageErr error) {
				logger.WarnCF("agent", "Context overflow recovery stage", map[string]interface{}{
					"stage":          stage,
					"attempt":        attempt,
					"max_attempts":   maxAttempts,
					"error":          stageErr.Error(),
					"session_key":    opts.SessionKey,
					"channel":        opts.Channel,
					"chat_id":        opts.ChatID,
					"context_tokens": al.contextWindow,
				})
				if stage == "compact" && !overflowNoticeSent && !constants.IsInternalChannel(opts.Channel) && opts.SendResponse {
					overflowNoticeSent = true
					al.publishOutbound(bus.OutboundMessage{
						Channel: opts.Channel,
						ChatID:  opts.ChatID,
						Content: "⚠️ Context window exceeded. Compacting memory and retrying...",
					}, "context_retry_notice")
				}
			},
			OnAssistantTurn: func(writeCtx context.Context, response *providers.LLMResponse, promptEstimateTokens int, _ int) error {
				if opts.NoHistory {
					return nil
				}
				if response != nil && response.Usage != nil && response.Usage.PromptTokens > 0 {
					al.memory.ObservePromptUsage(writeCtx, al.model, promptEstimateTokens, response.Usage.PromptTokens)
				}
				if err := al.memory.AppendEvent(writeCtx, memory.Event{
					ID:         "evt-" + uuid.NewString(),
					SessionKey: opts.SessionKey,
					TurnID:     turnID,
					Seq:        seq,
					Role:       "assistant",
					Content:    response.Content,
					Metadata: map[string]string{
						"channel":         opts.Channel,
						"chat_id":         opts.ChatID,
						"user_id":         opts.UserID,
						"tool_call_count": fmt.Sprintf("%d", len(response.ToolCalls)),
					},
				}); err != nil {
					logger.ErrorCF("agent", "Failed to append assistant tool-call event", map[string]interface{}{
						"error":       err.Error(),
						"session_key": opts.SessionKey,
						"turn_id":     turnID,
					})
				}
				seq++
				return nil
			},
			OnToolResult: func(writeCtx context.Context, call providers.ToolCall, _ *tools.ToolResult, contentForLLM string, _ int) error {
				if opts.NoHistory {
					return nil
				}
				if err := al.memory.AppendEvent(writeCtx, memory.Event{
					ID:         "evt-" + uuid.NewString(),
					SessionKey: opts.SessionKey,
					TurnID:     turnID,
					Seq:        seq,
					Role:       "tool",
					Content:    contentForLLM,
					ToolCallID: call.ID,
					ToolName:   call.Name,
					Metadata: map[string]string{
						"channel": opts.Channel,
						"chat_id": opts.ChatID,
						"user_id": opts.UserID,
					},
				}); err != nil {
					logger.ErrorCF("agent", "Failed to append tool event", map[string]interface{}{
						"error":       err.Error(),
						"session_key": opts.SessionKey,
						"turn_id":     turnID,
						"tool_name":   call.Name,
					})
				}
				seq++
				return nil
			},
			OnToolUserMessage: func(_ context.Context, call providers.ToolCall, result *tools.ToolResult, _ int) {
				if result == nil || result.Silent || result.ForUser == "" || !opts.SendResponse {
					return
				}
				al.publishOutbound(bus.OutboundMessage{
					Channel: opts.Channel,
					ChatID:  opts.ChatID,
					Content: result.ForUser,
				}, "tool_result")
				logger.DebugCF("agent", "Sent tool result to user", map[string]interface{}{
					"tool":        call.Name,
					"content_len": len(result.ForUser),
				})
			},
			OnLoopBreak: func(metricCtx context.Context, reason string, _ int) {
				if opts.NoHistory {
					return
				}
				_ = al.memory.AddMetric(metricCtx, "tool.loop.breaker", 1, map[string]string{
					"session_key": opts.SessionKey,
					"user_id":     opts.UserID,
					"reason":      reason,
				})
			},
			OnLoopWarning: func(metricCtx context.Context, reason string, level string, count int, message string, _ int) {
				if opts.NoHistory {
					return
				}
				_ = al.memory.AddMetric(metricCtx, "tool.loop.warning", 1, map[string]string{
					"session_key": opts.SessionKey,
					"user_id":     opts.UserID,
					"reason":      reason,
					"level":       level,
				})
				logger.WarnCF("agent", "Tool loop warning", map[string]interface{}{
					"session_key": opts.SessionKey,
					"reason":      reason,
					"level":       level,
					"count":       count,
					"message":     message,
				})
			},
		},
	}, messages, opts.Channel, opts.ChatID)
	if err != nil {
		return "", err
	}
	finalContent := loopResult.Content
	iteration := loopResult.Iterations

	// If last tool had ForUser content and we already sent it, we might not need to send final response
	// This is controlled by the tool's Silent flag and ForUser content

	// 5. Handle empty response
	if finalContent == "" {
		finalContent = opts.DefaultResponse
	}
	if streamForwarder != nil && streamForwarder.FlushFinal(finalContent) {
		tools.MarkRoundMessageSent(ctx)
	}

	// 6. Save final assistant event and schedule memory maintenance
	if !opts.NoHistory {
		if err := al.memory.AppendEvent(ctx, memory.Event{
			ID:         "evt-" + uuid.NewString(),
			SessionKey: opts.SessionKey,
			TurnID:     turnID,
			Seq:        seq,
			Role:       "assistant",
			Content:    finalContent,
			Metadata: map[string]string{
				"channel": opts.Channel,
				"chat_id": opts.ChatID,
				"user_id": opts.UserID,
			},
		}); err != nil {
			logger.ErrorCF("agent", "Failed to append final assistant event", map[string]interface{}{
				"error":       err.Error(),
				"session_key": opts.SessionKey,
				"turn_id":     turnID,
			})
		}
		seq++
		if opts.EnableSummary {
			al.memory.ScheduleTurnMaintenance(ctx, opts.SessionKey, turnID, opts.UserID)
		}
	}

	// 7. Optional: send response via bus
	if opts.SendResponse {
		if streamForwarder != nil && streamForwarder.HasSent() {
			al.publishOutbound(bus.OutboundMessage{
				Channel:     opts.Channel,
				ChatID:      opts.ChatID,
				Content:     finalContent,
				Stream:      true,
				StreamID:    streamID,
				StreamFinal: true,
			}, "stream_final")
		} else {
			al.publishOutbound(bus.OutboundMessage{
				Channel: opts.Channel,
				ChatID:  opts.ChatID,
				Content: finalContent,
			}, "final_response")
		}
	}

	// 8. Log response
	responsePreview := utils.Truncate(finalContent, 120)
	logger.InfoCF("agent", fmt.Sprintf("Response: %s", responsePreview),
		map[string]interface{}{
			"session_key":  opts.SessionKey,
			"iterations":   iteration,
			"final_length": len(finalContent),
		})

	return finalContent, nil
}

// GetStartupInfo returns information about loaded tools and skills for logging.
func (al *AgentLoop) GetStartupInfo() map[string]interface{} {
	info := make(map[string]interface{})

	// Tools info
	tools := al.tools.List()
	toolSummaries := al.tools.GetSummaries()
	info["tools"] = map[string]interface{}{
		"count":     len(tools),
		"names":     tools,
		"summaries": toolSummaries,
	}

	// Skills info
	info["skills"] = al.contextBuilder.GetSkillsInfo()

	if al.toolpacks != nil {
		if packs, err := al.toolpacks.List(); err == nil {
			info["toolpacks"] = map[string]interface{}{
				"count": len(packs),
			}
		}
	}

	return info
}

// formatMessagesForLog formats messages for logging
func formatMessagesForLog(messages []providers.Message) string {
	if len(messages) == 0 {
		return "[]"
	}

	var result string
	result += "[\n"
	for i, msg := range messages {
		result += fmt.Sprintf("  [%d] Role: %s\n", i, msg.Role)
		if len(msg.ToolCalls) > 0 {
			result += "  ToolCalls:\n"
			for _, tc := range msg.ToolCalls {
				result += fmt.Sprintf("    - ID: %s, Type: %s, Name: %s\n", tc.ID, tc.Type, tc.Name)
				if tc.Function != nil {
					result += fmt.Sprintf("      Arguments: %s\n", utils.Truncate(tc.Function.Arguments, 200))
				}
			}
		}
		if msg.Content != "" {
			content := utils.Truncate(msg.Content, 200)
			result += fmt.Sprintf("  Content: %s\n", content)
		}
		if msg.ToolCallID != "" {
			result += fmt.Sprintf("  ToolCallID: %s\n", msg.ToolCallID)
		}
		result += "\n"
	}
	result += "]"
	return result
}

// formatToolsForLog formats tool definitions for logging
func formatToolsForLog(tools []providers.ToolDefinition) string {
	if len(tools) == 0 {
		return "[]"
	}

	var result string
	result += "[\n"
	for i, tool := range tools {
		result += fmt.Sprintf("  [%d] Type: %s, Name: %s\n", i, tool.Type, tool.Function.Name)
		result += fmt.Sprintf("      Description: %s\n", tool.Function.Description)
		if len(tool.Function.Parameters) > 0 {
			result += fmt.Sprintf("      Parameters: %s\n", utils.Truncate(fmt.Sprintf("%v", tool.Function.Parameters), 200))
		}
	}
	result += "]"
	return result
}

func parsePersonaCandidatesResponse(raw string) ([]memory.PersonaUpdateCandidate, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, true
	}

	type candidate struct {
		FieldPath  string  `json:"field_path"`
		Operation  string  `json:"operation"`
		Value      string  `json:"value"`
		Confidence float64 `json:"confidence"`
		Evidence   string  `json:"evidence"`
	}
	type envelope struct {
		Candidates []candidate `json:"candidates"`
	}

	parseCandidates := func(cands []candidate) []memory.PersonaUpdateCandidate {
		out := make([]memory.PersonaUpdateCandidate, 0, len(cands))
		for _, c := range cands {
			field := strings.ToLower(strings.TrimSpace(c.FieldPath))
			op := strings.ToLower(strings.TrimSpace(c.Operation))
			if field == "" || op == "" {
				continue
			}
			if op != "set" && op != "append" && op != "delete" {
				continue
			}
			conf := c.Confidence
			if conf <= 0 {
				conf = 0.6
			}
			if conf > 1 {
				conf = 1
			}
			out = append(out, memory.PersonaUpdateCandidate{
				FieldPath:  field,
				Operation:  op,
				Value:      strings.TrimSpace(c.Value),
				Confidence: conf,
				Evidence:   strings.TrimSpace(c.Evidence),
				Source:     "llm",
			})
		}
		return out
	}

	var env envelope
	if err := json.Unmarshal([]byte(raw), &env); err == nil && len(env.Candidates) > 0 {
		return parseCandidates(env.Candidates), true
	}

	var arr []candidate
	if err := json.Unmarshal([]byte(raw), &arr); err == nil && len(arr) > 0 {
		return parseCandidates(arr), true
	}

	// Best effort extraction from markdown code fences or mixed output.
	start := strings.IndexAny(raw, "[{")
	end := strings.LastIndexAny(raw, "]}")
	if start >= 0 && end > start {
		candidateRaw := raw[start : end+1]
		if err := json.Unmarshal([]byte(candidateRaw), &env); err == nil && len(env.Candidates) > 0 {
			return parseCandidates(env.Candidates), true
		}
		if err := json.Unmarshal([]byte(candidateRaw), &arr); err == nil && len(arr) > 0 {
			return parseCandidates(arr), true
		}
	}

	return nil, false
}

func toProviderMessages(messages []memory.Message) []providers.Message {
	if len(messages) == 0 {
		return nil
	}
	out := make([]providers.Message, 0, len(messages))
	for _, m := range messages {
		out = append(out, providers.Message{
			Role:       m.Role,
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
		})
	}
	return out
}

func (al *AgentLoop) publishOutbound(msg bus.OutboundMessage, source string) {
	if al.bus == nil {
		logger.WarnCF("agent", "Message bus unavailable for outbound publish", map[string]interface{}{
			"source": source,
		})
		return
	}
	if err := al.bus.PublishOutbound(msg); err != nil {
		logger.WarnCF("agent", "Failed to publish outbound message", map[string]interface{}{
			"source":  source,
			"channel": msg.Channel,
			"chat_id": msg.ChatID,
			"error":   err.Error(),
		})
	}
}

func injectSystemNote(messages []providers.Message, note string) []providers.Message {
	note = strings.TrimSpace(note)
	if note == "" || len(messages) == 0 {
		return messages
	}
	insertAt := 0
	for insertAt < len(messages) && messages[insertAt].Role == "system" {
		insertAt++
	}
	out := make([]providers.Message, 0, len(messages)+1)
	out = append(out, messages[:insertAt]...)
	out = append(out, providers.Message{
		Role:    "system",
		Content: note,
	})
	out = append(out, messages[insertAt:]...)
	return out
}

func buildDegradedContinuitySystemNote(reasons []string) string {
	reasons = dedupeAndTrim(reasons)
	if len(reasons) == 0 {
		return ""
	}
	return strings.TrimSpace(fmt.Sprintf(`## Continuity Notice
Some prior-thread context is temporarily unavailable (%s).
Continue using the current turn, available history, and recalled memory. If exact prior details are needed, ask a brief clarifying question before making assumptions.`,
		strings.Join(reasons, ", ")))
}

func buildCompactionContinuationSystemNote(notes []string) string {
	notes = dedupeAndTrim(notes)
	if len(notes) == 0 {
		return ""
	}
	lines := []string{
		"## Continuation Constraints",
		"Maintain task continuity across prior compactions. Preserve these handoff notes unless superseded by newer evidence:",
	}
	for i, note := range notes {
		if i >= 4 {
			break
		}
		lines = append(lines, "- "+note)
	}
	return strings.Join(lines, "\n")
}

func dedupeAndTrim(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

func buildPersonaDecisionSystemNote(report memory.PersonaApplyReport) string {
	if len(report.Decisions) == 0 {
		return ""
	}
	accepted := report.AcceptedCount()
	rejected := report.RejectedCount()
	deferred := report.DeferredCount()
	lines := []string{
		"## Persona Mutation Decisions (Current Turn)",
		fmt.Sprintf("- Accepted: %d", accepted),
		fmt.Sprintf("- Rejected: %d", rejected),
		fmt.Sprintf("- Deferred: %d", deferred),
	}
	if accepted > 0 {
		lines = append(lines, "- Accepted persona updates are active in local context for this turn.")
	}
	if rejected > 0 || deferred > 0 {
		lines = append(lines,
			"- Do not claim rejected/deferred persona changes were applied.",
			"- If asked about rejected changes, explain they were not persisted due to policy/evidence checks.",
			"- If requested behavior is blocked despite accepted updates, attribute that to model/provider constraints.",
		)
	}
	return strings.Join(lines, "\n")
}

func valueOr(v, fallback string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return fallback
	}
	return v
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func resolveRuntimeContextWindow(provider providers.LLMProvider, model string, configured int) int {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	resolved, source := providers.ResolveContextWindow(ctx, provider, model, configured)
	if resolved < 32768 {
		logger.WarnCF("agent", "Context window is below recommended threshold", map[string]interface{}{
			"model":           model,
			"resolved":        resolved,
			"source":          source,
			"recommended_min": 32768,
		})
	} else {
		logger.InfoCF("agent", "Resolved context window", map[string]interface{}{
			"model":  model,
			"tokens": resolved,
			"source": source,
		})
	}
	return resolved
}

func cloneLLMCallOptions(opts map[string]interface{}) map[string]interface{} {
	if len(opts) == 0 {
		return map[string]interface{}{}
	}
	out := make(map[string]interface{}, len(opts)+2)
	for k, v := range opts {
		out[k] = v
	}
	return out
}

type llmStreamForwarder struct {
	publishFn func(string)

	pending   strings.Builder
	emitted   strings.Builder
	lastFlush time.Time
	sentAny   bool
}

func newLLMStreamForwarder(publishFn func(string)) *llmStreamForwarder {
	if publishFn == nil {
		return nil
	}
	return &llmStreamForwarder{publishFn: publishFn}
}

func (f *llmStreamForwarder) Push(delta string) {
	if f == nil {
		return
	}
	delta = strings.TrimRight(delta, "\r")
	if delta == "" {
		return
	}
	f.pending.WriteString(delta)
	flushBySize := f.pending.Len() >= 72
	flushByTime := !f.lastFlush.IsZero() && time.Since(f.lastFlush) >= 1200*time.Millisecond
	flushByBoundary := strings.Contains(delta, "\n")
	if flushBySize || flushByTime || flushByBoundary {
		f.flushPending()
	}
}

func (f *llmStreamForwarder) FlushFinal(final string) bool {
	if f == nil {
		return false
	}
	f.flushPending()
	if strings.TrimSpace(final) == "" {
		return f.sentAny
	}

	emitted := f.emitted.String()
	if emitted == final {
		return f.sentAny
	}
	prefix := commonPrefixLen(emitted, final)
	suffix := final[prefix:]
	if strings.TrimSpace(suffix) == "" {
		return f.sentAny
	}
	f.publish(suffix)
	f.emitted.WriteString(final[prefix:])
	return true
}

func (f *llmStreamForwarder) HasSent() bool {
	if f == nil {
		return false
	}
	return f.sentAny
}

func (f *llmStreamForwarder) flushPending() {
	if f == nil || f.pending.Len() == 0 {
		return
	}
	chunk := f.pending.String()
	f.pending.Reset()
	if strings.TrimSpace(chunk) == "" {
		return
	}
	f.publish(chunk)
	f.emitted.WriteString(chunk)
}

func (f *llmStreamForwarder) publish(chunk string) {
	if f == nil || f.publishFn == nil {
		return
	}
	f.publishFn(chunk)
	f.sentAny = true
	f.lastFlush = time.Now()
}

func commonPrefixLen(a, b string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

func (al *AgentLoop) resolveCommandSessionKey(msg bus.InboundMessage, userID string) string {
	// Commands should target the same canonical session key used by normal turns.
	if sk, err := resolveSessionKey(msg.SessionKey, al.workspaceID, msg.Channel, msg.ChatID, userID); err == nil {
		return sk
	}
	return strings.TrimSpace(msg.SessionKey)
}

func historyEndsWithUserMessage(history []providers.Message, content string) bool {
	content = strings.TrimSpace(content)
	if content == "" {
		return false
	}
	for i := len(history) - 1; i >= 0; i-- {
		role := strings.ToLower(strings.TrimSpace(history[i].Role))
		if role == "tool" {
			continue
		}
		if role != "user" {
			return false
		}
		return strings.TrimSpace(history[i].Content) == content
	}
	return false
}

func (al *AgentLoop) handleCommand(ctx context.Context, msg bus.InboundMessage) (string, bool) {
	content := strings.TrimSpace(msg.Content)
	if !strings.HasPrefix(content, "/") {
		return "", false
	}

	parts := strings.Fields(content)
	if len(parts) == 0 {
		return "", false
	}

	cmd := parts[0]
	args := parts[1:]

	switch cmd {
	case "/show":
		if len(args) < 1 {
			return "Usage: /show [model|channel]", true
		}
		switch args[0] {
		case "model":
			return fmt.Sprintf("Current model: %s", al.model), true
		case "channel":
			return fmt.Sprintf("Current channel: %s", msg.Channel), true
		default:
			return fmt.Sprintf("Unknown show target: %s", args[0]), true
		}

	case "/list":
		if len(args) < 1 {
			return "Usage: /list [models|channels]", true
		}
		switch args[0] {
		case "models":
			return fmt.Sprintf("Model is configured via config/env. Provider: %s. Provider default: %s", al.providerName, al.provider.GetDefaultModel()), true
		case "channels":
			if al.channelManager == nil {
				return "Channel manager not initialized", true
			}
			channels := al.channelManager.GetEnabledChannels()
			if len(channels) == 0 {
				return "No channels enabled", true
			}
			return fmt.Sprintf("Enabled channels: %s", strings.Join(channels, ", ")), true
		default:
			return fmt.Sprintf("Unknown list target: %s", args[0]), true
		}

	case "/switch":
		if len(args) < 3 || args[1] != "to" {
			return "Usage: /switch [model|channel] to <name>", true
		}
		target := args[0]
		value := args[2]

		switch target {
		case "model":
			oldModel := al.model
			al.model = value
			return fmt.Sprintf("Switched model from %s to %s", oldModel, value), true
		case "channel":
			// This changes the 'default' channel for some operations, or effectively redirects output?
			// For now, let's just validate if the channel exists
			if al.channelManager == nil {
				return "Channel manager not initialized", true
			}
			if _, exists := al.channelManager.GetChannel(value); !exists && value != "cli" {
				return fmt.Sprintf("Channel '%s' not found or not enabled", value), true
			}

			// If message came from CLI, maybe we want to redirect CLI output to this channel?
			// That would require state persistence about "redirected channel"
			// For now, just acknowledged.
			return fmt.Sprintf("Switched target channel to %s (Note: this currently only validates existence)", value), true
		default:
			return fmt.Sprintf("Unknown switch target: %s", target), true
		}

	case "/persona":
		if len(args) < 1 {
			return "Usage: /persona [show|revisions|candidates|rollback]", true
		}
		userID := strings.TrimSpace(msg.SenderID)
		if userID == "" {
			userID = "local-user"
		}
		resolvedSessionKey := al.resolveCommandSessionKey(msg, userID)
		switch args[0] {
		case "show":
			profile, err := al.memory.GetPersonaProfile(ctx, userID)
			if err != nil {
				return fmt.Sprintf("Failed to load persona profile: %v", err), true
			}
			return fmt.Sprintf(
				"Persona revision %d\n- Agent name: %s\n- Role: %s\n- Purpose: %s\n- User name: %s\n- User timezone: %s\n- User location: %s\n- User language: %s",
				profile.Revision,
				valueOr(profile.Identity.AgentName, "(unset)"),
				valueOr(profile.Identity.Role, "(unset)"),
				valueOr(profile.Identity.Purpose, "(unset)"),
				valueOr(profile.User.Name, "(unset)"),
				valueOr(profile.User.Timezone, "(unset)"),
				valueOr(profile.User.Location, "(unset)"),
				valueOr(profile.User.Language, "(unset)"),
			), true
		case "revisions":
			revs, err := al.memory.ListPersonaRevisions(ctx, userID, 10)
			if err != nil {
				return fmt.Sprintf("Failed to list persona revisions: %v", err), true
			}
			if len(revs) == 0 {
				return "No persona revisions found.", true
			}
			lines := []string{"Recent persona revisions:"}
			for i, rev := range revs {
				if i >= 10 {
					break
				}
				lines = append(lines, fmt.Sprintf("- %s %s %s -> %s (%s)", rev.ID, rev.FieldPath, rev.Operation, valueOr(rev.NewValue, "(empty)"), valueOr(rev.Source, "unknown")))
			}
			return strings.Join(lines, "\n"), true
		case "candidates":
			status := ""
			if len(args) > 1 {
				status = strings.TrimSpace(args[1])
			}
			cands, err := al.memory.ListPersonaCandidates(ctx, userID, resolvedSessionKey, "", status, 20)
			if err != nil {
				return fmt.Sprintf("Failed to list persona candidates: %v", err), true
			}
			if len(cands) == 0 {
				return "No persona candidates found.", true
			}
			lines := []string{"Recent persona candidates:"}
			for _, c := range cands {
				lines = append(lines, fmt.Sprintf("- %s %s=%s (%s, %.2f)", c.FieldPath, c.Operation, valueOr(c.Value, "(empty)"), c.Status, c.Confidence))
			}
			return strings.Join(lines, "\n"), true
		case "rollback":
			if err := al.memory.RollbackPersona(ctx, userID); err != nil {
				return fmt.Sprintf("Failed to rollback persona: %v", err), true
			}
			return "Rolled back the most recent persona revision.", true
		default:
			return "Usage: /persona [show|revisions|candidates|rollback]", true
		}
	}

	return "", false
}
