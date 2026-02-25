package memory

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestService_FileMemorySync_ReconcilesWorkspaceMarkdown(t *testing.T) {
	ctx := context.Background()
	workspace := t.TempDir()
	memoryDir := filepath.Join(workspace, "memory")
	if err := os.MkdirAll(memoryDir, 0o755); err != nil {
		t.Fatalf("mkdir memory dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(memoryDir, "MEMORY.md"), []byte(`# User Memory
- Prefers Go for backend services
- Uses Neovim daily
`), 0o600); err != nil {
		t.Fatalf("write memory file: %v", err)
	}

	svc, err := NewService(Config{
		Workspace:              workspace,
		AgentID:                "dotagent",
		WorkerPoll:             10 * time.Second,
		FileMemoryEnabled:      true,
		FileMemoryDir:          memoryDir,
		FileMemoryPoll:         10 * time.Millisecond,
		FileMemoryWatchEnabled: false,
		FileMemoryMaxFileBytes: 128 * 1024,
	}, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	defer svc.Close()

	now := time.Now().UnixMilli()
	svc.runFileMemorySyncIfDue(ctx, now)

	store, ok := svc.store.(*SQLiteStore)
	if !ok {
		t.Fatalf("expected sqlite store, got %T", svc.store)
	}
	keys, err := store.ListMemoryKeysByPrefix(ctx, svc.cfg.AgentID, fileMemoryKeyPrefix)
	if err != nil {
		t.Fatalf("list file memory keys: %v", err)
	}
	if len(keys) == 0 {
		t.Fatalf("expected file memory keys after sync")
	}

	candidates, err := svc.store.ListMemoryCandidates(ctx, "", svc.cfg.AgentID, "", 32)
	if err != nil {
		t.Fatalf("list memory candidates: %v", err)
	}
	foundEvergreen := false
	for _, item := range candidates {
		if item.Metadata["source"] != "file_memory" {
			continue
		}
		if item.Evergreen {
			foundEvergreen = true
			break
		}
	}
	if !foundEvergreen {
		t.Fatalf("expected evergreen file memory item")
	}

	// Remove file and confirm stale file-memory keys are reconciled.
	if err := os.Remove(filepath.Join(memoryDir, "MEMORY.md")); err != nil {
		t.Fatalf("remove memory file: %v", err)
	}
	svc.runFileMemorySyncIfDue(ctx, now+int64((2*time.Second)/time.Millisecond))
	keys, err = store.ListMemoryKeysByPrefix(ctx, svc.cfg.AgentID, fileMemoryKeyPrefix)
	if err != nil {
		t.Fatalf("list file memory keys after delete: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("expected stale file-memory keys to be deleted, got %d", len(keys))
	}
}

func TestService_FileMemorySync_DeltaSkipsUnchangedFiles(t *testing.T) {
	ctx := context.Background()
	workspace := t.TempDir()
	memoryDir := filepath.Join(workspace, "memory")
	if err := os.MkdirAll(memoryDir, 0o755); err != nil {
		t.Fatalf("mkdir memory dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(memoryDir, "notes.md"), []byte(`# Notes
- Prefers compact API responses
- Uses tmux for terminal workflows
`), 0o600); err != nil {
		t.Fatalf("write memory file: %v", err)
	}

	svc, err := NewService(Config{
		Workspace:              workspace,
		AgentID:                "dotagent",
		WorkerPoll:             10 * time.Second,
		FileMemoryEnabled:      true,
		FileMemoryDir:          memoryDir,
		FileMemoryPoll:         10 * time.Millisecond,
		FileMemoryWatchEnabled: false,
		FileMemoryMaxFileBytes: 128 * 1024,
	}, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	defer svc.Close()

	store, ok := svc.store.(*SQLiteStore)
	if !ok {
		t.Fatalf("expected sqlite store, got %T", svc.store)
	}
	countObs := func() int {
		var n int
		if err := store.db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM memory_observations mo
JOIN memory_items mi ON mi.id = mo.item_id
WHERE mi.item_key LIKE ?`, fileMemoryKeyPrefix+"%").Scan(&n); err != nil {
			t.Fatalf("count observations: %v", err)
		}
		return n
	}

	now := time.Now().UnixMilli()
	svc.runFileMemorySyncIfDue(ctx, now)
	firstObs := countObs()
	if firstObs == 0 {
		t.Fatalf("expected file-memory observations after first sync")
	}

	// Run a second sync without changing files. Delta sync should not write
	// duplicate upserts/observations for unchanged markdown content.
	svc.runFileMemorySyncIfDue(ctx, now+int64((2*time.Second)/time.Millisecond))
	secondObs := countObs()
	if secondObs != firstObs {
		t.Fatalf("expected unchanged sync to avoid duplicate upserts (first=%d second=%d)", firstObs, secondObs)
	}
}

func TestService_FileMemorySync_WatchDebounce(t *testing.T) {
	ctx := context.Background()
	workspace := t.TempDir()
	memoryDir := filepath.Join(workspace, "memory")
	if err := os.MkdirAll(memoryDir, 0o755); err != nil {
		t.Fatalf("mkdir memory dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(memoryDir, "stream.md"), []byte(`# Watch Debounce
- Remember this preference for debounce testing
`), 0o600); err != nil {
		t.Fatalf("write memory file: %v", err)
	}

	svc, err := NewService(Config{
		Workspace:               workspace,
		AgentID:                 "dotagent",
		WorkerPoll:              10 * time.Second,
		FileMemoryEnabled:       true,
		FileMemoryDir:           memoryDir,
		FileMemoryPoll:          5 * time.Minute,
		FileMemoryWatchEnabled:  true,
		FileMemoryWatchDebounce: 2 * time.Second,
		FileMemoryMaxFileBytes:  128 * 1024,
	}, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	defer svc.Close()

	store, ok := svc.store.(*SQLiteStore)
	if !ok {
		t.Fatalf("expected sqlite store, got %T", svc.store)
	}

	now := time.Now().UnixMilli()
	svc.markFileMemoryDirty(now)
	svc.runFileMemorySyncIfDue(ctx, now+500)
	keys, err := store.ListMemoryKeysByPrefix(ctx, svc.cfg.AgentID, fileMemoryKeyPrefix)
	if err != nil {
		t.Fatalf("list keys before debounce: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("expected sync to wait for debounce, got %d keys", len(keys))
	}

	svc.runFileMemorySyncIfDue(ctx, now+2500)
	keys, err = store.ListMemoryKeysByPrefix(ctx, svc.cfg.AgentID, fileMemoryKeyPrefix)
	if err != nil {
		t.Fatalf("list keys after debounce: %v", err)
	}
	if len(keys) == 0 {
		t.Fatalf("expected sync after debounce")
	}
}
