package memory

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRecallParityBeforeAfterCompaction(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	svc, err := NewService(Config{
		Workspace:        ws,
		AgentID:          "dotagent",
		MaxContextTokens: 1024,
		WorkerPoll:       100 * time.Millisecond,
	}, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	defer svc.Close()

	session := "discord:parity"
	userID := "u-parity"
	if err := svc.EnsureSession(ctx, session, "discord", "parity", userID); err != nil {
		t.Fatalf("ensure session: %v", err)
	}

	turnID := "turn-parity-1"
	_ = svc.AppendEvent(ctx, Event{SessionKey: session, TurnID: turnID, Seq: 1, Role: "user", Content: "I prefer medium roast coffee."})
	_ = svc.AppendEvent(ctx, Event{SessionKey: session, TurnID: turnID, Seq: 2, Role: "assistant", Content: "Understood."})
	svc.ScheduleTurnMaintenance(ctx, session, turnID, userID)
	time.Sleep(400 * time.Millisecond)

	before, err := svc.BuildPromptContext(ctx, session, userID, "what coffee do I prefer", 1024)
	if err != nil {
		t.Fatalf("build prompt context before compaction: %v", err)
	}
	if !containsRecall(before.RecallCards, "medium roast") {
		t.Fatalf("expected recall before compaction to include medium roast")
	}

	for i := 0; i < 60; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		_ = svc.AppendEvent(ctx, Event{SessionKey: session, TurnID: "turn-noise", Seq: i + 3, Role: role, Content: "filler context to trigger compaction and reduce active history"})
	}
	if err := svc.ForceCompact(ctx, session, userID, 1024); err != nil {
		t.Fatalf("force compact: %v", err)
	}

	after, err := svc.BuildPromptContext(ctx, session, userID, "what coffee do I prefer", 1024)
	if err != nil {
		t.Fatalf("build prompt context after compaction: %v", err)
	}
	if !containsRecall(after.RecallCards, "medium roast") {
		t.Fatalf("expected recall after compaction to include medium roast")
	}
}

func TestCrossTopicContinuity(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	svc, err := NewService(Config{
		Workspace:        ws,
		AgentID:          "dotagent",
		MaxContextTokens: 2048,
		WorkerPoll:       100 * time.Millisecond,
	}, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	defer svc.Close()

	session := "discord:topics"
	userID := "u-topics"
	if err := svc.EnsureSession(ctx, session, "discord", "topics", userID); err != nil {
		t.Fatalf("ensure session: %v", err)
	}

	_ = svc.AppendEvent(ctx, Event{SessionKey: session, TurnID: "turn-a", Seq: 1, Role: "user", Content: "I love mountain biking on weekends."})
	_ = svc.AppendEvent(ctx, Event{SessionKey: session, TurnID: "turn-a", Seq: 2, Role: "assistant", Content: "Sounds great."})
	svc.ScheduleTurnMaintenance(ctx, session, "turn-a", userID)
	time.Sleep(300 * time.Millisecond)

	_ = svc.AppendEvent(ctx, Event{SessionKey: session, TurnID: "turn-b", Seq: 3, Role: "user", Content: "Now help me debug a Go race condition."})
	_ = svc.AppendEvent(ctx, Event{SessionKey: session, TurnID: "turn-b", Seq: 4, Role: "assistant", Content: "Let's inspect the goroutines."})
	svc.ScheduleTurnMaintenance(ctx, session, "turn-b", userID)
	time.Sleep(300 * time.Millisecond)

	ctxOut, err := svc.BuildPromptContext(ctx, session, userID, "what hobby do I enjoy", 2048)
	if err != nil {
		t.Fatalf("build prompt context: %v", err)
	}
	if !containsRecall(ctxOut.RecallCards, "mountain biking") {
		t.Fatalf("expected recall to retain cross-topic preference")
	}
}

func TestCrashRecoveryMidTurn(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	dbPath := filepath.Join(ws, "state", "memory.db")

	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	if err := store.EnsureSession(ctx, "discord:crash", "discord", "crash", "u-crash"); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	_ = store.AppendEvent(ctx, Event{SessionKey: "discord:crash", TurnID: "turn-crash", Seq: 1, Role: "user", Content: "run command"})
	_ = store.AppendEvent(ctx, Event{SessionKey: "discord:crash", TurnID: "turn-crash", Seq: 2, Role: "assistant", Content: "calling tool"})
	_ = store.AppendEvent(ctx, Event{SessionKey: "discord:crash", TurnID: "turn-crash", Seq: 3, Role: "tool", Content: "tool output", ToolCallID: "tc1"})
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	store2, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer store2.Close()

	events, err := store2.ListRecentEvents(ctx, "discord:crash", 10, false)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 recovered events, got %d", len(events))
	}
}

