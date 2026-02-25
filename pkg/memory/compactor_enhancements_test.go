package memory

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestBuildCompactionTranscript_StripsToolPayloadDetails(t *testing.T) {
	events := []Event{
		{Role: "user", Content: "Please gather project diagnostics."},
		{Role: "tool", Content: `{"status":"ok","stdout":"` + strings.Repeat("x", 1600) + `"}`},
	}
	transcript := buildCompactionTranscript(events, 12000, 320, false)
	if !strings.Contains(transcript, "tool:") {
		t.Fatalf("expected tool role in transcript")
	}
	if strings.Contains(transcript, strings.Repeat("x", 400)) {
		t.Fatalf("expected large tool payload to be stripped")
	}
	lower := strings.ToLower(transcript)
	if !strings.Contains(lower, "stripped") && !strings.Contains(lower, "truncated") {
		t.Fatalf("expected stripping/truncation marker in transcript")
	}
}

func TestSessionCompactor_SummarizeWithRecoveryFallsBackOnTimeout(t *testing.T) {
	compactor := NewSessionCompactor(nil, func(ctx context.Context, existingSummary, transcript string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}, CompactorConfig{
		SummaryTimeout:     5 * time.Millisecond,
		ChunkChars:         128,
		MaxTranscriptChars: 1024,
		PartialSkipChars:   128,
	})

	events := []Event{
		{Role: "user", Content: "We should keep this as a durable reminder."},
		{Role: "assistant", Content: "Acknowledged. I will keep this in continuity notes."},
	}
	summary, mode, err := compactor.summarizeWithRecovery(context.Background(), "", events)
	if err != nil {
		t.Fatalf("expected graceful recovery without hard error, got %v", err)
	}
	if summary != "" {
		t.Fatalf("expected empty summary before heuristic fallback in caller, got %q", summary)
	}
	if mode != "heuristic_emergency" {
		t.Fatalf("expected heuristic emergency mode, got %q", mode)
	}
}
