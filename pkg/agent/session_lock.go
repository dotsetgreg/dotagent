package agent

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dotsetgreg/dotagent/pkg/logger"
)

type sessionLockOptions struct {
	WorkspaceRoot    string
	FileLockEnabled  bool
	LockTimeout      time.Duration
	StaleAfter       time.Duration
	MaxHoldDuration  time.Duration
	RetryPoll        time.Duration
	WatchdogInterval time.Duration
}

type sessionLockManager struct {
	mu    sync.Mutex
	locks map[string]*sessionLockRef

	fileMu   sync.Mutex
	opts     sessionLockOptions
	stopOnce sync.Once
	stopCh   chan struct{}
	heldFile map[string]heldSessionFileLock
}

type heldSessionFileLock struct {
	Path       string
	AcquiredAt time.Time
	Warned     bool
}

type sessionLockRef struct {
	mu   sync.Mutex
	refs int
}

type sessionLockFilePayload struct {
	PID       int    `json:"pid"`
	CreatedAt string `json:"created_at"`
}

func newSessionLockManager(opts sessionLockOptions) *sessionLockManager {
	if opts.LockTimeout <= 0 {
		opts.LockTimeout = 15 * time.Second
	}
	if opts.StaleAfter <= 0 {
		opts.StaleAfter = 30 * time.Minute
	}
	if opts.MaxHoldDuration <= 0 {
		opts.MaxHoldDuration = 7 * time.Minute
	}
	if opts.RetryPoll <= 0 {
		opts.RetryPoll = 75 * time.Millisecond
	}
	if opts.WatchdogInterval <= 0 {
		opts.WatchdogInterval = time.Minute
	}

	m := &sessionLockManager{
		locks:    map[string]*sessionLockRef{},
		opts:     opts,
		stopCh:   make(chan struct{}),
		heldFile: map[string]heldSessionFileLock{},
	}
	if opts.FileLockEnabled {
		go m.runWatchdog()
	}
	return m
}

func (m *sessionLockManager) Close() {
	if m == nil {
		return
	}
	m.stopOnce.Do(func() {
		close(m.stopCh)
	})

	m.fileMu.Lock()
	paths := make([]string, 0, len(m.heldFile))
	for _, held := range m.heldFile {
		paths = append(paths, held.Path)
	}
	m.heldFile = map[string]heldSessionFileLock{}
	m.fileMu.Unlock()

	for _, path := range paths {
		_ = os.Remove(path)
	}
}

func (m *sessionLockManager) Acquire(ctx context.Context, sessionKey string) (func(), error) {
	if m == nil {
		return func() {}, nil
	}
	if strings.TrimSpace(sessionKey) == "" {
		sessionKey = "__default__"
	}

	m.mu.Lock()
	ref := m.locks[sessionKey]
	if ref == nil {
		ref = &sessionLockRef{}
		m.locks[sessionKey] = ref
	}
	ref.refs++
	m.mu.Unlock()

	ref.mu.Lock()
	releaseFile, err := m.acquireFileLock(ctx, sessionKey)
	if err != nil {
		ref.mu.Unlock()
		m.releaseRef(sessionKey, ref)
		return nil, err
	}
	return func() {
		if releaseFile != nil {
			releaseFile()
		}
		ref.mu.Unlock()
		m.releaseRef(sessionKey, ref)
	}, nil
}

func (m *sessionLockManager) releaseRef(sessionKey string, ref *sessionLockRef) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ref.refs--
	if ref.refs <= 0 {
		delete(m.locks, sessionKey)
	}
}

