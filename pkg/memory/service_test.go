package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	if jobs != 5 {
		t.Fatalf("expected 5 deduplicated jobs (2 consolidate + 2 persona_apply + 1 compact), got %d", jobs)
	}
}

type failingRetriever struct{}

func (f failingRetriever) Recall(ctx context.Context, query string, opts RetrievalOptions) ([]MemoryCard, error) {
	return nil, fmt.Errorf("forced recall failure")
}

func TestBuildPromptContext_DegradesGracefullyOnRecallFailure(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	svc, err := NewService(Config{
		Workspace:  dir,
		AgentID:    "dotagent",
		WorkerPoll: 10 * time.Second,
	}, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	defer svc.Close()

	sessionKey := "discord:degrade"
	userID := "u-degrade"
	if err := svc.EnsureSession(ctx, sessionKey, "discord", "degrade", userID); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	if err := svc.AppendEvent(ctx, Event{SessionKey: sessionKey, TurnID: "t1", Seq: 1, Role: "user", Content: "I prefer pour over coffee."}); err != nil {
		t.Fatalf("append user event: %v", err)
	}
	if err := svc.AppendEvent(ctx, Event{SessionKey: sessionKey, TurnID: "t1", Seq: 2, Role: "assistant", Content: "Noted."}); err != nil {
		t.Fatalf("append assistant event: %v", err)
	}

	// Force recall path failure; context building should still succeed using history.
	svc.retriever = failingRetriever{}
	pc, err := svc.BuildPromptContext(ctx, sessionKey, userID, "what coffee do I prefer?", 2048)
	if err != nil {
		t.Fatalf("build prompt context should not fail on recall error: %v", err)
	}
	if len(pc.History) == 0 {
		t.Fatalf("expected history to remain available under recall failure")
	}
}

func TestBuildPromptContext_FailClosedWhenContinuityUnavailable(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	svc, err := NewService(Config{
		Workspace:  dir,
		AgentID:    "dotagent",
		WorkerPoll: 10 * time.Second,
	}, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	sessionKey := "discord:fail-closed"
	userID := "u-fail-closed"
	if err := svc.EnsureSession(ctx, sessionKey, "discord", "fail-closed", userID); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	if err := svc.AppendEvent(ctx, Event{SessionKey: sessionKey, TurnID: "t1", Seq: 1, Role: "user", Content: "I like Ethiopian coffee."}); err != nil {
		t.Fatalf("append user event: %v", err)
	}
	if err := svc.AppendEvent(ctx, Event{SessionKey: sessionKey, TurnID: "t1", Seq: 2, Role: "assistant", Content: "Noted."}); err != nil {
		t.Fatalf("append assistant event: %v", err)
	}
	if _, err := svc.store.ArchiveEventsBefore(ctx, sessionKey, 0); err != nil {
		t.Fatalf("archive events: %v", err)
	}
	if err := svc.store.SetSessionSummary(ctx, sessionKey, ""); err != nil {
		t.Fatalf("clear summary: %v", err)
	}
	if err := svc.Close(); err != nil {
		t.Fatalf("close initial service: %v", err)
	}

	// Re-open service so volatile snapshots are empty and continuity depends on durable artifacts.
	svc2, err := NewService(Config{
		Workspace:  dir,
		AgentID:    "dotagent",
		WorkerPoll: 10 * time.Second,
	}, nil)
	if err != nil {
		t.Fatalf("reopen service: %v", err)
	}
	defer svc2.Close()

	_, err = svc2.BuildPromptContext(ctx, sessionKey, userID, "you already know this, what coffee did I say?", 2048)
	if !errors.Is(err, ErrContinuityUnavailable) {
		t.Fatalf("expected ErrContinuityUnavailable, got %v", err)
	}
}

func TestRecordUserTurn_PersistsUserScopedMemoryAcrossSessions(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	svc, err := NewService(Config{
		Workspace:  dir,
		AgentID:    "dotagent",
		WorkerPoll: 10 * time.Second,
	}, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	defer svc.Close()

	userID := "u-shared"
	s1 := "discord:one"
	s2 := "discord:two"
	if err := svc.EnsureSession(ctx, s1, "discord", "one", userID); err != nil {
		t.Fatalf("ensure session one: %v", err)
	}
	if err := svc.EnsureSession(ctx, s2, "discord", "two", userID); err != nil {
		t.Fatalf("ensure session two: %v", err)
	}

	_, inserted, err := svc.RecordUserTurn(ctx, Event{
		SessionKey: s1,
		TurnID:     "turn-1",
		Seq:        1,
		Role:       "user",
		Content:    "I really prefer pour-over coffee.",
	}, userID)
	if err != nil {
		t.Fatalf("record user turn: %v", err)
	}
	if inserted == 0 {
		t.Fatalf("expected immediate memory capture inserts")
	}

	pc, err := svc.BuildPromptContext(ctx, s2, userID, "what coffee do I prefer?", 2048)
	if err != nil {
		t.Fatalf("build prompt context on second session: %v", err)
	}
	found := false
	for _, card := range pc.RecallCards {
		if card.Kind == MemoryUserPreference && strings.Contains(strings.ToLower(card.Content), "pour-over") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected user-scoped preference recall across sessions")
	}
}

func TestSQLiteStore_SessionProviderStateRoundTrip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := NewSQLiteStore(filepath.Join(dir, "state", "memory.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer store.Close()

	sessionKey := "discord:provider-state"
	if err := store.EnsureSession(ctx, sessionKey, "discord", "provider-state", "u1"); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	if err := store.SetSessionProviderState(ctx, sessionKey, "openrouter", "state-123"); err != nil {
		t.Fatalf("set provider state: %v", err)
	}
	got, err := store.GetSessionProviderState(ctx, sessionKey, "openrouter")
	if err != nil {
		t.Fatalf("get provider state: %v", err)
	}
	if got != "state-123" {
		t.Fatalf("expected provider state state-123, got %q", got)
	}

	if err := store.SetSessionProviderState(ctx, sessionKey, "openai", "state-999"); err != nil {
		t.Fatalf("set openai provider state: %v", err)
	}
	openAIState, err := store.GetSessionProviderState(ctx, sessionKey, "openai")
	if err != nil {
		t.Fatalf("get openai provider state: %v", err)
	}
	if openAIState != "state-999" {
		t.Fatalf("expected openai state state-999, got %q", openAIState)
	}

	openRouterState, err := store.GetSessionProviderState(ctx, sessionKey, "openrouter")
	if err != nil {
		t.Fatalf("get openrouter provider state: %v", err)
	}
	if openRouterState != "state-123" {
		t.Fatalf("expected openrouter state state-123 after openai write, got %q", openRouterState)
	}
}

func TestSQLiteStore_MigratesLegacySessionProviderState(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state", "memory.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("mkdir db dir: %v", err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS session_provider_state (
		session_key TEXT PRIMARY KEY,
		state_id TEXT NOT NULL DEFAULT '',
		updated_at_ms INTEGER NOT NULL
	);`); err != nil {
		t.Fatalf("create legacy table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO session_provider_state(session_key, state_id, updated_at_ms) VALUES(?, ?, ?)`, "discord:legacy", "legacy-state", 123); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw sqlite: %v", err)
	}

	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("open migrated store: %v", err)
	}
	defer store.Close()

	got, err := store.GetSessionProviderState(ctx, "discord:legacy", "openrouter")
	if err != nil {
		t.Fatalf("read migrated provider state: %v", err)
	}
	if got != "legacy-state" {
		t.Fatalf("expected migrated provider state legacy-state, got %q", got)
	}
}

func TestCompactor_WritesStructuredSnapshot(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := NewSQLiteStore(filepath.Join(dir, "state", "memory.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer store.Close()

	sessionKey := "discord:snapshot"
	if err := store.EnsureSession(ctx, sessionKey, "discord", "snapshot", "u1"); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	for i := 0; i < 40; i++ {
		role := "user"
		content := "I really prefer pour-over coffee and need to finish my migration task."
		if i%2 == 1 {
			role = "assistant"
			content = "Acknowledged."
		}
		if err := store.AppendEvent(ctx, Event{
			SessionKey: sessionKey,
			TurnID:     fmt.Sprintf("turn-%d", i/2),
			Seq:        i + 1,
			Role:       role,
			Content:    content,
		}); err != nil {
			t.Fatalf("append event %d: %v", i, err)
		}
	}
	comp := NewSessionCompactor(store, nil)
	if err := comp.CompactSession(ctx, sessionKey, "u1", "dotagent", DeriveContextBudget(1024)); err != nil {
		t.Fatalf("compact session: %v", err)
	}
	snap, err := store.GetLatestSessionSnapshot(ctx, sessionKey)
	if err != nil {
		t.Fatalf("get latest snapshot: %v", err)
	}
	if snap.Revision == 0 {
		t.Fatalf("expected non-zero snapshot revision")
	}
	if len(snap.Preferences) == 0 {
		t.Fatalf("expected snapshot preferences")
	}
}
