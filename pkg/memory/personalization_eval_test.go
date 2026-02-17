package memory

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Personalization evaluation smoke suite for long-horizon behavior.
func TestPersonalizationEval_LongHorizon(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	svc, err := NewService(Config{
		Workspace:        ws,
		AgentID:          "dotagent",
		MaxContextTokens: 2048,
		WorkerPoll:       40 * time.Millisecond,
	}, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	defer svc.Close()

	session := "discord:eval"
	userID := "u-eval"
	if err := svc.EnsureSession(ctx, session, "discord", "eval", userID); err != nil {
		t.Fatalf("ensure session: %v", err)
	}

	turns := []string{
		"My name is Quinn.",
		"My timezone is America/Chicago.",
		"I really prefer concise responses.",
		"I really prefer concise responses.",
		"I love pour-over coffee.",
		"I love pour-over coffee.",
		"My goal is to improve system design interviews.",
	}
	for i, msg := range turns {
		turn := "turn-eval-" + string(rune('a'+i))
		_ = svc.AppendEvent(ctx, Event{SessionKey: session, TurnID: turn, Seq: 1, Role: "user", Content: msg})
		_ = svc.AppendEvent(ctx, Event{SessionKey: session, TurnID: turn, Seq: 2, Role: "assistant", Content: "Noted."})
		svc.ScheduleTurnMaintenance(ctx, session, turn, userID)
	}

	waitForCondition(t, 5*time.Second, func() bool {
		p, err := svc.store.GetPersonaProfile(ctx, userID, "dotagent")
		return err == nil && p.User.Name == "Quinn" && p.User.Timezone == "America/Chicago"
	})

	pc, err := svc.BuildPromptContext(ctx, session, userID, "what do you know about me", 2048)
	if err != nil {
		t.Fatalf("build prompt context: %v", err)
	}
	prompt := strings.ToLower(pc.PersonaPrompt)
	if !strings.Contains(prompt, "quinn") {
		t.Fatalf("expected persona prompt to include name")
	}
	if !strings.Contains(prompt, "america/chicago") {
		t.Fatalf("expected persona prompt to include timezone")
	}
	if !strings.Contains(prompt, "concise") {
		t.Fatalf("expected persona prompt to include communication preference")
	}
}
