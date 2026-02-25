package tools

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dotsetgreg/dotagent/pkg/logger"
	"github.com/dotsetgreg/dotagent/pkg/providers"
	"github.com/dotsetgreg/dotagent/pkg/utils"
)

// LoopCallbacks allows callers to inject persistence/notification side-effects.
type LoopCallbacks struct {
	OnTransientRetry  func(ctx context.Context, info providers.RetryInfo)
	OnOverflowStage   func(ctx context.Context, stage string, attempt int, maxAttempts int, err error)
	OnAssistantTurn   func(ctx context.Context, response *providers.LLMResponse, promptEstimateTokens int, iteration int) error
	OnToolResult      func(ctx context.Context, call providers.ToolCall, result *ToolResult, contentForLLM string, iteration int) error
	OnToolUserMessage func(ctx context.Context, call providers.ToolCall, result *ToolResult, iteration int)
	OnLoopWarning     func(ctx context.Context, reason string, level string, count int, message string, iteration int)
	OnLoopBreak       func(ctx context.Context, reason string, iteration int)
}

// LLMCallFunc customizes provider invocation (for stateful providers, etc.).
type LLMCallFunc func(ctx context.Context, messages []providers.Message, toolDefs []providers.ToolDefinition, model string, options map[string]interface{}) (*providers.LLMResponse, error)

// ContextRebuildFunc is used for overflow recovery after compaction.
type ContextRebuildFunc func(ctx context.Context) ([]providers.Message, error)

type ToolLoopDetectionConfig struct {
	Enabled                     bool
	WarningsEnabled             bool
	SignatureWarnThreshold      int
	SignatureCriticalThreshold  int
	DriftWarnThreshold          int
	DriftCriticalThreshold      int
	PollingWarnThreshold        int
	PollingCriticalThreshold    int
	NoProgressWarnThreshold     int
	NoProgressCriticalThreshold int
	PingPongWarnThreshold       int
	PingPongCriticalThreshold   int
	GlobalCircuitThreshold      int
}

// ToolLoopConfig configures the tool execution loop.
type ToolLoopConfig struct {
	Provider               providers.LLMProvider
	Model                  string
	Tools                  *ToolRegistry
	MaxIterations          int
	LLMOptions             map[string]any
	ContextWindowTokens    int
	CallLLM                LLMCallFunc
	RebuildContext         ContextRebuildFunc
	Callbacks              LoopCallbacks
	Retry                  providers.RetryConfig
	MaxOverflowCompactions int
	MaxRunAttempts         int
	EstimatePromptTokens   func(model string, messages []providers.Message) int
	ContextPruningMode     string
	ContextPruningKeepLast int
	LoopDetection          ToolLoopDetectionConfig
}

// ToolLoopResult contains the result of running the tool loop.
type ToolLoopResult struct {
	Content     string
	Iterations  int
	BreakReason string
	Messages    []providers.Message
}

type runnerState struct {
	messages                    []providers.Message
	iteration                   int
	finalContent                string
	breakReason                 string
	overflowCompactionAttempts  int
	toolResultTruncationTried   bool
	runAttempts                 int
	detector                    *toolLoopDetector
	lastContextOverflowError    error
	hasContextOverflowCompacted bool
}

type loopDetectionOutcome struct {
	Level   string
	Reason  string
	Message string
	Count   int
}

func defaultToolLoopDetectionConfig() ToolLoopDetectionConfig {
	return ToolLoopDetectionConfig{
		Enabled:                     true,
		WarningsEnabled:             true,
		SignatureWarnThreshold:      2,
		SignatureCriticalThreshold:  3,
		DriftWarnThreshold:          6,
		DriftCriticalThreshold:      8,
		PollingWarnThreshold:        4,
		PollingCriticalThreshold:    5,
		NoProgressWarnThreshold:     4,
		NoProgressCriticalThreshold: 6,
		PingPongWarnThreshold:       4,
		PingPongCriticalThreshold:   6,
		GlobalCircuitThreshold:      12,
	}
}

