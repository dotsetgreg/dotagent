package tools

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/dotsetgreg/dotagent/pkg/providers"
)

type constantTool struct {
	name   string
	output string
}

func (t constantTool) Name() string { return t.name }

func (t constantTool) Description() string { return "constant tool" }

func (t constantTool) Parameters() map[string]interface{} {
	return map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
}

func (t constantTool) Execute(_ context.Context, _ map[string]interface{}) *ToolResult {
	return &ToolResult{ForLLM: t.output}
}

type overflowThenSuccessProvider struct {
	calls int
}

func (p *overflowThenSuccessProvider) Chat(_ context.Context, _ []providers.Message, _ []providers.ToolDefinition, _ string, _ map[string]interface{}) (*providers.LLMResponse, error) {
	p.calls++
	if p.calls == 1 {
		return nil, fmt.Errorf("maximum context length exceeded")
	}
	return &providers.LLMResponse{Content: "ok"}, nil
}

func (p *overflowThenSuccessProvider) GetDefaultModel() string { return "test-model" }

type scriptedToolProvider struct {
	responses []*providers.LLMResponse
	idx       int
}

func (p *scriptedToolProvider) Chat(_ context.Context, _ []providers.Message, _ []providers.ToolDefinition, _ string, _ map[string]interface{}) (*providers.LLMResponse, error) {
	if p.idx >= len(p.responses) {
		return &providers.LLMResponse{Content: "done"}, nil
	}
	resp := p.responses[p.idx]
	p.idx++
	return resp, nil
}

func (p *scriptedToolProvider) GetDefaultModel() string { return "test-model" }

func TestLoopGuard_TruncatesOversizedToolResults(t *testing.T) {
	messages := []providers.Message{
		{Role: "system", Content: "sys"},
		{Role: "tool", Content: strings.Repeat("A", 12000)},
		{Role: "tool", Content: strings.Repeat("B", 9000)},
	}
	enforceToolResultContextBudgetInPlace(messages, 1024)

	limit := maxSingleToolResultChars(1024)
	for _, msg := range messages {
		if msg.Role != "tool" {
			continue
		}
		if len(msg.Content) > limit {
			t.Fatalf("expected tool content <= %d chars, got %d", limit, len(msg.Content))
		}
		if !strings.Contains(msg.Content, "tool result trimmed") {
			t.Fatalf("expected truncation marker in tool content")
		}
	}
	if got := estimateContextChars(messages); got > contextBudgetChars(1024) {
		t.Fatalf("expected total context chars <= budget (%d), got %d", contextBudgetChars(1024), got)
	}
}

