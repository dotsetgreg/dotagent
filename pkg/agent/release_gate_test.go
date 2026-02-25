package agent

import (
	"context"
	"fmt"
	"testing"

	"github.com/dotsetgreg/dotagent/pkg/providers"
	"github.com/dotsetgreg/dotagent/pkg/tools"
)

type gateConstantTool struct {
	name   string
	output string
}

func (t gateConstantTool) Name() string { return t.name }

func (t gateConstantTool) Description() string { return "gate constant tool" }

func (t gateConstantTool) Parameters() map[string]interface{} {
	return map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
}

func (t gateConstantTool) Execute(_ context.Context, _ map[string]interface{}) *tools.ToolResult {
	return &tools.ToolResult{ForLLM: t.output}
}

type gateScriptProvider struct {
	responses []*providers.LLMResponse
	idx       int
}

func (p *gateScriptProvider) Chat(_ context.Context, _ []providers.Message, _ []providers.ToolDefinition, _ string, _ map[string]interface{}) (*providers.LLMResponse, error) {
	if p.idx >= len(p.responses) {
		return &providers.LLMResponse{Content: "done"}, nil
	}
	resp := p.responses[p.idx]
	p.idx++
	return resp, nil
}

func (p *gateScriptProvider) GetDefaultModel() string { return "test-model" }

type overflowStormProvider struct {
	calls int
}

func (p *overflowStormProvider) Chat(_ context.Context, _ []providers.Message, _ []providers.ToolDefinition, _ string, _ map[string]interface{}) (*providers.LLMResponse, error) {
	p.calls++
	if p.calls == 1 {
		return nil, fmt.Errorf("maximum context length exceeded")
	}
	return &providers.LLMResponse{Content: "ok"}, nil
}

func (p *overflowStormProvider) GetDefaultModel() string { return "test-model" }

func TestReleaseGate_ToolLoopRunawayRateZero(t *testing.T) {
	const runs = 220
	runaway := 0

	for i := 0; i < runs; i++ {
		responses := make([]*providers.LLMResponse, 0, 6)
		for j := 0; j < 6; j++ {
			responses = append(responses, &providers.LLMResponse{
				ToolCalls: []providers.ToolCall{
					{ID: fmt.Sprintf("poll-%d", j), Name: "process", Arguments: map[string]interface{}{"action": "poll", "process_id": "gate-proc"}},
				},
			})
		}
		provider := &gateScriptProvider{responses: responses}
		registry := tools.NewToolRegistry()
		registry.Register(gateConstantTool{name: "process", output: "status=running"})

		result, err := tools.RunToolLoop(context.Background(), tools.ToolLoopConfig{
			Provider:            provider,
			Model:               "test-model",
			Tools:               registry,
			MaxIterations:       10,
			ContextWindowTokens: 4096,
		}, nil, "cli", "direct")
		if err != nil {
			t.Fatalf("tool loop run %d failed unexpectedly: %v", i, err)
		}
		if result == nil || result.BreakReason == "" {
			runaway++
		}
	}

	if runaway != 0 {
		t.Fatalf("tool_loop_runaway_rate gate failed: runaway=%d runs=%d", runaway, runs)
	}
}

func TestReleaseGate_ContextOverflowHardFailRate(t *testing.T) {
	const runs = 220
	failures := 0

	for i := 0; i < runs; i++ {
		provider := &overflowStormProvider{}
		result, err := tools.RunToolLoop(context.Background(), tools.ToolLoopConfig{
			Provider:            provider,
			Model:               "test-model",
			MaxIterations:       4,
			ContextWindowTokens: 2048,
			RebuildContext: func(context.Context) ([]providers.Message, error) {
				return []providers.Message{{Role: "user", Content: "rebuilt context"}}, nil
			},
		}, []providers.Message{{Role: "user", Content: "initial context"}}, "cli", "direct")
		if err != nil || result == nil || result.Content != "ok" {
			failures++
		}
	}

	hardFailRate := float64(failures) / float64(runs)
	if hardFailRate >= 0.005 {
		t.Fatalf("context_overflow_hard_fail_rate gate failed: rate=%.4f failures=%d runs=%d", hardFailRate, failures, runs)
	}
}