func normalizeToolLoopDetectionConfig(cfg ToolLoopDetectionConfig) ToolLoopDetectionConfig {
	d := defaultToolLoopDetectionConfig()

	if !cfg.Enabled && !cfg.WarningsEnabled &&
		cfg.SignatureWarnThreshold == 0 &&
		cfg.SignatureCriticalThreshold == 0 &&
		cfg.DriftWarnThreshold == 0 &&
		cfg.DriftCriticalThreshold == 0 &&
		cfg.PollingWarnThreshold == 0 &&
		cfg.PollingCriticalThreshold == 0 &&
		cfg.NoProgressWarnThreshold == 0 &&
		cfg.NoProgressCriticalThreshold == 0 &&
		cfg.PingPongWarnThreshold == 0 &&
		cfg.PingPongCriticalThreshold == 0 &&
		cfg.GlobalCircuitThreshold == 0 {
		return d
	}

	d.Enabled = cfg.Enabled
	d.WarningsEnabled = cfg.WarningsEnabled

	if cfg.SignatureWarnThreshold > 0 {
		d.SignatureWarnThreshold = cfg.SignatureWarnThreshold
	}
	if cfg.SignatureCriticalThreshold > 0 {
		d.SignatureCriticalThreshold = cfg.SignatureCriticalThreshold
	}
	if cfg.DriftWarnThreshold > 0 {
		d.DriftWarnThreshold = cfg.DriftWarnThreshold
	}
	if cfg.DriftCriticalThreshold > 0 {
		d.DriftCriticalThreshold = cfg.DriftCriticalThreshold
	}
	if cfg.PollingWarnThreshold > 0 {
		d.PollingWarnThreshold = cfg.PollingWarnThreshold
	}
	if cfg.PollingCriticalThreshold > 0 {
		d.PollingCriticalThreshold = cfg.PollingCriticalThreshold
	}
	if cfg.NoProgressWarnThreshold > 0 {
		d.NoProgressWarnThreshold = cfg.NoProgressWarnThreshold
	}
	if cfg.NoProgressCriticalThreshold > 0 {
		d.NoProgressCriticalThreshold = cfg.NoProgressCriticalThreshold
	}
	if cfg.PingPongWarnThreshold > 0 {
		d.PingPongWarnThreshold = cfg.PingPongWarnThreshold
	}
	if cfg.PingPongCriticalThreshold > 0 {
		d.PingPongCriticalThreshold = cfg.PingPongCriticalThreshold
	}
	if cfg.GlobalCircuitThreshold > 0 {
		d.GlobalCircuitThreshold = cfg.GlobalCircuitThreshold
	}

	if d.SignatureCriticalThreshold <= d.SignatureWarnThreshold {
		d.SignatureCriticalThreshold = d.SignatureWarnThreshold + 1
	}
	if d.DriftCriticalThreshold <= d.DriftWarnThreshold {
		d.DriftCriticalThreshold = d.DriftWarnThreshold + 1
	}
	if d.PollingCriticalThreshold <= d.PollingWarnThreshold {
		d.PollingCriticalThreshold = d.PollingWarnThreshold + 1
	}
	if d.NoProgressCriticalThreshold <= d.NoProgressWarnThreshold {
		d.NoProgressCriticalThreshold = d.NoProgressWarnThreshold + 1
	}
	if d.PingPongCriticalThreshold <= d.PingPongWarnThreshold {
		d.PingPongCriticalThreshold = d.PingPongWarnThreshold + 1
	}
	if d.GlobalCircuitThreshold <= d.PollingCriticalThreshold {
		d.GlobalCircuitThreshold = d.PollingCriticalThreshold + 2
	}
	return d
}

