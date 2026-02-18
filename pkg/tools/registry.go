package tools

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/dotsetgreg/dotagent/pkg/logger"
	"github.com/dotsetgreg/dotagent/pkg/providers"
)

type ToolRegistry struct {
	tools map[string]Tool
	mu    sync.RWMutex
}

func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{
		tools: make(map[string]Tool),
	}
}

func (r *ToolRegistry) Register(tool Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[tool.Name()] = tool
}

// Close closes all registered tools that implement ClosableTool.
// It attempts all closes and returns an aggregated error if any fail.
func (r *ToolRegistry) Close() error {
	r.mu.RLock()
	closers := make([]ClosableTool, 0, len(r.tools))
	for _, tool := range r.tools {
		if closer, ok := tool.(ClosableTool); ok {
			closers = append(closers, closer)
		}
	}
	r.mu.RUnlock()

	var errs []string
	for _, closer := range closers {
		if err := closer.Close(); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", closer.Name(), err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("tool close failures: %s", strings.Join(errs, "; "))
	}
	return nil
}

func (r *ToolRegistry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	tool, ok := r.tools[name]
	return tool, ok
}

func (r *ToolRegistry) Execute(ctx context.Context, name string, args map[string]interface{}) *ToolResult {
	return r.ExecuteWithContext(ctx, name, args, "", "", nil)
}

// ExecuteWithContext executes a tool with channel/chatID context and optional async callback.
// If the tool implements AsyncTool and a non-nil callback is provided,
// the callback will be set on the tool before execution.
func (r *ToolRegistry) ExecuteWithContext(ctx context.Context, name string, args map[string]interface{}, channel, chatID string, asyncCallback AsyncCallback) *ToolResult {
	sanitizedArgs := sanitizeToolArgs(args)
	logger.InfoCF("tool", "Tool execution started",
		map[string]interface{}{
			"tool": name,
			"args": sanitizedArgs,
		})

	tool, ok := r.Get(name)
	if !ok {
		logger.ErrorCF("tool", "Tool not found",
			map[string]interface{}{
				"tool": name,
			})
		return ErrorResult(fmt.Sprintf("tool %q not found", name)).WithError(fmt.Errorf("tool not found"))
	}

	execCtx := withToolExecutionContext(ctx, channel, chatID, asyncCallback)

	start := time.Now()
	result := tool.Execute(execCtx, args)
	duration := time.Since(start)
	if result == nil {
		err := fmt.Errorf("tool %q returned nil result", name)
		logger.ErrorCF("tool", "Tool returned nil result",
			map[string]interface{}{
				"tool": name,
			})
		return ErrorResult(err.Error()).WithError(err)
	}

	// Log based on result type
	if result.IsError {
		logger.ErrorCF("tool", "Tool execution failed",
			map[string]interface{}{
				"tool":     name,
				"duration": duration.Milliseconds(),
				"error":    result.ForLLM,
			})
	} else if result.Async {
		logger.InfoCF("tool", "Tool started (async)",
			map[string]interface{}{
				"tool":     name,
				"duration": duration.Milliseconds(),
			})
	} else {
		logger.InfoCF("tool", "Tool execution completed",
			map[string]interface{}{
				"tool":          name,
				"duration_ms":   duration.Milliseconds(),
				"result_length": len(result.ForLLM),
			})
	}

	return result
}

func (r *ToolRegistry) GetDefinitions() []map[string]interface{} {
	r.mu.RLock()
	defer r.mu.RUnlock()

	definitions := make([]map[string]interface{}, 0, len(r.tools))
	for _, tool := range r.tools {
		definitions = append(definitions, ToolToSchema(tool))
	}
	return definitions
}

// ToProviderDefs converts tool definitions to provider-compatible format.
// This is the format expected by LLM provider APIs.
func (r *ToolRegistry) ToProviderDefs() []providers.ToolDefinition {
	r.mu.RLock()
	defer r.mu.RUnlock()

	definitions := make([]providers.ToolDefinition, 0, len(r.tools))
	for _, tool := range r.tools {
		schema := ToolToSchema(tool)

		// Safely extract nested values with type checks
		fn, ok := schema["function"].(map[string]interface{})
		if !ok {
			continue
		}

		name, _ := fn["name"].(string)
		desc, _ := fn["description"].(string)
		params, _ := fn["parameters"].(map[string]interface{})

		definitions = append(definitions, providers.ToolDefinition{
			Type: "function",
			Function: providers.ToolFunctionDefinition{
				Name:        name,
				Description: desc,
				Parameters:  params,
			},
		})
	}
	return definitions
}

// List returns a list of all registered tool names.
func (r *ToolRegistry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	return names
}

// Count returns the number of registered tools.
func (r *ToolRegistry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.tools)
}

// GetSummaries returns human-readable summaries of all registered tools.
// Returns a slice of "name - description" strings.
func (r *ToolRegistry) GetSummaries() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	summaries := make([]string, 0, len(r.tools))
	for _, tool := range r.tools {
		summaries = append(summaries, fmt.Sprintf("- `%s` - %s", tool.Name(), tool.Description()))
	}
	return summaries
}

var sensitiveArgKeyFragments = []string{
	"api_key",
	"apikey",
	"authorization",
	"auth",
	"bearer",
	"client_secret",
	"cookie",
	"password",
	"private",
	"secret",
	"session",
	"token",
}

func sanitizeToolArgs(args map[string]interface{}) map[string]interface{} {
	if args == nil {
		return nil
	}
	sanitized := make(map[string]interface{}, len(args))
	for key, value := range args {
		sanitized[key] = sanitizeToolArgValue(key, value, 0)
	}
	return sanitized
}

func sanitizeToolArgValue(key string, value interface{}, depth int) interface{} {
	if depth > 6 {
		return "<omitted>"
	}
	if isSensitiveArgKey(key) {
		return "<redacted>"
	}

	switch typed := value.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(typed))
		for k, v := range typed {
			out[k] = sanitizeToolArgValue(k, v, depth+1)
		}
		return out
	case []interface{}:
		out := make([]interface{}, 0, len(typed))
		for _, item := range typed {
			out = append(out, sanitizeToolArgValue(key, item, depth+1))
		}
		return out
	case []string:
		out := make([]interface{}, 0, len(typed))
		for _, item := range typed {
			out = append(out, truncateLogString(item))
		}
		return out
	case string:
		return truncateLogString(typed)
	default:
		return value
	}
}

func isSensitiveArgKey(key string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(key), "-", "_"))
	for _, fragment := range sensitiveArgKeyFragments {
		if strings.Contains(normalized, fragment) {
			return true
		}
	}
	return false
}

func truncateLogString(value string) string {
	const maxLen = 256
	if len(value) <= maxLen {
		return value
	}
	return value[:maxLen] + "...(truncated)"
}
