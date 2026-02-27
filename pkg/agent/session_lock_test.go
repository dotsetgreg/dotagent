package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSessionLockManager_FileLockLifecycle(t *testing.T) {
	root := t.TempDir()
	mgr := newSessionLockManager(sessionLockOptions{
		WorkspaceRoot:   root,
		FileLockEnabled: true,
		LockTimeout:     500 * time.Millisecond,
		StaleAfter:      2 * time.Minute,
		MaxHoldDuration: time.Minute,
	})
	defer mgr.Close()

	unlock, err := mgr.Acquire(context.Background(), "discord:chat-1:user-1")
	if err != nil {
		t.Fatalf("acquire lock: %v", err)
	}
	lockPath, pathErr := mgr.sessionLockPath("discord:chat-1:user-1")
	if pathErr != nil {
		t.Fatalf("session lock path: %v", pathErr)
	}
	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Fatalf("expected lock file to exist: %v", statErr)
	}

	unlock()
	if _, statErr := os.Stat(lockPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected lock file to be removed, stat err=%v", statErr)
	}
}

func TestSessionLockManager_ReclaimsStaleLock(t *testing.T) {
	root := t.TempDir()
	mgr := newSessionLockManager(sessionLockOptions{
		WorkspaceRoot:   root,
		FileLockEnabled: true,
		LockTimeout:     800 * time.Millisecond,
		StaleAfter:      10 * time.Millisecond,
		MaxHoldDuration: time.Minute,
	})
	defer mgr.Close()

	lockPath, err := mgr.sessionLockPath("discord:chat-stale:user-1")
	if err != nil {
		t.Fatalf("session lock path: %v", err)
	}
	if mkErr := os.MkdirAll(filepath.Dir(lockPath), 0o755); mkErr != nil {
		t.Fatalf("mkdir: %v", mkErr)
	}
	stalePayload := `{"pid":999999,"created_at":"2000-01-01T00:00:00Z"}`
	if writeErr := os.WriteFile(lockPath, []byte(stalePayload), 0o600); writeErr != nil {
		t.Fatalf("write stale lock: %v", writeErr)
	}

	unlock, lockErr := mgr.Acquire(context.Background(), "discord:chat-stale:user-1")
	if lockErr != nil {
		t.Fatalf("expected stale lock reclaim to succeed, got %v", lockErr)
	}
	unlock()
}

func TestSessionLockManager_TimesOutWhenContended(t *testing.T) {
	root := t.TempDir()
	mgrA := newSessionLockManager(sessionLockOptions{
		WorkspaceRoot:   root,
		FileLockEnabled: true,
		LockTimeout:     2 * time.Second,
		StaleAfter:      10 * time.Minute,
		MaxHoldDuration: time.Minute,
	})
	defer mgrA.Close()
	mgrB := newSessionLockManager(sessionLockOptions{
		WorkspaceRoot:   root,
		FileLockEnabled: true,
		LockTimeout:     150 * time.Millisecond,
		StaleAfter:      10 * time.Minute,
		MaxHoldDuration: time.Minute,
	})
	defer mgrB.Close()

	unlockA, err := mgrA.Acquire(context.Background(), "discord:chat-timeout:user-1")
	if err != nil {
		t.Fatalf("acquire A: %v", err)
	}
	defer unlockA()

	_, err = mgrB.Acquire(context.Background(), "discord:chat-timeout:user-1")
	if err == nil {
		t.Fatalf("expected timeout error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "timeout") {
		t.Fatalf("expected timeout in error, got %v", err)
	}
}

func TestSessionLockManager_DoesNotStealLiveLockByFileAgeOnly(t *testing.T) {
	root := t.TempDir()
	mgr := newSessionLockManager(sessionLockOptions{
		WorkspaceRoot:   root,
		FileLockEnabled: true,
		LockTimeout:     500 * time.Millisecond,
		StaleAfter:      50 * time.Millisecond,
		MaxHoldDuration: time.Minute,
	})
	defer mgr.Close()

	lockPath, err := mgr.sessionLockPath("discord:chat-live:user-1")
	if err != nil {
		t.Fatalf("session lock path: %v", err)
	}
	if mkErr := os.MkdirAll(filepath.Dir(lockPath), 0o755); mkErr != nil {
		t.Fatalf("mkdir: %v", mkErr)
	}
	payload := fmt.Sprintf(`{"pid":%d,"created_at":"%s"}`, os.Getpid(), time.Now().UTC().Add(-2*time.Second).Format(time.RFC3339Nano))
	if writeErr := os.WriteFile(lockPath, []byte(payload), 0o600); writeErr != nil {
		t.Fatalf("write lock file: %v", writeErr)
	}
	past := time.Now().Add(-2 * time.Second)
	if chtimesErr := os.Chtimes(lockPath, past, past); chtimesErr != nil {
		t.Fatalf("chtimes: %v", chtimesErr)
	}

	stale, staleErr := mgr.isLockFileStale(lockPath)
	if staleErr != nil {
		t.Fatalf("isLockFileStale: %v", staleErr)
	}
	if stale {
		t.Fatalf("expected live lock with active pid to remain valid")
	}
}

func TestSessionLockManager_WatchdogDoesNotDeleteHeldLock(t *testing.T) {
	root := t.TempDir()
	mgr := newSessionLockManager(sessionLockOptions{
		WorkspaceRoot:   root,
		FileLockEnabled: true,
		LockTimeout:     500 * time.Millisecond,
		StaleAfter:      time.Minute,
		MaxHoldDuration: 10 * time.Millisecond,
	})
	defer mgr.Close()

	unlock, err := mgr.Acquire(context.Background(), "discord:chat-watchdog:user-1")
	if err != nil {
		t.Fatalf("acquire lock: %v", err)
	}
	defer unlock()

	lockPath, err := mgr.sessionLockPath("discord:chat-watchdog:user-1")
	if err != nil {
		t.Fatalf("session lock path: %v", err)
	}

	mgr.fileMu.Lock()
	held := mgr.heldFile[lockPath]
	held.AcquiredAt = time.Now().Add(-2 * time.Second)
	mgr.heldFile[lockPath] = held
	mgr.fileMu.Unlock()

	mgr.pruneStaleHeldLocks()

	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Fatalf("expected watchdog to preserve held lock file, stat err=%v", statErr)
	}
	mgr.fileMu.Lock()
	_, ok := mgr.heldFile[lockPath]
	mgr.fileMu.Unlock()
	if !ok {
		t.Fatalf("expected held lock tracking to remain after watchdog warning")
	}
}
