package tools

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/dotsetgreg/dotagent/pkg/providers"
)

type scriptedLoopProvider struct {
	responses []*providers.LLMResponse
	index     int
}

func (p *scriptedLoopProvider) Chat(_ context.Context, _ []providers.Message, _ []providers.ToolDefinition, _ string, _ map[string]interface{}) (*providers.LLMResponse, error) {
	if len(p.responses) == 0 {
		return &providers.LLMResponse{Content: ""}, nil
	}
	if p.index >= len(p.responses) {
		return p.responses[len(p.responses)-1], nil
	}
	resp := p.responses[p.index]
	p.index++
	return resp, nil
}

func (p *scriptedLoopProvider) GetDefaultModel() string {
	return "test-model"
}

type loopTestTool struct {
	name string
}

func (t loopTestTool) Name() string { return t.name }

func (t loopTestTool) Description() string { return "loop test tool" }

func (t loopTestTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type":       "object",
		"properties": map[string]interface{}{},
	}
}

func (t loopTestTool) Execute(_ context.Context, _ map[string]interface{}) *ToolResult {
	return &ToolResult{ForLLM: "ok"}
}

func TestRunToolLoop_CircuitBreakerSignatureRepeat(t *testing.T) {
	provider := &scriptedLoopProvider{
		responses: []*providers.LLMResponse{
			{ToolCalls: []providers.ToolCall{{ID: "1", Name: "looptool", Arguments: map[string]interface{}{"q": "same"}}}},
			{ToolCalls: []providers.ToolCall{{ID: "2", Name: "looptool", Arguments: map[string]interface{}{"q": "same"}}}},
			{ToolCalls: []providers.ToolCall{{ID: "3", Name: "looptool", Arguments: map[string]interface{}{"q": "same"}}}},
		},
	}

	registry := NewToolRegistry()
	registry.Register(loopTestTool{name: "looptool"})

	result, err := RunToolLoop(context.Background(), ToolLoopConfig{
		Provider:      provider,
		Model:         "test-model",
		Tools:         registry,
		MaxIterations: 8,
	}, nil, "cli", "direct")
	if err != nil {
		t.Fatalf("RunToolLoop returned error: %v", err)
	}
	if result == nil {
		t.Fatal("RunToolLoop returned nil result")
	}
	if !strings.Contains(result.Content, "repeated tool-call loop") {
		t.Fatalf("expected signature circuit-breaker message, got: %q", result.Content)
	}
	if result.Iterations != 3 {
		t.Fatalf("expected 3 iterations before breaker, got %d", result.Iterations)
	}
}

func TestRunToolLoop_CircuitBreakerToolDrift(t *testing.T) {
	responses := make([]*providers.LLMResponse, 0, 8)
	for i := 1; i <= 8; i++ {
		mode := "a"
		if i%2 == 0 {
			mode = "b"
		}
		responses = append(responses, &providers.LLMResponse{
			ToolCalls: []providers.ToolCall{
				{ID: fmt.Sprintf("loop-%d", i), Name: "looptool", Arguments: map[string]interface{}{"mode": mode}},
				{ID: fmt.Sprintf("noise-%d", i), Name: "noisetool", Arguments: map[string]interface{}{"n": i}},
			},
		})
	}
	provider := &scriptedLoopProvider{responses: responses}

	registry := NewToolRegistry()
	registry.Register(loopTestTool{name: "looptool"})
	registry.Register(loopTestTool{name: "noisetool"})

	result, err := RunToolLoop(context.Background(), ToolLoopConfig{
		Provider:      provider,
		Model:         "test-model",
		Tools:         registry,
		MaxIterations: 12,
	}, nil, "cli", "direct")
	if err != nil {
		t.Fatalf("RunToolLoop returned error: %v", err)
	}
	if result == nil {
		t.Fatal("RunToolLoop returned nil result")
	}
	if !strings.Contains(result.Content, "one tool kept being called repeatedly") {
		t.Fatalf("expected drift circuit-breaker message, got: %q", result.Content)
	}
	if result.Iterations != 8 {
		t.Fatalf("expected 8 iterations before drift breaker, got %d", result.Iterations)
	}
}

func TestRunToolLoop_DoesNotTripDriftBreakerOnDiverseToolArgs(t *testing.T) {
	responses := make([]*providers.LLMResponse, 0, 8)
	for i := 1; i <= 8; i++ {
		responses = append(responses, &providers.LLMResponse{
			ToolCalls: []providers.ToolCall{{ID: fmt.Sprintf("%d", i), Name: "looptool", Arguments: map[string]interface{}{"file": fmt.Sprintf("f-%d.txt", i)}}},
		})
	}
	provider := &scriptedLoopProvider{responses: responses}

	registry := NewToolRegistry()
	registry.Register(loopTestTool{name: "looptool"})

	result, err := RunToolLoop(context.Background(), ToolLoopConfig{
		Provider:      provider,
		Model:         "test-model",
		Tools:         registry,
		MaxIterations: 8,
	}, nil, "cli", "direct")
	if err != nil {
		t.Fatalf("RunToolLoop returned error: %v", err)
	}
	if result == nil {
		t.Fatal("RunToolLoop returned nil result")
	}
	if strings.Contains(result.Content, "one tool kept being called repeatedly") {
		t.Fatalf("did not expect drift breaker for diverse tool args, got: %q", result.Content)
	}
	if !strings.Contains(result.Content, "maximum number of consecutive actions (8)") {
		t.Fatalf("expected max-iterations message, got: %q", result.Content)
	}
}

func TestRunToolLoop_MaxIterationsFallbackMessage(t *testing.T) {
	provider := &scriptedLoopProvider{
		responses: []*providers.LLMResponse{
			{ToolCalls: []providers.ToolCall{{ID: "1", Name: "looptool", Arguments: map[string]interface{}{"n": 1}}}},
			{ToolCalls: []providers.ToolCall{{ID: "2", Name: "looptool", Arguments: map[string]interface{}{"n": 2}}}},
		},
	}
	registry := NewToolRegistry()
	registry.Register(loopTestTool{name: "looptool"})

	result, err := RunToolLoop(context.Background(), ToolLoopConfig{
		Provider:      provider,
		Model:         "test-model",
		Tools:         registry,
		MaxIterations: 2,
	}, nil, "cli", "direct")
	if err != nil {
		t.Fatalf("RunToolLoop returned error: %v", err)
	}
	if result == nil {
		t.Fatal("RunToolLoop returned nil result")
	}
	if !strings.Contains(result.Content, "maximum number of consecutive actions (2)") {
		t.Fatalf("expected max-iterations fallback message, got: %q", result.Content)
	}
	if result.Iterations != 2 {
		t.Fatalf("expected 2 iterations, got %d", result.Iterations)
	}
}
