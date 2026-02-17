package memory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func waitForCondition(tb testing.TB, timeout time.Duration, fn func() bool) {
	tb.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(40 * time.Millisecond)
	}
	tb.Fatalf("condition not met within %s", timeout)
}

func TestPersonaApplyAndRender(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	svc, err := NewService(Config{
		Workspace:       ws,
		AgentID:         "dotagent",
		WorkerPoll:      40 * time.Millisecond,
		PersonaFileSync: PersonaFileSyncImportExport,
	}, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	defer svc.Close()

	session := "discord:persona-render"
	userID := "u-persona-render"
	if err := svc.EnsureSession(ctx, session, "discord", "persona-render", userID); err != nil {
		t.Fatalf("ensure session: %v", err)
	}

	turnA := "turn-a"
	_ = svc.AppendEvent(ctx, Event{SessionKey: session, TurnID: turnA, Seq: 1, Role: "user", Content: "My name is Alex and my timezone is America/Los_Angeles."})
	_ = svc.AppendEvent(ctx, Event{SessionKey: session, TurnID: turnA, Seq: 2, Role: "assistant", Content: "Got it."})
	svc.ScheduleTurnMaintenance(ctx, session, turnA, userID)

	turnB := "turn-b"
	_ = svc.AppendEvent(ctx, Event{SessionKey: session, TurnID: turnB, Seq: 1, Role: "user", Content: "I really prefer pour-over coffee."})
	_ = svc.AppendEvent(ctx, Event{SessionKey: session, TurnID: turnB, Seq: 2, Role: "assistant", Content: "Noted."})
	svc.ScheduleTurnMaintenance(ctx, session, turnB, userID)

	turnC := "turn-c"
	_ = svc.AppendEvent(ctx, Event{SessionKey: session, TurnID: turnC, Seq: 1, Role: "user", Content: "I really prefer pour-over coffee."})
	_ = svc.AppendEvent(ctx, Event{SessionKey: session, TurnID: turnC, Seq: 2, Role: "assistant", Content: "Still noted."})
	svc.ScheduleTurnMaintenance(ctx, session, turnC, userID)

	waitForCondition(t, 4*time.Second, func() bool {
		profile, pErr := svc.store.GetPersonaProfile(ctx, userID, "dotagent")
		return pErr == nil && strings.EqualFold(profile.User.Name, "Alex") && len(profile.User.Preferences) > 0
	})

	profile, err := svc.store.GetPersonaProfile(ctx, userID, "dotagent")
	if err != nil {
		t.Fatalf("get profile: %v", err)
	}
	if profile.User.Name != "Alex" {
		t.Fatalf("expected name Alex, got %q", profile.User.Name)
	}
	if profile.User.Timezone != "America/Los_Angeles" {
		t.Fatalf("expected timezone America/Los_Angeles, got %q", profile.User.Timezone)
	}
	if len(profile.User.Preferences) == 0 {
		t.Fatalf("expected at least one preference to be persisted")
	}

	userRaw, err := os.ReadFile(filepath.Join(ws, "USER.md"))
	if err != nil {
		t.Fatalf("read USER.md: %v", err)
	}
	if !strings.Contains(string(userRaw), "Alex") {
		t.Fatalf("expected USER.md to include applied name")
	}

	pc, err := svc.BuildPromptContext(ctx, session, userID, "what do you know about me", 2048)
	if err != nil {
		t.Fatalf("build prompt context: %v", err)
	}
	if !strings.Contains(strings.ToLower(pc.PersonaPrompt), "alex") {
		t.Fatalf("expected persona prompt to include user name, got: %s", pc.PersonaPrompt)
	}
}

func TestPersonaPersistsAcrossRestartAndCompaction(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()

	create := func() *Service {
		svc, err := NewService(Config{
			Workspace:        ws,
			AgentID:          "dotagent",
			MaxContextTokens: 1024,
			WorkerPoll:       40 * time.Millisecond,
		}, nil)
		if err != nil {
			t.Fatalf("new service: %v", err)
		}
		return svc
	}

	session := "discord:persona-persist"
	userID := "u-persona-persist"

	svc := create()
	if err := svc.EnsureSession(ctx, session, "discord", "persona-persist", userID); err != nil {
		t.Fatalf("ensure session: %v", err)
	}

	turn := "turn-1"
	_ = svc.AppendEvent(ctx, Event{SessionKey: session, TurnID: turn, Seq: 1, Role: "user", Content: "Call me Casey. My timezone is America/New_York."})
	_ = svc.AppendEvent(ctx, Event{SessionKey: session, TurnID: turn, Seq: 2, Role: "assistant", Content: "Done."})
	svc.ScheduleTurnMaintenance(ctx, session, turn, userID)

	waitForCondition(t, 4*time.Second, func() bool {
		profile, pErr := svc.store.GetPersonaProfile(ctx, userID, "dotagent")
		return pErr == nil && strings.EqualFold(profile.User.Name, "Casey")
	})

	for i := 0; i < 60; i++ {
		_ = svc.AppendEvent(ctx, Event{
			SessionKey: session,
			TurnID:     "noise",
			Seq:        i + 10,
			Role:       "user",
			Content:    "filler noise for compaction trigger",
		})
	}
	if err := svc.ForceCompact(ctx, session, userID, 1024); err != nil {
		t.Fatalf("force compact: %v", err)
	}
	_ = svc.Close()

	svc2 := create()
	defer svc2.Close()
	pc, err := svc2.BuildPromptContext(ctx, session, userID, "who am i", 1024)
	if err != nil {
		t.Fatalf("build prompt context after restart: %v", err)
	}
	if !strings.Contains(strings.ToLower(pc.PersonaPrompt), "casey") {
		t.Fatalf("expected persona prompt to preserve Casey after restart+compaction; got %s", pc.PersonaPrompt)
	}
}

func TestPersonaRollback(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	svc, err := NewService(Config{
		Workspace:       ws,
		AgentID:         "dotagent",
		WorkerPoll:      40 * time.Millisecond,
		PersonaFileSync: PersonaFileSyncImportExport,
	}, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	defer svc.Close()

	session := "discord:persona-rollback"
	userID := "u-persona-rollback"
	_ = svc.EnsureSession(ctx, session, "discord", "persona-rollback", userID)

	turn1 := "turn-1"
	_ = svc.AppendEvent(ctx, Event{SessionKey: session, TurnID: turn1, Seq: 1, Role: "user", Content: "My name is Morgan."})
	_ = svc.AppendEvent(ctx, Event{SessionKey: session, TurnID: turn1, Seq: 2, Role: "assistant", Content: "Saved."})
	svc.ScheduleTurnMaintenance(ctx, session, turn1, userID)
	waitForCondition(t, 4*time.Second, func() bool {
		p, _ := svc.store.GetPersonaProfile(ctx, userID, "dotagent")
		return p.User.Name == "Morgan"
	})

	turn2 := "turn-2"
	_ = svc.AppendEvent(ctx, Event{SessionKey: session, TurnID: turn2, Seq: 1, Role: "user", Content: "Actually, call me Riley."})
	_ = svc.AppendEvent(ctx, Event{SessionKey: session, TurnID: turn2, Seq: 2, Role: "assistant", Content: "Updated."})
	svc.ScheduleTurnMaintenance(ctx, session, turn2, userID)
	waitForCondition(t, 4*time.Second, func() bool {
		p, _ := svc.store.GetPersonaProfile(ctx, userID, "dotagent")
		return p.User.Name == "Riley"
	})

	if err := svc.RollbackPersona(ctx, userID); err != nil {
		t.Fatalf("rollback persona: %v", err)
	}
	p, err := svc.store.GetPersonaProfile(ctx, userID, "dotagent")
	if err != nil {
		t.Fatalf("get profile after rollback: %v", err)
	}
	if p.User.Name != "Morgan" {
		t.Fatalf("expected rollback to restore Morgan, got %q", p.User.Name)
	}
}

func TestPersonaFileImportMerge(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	svc, err := NewService(Config{
		Workspace:       ws,
		AgentID:         "dotagent",
		WorkerPoll:      40 * time.Millisecond,
		PersonaFileSync: PersonaFileSyncImportExport,
	}, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	defer svc.Close()

	userID := "u-file-import"
	session := "discord:file-import"
	_ = svc.EnsureSession(ctx, session, "discord", "file-import", userID)

	userFile := `# User

## Name
Jordan

## Timezone
Europe/Berlin

## Preferences
- editor: neovim
`
	if err := os.WriteFile(filepath.Join(ws, "USER.md"), []byte(userFile), 0o644); err != nil {
		t.Fatalf("write USER.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "IDENTITY.md"), []byte("# Identity\n"), 0o644); err != nil {
		t.Fatalf("write IDENTITY.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "SOUL.md"), []byte("# Soul\n"), 0o644); err != nil {
		t.Fatalf("write SOUL.md: %v", err)
	}

	pc, err := svc.BuildPromptContext(ctx, session, userID, "what is my profile", 2048)
	if err != nil {
		t.Fatalf("build prompt context: %v", err)
	}
	if !strings.Contains(strings.ToLower(pc.PersonaPrompt), "jordan") {
		t.Fatalf("expected persona prompt to include imported name; got %s", pc.PersonaPrompt)
	}

	p, err := svc.store.GetPersonaProfile(ctx, userID, "dotagent")
	if err != nil {
		t.Fatalf("get profile: %v", err)
	}
	if p.User.Name != "Jordan" {
		t.Fatalf("expected imported name Jordan, got %q", p.User.Name)
	}
	if p.User.Timezone != "Europe/Berlin" {
		t.Fatalf("expected imported timezone Europe/Berlin, got %q", p.User.Timezone)
	}
}
