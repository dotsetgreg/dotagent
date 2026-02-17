// DotAgent - Ultra-lightweight personal AI agent
// Inspired by and based on nanobot: https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 DotAgent contributors

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/dotsetgreg/dotagent/pkg/bus"
	"github.com/dotsetgreg/dotagent/pkg/channels"
	"github.com/dotsetgreg/dotagent/pkg/config"
	"github.com/dotsetgreg/dotagent/pkg/constants"
	"github.com/dotsetgreg/dotagent/pkg/logger"
	"github.com/dotsetgreg/dotagent/pkg/memory"
	"github.com/dotsetgreg/dotagent/pkg/providers"
	"github.com/dotsetgreg/dotagent/pkg/state"
	"github.com/dotsetgreg/dotagent/pkg/tools"
	"github.com/dotsetgreg/dotagent/pkg/utils"
	"github.com/google/uuid"
)

type AgentLoop struct {
	bus            *bus.MessageBus
	provider       providers.LLMProvider
	workspace      string
	workspaceID    string
	model          string
	contextWindow  int // Maximum context window size in tokens
	maxIterations  int
	memory         *memory.Service
	state          *state.Manager
	contextBuilder *ContextBuilder
	tools          *tools.ToolRegistry
	running        atomic.Bool
	channelManager *channels.Manager
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
	NoHistory       bool   // If true, don't load session history (for heartbeat)
}

// createToolRegistry creates a tool registry with common tools.
// This is shared between main agent and subagents.
func createToolRegistry(workspace string, restrict bool, cfg *config.Config, msgBus *bus.MessageBus) *tools.ToolRegistry {
	registry := tools.NewToolRegistry()

	// File system tools
	registry.Register(tools.NewReadFileTool(workspace, restrict))
	registry.Register(tools.NewWriteFileTool(workspace, restrict))
	registry.Register(tools.NewListDirTool(workspace, restrict))
	registry.Register(tools.NewEditFileTool(workspace, restrict))
	registry.Register(tools.NewAppendFileTool(workspace, restrict))

	// Shell execution
	registry.Register(tools.NewExecTool(workspace, restrict))

	if searchTool := tools.NewWebSearchTool(tools.WebSearchToolOptions{
		BraveAPIKey:          cfg.Tools.Web.Brave.APIKey,
		BraveMaxResults:      cfg.Tools.Web.Brave.MaxResults,
		BraveEnabled:         cfg.Tools.Web.Brave.Enabled,
		DuckDuckGoMaxResults: cfg.Tools.Web.DuckDuckGo.MaxResults,
		DuckDuckGoEnabled:    cfg.Tools.Web.DuckDuckGo.Enabled,
	}); searchTool != nil {
		registry.Register(searchTool)
	}
	registry.Register(tools.NewWebFetchTool(50000))

	// Hardware tools (I2C, SPI) - Linux only, returns error on other platforms
	registry.Register(tools.NewI2CTool())
	registry.Register(tools.NewSPITool())

	// Message tool - available to both agent and subagent
	// Subagent uses it to communicate directly with user
	messageTool := tools.NewMessageTool()
	messageTool.SetSendCallback(func(channel, chatID, content string) error {
		msgBus.PublishOutbound(bus.OutboundMessage{
			Channel: channel,
			ChatID:  chatID,
			Content: content,
		})
		return nil
	})
	registry.Register(messageTool)

	return registry
}

