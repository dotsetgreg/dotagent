package memory

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHybridRetriever_RespectsScopeOptions(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := NewSQLiteStore(filepath.Join(dir, "state", "memory.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer store.Close()

	userID := "u-scope"
	agentID := "dotagent"
	sessionKey := "discord:scope"

	_, err = store.UpsertMemoryItem(ctx, MemoryItem{
		UserID:       userID,
		AgentID:      agentID,
		SessionKey:   "",
		Kind:         MemoryUserPreference,
		Key:          "pref/global",
		Content:      "I prefer global tea",
		Confidence:   0.9,
		Weight:       1,
		LastSeenAtMS: time.Now().UnixMilli(),
	})
	if err != nil {
		t.Fatalf("upsert global memory: %v", err)
	}

	_, err = store.UpsertMemoryItem(ctx, MemoryItem{
		UserID:       userID,
		AgentID:      agentID,
		SessionKey:   sessionKey,
		Kind:         MemoryUserPreference,
		Key:          "pref/session",
		Content:      "I prefer session coffee",
		Confidence:   0.9,
		Weight:       1,
		LastSeenAtMS: time.Now().UnixMilli(),
	})
	if err != nil {
		t.Fatalf("upsert session memory: %v", err)
	}

	r := NewHybridRetriever(store, nil)

	sessionOnly, err := r.Recall(ctx, "prefer", RetrievalOptions{
		SessionKey:      sessionKey,
		UserID:          userID,
		AgentID:         agentID,
		MaxCards:        10,
		CandidateLimit:  20,
		MinScore:        0.0,
		IncludeSession:  true,
		IncludeGlobal:   false,
		RecencyHalfLife: 24 * time.Hour,
		NowMS:           time.Now().UnixMilli(),
	})
	if err != nil {
		t.Fatalf("recall session-only: %v", err)
	}
	if len(sessionOnly) == 0 {
		t.Fatalf("expected session-only recall cards")
	}
	for _, c := range sessionOnly {
		if strings.Contains(strings.ToLower(c.Content), "global tea") {
			t.Fatalf("session-only recall unexpectedly included global memory: %#v", c)
		}
	}

	globalOnly, err := r.Recall(ctx, "prefer", RetrievalOptions{
		SessionKey:      sessionKey,
		UserID:          userID,
		AgentID:         agentID,
		MaxCards:        10,
		CandidateLimit:  20,
		MinScore:        0.0,
		IncludeSession:  false,
		IncludeGlobal:   true,
		RecencyHalfLife: 24 * time.Hour,
		NowMS:           time.Now().UnixMilli() + 1,
	})
	if err != nil {
		t.Fatalf("recall global-only: %v", err)
	}
	if len(globalOnly) == 0 {
		t.Fatalf("expected global-only recall cards")
	}
	for _, c := range globalOnly {
		if strings.Contains(strings.ToLower(c.Content), "session coffee") {
			t.Fatalf("global-only recall unexpectedly included session memory: %#v", c)
		}
	}
}
