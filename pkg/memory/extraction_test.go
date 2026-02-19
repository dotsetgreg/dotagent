package memory

import (
	"strings"
	"testing"
)

func TestExtractUserContentUpsertOps_QuestionAvoidsFactCapture(t *testing.T) {
	ops := extractUserContentUpsertOps("Do I like TypeScript?", "evt-1")
	for _, op := range ops {
		if op.Action != "upsert" {
			continue
		}
		if op.Kind == MemoryUserPreference || op.Key == "identity/name" || op.Key == "profile/timezone_or_location" {
			t.Fatalf("unexpected fact capture for question-only input: %+v", op)
		}
	}
}

func TestExtractUserContentUpsertOps_DeclarativeFactCapture(t *testing.T) {
	ops := extractUserContentUpsertOps("My name is Alex and my timezone is America/Chicago.", "evt-2")
	foundName := false
	foundTZ := false
	for _, op := range ops {
		if op.Action != "upsert" {
			continue
		}
		if op.Key == "identity/name" && strings.Contains(strings.ToLower(op.Content), "alex") {
			foundName = true
		}
		if op.Key == "profile/timezone_or_location" && strings.Contains(strings.ToLower(op.Content), "america/chicago") {
			foundTZ = true
		}
	}
	if !foundName || !foundTZ {
		t.Fatalf("expected declarative facts to be captured, foundName=%v foundTZ=%v ops=%+v", foundName, foundTZ, ops)
	}
}

func TestExtractUserContentUpsertOps_QuestionWithPersistenceCueStillCaptures(t *testing.T) {
	ops := extractUserContentUpsertOps("Can you remember this: my name is Alex?", "evt-3")
	for _, op := range ops {
		if op.Key == "identity/name" && strings.Contains(strings.ToLower(op.Content), "alex") {
			return
		}
	}
	t.Fatalf("expected identity capture when explicit persistence cue is present; ops=%+v", ops)
}
