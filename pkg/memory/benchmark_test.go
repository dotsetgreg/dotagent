package memory

import (
	"context"
	"fmt"
	"testing"
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
