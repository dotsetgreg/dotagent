package memory

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestSQLiteStore_EventPersistence(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state", "memory.db")

	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	sessionKey := "discord:123"
	if err := store.EnsureSession(ctx, sessionKey, "discord", "123", "u1"); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	if err := store.AppendEvent(ctx, Event{SessionKey: sessionKey, TurnID: "t1", Seq: 1, Role: "user", Content: "hello"}); err != nil {
		t.Fatalf("append user event: %v", err)
	}
	if err := store.AppendEvent(ctx, Event{SessionKey: sessionKey, TurnID: "t1", Seq: 2, Role: "assistant", Content: "world"}); err != nil {
		t.Fatalf("append assistant event: %v", err)
	}

	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	store2, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer store2.Close()

	events, err := store2.ListRecentEvents(ctx, sessionKey, 10, false)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].Content != "hello" || events[1].Content != "world" {
		t.Fatalf("unexpected event contents: %#v", events)
	}
}

func TestService_ConsolidateAndRecall(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	svc, err := NewService(Config{Workspace: dir, AgentID: "dotagent", MaxContextTokens: 4096, WorkerPoll: 100 * time.Millisecond}, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	defer svc.Close()

	sessionKey := "discord:456"
	userID := "user-1"
	if err := svc.EnsureSession(ctx, sessionKey, "discord", "456", userID); err != nil {
		t.Fatalf("ensure session: %v", err)
	}

	turnID := "turn-1"
	if err := svc.AppendEvent(ctx, Event{SessionKey: sessionKey, TurnID: turnID, Seq: 1, Role: "user", Content: "I prefer dark roast coffee"}); err != nil {
		t.Fatalf("append event: %v", err)
	}
	if err := svc.AppendEvent(ctx, Event{SessionKey: sessionKey, TurnID: turnID, Seq: 2, Role: "assistant", Content: "Noted, I will remember your coffee preference."}); err != nil {
		t.Fatalf("append event: %v", err)
	}

	svc.ScheduleTurnMaintenance(ctx, sessionKey, turnID, userID)
	time.Sleep(600 * time.Millisecond)

	pc, err := svc.BuildPromptContext(ctx, sessionKey, userID, "What coffee do I like?", 4096)
	if err != nil {
		t.Fatalf("build prompt context: %v", err)
	}
	if len(pc.RecallCards) == 0 {
		t.Fatalf("expected recall cards, got none")
	}
}

func TestCompactor_ArchivesOldEvents(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	store, err := NewSQLiteStore(filepath.Join(dir, "state", "memory.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer store.Close()

	sessionKey := "discord:789"
	if err := store.EnsureSession(ctx, sessionKey, "discord", "789", "u1"); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	for i := 0; i < 40; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		if err := store.AppendEvent(ctx, Event{SessionKey: sessionKey, TurnID: "turn", Seq: i + 1, Role: role, Content: "message content payload long enough to trigger compaction"}); err != nil {
			t.Fatalf("append event %d: %v", i, err)
		}
	}

	comp := NewSessionCompactor(store, nil)
	if err := comp.CompactSession(ctx, sessionKey, "u1", "dotagent", DeriveContextBudget(1024)); err != nil {
		t.Fatalf("compact session: %v", err)
	}

	active, err := store.ListRecentEvents(ctx, sessionKey, 100, false)
	if err != nil {
		t.Fatalf("list active events: %v", err)
	}
	if len(active) >= 40 {
		t.Fatalf("expected compaction to reduce active events, got %d", len(active))
	}

	summary, err := store.GetSessionSummary(ctx, sessionKey)
	if err != nil {
		t.Fatalf("get summary: %v", err)
	}
	if summary == "" {
		t.Fatalf("expected non-empty summary after compaction")
	}
}

func TestService_CloseIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	svc, err := NewService(Config{Workspace: dir, AgentID: "dotagent", WorkerPoll: 100 * time.Millisecond}, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	if err := svc.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := svc.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestScheduleTurnMaintenance_DeduplicatesJobs(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	svc, err := NewService(Config{
		Workspace:  dir,
		AgentID:    "dotagent",
		WorkerPoll: 10 * time.Second, // keep jobs queued for assertion
	}, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	defer svc.Close()

	sessionKey := "discord:dedupe"
	userID := "u-dedupe"
	if err := svc.EnsureSession(ctx, sessionKey, "discord", "dedupe", userID); err != nil {
		t.Fatalf("ensure session: %v", err)
	}

	svc.ScheduleTurnMaintenance(ctx, sessionKey, "turn-1", userID)
	svc.ScheduleTurnMaintenance(ctx, sessionKey, "turn-1", userID)
	svc.ScheduleTurnMaintenance(ctx, sessionKey, "turn-2", userID)

	store, ok := svc.store.(*SQLiteStore)
	if !ok {
		t.Fatalf("expected sqlite store, got %T", svc.store)
	}

	var jobs int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_jobs`).Scan(&jobs); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if jobs != 3 {
		t.Fatalf("expected 3 deduplicated jobs (2 consolidate + 1 compact), got %d", jobs)
	}
}