// RunToolLoop executes the LLM + tool call iteration loop.
// This is the core agent logic reused by both main agent and subagents.
func RunToolLoop(ctx context.Context, config ToolLoopConfig, messages []providers.Message, channel, chatID string) (*ToolLoopResult, error) {
	if config.Provider == nil {
		return nil, fmt.Errorf("LLM provider is required")
	}
	if config.MaxIterations <= 0 {
		config.MaxIterations = 10
	}
	if config.MaxOverflowCompactions <= 0 {
		config.MaxOverflowCompactions = 3
	}
	if config.MaxRunAttempts <= 0 {
		config.MaxRunAttempts = maxInt(24, config.MaxIterations*4)
	}
	if config.ContextWindowTokens <= 0 {
		config.ContextWindowTokens = 16384
	}
	if config.Retry.MaxAttempts <= 0 {
		config.Retry = providers.DefaultRetryConfig()
	}
	if config.LLMOptions == nil {
		config.LLMOptions = map[string]any{
			"max_tokens":  4096,
			"temperature": 0.7,
		}
	}
	detectionCfg := normalizeToolLoopDetectionConfig(config.LoopDetection)

	state := &runnerState{
		messages: cloneMessages(messages),
		detector: newToolLoopDetector(detectionCfg),
	}

	for state.iteration < config.MaxIterations {
		if state.runAttempts >= config.MaxRunAttempts {
			return nil, fmt.Errorf("retry limit exceeded after %d attempts", state.runAttempts)
		}
		state.iteration++
		state.runAttempts++

		logger.DebugCF("toolloop", "LLM iteration", map[string]any{
			"iteration": state.iteration,
			"max":       config.MaxIterations,
			"attempts":  state.runAttempts,
		})

		applyContextPruningInPlace(state.messages, config.ContextPruningMode, config.ContextPruningKeepLast)
		enforceToolResultContextBudgetInPlace(state.messages, config.ContextWindowTokens)

		promptEstimateTokens := estimatePromptTokens(config, state.messages)
		response, err := callModelWithRetry(ctx, config, state.messages)
		if err != nil {
			recovered, recErr := recoverFromModelError(ctx, config, state, err)
			if recErr != nil {
				logger.ErrorCF("toolloop", "LLM call failed", map[string]any{
					"iteration": state.iteration,
					"error":     recErr.Error(),
				})
				return nil, fmt.Errorf("LLM call failed after retries: %w", recErr)
			}
			if recovered {
				state.iteration--
				continue
			}
			continue
		}
		if response == nil {
			return nil, fmt.Errorf("LLM provider returned nil response")
		}

		if len(response.ToolCalls) == 0 {
			state.finalContent = strings.TrimSpace(response.Content)
			logger.InfoCF("toolloop", "LLM response without tool calls (direct answer)", map[string]any{
				"iteration":     state.iteration,
				"content_chars": len(state.finalContent),
			})
			break
		}

		if outcome := state.detector.checkResponsePattern(response.ToolCalls); outcome != nil {
			if strings.EqualFold(outcome.Level, "warning") {
				if config.Callbacks.OnLoopWarning != nil {
					config.Callbacks.OnLoopWarning(ctx, outcome.Reason, outcome.Level, outcome.Count, outcome.Message, state.iteration)
				}
			} else {
				state.finalContent = outcome.Message
				state.breakReason = outcome.Reason
				if config.Callbacks.OnLoopBreak != nil {
					config.Callbacks.OnLoopBreak(ctx, outcome.Reason, state.iteration)
				}
				break
			}
		}

		toolNames := make([]string, 0, len(response.ToolCalls))
		for _, tc := range response.ToolCalls {
			toolNames = append(toolNames, tc.Name)
		}
		logger.InfoCF("toolloop", "LLM requested tool calls", map[string]any{
			"tools":     toolNames,
			"count":     len(response.ToolCalls),
			"iteration": state.iteration,
		})

		assistantMsg := providers.Message{Role: "assistant", Content: response.Content}
		for _, tc := range response.ToolCalls {
			argsJSON, _ := json.Marshal(tc.Arguments)
			assistantMsg.ToolCalls = append(assistantMsg.ToolCalls, providers.ToolCall{
				ID:   tc.ID,
				Type: "function",
				Function: &providers.FunctionCall{
					Name:      tc.Name,
					Arguments: string(argsJSON),
				},
			})
		}
		state.messages = append(state.messages, assistantMsg)
		if config.Callbacks.OnAssistantTurn != nil {
			if err := config.Callbacks.OnAssistantTurn(ctx, response, promptEstimateTokens, state.iteration); err != nil {
				return nil, err
			}
		}

		breakByToolResult := false
		for _, tc := range response.ToolCalls {
			argsJSON, _ := json.Marshal(tc.Arguments)
			argsPreview := utils.Truncate(string(argsJSON), 200)
			logger.InfoCF("toolloop", fmt.Sprintf("Tool call: %s(%s)", tc.Name, argsPreview), map[string]any{
				"tool":      tc.Name,
				"iteration": state.iteration,
			})

			toolResult := executeToolCall(ctx, config, channel, chatID, tc)
			if toolResult == nil {
				toolResult = ErrorResult(fmt.Sprintf("tool %s returned no result", tc.Name))
			}

			if config.Callbacks.OnToolUserMessage != nil && !toolResult.Silent && toolResult.ForUser != "" {
				config.Callbacks.OnToolUserMessage(ctx, tc, toolResult, state.iteration)
			}

			contentForLLM := strings.TrimSpace(toolResult.ForLLM)
			if contentForLLM == "" && toolResult.Err != nil {
				contentForLLM = toolResult.Err.Error()
			}
			contentForLLM = normalizeSingleToolResult(contentForLLM, config.ContextWindowTokens)

			state.messages = append(state.messages, providers.Message{
				Role:       "tool",
				Content:    contentForLLM,
				ToolCallID: tc.ID,
			})

			if config.Callbacks.OnToolResult != nil {
				if err := config.Callbacks.OnToolResult(ctx, tc, toolResult, contentForLLM, state.iteration); err != nil {
					return nil, err
				}
			}

			if outcome := state.detector.recordToolOutcome(tc, contentForLLM); outcome != nil {
				if strings.EqualFold(outcome.Level, "warning") {
					if config.Callbacks.OnLoopWarning != nil {
						config.Callbacks.OnLoopWarning(ctx, outcome.Reason, outcome.Level, outcome.Count, outcome.Message, state.iteration)
					}
				} else {
					state.finalContent = outcome.Message
					state.breakReason = outcome.Reason
					if config.Callbacks.OnLoopBreak != nil {
						config.Callbacks.OnLoopBreak(ctx, outcome.Reason, state.iteration)
					}
					breakByToolResult = true
					break
				}
			}
		}
		if breakByToolResult {
			break
		}
	}

	if state.finalContent == "" && state.iteration >= config.MaxIterations {
		state.finalContent = fmt.Sprintf("I paused because I reached the maximum number of consecutive actions (%d) allowed in a single turn. Let me know if you would like me to continue.", config.MaxIterations)
		if state.breakReason == "" {
			state.breakReason = "max_iterations"
		}
	}

	return &ToolLoopResult{
		Content:     state.finalContent,
		Iterations:  state.iteration,
		BreakReason: state.breakReason,
		Messages:    cloneMessages(state.messages),
	}, nil
}

