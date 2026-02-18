package tools

import (
	"context"
	"strings"
	"testing"
)

type executionContextProbeTool struct {
	setContextCalls int
}

func (t *executionContextProbeTool) Name() string        { return "probe" }
func (t *executionContextProbeTool) Description() string { return "probe" }
func (t *executionContextProbeTool) Parameters() map[string]interface{} {
	return map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
}
func (t *executionContextProbeTool) SetContext(channel, chatID string) {
	t.setContextCalls++
}
func (t *executionContextProbeTool) Execute(ctx context.Context, args map[string]interface{}) *ToolResult {
	channel, chatID := channelChatFromContext(ctx)
	return SilentResult(channel + ":" + chatID)
}

type asyncContextProbeTool struct {
	setCallbackCalls int
}

func (t *asyncContextProbeTool) Name() string        { return "async-probe" }
func (t *asyncContextProbeTool) Description() string { return "async probe" }
func (t *asyncContextProbeTool) Parameters() map[string]interface{} {
	return map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
}
func (t *asyncContextProbeTool) SetCallback(cb AsyncCallback) {
	t.setCallbackCalls++
}
func (t *asyncContextProbeTool) Execute(ctx context.Context, args map[string]interface{}) *ToolResult {
	if asyncCallbackFromContext(ctx) == nil {
		return ErrorResult("missing callback")
	}
	return AsyncResult("ok")
}

func TestToolRegistry_ExecuteWithContext_UsesRequestScopedContext(t *testing.T) {
	registry := NewToolRegistry()
	tool := &executionContextProbeTool{}
	registry.Register(tool)

	result := registry.ExecuteWithContext(context.Background(), "probe", map[string]interface{}{}, "discord", "chat-1", nil)
	if result.IsError {
		t.Fatalf("expected success, got error: %s", result.ForLLM)
	}
	if result.ForLLM != "discord:chat-1" {
		t.Fatalf("expected request context channel/chat, got %q", result.ForLLM)
	}
	if tool.setContextCalls != 0 {
		t.Fatalf("expected SetContext to not be called, got %d calls", tool.setContextCalls)
	}
}

func TestToolRegistry_ExecuteWithContext_UsesRequestScopedAsyncCallback(t *testing.T) {
	registry := NewToolRegistry()
	tool := &asyncContextProbeTool{}
	registry.Register(tool)

	callbackCalled := false
	cb := func(ctx context.Context, result *ToolResult) {
		callbackCalled = true
	}
	result := registry.ExecuteWithContext(context.Background(), "async-probe", map[string]interface{}{}, "discord", "chat-1", cb)
	if result.IsError {
		t.Fatalf("expected success, got error: %s", result.ForLLM)
	}
	if !result.Async {
		t.Fatalf("expected async result")
	}
	if tool.setCallbackCalls != 0 {
		t.Fatalf("expected SetCallback to not be called, got %d calls", tool.setCallbackCalls)
	}
	if callbackCalled {
		t.Fatalf("callback should not be invoked by ExecuteWithContext itself")
	}
}

func TestSanitizeToolArgs_RedactsSensitiveValues(t *testing.T) {
	args := map[string]interface{}{
		"api_key": "super-secret",
		"query":   "weather tomorrow",
		"nested": map[string]interface{}{
			"token": "nested-secret",
			"note":  strings.Repeat("x", 400),
		},
	}

	sanitized := sanitizeToolArgs(args)
	if sanitized["api_key"] != "<redacted>" {
		t.Fatalf("expected api_key to be redacted, got %v", sanitized["api_key"])
	}
	nested, ok := sanitized["nested"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected nested map")
	}
	if nested["token"] != "<redacted>" {
		t.Fatalf("expected nested token to be redacted, got %v", nested["token"])
	}
	note, _ := nested["note"].(string)
	if len(note) >= 400 {
		t.Fatalf("expected long values to be truncated")
	}
}