func NewAgentLoop(cfg *config.Config, msgBus *bus.MessageBus, provider providers.LLMProvider) (*AgentLoop, error) {
	workspace := cfg.WorkspacePath()
	os.MkdirAll(workspace, 0755)

	restrict := cfg.Agents.Defaults.RestrictToWorkspace

	// Create tool registry for main agent
	toolsRegistry := createToolRegistry(workspace, restrict, cfg, msgBus)

	// Create subagent manager with its own tool registry
	subagentManager := tools.NewSubagentManager(provider, cfg.Agents.Defaults.Model, workspace, msgBus)
	subagentTools := createToolRegistry(workspace, restrict, cfg, msgBus)
	// Subagent doesn't need spawn/subagent tools to avoid recursion
	subagentManager.SetTools(subagentTools)

	// Register spawn tool (for main agent)
	spawnTool := tools.NewSpawnTool(subagentManager)
	toolsRegistry.Register(spawnTool)

	// Register subagent tool (synchronous execution)
	subagentTool := tools.NewSubagentTool(subagentManager)
	toolsRegistry.Register(subagentTool)

	// Create state manager for atomic state persistence
	stateManager := state.NewManager(workspace)

	// Create context builder and set tools registry
	contextBuilder := NewContextBuilder(workspace)
	contextBuilder.SetToolsRegistry(toolsRegistry)

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
		return parsePersonaCandidatesResponse(resp.Content), nil
	}

	memSvc, err := memory.NewService(memory.Config{
		Workspace:            workspace,
		AgentID:              "dotagent",
		EmbeddingModel:       cfg.Memory.EmbeddingModel,
		MaxContextTokens:     cfg.Agents.Defaults.MaxTokens,
		MaxRecallItems:       cfg.Memory.MaxRecallItems,
		CandidateLimit:       cfg.Memory.CandidateLimit,
		RetrievalCache:       time.Duration(cfg.Memory.RetrievalCacheSeconds) * time.Second,
		WorkerLease:          time.Duration(cfg.Memory.WorkerLeaseSeconds) * time.Second,
		WorkerPoll:           time.Duration(cfg.Memory.WorkerPollMS) * time.Millisecond,
		EventRetention:       time.Duration(cfg.Memory.EventRetentionDays) * 24 * time.Hour,
		AuditRetention:       time.Duration(cfg.Memory.AuditRetentionDays) * 24 * time.Hour,
		PersonaCardTokens:    480,
		PersonaExtractor:     personaExtractFn,
		PersonaSyncApply:     cfg.Memory.PersonaSyncApply,
		PersonaFileSync:      memory.NormalizePersonaFileSyncMode(cfg.Memory.PersonaFileSyncMode),
		PersonaPolicyMode:    cfg.Memory.PersonaPolicyMode,
		PersonaMinConfidence: cfg.Memory.PersonaMinConfidence,
	}, summarizeFn)
	if err != nil {
		return nil, fmt.Errorf("initialize memory service: %w", err)
	}

	return &AgentLoop{
		bus:            msgBus,
		provider:       provider,
		workspace:      workspace,
		workspaceID:    workspaceNamespace(workspace),
		model:          cfg.Agents.Defaults.Model,
		contextWindow:  cfg.Agents.Defaults.MaxTokens, // Restore context window for summarization
		maxIterations:  cfg.Agents.Defaults.MaxToolIterations,
		memory:         memSvc,
		state:          stateManager,
		contextBuilder: contextBuilder,
		tools:          toolsRegistry,
	}, nil
}

func (al *AgentLoop) Run(ctx context.Context) error {
	al.running.Store(true)

	for al.running.Load() {
		select {
		case <-ctx.Done():
			return nil
		default:
			msg, ok := al.bus.ConsumeInbound(ctx)
			if !ok {
				continue
			}

			response, err := al.processMessage(ctx, msg)
			if err != nil {
				response = fmt.Sprintf("Error processing message: %v", err)
			}

			if response != "" {
				// Check if the message tool already sent a response during this round.
				// If so, skip publishing to avoid duplicate messages to the user.
				alreadySent := false
				if tool, ok := al.tools.Get("message"); ok {
					if mt, ok := tool.(*tools.MessageTool); ok {
						alreadySent = mt.HasSentInRound()
					}
				}

				if !alreadySent {
					al.bus.PublishOutbound(bus.OutboundMessage{
						Channel: msg.Channel,
						ChatID:  msg.ChatID,
						Content: response,
					})
				}
			}
		}
	}

	return nil
}

func (al *AgentLoop) Stop() {
	al.running.Store(false)
	if al.memory != nil {
		_ = al.memory.Close()
	}
}