func TestRunToolLoop_OverflowRecoveryRebuildContext(t *testing.T) {
	provider := &overflowThenSuccessProvider{}
	rebuildCalls := 0

	result, err := RunToolLoop(context.Background(), ToolLoopConfig{
		Provider:            provider,
		Model:               "test-model",
		MaxIterations:       4,
		ContextWindowTokens: 4096,
		RebuildContext: func(context.Context) ([]providers.Message, error) {
			rebuildCalls++
			return []providers.Message{{Role: "user", Content: "rebuilt"}}, nil
		},
	}, []providers.Message{{Role: "user", Content: "original"}}, "cli", "direct")
	if err != nil {
		t.Fatalf("RunToolLoop returned error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.Content != "ok" {
		t.Fatalf("expected final content 'ok', got %q", result.Content)
	}
	if provider.calls != 2 {
		t.Fatalf("expected 2 provider calls, got %d", provider.calls)
	}
	if rebuildCalls != 1 {
		t.Fatalf("expected 1 rebuild call, got %d", rebuildCalls)
	}
}

func TestRunToolLoop_PollingNoProgressBreaker(t *testing.T) {
	responses := make([]*providers.LLMResponse, 0, 6)
	for i := 0; i < 6; i++ {
		responses = append(responses, &providers.LLMResponse{ToolCalls: []providers.ToolCall{
			{ID: fmt.Sprintf("poll-%d", i), Name: "process", Arguments: map[string]interface{}{"action": "poll", "process_id": "1"}},
			{ID: fmt.Sprintf("noise-%d", i), Name: "noisetool", Arguments: map[string]interface{}{"n": i}},
		}})
	}

	registry := NewToolRegistry()
	registry.Register(constantTool{name: "process", output: "status=running"})
	registry.Register(constantTool{name: "noisetool", output: "ok"})

	result, err := RunToolLoop(context.Background(), ToolLoopConfig{
		Provider:            &scriptedToolProvider{responses: responses},
		Model:               "test-model",
		Tools:               registry,
		MaxIterations:       10,
		ContextWindowTokens: 4096,
	}, nil, "cli", "direct")
	if err != nil {
		t.Fatalf("RunToolLoop returned error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if !strings.Contains(strings.ToLower(result.Content), "polling") {
		t.Fatalf("expected polling loop breaker message, got: %q", result.Content)
	}
	if result.BreakReason != "known_poll_no_progress" {
		t.Fatalf("expected break reason known_poll_no_progress, got %q", result.BreakReason)
	}
}

func TestLoopDetector_PingPongNoProgress(t *testing.T) {
	d := newToolLoopDetector(defaultToolLoopDetectionConfig())
	callA := providers.ToolCall{ID: "a", Name: "alpha", Arguments: map[string]interface{}{"k": "a"}}
	callB := providers.ToolCall{ID: "b", Name: "beta", Arguments: map[string]interface{}{"k": "b"}}

	var outcome *loopDetectionOutcome
	for i := 0; i < 6; i++ {
		call := callA
		result := "res-a"
		if i%2 == 1 {
			call = callB
			result = "res-b"
		}
		outcome = d.recordToolOutcome(call, result)
	}
	if outcome == nil {
		t.Fatalf("expected ping-pong detector to break")
	}
	if !strings.EqualFold(outcome.Level, "critical") {
		t.Fatalf("expected critical level, got %q", outcome.Level)
	}
	if outcome.Reason != "ping_pong" {
		t.Fatalf("expected ping_pong reason, got %q", outcome.Reason)
	}
}

func TestApplyContextPruningInPlace_BalancedKeepsRecent(t *testing.T) {
	messages := []providers.Message{{Role: "system", Content: "sys"}}
	for i := 0; i < 7; i++ {
		messages = append(messages, providers.Message{
			Role:    "tool",
			Content: fmt.Sprintf("tool-%d\n%s", i, strings.Repeat("X", 900)),
		})
	}

	applyContextPruningInPlace(messages, "balanced", 3)

	for i := 0; i < 7; i++ {
		msg := messages[i+1]
		if i < 4 {
			if !(strings.Contains(msg.Content, "tool result trimmed") || strings.Contains(msg.Content, "pruned by context policy")) {
				t.Fatalf("expected old tool message %d to be pruned, got %q", i, msg.Content[:minInt(len(msg.Content), 80)])
			}
			continue
		}
		if !strings.HasPrefix(msg.Content, fmt.Sprintf("tool-%d", i)) {
			t.Fatalf("expected recent tool message %d to remain intact", i)
		}
	}
}

func TestLoopDetector_SignatureWarningThenCritical(t *testing.T) {
	cfg := normalizeToolLoopDetectionConfig(ToolLoopDetectionConfig{
		Enabled:                    true,
		WarningsEnabled:            true,
		SignatureWarnThreshold:     2,
		SignatureCriticalThreshold: 3,
	})
	d := newToolLoopDetector(cfg)
	calls := []providers.ToolCall{{ID: "1", Name: "echo", Arguments: map[string]interface{}{"q": "x"}}}

	if out := d.checkResponsePattern(calls); out != nil {
		t.Fatalf("unexpected first outcome: %+v", out)
	}
	out := d.checkResponsePattern(calls)
	if out == nil || !strings.EqualFold(out.Level, "warning") {
		t.Fatalf("expected warning outcome on second repeat, got %+v", out)
	}
	out = d.checkResponsePattern(calls)
	if out == nil || !strings.EqualFold(out.Level, "critical") {
		t.Fatalf("expected critical outcome on third repeat, got %+v", out)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
