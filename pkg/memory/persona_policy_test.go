package memory

import "testing"

func TestPersonaPolicy_RejectsStableConflictsWithoutExplicitOverride(t *testing.T) {
	policy := NewPersonaPolicyEngine(PersonaPolicyConfig{
		Mode:          "balanced",
		MinConfidence: 0.52,
	})

	cand := PersonaUpdateCandidate{
		FieldPath:  "identity.agent_name",
		Operation:  "set",
		Value:      "Luna",
		Confidence: 0.7,
		Evidence:   "you are now Luna",
	}
	allowed, reason := policy.Evaluate(cand, "DotAgent", "Luna")
	if allowed {
		t.Fatalf("expected stable conflict to be rejected")
	}
	if reason != PersonaReasonStableFieldConflict {
		t.Fatalf("unexpected reason: %s", reason)
	}
}

func TestPersonaPolicy_AllowsExplicitStableOverride(t *testing.T) {
	policy := NewPersonaPolicyEngine(PersonaPolicyConfig{
		Mode:          "balanced",
		MinConfidence: 0.52,
	})

	cand := PersonaUpdateCandidate{
		FieldPath:  "identity.agent_name",
		Operation:  "set",
		Value:      "Luna",
		Confidence: 0.7,
		Evidence:   "Your name is Luna from now on.",
	}
	allowed, reason := policy.Evaluate(cand, "DotAgent", "Luna")
	if !allowed {
		t.Fatalf("expected explicit override to be allowed, reason: %s", reason)
	}
}
