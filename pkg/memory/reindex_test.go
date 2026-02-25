package memory

import (
	"context"
	"testing"
	"time"
)

func TestService_ReindexEmbeddingsAtomic_UsesFallbackModel(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	svc, err := NewService(Config{
		Workspace:               dir,
		AgentID:                 "dotagent",
		EmbeddingModel:          defaultEmbeddingModel,
		EmbeddingFallbackModels: []string{"unknown-model", hashEmbeddingModel},
		WorkerPoll:              10 * time.Second,
	}, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	defer svc.Close()

	store, ok := svc.store.(*SQLiteStore)
	if !ok {
		t.Fatalf("expected sqlite store, got %T", svc.store)
	}

	sessionKey := "discord:reindex-fallback"
	userID := "u-reindex-fallback"
	if err := svc.EnsureSession(ctx, sessionKey, "discord", "reindex-fallback", userID); err != nil {
		t.Fatalf("ensure session: %v", err)
	}

	now := time.Now().UnixMilli()
	item, err := store.UpsertMemoryItem(ctx, MemoryItem{
		UserID:        userID,
		AgentID:       "dotagent",
		ScopeType:     MemoryScopeSession,
		ScopeID:       sessionKey,
		SessionKey:    sessionKey,
		Kind:          MemorySemanticFact,
		Key:           "fact/reindex-fallback",
		Content:       "Fallback embeddings should still index this memory.",
		Confidence:    0.92,
		Weight:        1.0,
		SourceEventID: "evt-reindex-fallback",
		FirstSeenAtMS: now,
		LastSeenAtMS:  now,
	})
	if err != nil {
		t.Fatalf("upsert memory item: %v", err)
	}

	report, err := svc.ReindexEmbeddingsAtomic(ctx)
	if err != nil {
		t.Fatalf("reindex embeddings: %v", err)
	}
	if report.IndexedItems == 0 {
		t.Fatalf("expected indexed items > 0")
	}
	if report.FallbackHits == 0 {
		t.Fatalf("expected fallback hits > 0 when first fallback model is invalid")
	}

	var model string
	if err := store.db.QueryRowContext(ctx, `SELECT model FROM memory_embeddings WHERE item_id = ?`, item.ID).Scan(&model); err != nil {
		t.Fatalf("read embedding model: %v", err)
	}
	if model != hashEmbeddingModel {
		t.Fatalf("expected fallback model %q, got %q", hashEmbeddingModel, model)
	}
}

func TestService_ReindexEmbeddingsAtomic_RollsBackOnFailure(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	svc, err := NewService(Config{
		Workspace:               dir,
		AgentID:                 "dotagent",
		EmbeddingModel:          defaultEmbeddingModel,
		EmbeddingFallbackModels: []string{"unknown-model"},
		WorkerPoll:              10 * time.Second,
	}, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	defer svc.Close()

	store, ok := svc.store.(*SQLiteStore)
	if !ok {
		t.Fatalf("expected sqlite store, got %T", svc.store)
	}

	sessionKey := "discord:reindex-rollback"
	userID := "u-reindex-rollback"
	if err := svc.EnsureSession(ctx, sessionKey, "discord", "reindex-rollback", userID); err != nil {
		t.Fatalf("ensure session: %v", err)
	}

	now := time.Now().UnixMilli()
	item, err := store.UpsertMemoryItem(ctx, MemoryItem{
		UserID:        userID,
		AgentID:       "dotagent",
		ScopeType:     MemoryScopeSession,
		ScopeID:       sessionKey,
		SessionKey:    sessionKey,
		Kind:          MemorySemanticFact,
		Key:           "fact/reindex-rollback",
		Content:       "Rollback should keep previous embeddings intact.",
		Confidence:    0.92,
		Weight:        1.0,
		SourceEventID: "evt-reindex-rollback",
		FirstSeenAtMS: now,
		LastSeenAtMS:  now,
	})
	if err != nil {
		t.Fatalf("upsert memory item: %v", err)
	}
	if err := store.UpsertEmbedding(ctx, item.ID, defaultEmbeddingModel, embedText(item.Content)); err != nil {
		t.Fatalf("seed embedding: %v", err)
	}

	if _, err := svc.ReindexEmbeddingsAtomic(ctx); err == nil {
		t.Fatalf("expected reindex failure when all fallback models are invalid")
	}

	emb, err := store.GetEmbeddings(ctx, []string{item.ID})
	if err != nil {
		t.Fatalf("get embeddings after failed reindex: %v", err)
	}
	if len(emb[item.ID]) == 0 {
		t.Fatalf("expected existing embedding to remain after failed reindex rollback")
	}
}