func callModelWithRetry(ctx context.Context, config ToolLoopConfig, messages []providers.Message) (*providers.LLMResponse, error) {
	toolDefs := []providers.ToolDefinition(nil)
	if config.Tools != nil {
		toolDefs = config.Tools.ToProviderDefs()
	}
	call := config.CallLLM
	if call == nil {
		call = func(ctx context.Context, messages []providers.Message, toolDefs []providers.ToolDefinition, model string, options map[string]interface{}) (*providers.LLMResponse, error) {
			return config.Provider.Chat(ctx, messages, toolDefs, model, options)
		}
	}

	resp, err := providers.RetryCall(ctx, config.Retry, func() (*providers.LLMResponse, error) {
		return call(ctx, messages, toolDefs, config.Model, config.LLMOptions)
	}, providers.IsTransientError, func(info providers.RetryInfo) {
		if config.Callbacks.OnTransientRetry != nil {
			config.Callbacks.OnTransientRetry(ctx, info)
		}
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func recoverFromModelError(ctx context.Context, config ToolLoopConfig, state *runnerState, err error) (bool, error) {
	if !providers.IsContextOverflowError(err) {
		return false, err
	}
	state.lastContextOverflowError = err

	if state.overflowCompactionAttempts < config.MaxOverflowCompactions && config.RebuildContext != nil {
		state.overflowCompactionAttempts++
		if config.Callbacks.OnOverflowStage != nil {
			config.Callbacks.OnOverflowStage(ctx, "compact", state.overflowCompactionAttempts, config.MaxOverflowCompactions, err)
		}
		rebuilt, rebuildErr := config.RebuildContext(ctx)
		if rebuildErr != nil {
			logger.WarnCF("toolloop", "Failed rebuilding context after compaction", map[string]any{
				"attempt": state.overflowCompactionAttempts,
				"error":   rebuildErr.Error(),
			})
		} else if len(rebuilt) > 0 {
			state.messages = cloneMessages(rebuilt)
			state.hasContextOverflowCompacted = true
			return true, nil
		}
	}

	if !state.toolResultTruncationTried {
		state.toolResultTruncationTried = true
		if config.Callbacks.OnOverflowStage != nil {
			config.Callbacks.OnOverflowStage(ctx, "truncate_tool_results", state.overflowCompactionAttempts, config.MaxOverflowCompactions, err)
		}
		if truncated := truncateOversizedToolResultsInPlace(state.messages, config.ContextWindowTokens); truncated > 0 {
			return true, nil
		}
	}

	if config.Callbacks.OnOverflowStage != nil {
		config.Callbacks.OnOverflowStage(ctx, "give_up", state.overflowCompactionAttempts, config.MaxOverflowCompactions, err)
	}
	return false, err
}

func executeToolCall(ctx context.Context, config ToolLoopConfig, channel, chatID string, tc providers.ToolCall) *ToolResult {
	if config.Tools == nil {
		return ErrorResult("No tools available")
	}
	return config.Tools.ExecuteWithContext(ctx, tc.Name, tc.Arguments, channel, chatID, nil)
}

func cloneMessages(messages []providers.Message) []providers.Message {
	if len(messages) == 0 {
		return nil
	}
	out := make([]providers.Message, len(messages))
	copy(out, messages)
	return out
}

type toolLoopDetector struct {
	cfg                ToolLoopDetectionConfig
	responseSignatures map[string]int
	toolNameCounts     map[string]int
	toolDistinct       map[string]map[string]struct{}
	resultStreak       map[string]streakState
	recentOutcomes     []toolOutcome
	warned             map[string]struct{}
}

type streakState struct {
	lastResultHash string
	count          int
}

type toolOutcome struct {
	callHash   string
	resultHash string
}

func newToolLoopDetector(cfg ToolLoopDetectionConfig) *toolLoopDetector {
	return &toolLoopDetector{
		cfg:                cfg,
		responseSignatures: map[string]int{},
		toolNameCounts:     map[string]int{},
		toolDistinct:       map[string]map[string]struct{}{},
		resultStreak:       map[string]streakState{},
		recentOutcomes:     make([]toolOutcome, 0, 12),
		warned:             map[string]struct{}{},
	}
}

func (d *toolLoopDetector) checkResponsePattern(calls []providers.ToolCall) *loopDetectionOutcome {
	if d == nil || !d.cfg.Enabled {
		return nil
	}
	signature := toolCallSignature(calls)
	if signature != "" {
		d.responseSignatures[signature]++
		count := d.responseSignatures[signature]
		if count >= d.cfg.SignatureCriticalThreshold {
			return &loopDetectionOutcome{
				Level:   "critical",
				Reason:  "signature_repeat",
				Message: "I’m stopping tool execution because I detected a repeated tool-call loop. If you still want this action, restate it with a narrower scope.",
				Count:   count,
			}
		}
		if count >= d.cfg.SignatureWarnThreshold {
			if outcome := d.warnOutcome("signature_repeat|"+signature, "signature_repeat", count,
				"I’m seeing repeated tool-call patterns. I will continue for now, but if this keeps repeating I will stop the loop."); outcome != nil {
				return outcome
			}
		}
	}

	const toolDriftDistinctSigCeiling = 2
	for _, tc := range calls {
		name := strings.TrimSpace(tc.Name)
		if name == "" {
			name = "(unknown)"
		}
		d.toolNameCounts[name]++
		argsJSON, _ := json.Marshal(tc.Arguments)
		if _, ok := d.toolDistinct[name]; !ok {
			d.toolDistinct[name] = map[string]struct{}{}
		}
		d.toolDistinct[name][string(argsJSON)] = struct{}{}
		distinct := len(d.toolDistinct[name])
		count := d.toolNameCounts[name]
		if count >= d.cfg.DriftCriticalThreshold && distinct <= toolDriftDistinctSigCeiling {
			return &loopDetectionOutcome{
				Level:   "critical",
				Reason:  "tool_name_low_variance_repeat",
				Message: "I’m stopping tool execution because one tool kept being called repeatedly. If you still want this action, restate it with a narrower scope.",
				Count:   count,
			}
		}
		if count >= d.cfg.DriftWarnThreshold && distinct <= toolDriftDistinctSigCeiling {
			if outcome := d.warnOutcome("tool_name_low_variance_repeat|"+name, "tool_name_low_variance_repeat", count,
				"I’m seeing the same tool called repeatedly with little parameter variation. I will continue for now."); outcome != nil {
				return outcome
			}
		}
	}

	return nil
}

func (d *toolLoopDetector) recordToolOutcome(tc providers.ToolCall, contentForLLM string) *loopDetectionOutcome {
	if d == nil || !d.cfg.Enabled {
		return nil
	}
	argsJSON, _ := json.Marshal(tc.Arguments)
	argsHash := hashString(string(argsJSON))
	callHash := hashString(strings.TrimSpace(tc.Name) + "|" + argsHash)
	resultHash := hashString(contentForLLM)
	streakKey := strings.TrimSpace(tc.Name) + "|" + argsHash

	st := d.resultStreak[streakKey]
	if st.lastResultHash == resultHash {
		st.count++
	} else {
		st.lastResultHash = resultHash
		st.count = 1
	}
	d.resultStreak[streakKey] = st

	if st.count >= d.cfg.GlobalCircuitThreshold {
		return &loopDetectionOutcome{
			Level:   "critical",
			Reason:  "global_circuit_breaker",
			Message: "I’m stopping tool execution because a tool pattern repeated too many times without progress.",
			Count:   st.count,
		}
	}
	if isPollingCall(tc) {
		if st.count >= d.cfg.PollingCriticalThreshold {
			return &loopDetectionOutcome{
				Level:   "critical",
				Reason:  "known_poll_no_progress",
				Message: "I’m stopping tool execution because repeated polling showed no progress. Increase wait time or report the task as failed if it appears stuck.",
				Count:   st.count,
			}
		}
		if st.count >= d.cfg.PollingWarnThreshold {
			if outcome := d.warnOutcome("known_poll_no_progress|"+streakKey, "known_poll_no_progress", st.count,
				"I’m seeing repeated polling with no progress. I will continue for now, but this may be stuck."); outcome != nil {
				return outcome
			}
		}
	} else {
		if st.count >= d.cfg.NoProgressCriticalThreshold {
			return &loopDetectionOutcome{
				Level:   "critical",
				Reason:  "no_progress_repeat",
				Message: "I’m stopping tool execution because the same tool call produced no progress repeatedly. If you want me to continue, provide a different strategy or narrower scope.",
				Count:   st.count,
			}
		}
		if st.count >= d.cfg.NoProgressWarnThreshold {
			if outcome := d.warnOutcome("no_progress_repeat|"+streakKey, "no_progress_repeat", st.count,
				"I’m seeing repeated tool outcomes with no progress. I will continue for now."); outcome != nil {
				return outcome
			}
		}
	}

	d.recentOutcomes = append(d.recentOutcomes, toolOutcome{callHash: callHash, resultHash: resultHash})
	if len(d.recentOutcomes) > 12 {
		d.recentOutcomes = d.recentOutcomes[len(d.recentOutcomes)-12:]
	}
	if pingPong, count := detectPingPongNoProgress(d.recentOutcomes); pingPong {
		if count >= d.cfg.PingPongCriticalThreshold {
			return &loopDetectionOutcome{
				Level:   "critical",
				Reason:  "ping_pong",
				Message: "I’m stopping tool execution because I detected an alternating ping-pong loop with no progress.",
				Count:   count,
			}
		}
		if count >= d.cfg.PingPongWarnThreshold {
			if outcome := d.warnOutcome("ping_pong|"+callHash, "ping_pong", count,
				"I’m seeing an alternating ping-pong loop pattern. I will continue for now."); outcome != nil {
				return outcome
			}
		}
	}
	return nil
}

func (d *toolLoopDetector) warnOutcome(key, reason string, count int, message string) *loopDetectionOutcome {
	if d == nil || !d.cfg.WarningsEnabled {
		return nil
	}
	if _, exists := d.warned[key]; exists {
		return nil
	}
	d.warned[key] = struct{}{}
	return &loopDetectionOutcome{
		Level:   "warning",
		Reason:  reason,
		Message: message,
		Count:   count,
	}
}

func detectPingPongNoProgress(outcomes []toolOutcome) (bool, int) {
	if len(outcomes) < 6 {
		return false, 0
	}
	tail := outcomes[len(outcomes)-6:]
	a := tail[0].callHash
	b := tail[1].callHash
	if a == "" || b == "" || a == b {
		return false, 0
	}
	seenResult := map[string]string{}
	for i, outcome := range tail {
		expected := a
		if i%2 == 1 {
			expected = b
		}
		if outcome.callHash != expected {
			return false, 0
		}
		prev, ok := seenResult[outcome.callHash]
		if !ok {
			seenResult[outcome.callHash] = outcome.resultHash
			continue
		}
		if prev != outcome.resultHash {
			return false, 0
		}
	}
	return true, len(tail)
}

func toolCallSignature(calls []providers.ToolCall) string {
	if len(calls) == 0 {
		return ""
	}
	parts := make([]string, 0, len(calls))
	for _, tc := range calls {
		argsJSON, _ := json.Marshal(tc.Arguments)
		parts = append(parts, tc.Name+":"+string(argsJSON))
	}
	return strings.Join(parts, "|")
}

func hashString(value string) string {
	sum := sha1.Sum([]byte(value))
	return hex.EncodeToString(sum[:8])
}

func isPollingCall(tc providers.ToolCall) bool {
	if strings.EqualFold(strings.TrimSpace(tc.Name), "process") {
		action, _ := tc.Arguments["action"].(string)
		a := strings.ToLower(strings.TrimSpace(action))
		return a == "poll" || a == "log"
	}
	action, _ := tc.Arguments["action"].(string)
	a := strings.ToLower(strings.TrimSpace(action))
	return a == "poll" || a == "log"
}

func contextBudgetChars(contextWindowTokens int) int {
	if contextWindowTokens <= 0 {
		contextWindowTokens = 16384
	}
	budget := int(float64(contextWindowTokens) * 4.0 * 0.82)
	if budget < 4096 {
		budget = 4096
	}
	return budget
}

func maxSingleToolResultChars(contextWindowTokens int) int {
	if contextWindowTokens <= 0 {
		contextWindowTokens = 16384
	}
	limit := int(float64(contextWindowTokens) * 4.0 * 0.24)
	if limit < 2048 {
		limit = 2048
	}
	if limit > 120000 {
		limit = 120000
	}
	return limit
}

func normalizeSingleToolResult(content string, contextWindowTokens int) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return content
	}
	return truncateWithMarker(content, maxSingleToolResultChars(contextWindowTokens))
}

func applyContextPruningInPlace(messages []providers.Message, mode string, keepLast int) {
	if len(messages) == 0 {
		return
	}
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" || mode == "off" || mode == "disabled" {
		return
	}
	trimLimit := 0
	placeholder := "[Older tool result pruned by context policy]"
	switch mode {
	case "conservative":
		if keepLast <= 0 {
			keepLast = 8
		}
		trimLimit = 900
	case "balanced":
		if keepLast <= 0 {
			keepLast = 5
		}
		trimLimit = 560
	case "aggressive":
		if keepLast <= 0 {
			keepLast = 3
		}
		trimLimit = 260
	default:
		return
	}

	toolIdx := make([]int, 0, len(messages))
	for i := range messages {
		if messages[i].Role == "tool" {
			toolIdx = append(toolIdx, i)
		}
	}
	if len(toolIdx) <= keepLast {
		return
	}
	cutoff := len(toolIdx) - keepLast
	for pos, msgIdx := range toolIdx {
		if pos >= cutoff {
			continue
		}
		content := strings.TrimSpace(messages[msgIdx].Content)
		if content == "" {
			continue
		}
		if mode == "aggressive" && pos < cutoff-1 {
			messages[msgIdx].Content = placeholder
			continue
		}
		if len(content) > trimLimit {
			messages[msgIdx].Content = truncateWithMarker(content, trimLimit)
			continue
		}
		if mode == "aggressive" {
			messages[msgIdx].Content = placeholder
		}
	}
}

