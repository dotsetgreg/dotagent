package providers

import (
	"context"
	"testing"
)

type mockWindowProvider struct {
	window int
}

func (m mockWindowProvider) Chat(ctx context.Context, messages []Message, tools []ToolDefinition, model string, options map[string]interface{}) (*LLMResponse, error) {
	_ = ctx
	_ = messages
	_ = tools
	_ = model
	_ = options
	return &LLMResponse{Content: "ok", FinishReason: "stop"}, nil
}

func (m mockWindowProvider) GetDefaultModel() string { return "mock/model" }

func (m mockWindowProvider) ResolveContextWindow(ctx context.Context, model string) (int, error) {
	_ = ctx
	_ = model
	return m.window, nil
}

func TestResolveContextWindow_PrefersLargestKnownWindow(t *testing.T) {
	ctx := context.Background()
	provider := mockWindowProvider{window: 262144}
	tokens, source := ResolveContextWindow(ctx, provider, "openai/gpt-5", 16384)
	if tokens != 400000 {
		t.Fatalf("expected model metadata window 400000, got %d (source=%s)", tokens, source)
	}
}

func TestResolveContextWindow_ClampsToMinimum(t *testing.T) {
	ctx := context.Background()
	provider := mockWindowProvider{window: 2048}
	tokens, _ := ResolveContextWindow(ctx, provider, "unknown-model", 0)
	if tokens < minContextWindowTokens {
		t.Fatalf("expected minimum context window clamp, got %d", tokens)
	}
}