func (m *sessionLockManager) acquireFileLock(ctx context.Context, sessionKey string) (func(), error) {
	if m == nil || !m.opts.FileLockEnabled {
		return func() {}, nil
	}
	lockPath, err := m.sessionLockPath(sessionKey)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, fmt.Errorf("create session lock dir: %w", err)
	}

	deadline := time.Now().Add(m.opts.LockTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}

	attempt := 0
	for {
		attempt++
		release, acquired, acqErr := m.tryAcquireFileLock(lockPath)
		if acqErr != nil {
			return nil, acqErr
		}
		if acquired {
			m.fileMu.Lock()
			m.heldFile[lockPath] = heldSessionFileLock{
				Path:       lockPath,
				AcquiredAt: time.Now(),
			}
			m.fileMu.Unlock()
			return func() {
				m.fileMu.Lock()
				delete(m.heldFile, lockPath)
				m.fileMu.Unlock()
				release()
			}, nil
		}

		if time.Now().After(deadline) {
			return nil, fmt.Errorf("session lock timeout after %s (%s)", m.opts.LockTimeout, lockPath)
		}
		sleep := time.Duration(attempt) * m.opts.RetryPoll
		if sleep > time.Second {
			sleep = time.Second
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(sleep):
		}
	}
}

func (m *sessionLockManager) tryAcquireFileLock(lockPath string) (release func(), acquired bool, err error) {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err == nil {
		payload := sessionLockFilePayload{
			PID:       os.Getpid(),
			CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		}
		raw, _ := json.Marshal(payload)
		if _, writeErr := f.Write(raw); writeErr != nil {
			_ = f.Close()
			_ = os.Remove(lockPath)
			return nil, false, writeErr
		}
		if closeErr := f.Close(); closeErr != nil {
			_ = os.Remove(lockPath)
			return nil, false, closeErr
		}
		return func() {
			_ = os.Remove(lockPath)
		}, true, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return nil, false, err
	}

	stale, staleErr := m.isLockFileStale(lockPath)
	if staleErr != nil {
		return nil, false, staleErr
	}
	if stale {
		_ = os.Remove(lockPath)
	}
	return nil, false, nil
}

func (m *sessionLockManager) isLockFileStale(lockPath string) (bool, error) {
	info, err := os.Stat(lockPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	now := time.Now()
	fileAge := now.Sub(info.ModTime())

	raw, err := os.ReadFile(lockPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}

	var payload sessionLockFilePayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		// malformed lock files are reclaimed only after stale timeout.
		return fileAge > m.opts.StaleAfter, nil
	}
	if payload.PID <= 0 {
		return fileAge > m.opts.StaleAfter, nil
	}
	if !processIsAlive(payload.PID) {
		return true, nil
	}

	createdAt := strings.TrimSpace(payload.CreatedAt)
	if createdAt != "" {
		if _, parseErr := time.Parse(time.RFC3339Nano, createdAt); parseErr == nil {
			return false, nil
		}
	}
	return fileAge > m.opts.StaleAfter, nil
}

func (m *sessionLockManager) runWatchdog() {
	ticker := time.NewTicker(m.opts.WatchdogInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			m.pruneStaleHeldLocks()
		case <-m.stopCh:
			return
		}
	}
}

func (m *sessionLockManager) pruneStaleHeldLocks() {
	m.fileMu.Lock()
	defer m.fileMu.Unlock()
	if len(m.heldFile) == 0 {
		return
	}
	now := time.Now()
	for key, held := range m.heldFile {
		if now.Sub(held.AcquiredAt) <= m.opts.MaxHoldDuration {
			continue
		}
		if held.Warned {
			continue
		}
		logger.WarnCF("agent", "Session lock exceeded max hold duration; leaving lock intact to preserve exclusivity", map[string]interface{}{
			"lock_path":      held.Path,
			"held_seconds":   now.Sub(held.AcquiredAt).Seconds(),
			"max_hold_secs":  m.opts.MaxHoldDuration.Seconds(),
			"workspace_root": m.opts.WorkspaceRoot,
		})
		held.Warned = true
		m.heldFile[key] = held
	}
}

func (m *sessionLockManager) sessionLockPath(sessionKey string) (string, error) {
	root := strings.TrimSpace(m.opts.WorkspaceRoot)
	if root == "" {
		return "", fmt.Errorf("workspace root is required for session file lock")
	}
	sum := sha1.Sum([]byte(strings.TrimSpace(sessionKey)))
	name := "session-" + hex.EncodeToString(sum[:])[:20] + ".lock"
	return filepath.Join(root, "state", "session_locks", name), nil
}

func processIsAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		// os.FindProcess succeeds even for dead pids on windows and signal(0) is unsupported.
		return true
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}