func (al *AgentLoop) RegisterTool(tool tools.Tool) {
	al.tools.Register(tool)
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
	if idx := strings.Index(msg.ChatID, ":"); idx > 0 {
		originChannel = msg.ChatID[:idx]
	} else {
		// Fallback
		originChannel = "cli"
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

	// Agent only logs, does not respond to user
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

	// 1. Update tool contexts
	al.updateToolContexts(opts.Channel, opts.ChatID)

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

	// 1.5 Ensure memory session exists
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

		if recordedUserTurn {
			report, applyErr := al.memory.ApplyPersonaDirectivesSync(ctx, opts.SessionKey, turnID, opts.UserID)
			if applyErr != nil {
				logger.WarnCF("agent", "Synchronous persona apply failed", map[string]interface{}{
					"error":       applyErr.Error(),
					"session_key": opts.SessionKey,
					"turn_id":     turnID,
				})
			} else {
				syncPersonaReport = report
			}
		}
	}

	// 3. Build messages (skip history for heartbeat)
	var history []providers.Message
	var summary string
	var recall string
	if !opts.NoHistory {
		promptCtx, err := al.memory.BuildPromptContext(ctx, opts.SessionKey, opts.UserID, opts.UserMessage, al.contextWindow)
		if err != nil {
			if errors.Is(err, memory.ErrContinuityUnavailable) {
				_ = al.memory.AddMetric(ctx, "memory.context.fail_closed", 1, map[string]string{
					"session_key": opts.SessionKey,
					"user_id":     opts.UserID,
				})
				return "I can’t safely continue this thread right now because prior context is temporarily unavailable. Please retry in a moment.", nil
			}
			logger.WarnCF("agent", "Failed to build memory prompt context", map[string]interface{}{"error": err.Error(), "session_key": opts.SessionKey})
		} else {
			history = toProviderMessages(promptCtx.History)
			summary = promptCtx.Summary
			recall = promptCtx.RecallPrompt
		}
	}
	currentUserPrompt := opts.UserMessage
	if !opts.NoHistory && recordedUserTurn && historyContainsUserMessage(history, opts.UserMessage) {
		// Current user turn is already in persisted history; avoid duplicate copy.
		currentUserPrompt = ""
	}
	messages := al.contextBuilder.BuildMessages(
		history,
		summary,
		recall,
		currentUserPrompt,
		nil,
		opts.Channel,
		opts.ChatID,
	)
	if note := buildPersonaDecisionSystemNote(syncPersonaReport); note != "" {
		messages = injectSystemNote(messages, note)
	}

	// 4. Run LLM iteration loop
	finalContent, iteration, err := al.runLLMIteration(ctx, messages, opts, turnID, &seq, currentUserPrompt)
	if err != nil {
		return "", err
	}

	// If last tool had ForUser content and we already sent it, we might not need to send final response
	// This is controlled by the tool's Silent flag and ForUser content

	// 5. Handle empty response
	if finalContent == "" {
		finalContent = opts.DefaultResponse
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
		al.bus.PublishOutbound(bus.OutboundMessage{
			Channel: opts.Channel,
			ChatID:  opts.ChatID,
			Content: finalContent,
		})
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

// runLLMIteration executes the LLM call loop with tool handling.
// Returns the final content, iteration count, and any error.
func (al *AgentLoop) runLLMIteration(ctx context.Context, messages []providers.Message, opts processOptions, turnID string, seq *int, currentUserPrompt string) (string, int, error) {
	iteration := 0
	var finalContent string
	providerStateID := ""
	if !opts.NoHistory {
		if sid, err := al.memory.GetProviderState(ctx, opts.SessionKey); err == nil {
			providerStateID = strings.TrimSpace(sid)
		}
	}

	for iteration < al.maxIterations {
		iteration++

		logger.DebugCF("agent", "LLM iteration",
			map[string]interface{}{
				"iteration": iteration,
				"max":       al.maxIterations,
			})

		// Build tool definitions
		providerToolDefs := al.tools.ToProviderDefs()

		// Log LLM request details
		logger.DebugCF("agent", "LLM request",
			map[string]interface{}{
				"iteration":         iteration,
				"model":             al.model,
				"messages_count":    len(messages),
				"tools_count":       len(providerToolDefs),
				"max_tokens":        8192,
				"temperature":       0.7,
				"system_prompt_len": len(messages[0].Content),
			})

		// Log full messages (detailed)
		logger.DebugCF("agent", "Full LLM request",
			map[string]interface{}{
				"iteration":     iteration,
				"messages_json": formatMessagesForLog(messages),
				"tools_json":    formatToolsForLog(providerToolDefs),
			})

		var response *providers.LLMResponse
		var err error

		// Retry loop for context/token errors
		maxRetries := 2
		for retry := 0; retry <= maxRetries; retry++ {
			callOpts := map[string]interface{}{
				"max_tokens":  8192,
				"temperature": 0.7,
			}
			if stateful, ok := al.provider.(providers.StatefulLLMProvider); ok && !opts.NoHistory {
				var newState string
				response, newState, err = stateful.ChatWithState(ctx, providerStateID, messages, providerToolDefs, al.model, callOpts)
				if err == nil {
					newState = strings.TrimSpace(newState)
					if newState != "" && newState != providerStateID {
						providerStateID = newState
						_ = al.memory.SetProviderState(ctx, opts.SessionKey, newState)
					}
				}
			} else {
				response, err = al.provider.Chat(ctx, messages, providerToolDefs, al.model, callOpts)
			}

			if err == nil {
				break // Success
			}

			errMsg := strings.ToLower(err.Error())
			// Check for context window errors (provider specific, but usually contain "token" or "invalid")
			isContextError := strings.Contains(errMsg, "token") ||
				strings.Contains(errMsg, "context") ||
				strings.Contains(errMsg, "invalidparameter") ||
				strings.Contains(errMsg, "length")

			if isContextError && retry < maxRetries {
				logger.WarnCF("agent", "Context window error detected, attempting compression", map[string]interface{}{
					"error": err.Error(),
					"retry": retry,
				})

				// Notify user on first retry only
				if retry == 0 && !constants.IsInternalChannel(opts.Channel) && opts.SendResponse {
					al.bus.PublishOutbound(bus.OutboundMessage{
						Channel: opts.Channel,
						ChatID:  opts.ChatID,
						Content: "⚠️ Context window exceeded. Compacting memory and retrying...",
					})
				}

				if err := al.memory.ForceCompact(ctx, opts.SessionKey, opts.UserID, al.contextWindow); err != nil {
					logger.WarnCF("agent", "Memory force compaction failed", map[string]interface{}{"error": err.Error(), "session_key": opts.SessionKey})
				}
				rebuilt, bErr := al.memory.BuildPromptContext(ctx, opts.SessionKey, opts.UserID, opts.UserMessage, al.contextWindow)
				if bErr != nil {
					logger.WarnCF("agent", "Failed rebuilding prompt context after compaction", map[string]interface{}{"error": bErr.Error()})
				} else {
					messages = al.contextBuilder.BuildMessages(
						toProviderMessages(rebuilt.History),
						rebuilt.Summary,
						rebuilt.RecallPrompt,
						currentUserPrompt,
						nil,
						opts.Channel,
						opts.ChatID,
					)
				}

				continue
			}

			// Real error or success, break loop
			break
		}

		if err != nil {
			logger.ErrorCF("agent", "LLM call failed",
				map[string]interface{}{
					"iteration": iteration,
					"error":     err.Error(),
				})
			return "", iteration, fmt.Errorf("LLM call failed after retries: %w", err)
		}

		// Check if no tool calls - we're done
		if len(response.ToolCalls) == 0 {
			finalContent = strings.TrimSpace(response.Content)
			logger.InfoCF("agent", "LLM response without tool calls (direct answer)",
				map[string]interface{}{
					"iteration":     iteration,
					"content_chars": len(finalContent),
				})
			break
		}

		// Log tool calls
		toolNames := make([]string, 0, len(response.ToolCalls))
		for _, tc := range response.ToolCalls {
			toolNames = append(toolNames, tc.Name)
		}
		logger.InfoCF("agent", "LLM requested tool calls",
			map[string]interface{}{
				"tools":     toolNames,
				"count":     len(response.ToolCalls),
				"iteration": iteration,
			})

		// Build assistant message with tool calls
		assistantMsg := providers.Message{
			Role:    "assistant",
			Content: response.Content,
		}
		for _, tc := range response.ToolCalls {
			argumentsJSON, _ := json.Marshal(tc.Arguments)
			assistantMsg.ToolCalls = append(assistantMsg.ToolCalls, providers.ToolCall{
				ID:   tc.ID,
				Type: "function",
				Function: &providers.FunctionCall{
					Name:      tc.Name,
					Arguments: string(argumentsJSON),
				},
			})
		}
		messages = append(messages, assistantMsg)

		if !opts.NoHistory {
			if err := al.memory.AppendEvent(ctx, memory.Event{
				ID:         "evt-" + uuid.NewString(),
				SessionKey: opts.SessionKey,
				TurnID:     turnID,
				Seq:        *seq,
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
			*seq = *seq + 1
		}

		// Execute tool calls
		for _, tc := range response.ToolCalls {
			// Log tool call with arguments preview
			argsJSON, _ := json.Marshal(tc.Arguments)
			argsPreview := utils.Truncate(string(argsJSON), 200)
			logger.InfoCF("agent", fmt.Sprintf("Tool call: %s(%s)", tc.Name, argsPreview),
				map[string]interface{}{
					"tool":      tc.Name,
					"iteration": iteration,
				})

			// Create async callback for tools that implement AsyncTool.
			// Async tools do not send directly to users. They notify the agent via
			// PublishInbound, and processSystemMessage decides whether to forward output.
			asyncCallback := func(callbackCtx context.Context, result *tools.ToolResult) {
				// Log the async completion but don't send directly to user
				// The agent will handle user notification via processSystemMessage
				if !result.Silent && result.ForUser != "" {
					logger.InfoCF("agent", "Async tool completed, agent will handle notification",
						map[string]interface{}{
							"tool":        tc.Name,
							"content_len": len(result.ForUser),
						})
				}
			}

			toolResult := al.tools.ExecuteWithContext(ctx, tc.Name, tc.Arguments, opts.Channel, opts.ChatID, asyncCallback)

			// Send ForUser content to user immediately if not Silent
			if !toolResult.Silent && toolResult.ForUser != "" && opts.SendResponse {
				al.bus.PublishOutbound(bus.OutboundMessage{
					Channel: opts.Channel,
					ChatID:  opts.ChatID,
					Content: toolResult.ForUser,
				})
				logger.DebugCF("agent", "Sent tool result to user",
					map[string]interface{}{
						"tool":        tc.Name,
						"content_len": len(toolResult.ForUser),
					})
			}

			// Determine content for LLM based on tool result
			contentForLLM := toolResult.ForLLM
			if contentForLLM == "" && toolResult.Err != nil {
				contentForLLM = toolResult.Err.Error()
			}

			toolResultMsg := providers.Message{
				Role:       "tool",
				Content:    contentForLLM,
				ToolCallID: tc.ID,
			}
			messages = append(messages, toolResultMsg)

			if !opts.NoHistory {
				if err := al.memory.AppendEvent(ctx, memory.Event{
					ID:         "evt-" + uuid.NewString(),
					SessionKey: opts.SessionKey,
					TurnID:     turnID,
					Seq:        *seq,
					Role:       "tool",
					Content:    contentForLLM,
					ToolCallID: tc.ID,
					ToolName:   tc.Name,
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
						"tool_name":   tc.Name,
					})
				}
				*seq = *seq + 1
			}
		}
	}

	return finalContent, iteration, nil
}

// updateToolContexts updates the context for tools that need channel/chatID info.
func (al *AgentLoop) updateToolContexts(channel, chatID string) {
	// Use ContextualTool interface instead of type assertions
	if tool, ok := al.tools.Get("message"); ok {
		if mt, ok := tool.(tools.ContextualTool); ok {
			mt.SetContext(channel, chatID)
		}
	}
	if tool, ok := al.tools.Get("spawn"); ok {
		if st, ok := tool.(tools.ContextualTool); ok {
			st.SetContext(channel, chatID)
		}
	}
	if tool, ok := al.tools.Get("subagent"); ok {
		if st, ok := tool.(tools.ContextualTool); ok {
			st.SetContext(channel, chatID)
		}
	}
}

// GetStartupInfo returns information about loaded tools and skills for logging.
func (al *AgentLoop) GetStartupInfo() map[string]interface{} {
	info := make(map[string]interface{})

	// Tools info
	tools := al.tools.List()
	info["tools"] = map[string]interface{}{
		"count": len(tools),
		"names": tools,
	}

	// Skills info
	info["skills"] = al.contextBuilder.GetSkillsInfo()

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

func parsePersonaCandidatesResponse(raw string) []memory.PersonaUpdateCandidate {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
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
		return parseCandidates(env.Candidates)
	}

	var arr []candidate
	if err := json.Unmarshal([]byte(raw), &arr); err == nil && len(arr) > 0 {
		return parseCandidates(arr)
	}

	// Best effort extraction from markdown code fences or mixed output.
	start := strings.IndexAny(raw, "[{")
	end := strings.LastIndexAny(raw, "]}")
	if start >= 0 && end > start {
		candidateRaw := raw[start : end+1]
		if err := json.Unmarshal([]byte(candidateRaw), &env); err == nil && len(env.Candidates) > 0 {
			return parseCandidates(env.Candidates)
		}
		if err := json.Unmarshal([]byte(candidateRaw), &arr); err == nil && len(arr) > 0 {
			return parseCandidates(arr)
		}
	}

	return nil
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

func historyContainsUserMessage(history []providers.Message, content string) bool {
	content = strings.TrimSpace(content)
	if content == "" {
		return false
	}
	for _, m := range history {
		if m.Role == "user" && strings.TrimSpace(m.Content) == content {
			return true
		}
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
			return "OpenRouter model is configured via config/env. Current default: openai/gpt-5.2", true
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
		resolvedSessionKey := strings.TrimSpace(msg.SessionKey)
		if resolvedSessionKey == "" {
			if sk, err := resolveSessionKey(msg.SessionKey, al.workspaceID, msg.Channel, msg.ChatID, userID); err == nil {
				resolvedSessionKey = sk
			}
		}
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
