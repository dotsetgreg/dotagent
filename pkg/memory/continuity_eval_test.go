package memory

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"
)

// Long-horizon synthetic continuity regression suite.
func TestContinuityEval_LongHorizonSynthetic(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping long-horizon continuity eval in short mode")
	}
	ctx := context.Background()
	dir := t.TempDir()
	svc, err := NewService(Config{
		Workspace:        dir,
		AgentID:          "dotagent",
		MaxContextTokens: 4096,
		WorkerPoll:       35 * time.Millisecond,
	}, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	defer svc.Close()

	sessionKey := "discord:continuity-eval"
	userID := "u-eval"
	if err := svc.EnsureSession(ctx, sessionKey, "discord", "continuity-eval", userID); err != nil {
		t.Fatalf("ensure session: %v", err)
	}

	type factCase struct {
		statement string
		query     string
		needle    string
	}
	facts := []factCase{
		{"I really prefer pour-over coffee.", "what coffee method do I prefer?", "pour-over"},
		{"I use Neovim for coding.", "which editor do I use?", "neovim"},
		{"My timezone is America/New_York.", "what timezone am I in?", "america/new_york"},
		{"I live in Washington, DC.", "where do I live?", "washington, dc"},
		{"I love Ethiopian coffee beans.", "what beans do I like?", "ethiopian"},
		{"I am reading the third book in a space-opera series.", "what am I reading?", "third book"},
	}

	rng := rand.New(rand.NewSource(42))
	totalTurns := 180
	for i := 0; i < totalTurns; i++ {
		f := facts[rng.Intn(len(facts))]
		turnID := fmt.Sprintf("turn-%03d", i)
		if _, _, err := svc.RecordUserTurn(ctx, Event{
			SessionKey: sessionKey,
			TurnID:     turnID,
			Seq:        1,
			Content:    f.statement,
		}, userID); err != nil {
			t.Fatalf("record user turn %d: %v", i, err)
		}
		if err := svc.AppendEvent(ctx, Event{
			SessionKey: sessionKey,
			TurnID:     turnID,
			Seq:        2,
			Role:       "assistant",
			Content:    "Noted.",
		}); err != nil {
			t.Fatalf("append assistant turn %d: %v", i, err)
		}
		svc.ScheduleTurnMaintenance(ctx, sessionKey, turnID, userID)
	}
	time.Sleep(2 * time.Second)

	hits := 0
	for _, f := range facts {
		pc, err := svc.BuildPromptContext(ctx, sessionKey, userID, f.query, 4096)
		if err != nil {
			t.Fatalf("build prompt context query %q: %v", f.query, err)
		}
		if recallContains(pc.RecallCards, f.needle) {
			hits++
		}
	}
	hitRate := float64(hits) / float64(len(facts))
	if hitRate < 0.83 {
		t.Fatalf("continuity eval hit rate too low: %.2f", hitRate)
	}
}

func recallContains(cards []MemoryCard, needle string) bool {
	needle = strings.ToLower(strings.TrimSpace(needle))
	for _, c := range cards {
		if strings.Contains(strings.ToLower(c.Content), needle) {
			return true
		}
	}
	return false
}
