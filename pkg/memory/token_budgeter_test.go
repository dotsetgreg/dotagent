package memory

import (
	"strings"
	"testing"
)

func TestTokenBudgeter_ModelAwareEstimate(t *testing.T) {
	b := NewTokenBudgeter("")
	text := strings.Repeat("I prefer highly structured context for long running tasks. ", 48)

	gpt := b.EstimateTextTokens("openai/gpt-5.2", text)
	claude := b.EstimateTextTokens("anthropic/claude-3-7-sonnet", text)
	if gpt <= 0 || claude <= 0 {
		t.Fatalf("expected positive estimates, got gpt=%d claude=%d", gpt, claude)
	}
	if gpt == claude {
		t.Fatalf("expected model-aware estimator to produce different counts, got gpt=%d claude=%d", gpt, claude)
	}
}

func TestTokenBudgeter_UsageFeedbackAdjustsEstimate(t *testing.T) {
	b := NewTokenBudgeter("")
	text := strings.Repeat("please keep this as durable memory ", 70)
	model := "openai/gpt-5.2"

	before := b.EstimateTextTokens(model, text)
	for i := 0; i < 12; i++ {
		// Simulate provider-reported prompt usage significantly above estimate.
		b.ObservePromptUsage(model, before, int(float64(before)*1.35))
	}
	after := b.EstimateTextTokens(model, text)
	if after <= before {
		t.Fatalf("expected estimate to increase after feedback, before=%d after=%d", before, after)
	}
}

func TestTokenBudgeter_PersistRoundTrip(t *testing.T) {
	ws := t.TempDir()
	model := "openai/gpt-5.2"
	text := strings.Repeat("stateful calibration signal ", 55)

	b1 := NewTokenBudgeter(ws)
	initial := b1.EstimateTextTokens(model, text)
	b1.ObservePromptUsage(model, initial, int(float64(initial)*1.4))

	b2 := NewTokenBudgeter(ws)
	reloaded := b2.EstimateTextTokens(model, text)
	if reloaded <= initial {
		t.Fatalf("expected persisted calibration to affect estimate, initial=%d reloaded=%d", initial, reloaded)
	}
}