func enforceToolResultContextBudgetInPlace(messages []providers.Message, contextWindowTokens int) {
	if len(messages) == 0 {
		return
	}
	singleLimit := maxSingleToolResultChars(contextWindowTokens)
	for i := range messages {
		if messages[i].Role != "tool" {
			continue
		}
		messages[i].Content = truncateWithMarker(messages[i].Content, singleLimit)
	}

	budget := contextBudgetChars(contextWindowTokens)
	current := estimateContextChars(messages)
	if current <= budget {
		return
	}

	need := current - budget
	for i := 0; i < len(messages) && need > 0; i++ {
		if messages[i].Role != "tool" {
			continue
		}
		before := len(messages[i].Content)
		if before <= 512 {
			continue
		}
		target := maxInt(256, before/4)
		messages[i].Content = truncateWithMarker(messages[i].Content, target)
		after := len(messages[i].Content)
		if after < before {
			need -= before - after
		}
	}

	if need <= 0 {
		return
	}
	placeholder := "[Old tool result content cleared to stay within context budget]"
	for i := 0; i < len(messages) && need > 0; i++ {
		if messages[i].Role != "tool" {
			continue
		}
		before := len(messages[i].Content)
		if before <= len(placeholder) {
			continue
		}
		messages[i].Content = placeholder
		need -= before - len(placeholder)
	}
}

