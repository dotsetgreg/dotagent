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

type actorContextProbeTool struct{}

func (t *actorContextProbeTool) Name() string        { return "actor-probe" }
func (t *actorContextProbeTool) Description() string { return "actor probe" }
func (t *actorContextProbeTool) Parameters() map[string]interface{} {
	return map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
}
func (t *actorContextProbeTool) Execute(ctx context.Context, args map[string]interface{}) *ToolResult {
	return SilentResult(actorFromContext(ctx))
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

type namedTool struct{ name string }

func (t *namedTool) Name() string        { return t.name }
func (t *namedTool) Description() string { return t.name + " desc" }
func (t *namedTool) Parameters() map[string]interface{} {
	return map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
}
func (t *namedTool) Execute(ctx context.Context, args map[string]interface{}) *ToolResult {
	return SilentResult("ok")
}

func TestToolRegistry_ExecuteWithContext_UsesRequestScopedContext(t *testing.T) {
	registry := NewToolRegistry()
	tool := &executionContextProbeTool{}
	if err := registry.Register(tool); err != nil {
		t.Fatalf("register probe: %v", err)
	}

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
	if err := registry.Register(tool); err != nil {
		t.Fatalf("register async probe: %v", err)
	}

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

func TestToolRegistry_ExecuteWithContext_PreservesActorIDFromContext(t *testing.T) {
	registry := NewToolRegistry()
	if err := registry.Register(&actorContextProbeTool{}); err != nil {
		t.Fatalf("register actor probe: %v", err)
	}
	ctx := WithToolExecutionActor(context.Background(), "user-123")
	result := registry.ExecuteWithContext(ctx, "actor-probe", map[string]interface{}{}, "discord", "chat-1", nil)
	if result.IsError {
		t.Fatalf("expected success, got error: %s", result.ForLLM)
	}
	if result.ForLLM != "user-123" {
		t.Fatalf("expected actor user-123, got %q", result.ForLLM)
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

func TestToolRegistry_RegisterRejectsDuplicateNames(t *testing.T) {
	registry := NewToolRegistry()
	tool := &executionContextProbeTool{}
	if err := registry.Register(tool); err != nil {
		t.Fatalf("first register failed: %v", err)
	}
	if err := registry.Register(tool); err == nil {
		t.Fatalf("expected duplicate registration error")
	}
}

func TestToolRegistry_RegisterOverrideReplacesTool(t *testing.T) {
	registry := NewToolRegistry()
	first := &executionContextProbeTool{}
	second := &executionContextProbeTool{}
	if err := registry.Register(first); err != nil {
		t.Fatalf("register first tool: %v", err)
	}
	if err := registry.RegisterOverride(second); err != nil {
		t.Fatalf("override tool: %v", err)
	}
	got, ok := registry.Get("probe")
	if !ok {
		t.Fatalf("expected probe tool to exist")
	}
	if got != second {
		t.Fatalf("expected overridden tool instance")
	}
}

func TestToolRegistry_DefinitionsAreDeterministic(t *testing.T) {
	registry := NewToolRegistry()
	for _, name := range []string{"zeta", "alpha", "mike"} {
		if err := registry.Register(&namedTool{name: name}); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}

	defs := registry.ToProviderDefs()
	if len(defs) != 3 {
		t.Fatalf("expected 3 definitions, got %d", len(defs))
	}
	if defs[0].Function.Name != "alpha" || defs[1].Function.Name != "mike" || defs[2].Function.Name != "zeta" {
		t.Fatalf("expected deterministic sorted definitions, got [%s, %s, %s]",
			defs[0].Function.Name, defs[1].Function.Name, defs[2].Function.Name)
	}
}
