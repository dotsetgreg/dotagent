package memory

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func BenchmarkAppendEvent(b *testing.B) {
	ctx := context.Background()
	ws := b.TempDir()
	store, err := NewSQLiteStore(ws + "/state/memory.db")
	if err != nil {
		b.Fatalf("new store: %v", err)
	}
	defer store.Close()

	session := "discord:bench"
	_ = store.EnsureSession(ctx, session, "discord", "bench", "user")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := store.AppendEvent(ctx, Event{SessionKey: session, TurnID: "turn-bench", Seq: i + 1, Role: "user", Content: "benchmark append event content"}); err != nil {
			b.Fatalf("append: %v", err)
		}
	}
}

func BenchmarkRecallLatency(b *testing.B) {
	ctx := context.Background()
	ws := b.TempDir()
	svc, err := NewService(Config{Workspace: ws, AgentID: "dotagent", MaxContextTokens: 4096}, nil)
	if err != nil {
		b.Fatalf("new service: %v", err)
	}
	defer svc.Close()

	session := "discord:bench-recall"
	user := "bench-user"
	_ = svc.EnsureSession(ctx, session, "discord", "bench-recall", user)

	for i := 0; i < 500; i++ {
		turnID := fmt.Sprintf("turn-%d", i)
		_ = svc.AppendEvent(ctx, Event{SessionKey: session, TurnID: turnID, Seq: 1, Role: "user", Content: fmt.Sprintf("I like topic-%d and project-%d", i, i%10)})
		_ = svc.AppendEvent(ctx, Event{SessionKey: session, TurnID: turnID, Seq: 2, Role: "assistant", Content: "Noted"})
		svc.ScheduleTurnMaintenance(ctx, session, turnID, user)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := svc.BuildPromptContext(ctx, session, user, "what topic 123 do I like", 4096); err != nil {
			b.Fatalf("recall: %v", err)
		}
	}
}

func BenchmarkCompactionChurnPromptContext(b *testing.B) {
	ctx := context.Background()
	ws := b.TempDir()
	svc, err := NewService(Config{
		Workspace:        ws,
		AgentID:          "dotagent",
		ContextModel:     "openai/gpt-5.2",
		MaxContextTokens: 2048,
		WorkerPoll:       30 * time.Millisecond,
	}, nil)
	if err != nil {
		b.Fatalf("new service: %v", err)
	}
	defer svc.Close()

	session := "discord:bench-churn"
	user := "bench-user-churn"
	_ = svc.EnsureSession(ctx, session, "discord", "bench-churn", user)

	for i := 0; i < 180; i++ {
		turn := fmt.Sprintf("turn-churn-%d", i)
		_, _, _ = svc.RecordUserTurn(ctx, Event{
			SessionKey: session,
			TurnID:     turn,
			Seq:        1,
			Role:       "user",
			Content:    fmt.Sprintf("I prefer profile item %d and need to finish task %d", i%12, i),
		}, user)
		_ = svc.AppendEvent(ctx, Event{SessionKey: session, TurnID: turn, Seq: 2, Role: "assistant", Content: "Noted"})
		svc.ScheduleTurnMaintenance(ctx, session, turn, user)
		if i > 0 && i%30 == 0 {
			_ = svc.ForceCompact(ctx, session, user, 2048)
		}
	}
	time.Sleep(800 * time.Millisecond)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := svc.BuildPromptContext(ctx, session, user, "what profile item do i prefer", 2048); err != nil {
			b.Fatalf("build prompt context: %v", err)
		}
	}
}

func BenchmarkEmbeddingDeltaSync(b *testing.B) {
	ctx := context.Background()
	ws := b.TempDir()
	svc, err := NewService(Config{
		Workspace:  ws,
		AgentID:    "dotagent",
		WorkerPoll: 10 * time.Second,
	}, nil)
	if err != nil {
		b.Fatalf("new service: %v", err)
	}
	defer svc.Close()

	session := "discord:bench-embed-sync"
	user := "bench-user-sync"
	_ = svc.EnsureSession(ctx, session, "discord", "bench-embed-sync", user)

	store, ok := svc.store.(*SQLiteStore)
	if !ok {
		b.Fatalf("expected sqlite store, got %T", svc.store)
	}

	for i := 0; i < 300; i++ {
		_, upsertErr := store.UpsertMemoryItem(ctx, MemoryItem{
			UserID:        user,
			AgentID:       "dotagent",
			ScopeType:     MemoryScopeSession,
			ScopeID:       session,
			SessionKey:    session,
			Kind:          MemorySemanticFact,
			Key:           fmt.Sprintf("fact/%d", i),
			Content:       fmt.Sprintf("embedding delta benchmark content %d", i),
			Confidence:    0.9,
			Weight:        1.0,
			SourceEventID: fmt.Sprintf("evt-%d", i),
			FirstSeenAtMS: time.Now().UnixMilli(),
			LastSeenAtMS:  time.Now().UnixMilli(),
		})
		if upsertErr != nil {
			b.Fatalf("upsert memory item: %v", upsertErr)
		}
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM memory_embeddings`); err != nil {
		b.Fatalf("clear embeddings: %v", err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := svc.syncSessionEmbeddingDeltas(ctx, session); err != nil {
			b.Fatalf("sync session embedding deltas: %v", err)
		}
	}
}