func truncateOversizedToolResultsInPlace(messages []providers.Message, contextWindowTokens int) int {
	if len(messages) == 0 {
		return 0
	}
	singleLimit := maxSingleToolResultChars(contextWindowTokens)
	truncated := 0
	for i := range messages {
		if messages[i].Role != "tool" {
			continue
		}
		before := len(messages[i].Content)
		after := truncateWithMarker(messages[i].Content, singleLimit)
		if len(after) < before {
			messages[i].Content = after
			truncated++
		}
	}
	if truncated > 0 {
		enforceToolResultContextBudgetInPlace(messages, contextWindowTokens)
	}
	return truncated
}

func estimateContextChars(messages []providers.Message) int {
	total := 0
	for _, msg := range messages {
		total += len(msg.Content)
		if len(msg.ToolCalls) > 0 {
			for _, tc := range msg.ToolCalls {
				if tc.Function != nil {
					total += len(tc.Function.Arguments)
				}
			}
		}
	}
	return total
}

func estimatePromptTokens(config ToolLoopConfig, messages []providers.Message) int {
	if config.EstimatePromptTokens != nil {
		if n := config.EstimatePromptTokens(config.Model, messages); n > 0 {
			return n
		}
	}
	chars := estimateContextChars(messages)
	if chars <= 0 {
		return 0
	}
	charsPerToken := 3.55
	model := strings.ToLower(strings.TrimSpace(config.Model))
	switch {
	case strings.Contains(model, "claude"):
		charsPerToken = 3.80
	case strings.Contains(model, "qwen"), strings.Contains(model, "deepseek"):
		charsPerToken = 3.30
	case strings.Contains(model, "gemini"):
		charsPerToken = 3.55
	}
	estimated := int(float64(chars)/charsPerToken) + len(messages)*6
	if estimated < 16 {
		estimated = 16
	}
	return estimated
}

func truncateWithMarker(content string, maxChars int) string {
	content = strings.TrimSpace(content)
	if maxChars <= 0 || len(content) <= maxChars {
		return content
	}
	if maxChars < 64 {
		maxChars = 64
	}
	head := int(float64(maxChars) * 0.55)
	tail := int(float64(maxChars) * 0.25)
	if head+tail > maxChars-24 {
		tail = maxInt(16, maxChars-head-24)
	}
	if head < 16 {
		head = 16
	}
	if tail < 16 {
		tail = 16
	}
	if head+tail >= len(content) {
		return content[:maxChars]
	}
	truncated := content[:head] + "\n...\n" + content[len(content)-tail:]
	note := fmt.Sprintf("\n[tool result trimmed from %d to %d chars]", len(content), len(truncated))
	if len(truncated)+len(note) > maxChars {
		allowed := maxChars - len(note)
		if allowed < 32 {
			allowed = maxChars
			note = ""
		}
		if allowed < len(truncated) {
			truncated = truncated[:allowed]
		}
	}
	return truncated + note
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
