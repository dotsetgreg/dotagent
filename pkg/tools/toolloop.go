// DotAgent - Ultra-lightweight personal AI agent
// Inspired by and based on nanobot: https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 DotAgent contributors

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dotsetgreg/dotagent/pkg/logger"
	"github.com/dotsetgreg/dotagent/pkg/providers"
	"github.com/dotsetgreg/dotagent/pkg/utils"
)

// ToolLoopConfig configures the tool execution loop.
type ToolLoopConfig struct {
	Provider      providers.LLMProvider
	Model         string
	Tools         *ToolRegistry
	MaxIterations int
	LLMOptions    map[string]any
}

// ToolLoopResult contains the result of running the tool loop.
type ToolLoopResult struct {
	Content    string
	Iterations int
}

// RunToolLoop executes the LLM + tool call iteration loop.
// This is the core agent logic that can be reused by both main agent and subagents.
func RunToolLoop(ctx context.Context, config ToolLoopConfig, messages []providers.Message, channel, chatID string) (*ToolLoopResult, error) {
	iteration := 0
	var finalContent string
	toolCallSignatures := map[string]int{}
	toolNameCounts := map[string]int{}
	toolDistinctSignatures := map[string]map[string]struct{}{}
	const (
		toolDriftCountThreshold     = 8
		toolDriftDistinctSigCeiling = 2
	)

	for iteration < config.MaxIterations {
		iteration++

		logger.DebugCF("toolloop", "LLM iteration",
			map[string]any{
				"iteration": iteration,
				"max":       config.MaxIterations,
			})

		// 1. Build tool definitions
		var providerToolDefs []providers.ToolDefinition
		if config.Tools != nil {
			providerToolDefs = config.Tools.ToProviderDefs()
		}

		// 2. Set default LLM options
		llmOpts := config.LLMOptions
		if llmOpts == nil {
			llmOpts = map[string]any{
				"max_tokens":  4096,
				"temperature": 0.7,
			}
		}

		// 3. Call LLM
		response, err := config.Provider.Chat(ctx, messages, providerToolDefs, config.Model, llmOpts)
		if err != nil {
			logger.ErrorCF("toolloop", "LLM call failed",
				map[string]any{
					"iteration": iteration,
					"error":     err.Error(),
				})
			return nil, fmt.Errorf("LLM call failed: %w", err)
		}

		// 4. If no tool calls, we're done
		if len(response.ToolCalls) == 0 {
			finalContent = response.Content
			logger.InfoCF("toolloop", "LLM response without tool calls (direct answer)",
				map[string]any{
					"iteration":     iteration,
					"content_chars": len(finalContent),
				})
			break
		}
		signature := toolCallSignature(response.ToolCalls)
		if signature != "" {
			toolCallSignatures[signature]++
			if toolCallSignatures[signature] >= 3 {
				logger.WarnCF("toolloop", "Tool-call loop detected; tripping circuit breaker",
					map[string]any{
						"signature": signature,
						"count":     toolCallSignatures[signature],
						"iteration": iteration,
					})
				finalContent = "I’m stopping tool execution because I detected a repeated tool-call loop. If you still want this action, restate it with a narrower scope."
				break
			}
		}
		driftLoopDetected := false
		driftToolName := ""
		driftCount := 0
		driftDistinct := 0
		for _, tc := range response.ToolCalls {
			name := strings.TrimSpace(tc.Name)
			if name == "" {
				name = "(unknown)"
			}
			toolNameCounts[name]++
			argsJSON, _ := json.Marshal(tc.Arguments)
			if _, ok := toolDistinctSignatures[name]; !ok {
				toolDistinctSignatures[name] = map[string]struct{}{}
			}
			toolDistinctSignatures[name][string(argsJSON)] = struct{}{}
			distinct := len(toolDistinctSignatures[name])
			if toolNameCounts[name] >= toolDriftCountThreshold && distinct <= toolDriftDistinctSigCeiling {
				driftLoopDetected = true
				driftToolName = name
				driftCount = toolNameCounts[name]
				driftDistinct = distinct
				break
			}
		}
		if driftLoopDetected {
			logger.WarnCF("toolloop", "Tool drift loop detected; tripping circuit breaker",
				map[string]any{
					"tool":                driftToolName,
					"count":               driftCount,
					"distinct_signatures": driftDistinct,
					"iteration":           iteration,
				})
			finalContent = "I’m stopping tool execution because one tool kept being called repeatedly. If you still want this action, restate it with a narrower scope."
			break
		}

		// 5. Log tool calls
		toolNames := make([]string, 0, len(response.ToolCalls))
		for _, tc := range response.ToolCalls {
			toolNames = append(toolNames, tc.Name)
		}
		logger.InfoCF("toolloop", "LLM requested tool calls",
			map[string]any{
				"tools":     toolNames,
				"count":     len(response.ToolCalls),
				"iteration": iteration,
			})

		// 6. Build assistant message with tool calls
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

		// 7. Execute tool calls
		for _, tc := range response.ToolCalls {
			argsJSON, _ := json.Marshal(tc.Arguments)
			argsPreview := utils.Truncate(string(argsJSON), 200)
			logger.InfoCF("toolloop", fmt.Sprintf("Tool call: %s(%s)", tc.Name, argsPreview),
				map[string]any{
					"tool":      tc.Name,
					"iteration": iteration,
				})

			// Execute tool (no async callback for subagents - they run independently)
			var toolResult *ToolResult
			if config.Tools != nil {
				toolResult = config.Tools.ExecuteWithContext(ctx, tc.Name, tc.Arguments, channel, chatID, nil)
			} else {
				toolResult = ErrorResult("No tools available")
			}

			// Determine content for LLM
			contentForLLM := toolResult.ForLLM
			if contentForLLM == "" && toolResult.Err != nil {
				contentForLLM = toolResult.Err.Error()
			}

			// Add tool result message
			toolResultMsg := providers.Message{
				Role:       "tool",
				Content:    contentForLLM,
				ToolCallID: tc.ID,
			}
			messages = append(messages, toolResultMsg)
		}
	}
	if finalContent == "" && iteration >= config.MaxIterations {
		finalContent = fmt.Sprintf("I paused because I reached the maximum number of consecutive actions (%d) allowed in a single turn. Let me know if you would like me to continue.", config.MaxIterations)
	}

	return &ToolLoopResult{
		Content:    finalContent,
		Iterations: iteration,
	}, nil
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