func TestRecallQualitySmoke(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	svc, err := NewService(Config{Workspace: ws, AgentID: "dotagent", MaxContextTokens: 4096, WorkerPoll: 100 * time.Millisecond}, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	defer svc.Close()

	session := "discord:quality"
	userID := "u-quality"
	_ = svc.EnsureSession(ctx, session, "discord", "quality", userID)

	pairs := []struct {
		fact  string
		query string
		want  string
	}{
		{"I prefer TypeScript over JavaScript", "what language do I prefer", "typescript"},
		{"My timezone is America/New_York", "what timezone am I in", "america/new_york"},
		{"I love pour-over coffee", "coffee preference", "pour-over"},
	}

	for i, p := range pairs {
		turn := "turn-quality-" + string(rune('A'+i))
		_ = svc.AppendEvent(ctx, Event{SessionKey: session, TurnID: turn, Seq: 1, Role: "user", Content: p.fact})
		_ = svc.AppendEvent(ctx, Event{SessionKey: session, TurnID: turn, Seq: 2, Role: "assistant", Content: "Noted."})
		svc.ScheduleTurnMaintenance(ctx, session, turn, userID)
	}
	time.Sleep(800 * time.Millisecond)

	for _, p := range pairs {
		ctxOut, err := svc.BuildPromptContext(ctx, session, userID, p.query, 4096)
		if err != nil {
			t.Fatalf("build prompt context: %v", err)
		}
		if !containsRecall(ctxOut.RecallCards, p.want) {
			t.Fatalf("expected recall for query %q to include %q", p.query, p.want)
		}
	}
}

func TestRecallLatencyBudget(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	svc, err := NewService(Config{Workspace: ws, AgentID: "dotagent", MaxContextTokens: 4096, WorkerPoll: 100 * time.Millisecond}, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	defer svc.Close()

	session := "discord:latency"
	userID := "u-latency"
	_ = svc.EnsureSession(ctx, session, "discord", "latency", userID)

	for i := 0; i < 300; i++ {
		turn := fmt.Sprintf("turn-latency-%d", i)
		_ = svc.AppendEvent(ctx, Event{SessionKey: session, TurnID: turn, Seq: 1, Role: "user", Content: fmt.Sprintf("I prefer profile item %d", i)})
		_ = svc.AppendEvent(ctx, Event{SessionKey: session, TurnID: turn, Seq: 2, Role: "assistant", Content: "Noted."})
		svc.ScheduleTurnMaintenance(ctx, session, turn, userID)
	}
	time.Sleep(1 * time.Second)

	start := time.Now()
	if _, err := svc.BuildPromptContext(ctx, session, userID, "which profile item do I prefer", 4096); err != nil {
		t.Fatalf("build prompt context: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 700*time.Millisecond {
		t.Fatalf("recall latency too high: %s", elapsed)
	}
}

func containsRecall(cards []MemoryCard, needle string) bool {
	needle = strings.ToLower(needle)
	for _, card := range cards {
		if strings.Contains(strings.ToLower(card.Content), needle) {
			return true
		}
	}
	return false
}
