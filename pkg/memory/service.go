package memory

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Config configures the memory subsystem.
type Config struct {
	Workspace                    string
	AgentID                      string
	ContextModel                 string
	EmbeddingModel               string
	EmbeddingFallbackModels      []string
	EmbeddingOpenAIToken         string
	EmbeddingOpenAIAPIBase       string
	EmbeddingOpenRouterKey       string
	EmbeddingOpenRouterBase      string
	EmbeddingOllamaAPIBase       string
	EmbeddingBatchSize           int
	EmbeddingConcurrency         int
	MaxContextTokens             int
	MaxRecallItems               int
	CandidateLimit               int
	RetrievalCache               time.Duration
	WorkerLease                  time.Duration
	WorkerPoll                   time.Duration
	PersonaCardTokens            int
	PersonaExtractor             PersonaExtractionFunc
	PersonaSyncApply             bool
	PersonaFileSync              PersonaFileSyncMode
	PersonaPolicyMode            string
	PersonaMinConfidence         float64
	EventRetention               time.Duration
	AuditRetention               time.Duration
	CompactionSummaryTimeout     time.Duration
	CompactionChunkChars         int
	CompactionMaxTranscriptChars int
	CompactionPartialSkipChars   int
	CompactionHooks              CompactionHooks
	FileMemoryEnabled            bool
	FileMemoryDir                string
	FileMemoryPoll               time.Duration
	FileMemoryWatchEnabled       bool
	FileMemoryWatchDebounce      time.Duration
	FileMemoryMaxFileBytes       int
}

// Service is the orchestrator for memory capture, retrieval and compaction.
type Service struct {
	cfg                     Config
	contextModel            string
	store                   Store
	retriever               Retriever
	consolidator            Consolidator
	compactor               Compactor
	policy                  Policy
	persona                 *PersonaManager
	budgeter                *TokenBudgeter
	embeddingEngine         *EmbeddingEngine
	embeddingFallbackModels []string

	stopCh chan struct{}
	wg     sync.WaitGroup

	closeOnce sync.Once
	closeErr  error

	snapshotMu          sync.RWMutex
	snapshots           map[string][]Event
	snapshotAccess      map[string]int64 // last access timestamp per session
	snapshotLimit       int
	snapshotMaxSessions int

	lastRetentionSweep int64
	lastFileMemorySync int64

	fileMemoryMu      sync.Mutex
	fileMemoryIndex   map[string]fileMemorySnapshot
	fileMemoryPrimed  bool
	fileMemoryDirty   bool
	fileMemoryEventMS int64
	fileMemoryWatchFP string

	compactionMu    sync.Mutex
	compactionState map[string]*compactionFlight
}

type compactionFlight struct {
	done chan struct{}
}

func NewService(cfg Config, summarize SummaryFunc) (*Service, error) {
	if strings.TrimSpace(cfg.Workspace) == "" {
		return nil, fmt.Errorf("memory workspace is required")
	}
	if cfg.AgentID == "" {
		cfg.AgentID = "dotagent"
	}
	if strings.TrimSpace(cfg.ContextModel) == "" {
		cfg.ContextModel = cfg.AgentID
	}
	if cfg.MaxContextTokens <= 0 {
		cfg.MaxContextTokens = 16384
	}
	if cfg.MaxRecallItems <= 0 {
		cfg.MaxRecallItems = 8
	}
	if cfg.CandidateLimit <= 0 {
		cfg.CandidateLimit = 80
	}
	if cfg.RetrievalCache <= 0 {
		cfg.RetrievalCache = 20 * time.Second
	}
	if cfg.WorkerLease <= 0 {
		cfg.WorkerLease = 45 * time.Second
	}
	if cfg.WorkerPoll <= 0 {
		cfg.WorkerPoll = 800 * time.Millisecond
	}
	if cfg.PersonaCardTokens <= 0 {
		cfg.PersonaCardTokens = 480
	}
	if cfg.PersonaFileSync == "" {
		cfg.PersonaFileSync = PersonaFileSyncExportOnly
	}
	if strings.TrimSpace(cfg.PersonaPolicyMode) == "" {
		cfg.PersonaPolicyMode = "balanced"
	}
	if cfg.PersonaMinConfidence <= 0 {
		cfg.PersonaMinConfidence = 0.52
	}
	if cfg.EventRetention <= 0 {
		cfg.EventRetention = 90 * 24 * time.Hour
	}
	if cfg.AuditRetention <= 0 {
		cfg.AuditRetention = 365 * 24 * time.Hour
	}
	if cfg.CompactionSummaryTimeout <= 0 {
		cfg.CompactionSummaryTimeout = 60 * time.Second
	}
	if cfg.CompactionChunkChars <= 0 {
		cfg.CompactionChunkChars = 9000
	}
	if cfg.CompactionMaxTranscriptChars <= 0 {
		cfg.CompactionMaxTranscriptChars = 48000
	}
	if cfg.CompactionPartialSkipChars <= 0 {
		cfg.CompactionPartialSkipChars = 2600
	}
	if strings.TrimSpace(cfg.FileMemoryDir) == "" {
		cfg.FileMemoryDir = filepath.Join(cfg.Workspace, "memory")
	}
	if cfg.FileMemoryPoll <= 0 {
		cfg.FileMemoryPoll = 15 * time.Second
	}
	if cfg.FileMemoryWatchDebounce <= 0 {
		cfg.FileMemoryWatchDebounce = 1200 * time.Millisecond
	}
	if cfg.FileMemoryMaxFileBytes <= 0 {
		cfg.FileMemoryMaxFileBytes = 256 * 1024
	}

	cfg.EmbeddingModel, cfg.EmbeddingFallbackModels = normalizeEmbeddingConfig(cfg)
	if spec, err := parseEmbeddingModelSpec(cfg.EmbeddingModel); err == nil && spec.Provider == embeddingProviderLocal {
		SetEmbedderByName(spec.Model)
	} else {
		SetEmbedderByName(defaultEmbeddingModel)
	}

	dbPath := filepath.Join(cfg.Workspace, "state", "memory.db")
	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		return nil, err
	}
	embeddingEngine := NewEmbeddingEngine(EmbeddingEngineConfig{
		OpenAIToken:       cfg.EmbeddingOpenAIToken,
		OpenAIAPIBase:     cfg.EmbeddingOpenAIAPIBase,
		OpenRouterToken:   cfg.EmbeddingOpenRouterKey,
		OpenRouterAPIBase: cfg.EmbeddingOpenRouterBase,
		OllamaAPIBase:     cfg.EmbeddingOllamaAPIBase,
		BatchSize:         cfg.EmbeddingBatchSize,
		Concurrency:       cfg.EmbeddingConcurrency,
		Cache:             store,
	})
	policy := NewDefaultPolicy()

	personaPolicy := NewPersonaPolicyEngine(PersonaPolicyConfig{
		Mode:          cfg.PersonaPolicyMode,
		MinConfidence: cfg.PersonaMinConfidence,
	})

	svc := &Service{
		cfg:          cfg,
		contextModel: strings.TrimSpace(cfg.ContextModel),
		store:        store,
		policy:       policy,
		retriever: NewHybridRetriever(store, policy, HybridRetrieverOptions{
			EmbeddingEngine:         embeddingEngine,
			EmbeddingFallbackModels: cfg.EmbeddingFallbackModels,
		}),
		consolidator: NewHeuristicConsolidator(store, policy),
		compactor: NewSessionCompactor(store, summarize, CompactorConfig{
			SummaryTimeout:     cfg.CompactionSummaryTimeout,
			ChunkChars:         cfg.CompactionChunkChars,
			MaxTranscriptChars: cfg.CompactionMaxTranscriptChars,
			PartialSkipChars:   cfg.CompactionPartialSkipChars,
			Hooks:              cfg.CompactionHooks,
		}),
		persona:                 NewPersonaManager(store, cfg.Workspace, cfg.PersonaExtractor, cfg.PersonaFileSync, personaPolicy),
		budgeter:                NewTokenBudgeter(cfg.Workspace),
		embeddingEngine:         embeddingEngine,
		embeddingFallbackModels: append([]string(nil), cfg.EmbeddingFallbackModels...),
		stopCh:                  make(chan struct{}),
		snapshots:               map[string][]Event{},
		snapshotAccess:          map[string]int64{},
		snapshotLimit:           128,
		snapshotMaxSessions:     256,
		fileMemoryIndex:         map[string]fileMemorySnapshot{},
		fileMemoryDirty:         true,
		compactionState:         map[string]*compactionFlight{},
	}

	svc.startFileMemoryWatcher()
	svc.wg.Add(1)
	go svc.runWorker()
	return svc, nil
}

func (s *Service) Close() error {
	s.closeOnce.Do(func() {
		close(s.stopCh)
		s.wg.Wait()
		s.closeErr = s.store.Close()
	})
	return s.closeErr
}

func (s *Service) startFileMemoryWatcher() {
	if s == nil || !s.cfg.FileMemoryEnabled || !s.cfg.FileMemoryWatchEnabled {
		return
	}
	root := strings.TrimSpace(s.cfg.FileMemoryDir)
	if root == "" {
		return
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return
	}
	fingerprint, err := fileMemoryWatchFingerprint(root)
	if err != nil {
		fingerprint = ""
	}

	s.fileMemoryMu.Lock()
	s.fileMemoryDirty = true
	s.fileMemoryEventMS = time.Now().UnixMilli()
	s.fileMemoryWatchFP = fingerprint
	s.fileMemoryMu.Unlock()

	s.wg.Add(1)
	go s.runFileMemoryWatchPoller(root, fileMemoryWatchInterval(s.cfg.FileMemoryWatchDebounce))
}

func fileMemoryWatchInterval(debounce time.Duration) time.Duration {
	if debounce <= 0 {
		debounce = 1200 * time.Millisecond
	}
	interval := debounce / 3
	if interval < 250*time.Millisecond {
		interval = 250 * time.Millisecond
	}
	if interval > 2*time.Second {
		interval = 2 * time.Second
	}
	return interval
}

func (s *Service) runFileMemoryWatchPoller(root string, interval time.Duration) {
	defer s.wg.Done()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			fingerprint, err := fileMemoryWatchFingerprint(root)
			if err != nil {
				continue
			}
			s.fileMemoryMu.Lock()
			if fingerprint != s.fileMemoryWatchFP {
				s.fileMemoryWatchFP = fingerprint
				s.fileMemoryDirty = true
				s.fileMemoryEventMS = time.Now().UnixMilli()
			}
			s.fileMemoryMu.Unlock()
		}
	}
}

func fileMemoryWatchFingerprint(root string) (string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return "", nil
	}
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	hasher := sha1.New()
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		if d == nil {
			return nil
		}
		if shouldIgnoreFileMemoryWatchPath(root, path) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(d.Name()))
		if ext != ".md" && ext != ".markdown" {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			rel = d.Name()
		}
		rel = filepath.ToSlash(strings.TrimSpace(rel))
		if rel == "" {
			return nil
		}
		_, _ = hasher.Write([]byte(rel))
		_, _ = hasher.Write([]byte{'\n'})
		_, _ = hasher.Write([]byte(strconv.FormatInt(info.Size(), 10)))
		_, _ = hasher.Write([]byte{'\n'})
		_, _ = hasher.Write([]byte(strconv.FormatInt(info.ModTime().UnixNano(), 10)))
		_, _ = hasher.Write([]byte{'\n'})
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func shouldIgnoreFileMemoryWatchPath(root, path string) bool {
	root = strings.TrimSpace(root)
	path = strings.TrimSpace(path)
	if root == "" || path == "" {
		return false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	rel = filepath.ToSlash(rel)
	if rel == "." {
		return false
	}
	parts := strings.Split(rel, "/")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.HasPrefix(part, ".") {
			return true
		}
	}
	return false
}

func (s *Service) markFileMemoryDirty(ts int64) {
	s.fileMemoryMu.Lock()
	s.fileMemoryDirty = true
	if ts <= 0 {
		ts = time.Now().UnixMilli()
	}
	s.fileMemoryEventMS = ts
	s.fileMemoryMu.Unlock()
}

func (s *Service) EnsureSession(ctx context.Context, sessionKey, channel, chatID, userID string) error {
	return s.store.EnsureSession(ctx, sessionKey, channel, chatID, userID)
}

func (s *Service) GetSession(ctx context.Context, sessionKey string) (Session, error) {
	return s.store.GetSession(ctx, sessionKey)
}

func (s *Service) ListSessions(ctx context.Context, userID string, limit int) ([]Session, error) {
	return s.store.ListSessions(ctx, userID, limit)
}

func (s *Service) ListSessionEvents(ctx context.Context, sessionKey string, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 32
	}
	return s.store.ListRecentEvents(ctx, sessionKey, limit, false)
}

func (s *Service) AppendEvent(ctx context.Context, ev Event) error {
	ev = normalizeEvent(ev)
	s.appendSnapshot(ev)
	if err := s.store.AppendEvent(ctx, ev); err != nil {
		_ = s.store.AddMetric(ctx, "memory.append_event.error", 1, map[string]string{
			"session_key": ev.SessionKey,
			"role":        ev.Role,
		})
		return err
	}
	return nil
}

func (s *Service) BuildPromptContext(ctx context.Context, sessionKey, userID, query string, maxTokens int) (PromptContext, error) {
	if maxTokens <= 0 {
		maxTokens = s.cfg.MaxContextTokens
	}
	safetyFactor := 0.90
	if s.budgeter != nil {
		safetyFactor = s.budgeter.PromptSafetyFactor(s.contextModel)
	}
	budget := ScaleContextBudget(DeriveContextBudget(maxTokens), safetyFactor)
	degradedReasons := []string{}
	continuity := PromptContinuity{}

	session, sessErr := s.store.GetSession(ctx, sessionKey)
	if sessErr == nil && session.MessageCount > 0 {
		continuity.HasPriorTurns = true
	}

	summary, err := s.store.GetSessionSummary(ctx, sessionKey)
	if err != nil {
		degradedReasons = append(degradedReasons, "summary")
		summary = ""
	}
	snapshot, snapErr := s.store.GetLatestSessionSnapshot(ctx, sessionKey)
	if snapErr != nil {
		degradedReasons = append(degradedReasons, "snapshot")
	} else {
		continuity.SnapshotRevision = snapshot.Revision
	}
	if strings.TrimSpace(summary) == "" && strings.TrimSpace(snapshot.Summary) != "" {
		summary = strings.TrimSpace(snapshot.Summary)
	}
	continuity.HasSummary = strings.TrimSpace(summary) != ""

	fetchLimit := budget.ThreadTokens / 120 * 2
	if fetchLimit < 24 {
		fetchLimit = 24
	}
	if fetchLimit > 96 {
		fetchLimit = 96
	}
	events, err := s.store.ListRecentEvents(ctx, sessionKey, fetchLimit, false)
	if err != nil {
		degradedReasons = append(degradedReasons, "history")
		events = s.getSnapshotEvents(sessionKey, fetchLimit)
	} else {
		events = mergeEventStreams(events, s.getSnapshotEvents(sessionKey, fetchLimit), fetchLimit)
	}
	events, droppedOrphans := repairOrphanToolHistory(events)
	if droppedOrphans > 0 {
		_ = s.store.AddMetric(ctx, "memory.history.tool_orphans_repaired", float64(droppedOrphans), map[string]string{
			"session_key": sessionKey,
			"user_id":     userID,
		})
	}

	recallCards := []MemoryCard{}
	recallOut, err := s.retriever.Recall(ctx, query, RetrievalOptions{
		SessionKey:      sessionKey,
		UserID:          userID,
		AgentID:         s.cfg.AgentID,
		MaxCards:        s.cfg.MaxRecallItems,
		CandidateLimit:  s.cfg.CandidateLimit,
		MinScore:        0.32,
		CacheTTL:        s.cfg.RetrievalCache,
		NowMS:           time.Now().UnixMilli(),
		IncludeSession:  true,
		IncludeUser:     true,
		IncludeGlobal:   true,
		RecencyHalfLife: 14 * 24 * time.Hour,
	})
	if err != nil {
		degradedReasons = append(degradedReasons, "recall")
	} else {
		recallCards = recallOut
	}
	continuity.HasRecall = len(recallCards) > 0
	budget = ScaleContextBudget(DeriveAdaptiveContextBudget(maxTokens, BudgetSignals{
		RecentEventCount: len(events),
		Query:            query,
		HasSummary:       continuity.HasSummary,
		HasRecall:        continuity.HasRecall,
	}), safetyFactor)
	history := selectHistoryWithinBudget(events, budget.ThreadTokens, s.estimateMessageTokens)
	continuity.HasHistory = len(history) > 0
	_ = s.store.AddMetric(ctx, "memory.recall.cards", float64(len(recallCards)), map[string]string{
		"session_key": sessionKey,
		"user_id":     userID,
	})
	_ = s.store.AddMetric(ctx, "memory.context.history_messages", float64(len(history)), map[string]string{
		"session_key": sessionKey,
		"user_id":     userID,
	})

	personaPrompt := ""
	if s.persona != nil {
		pp, pErr := s.persona.BuildPrompt(ctx, userID, s.cfg.AgentID, query, s.cfg.PersonaCardTokens)
		if pErr == nil {
			personaPrompt = strings.TrimSpace(pp)
		} else {
			degradedReasons = append(degradedReasons, "persona")
		}
	}

	remainingMemoryBudget := budget.MemoryTokens
	if personaPrompt != "" {
		personaTokens := s.estimateMessageTokens(personaPrompt)
		remainingMemoryBudget = budget.MemoryTokens - personaTokens
		if remainingMemoryBudget < 128 {
			remainingMemoryBudget = 128
		}
	}
	recallPrompt := formatSnapshotAndRecall(snapshot, recallCards, remainingMemoryBudget, s.estimateMessageTokens)
	if personaPrompt != "" {
		if recallPrompt != "" {
			recallPrompt = personaPrompt + "\n\n" + recallPrompt
		} else {
			recallPrompt = personaPrompt
		}
	}
	continuationNotes := deriveContinuationNotes(snapshot)
	continuity.ContinuationNotes = continuationNotes
	if len(continuationNotes) > 0 {
		noteBlock := formatContinuationNotes(continuationNotes, budget.SummaryTokens, s.estimateMessageTokens)
		if noteBlock != "" {
			if recallPrompt != "" {
				recallPrompt += "\n\n" + noteBlock
			} else {
				recallPrompt = noteBlock
			}
		}
	}

	hasContinuityArtifacts := continuity.HasHistory || continuity.HasSummary || continuity.HasRecall
	if continuity.HasPriorTurns && !hasContinuityArtifacts {
		degradedReasons = append(degradedReasons, "no_continuity_artifacts")
	}
	if len(degradedReasons) > 0 {
		_ = s.store.AddMetric(ctx, "memory.context.degraded", float64(len(degradedReasons)), map[string]string{
			"session_key": sessionKey,
			"user_id":     userID,
			"reasons":     strings.Join(dedupeStrings(degradedReasons), ","),
		})
	}
	continuity.Degraded = len(degradedReasons) > 0
	continuity.DegradedBy = dedupeStrings(degradedReasons)

	return PromptContext{
		History:       history,
		Summary:       summary,
		PersonaPrompt: personaPrompt,
		RecallCards:   recallCards,
		RecallPrompt:  recallPrompt,
		Budget:        budget,
		Continuity:    continuity,
	}, nil
}

func (s *Service) RecordUserTurn(ctx context.Context, ev Event, userID string) (Event, int, error) {
	ev = normalizeEvent(ev)
	ev.Role = "user"
	if strings.TrimSpace(userID) == "" {
		userID = "local-user"
	}

	ops := extractUserContentUpsertOps(ev.Content, ev.ID)
	filtered := make([]ConsolidationOp, 0, len(ops))
	for _, op := range ops {
		op.Confidence = calibrateSignalConfidence(op)
		if containsSensitiveContent(op.Content) {
			continue
		}
		if s.policy != nil && op.Confidence < s.policy.MinConfidence(op.Kind) {
			continue
		}
		filtered = append(filtered, op)
	}

	inserted, err := s.store.AppendUserEventAndMemories(ctx, ev, userID, s.cfg.AgentID, filtered)
	if err != nil {
		_ = s.store.AddMetric(ctx, "memory.record_user_turn.error", 1, map[string]string{
			"session_key": ev.SessionKey,
			"user_id":     userID,
		})
		return Event{}, 0, err
	}
	s.appendSnapshot(ev)
	_ = s.store.AddMetric(ctx, "memory.record_user_turn.memories", float64(inserted), map[string]string{
		"session_key": ev.SessionKey,
		"user_id":     userID,
	})
	return ev, inserted, nil
}

func (s *Service) CaptureImmediateUserSignals(ctx context.Context, sessionKey, userID, sourceEventID, content string) error {
	ops := extractUserContentUpsertOps(content, sourceEventID)
	if len(ops) == 0 {
		return nil
	}

	now := time.Now().UnixMilli()
	inserted := 0
	for _, op := range ops {
		if op.Action != "upsert" {
			continue
		}
		if (op.Kind == MemorySemanticFact || op.Kind == MemoryUserPreference) && op.Confidence < 0.88 {
			op.Confidence = 0.88
		}
		if s.policy != nil && op.Confidence < s.policy.MinConfidence(op.Kind) {
			continue
		}
		scopeType, scopeID := deriveScopeForOp(op.Kind, sessionKey, userID, op.Metadata)
		item, err := s.store.UpsertMemoryItem(ctx, MemoryItem{
			ID:            "mem-" + uuid.NewString(),
			UserID:        userID,
			AgentID:       s.cfg.AgentID,
			ScopeType:     scopeType,
			ScopeID:       scopeID,
			SessionKey:    sessionKey,
			Kind:          op.Kind,
			Key:           op.Key,
			Content:       strings.TrimSpace(op.Content),
			Confidence:    op.Confidence,
			Weight:        1.2,
			SourceEventID: sourceEventID,
			FirstSeenAtMS: now,
			LastSeenAtMS:  now,
			ExpiresAtMS:   s.ttlFor(op.Kind, op.TTL),
			Metadata:      op.Metadata,
		})
		if err != nil {
			_ = s.store.AddMetric(ctx, "memory.capture.immediate.error", 1, map[string]string{
				"session_key": sessionKey,
				"user_id":     userID,
			})
			return err
		}
		vec := embedText(item.Content)
		if err := s.store.UpsertEmbedding(ctx, item.ID, currentEmbeddingModel(), vec); err != nil {
			_ = s.store.AddMetric(ctx, "memory.capture.immediate.error", 1, map[string]string{
				"session_key": sessionKey,
				"user_id":     userID,
			})
			return err
		}
		inserted++
	}
	_ = s.store.AddMetric(ctx, "memory.capture.immediate.items", float64(inserted), map[string]string{
		"session_key": sessionKey,
		"user_id":     userID,
	})
	return nil
}

func (s *Service) AddMetric(ctx context.Context, metric string, value float64, labels map[string]string) error {
	return s.store.AddMetric(ctx, metric, value, labels)
}

func (s *Service) estimateMessageTokens(content string) int {
	if s.budgeter == nil {
		return estimateMessageTokens(content)
	}
	return s.budgeter.EstimateTextTokens(s.contextModel, content)
}

// ObservePromptUsage calibrates token estimation against provider-reported usage.
func (s *Service) ObservePromptUsage(ctx context.Context, model string, estimatedPromptTokens, actualPromptTokens int) {
	if s.budgeter == nil {
		return
	}
	s.budgeter.ObservePromptUsage(model, estimatedPromptTokens, actualPromptTokens)
	if estimatedPromptTokens <= 0 || actualPromptTokens <= 0 {
		return
	}
	ratio := float64(actualPromptTokens) / float64(estimatedPromptTokens)
	_ = s.store.AddMetric(ctx, "memory.token_budget.estimate_ratio", ratio, map[string]string{
		"model": strings.TrimSpace(model),
	})
}

func (s *Service) GetProviderState(ctx context.Context, sessionKey, provider string) (string, error) {
	return s.store.GetSessionProviderState(ctx, sessionKey, provider)
}

func (s *Service) SetProviderState(ctx context.Context, sessionKey, provider, stateID string) error {
	return s.store.SetSessionProviderState(ctx, sessionKey, provider, stateID)
}

func (s *Service) ForceCompact(ctx context.Context, sessionKey, userID string, maxTokens int) error {
	budget := DeriveContextBudget(maxTokens)
	return s.compactSessionSerialized(ctx, sessionKey, userID, budget)
}

func (s *Service) compactSessionSerialized(ctx context.Context, sessionKey, userID string, budget ContextBudget) error {
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return nil
	}

	for {
		s.compactionMu.Lock()
		flight, busy := s.compactionState[sessionKey]
		if !busy {
			flight = &compactionFlight{done: make(chan struct{})}
			s.compactionState[sessionKey] = flight
			s.compactionMu.Unlock()
			break
		}
		done := flight.done
		s.compactionMu.Unlock()
		_ = s.store.AddMetric(ctx, "memory.compaction.waiting", 1, map[string]string{
			"session_key": sessionKey,
		})
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-done:
		}
	}

	err := s.compactor.CompactSession(ctx, sessionKey, userID, s.cfg.AgentID, budget)
	s.compactionMu.Lock()
	if flight := s.compactionState[sessionKey]; flight != nil {
		close(flight.done)
		delete(s.compactionState, sessionKey)
	}
	s.compactionMu.Unlock()
	return err
}

func (s *Service) RollbackPersona(ctx context.Context, userID string) error {
	if s.persona == nil {
		return nil
	}
	return s.persona.RollbackLastRevision(ctx, userID, s.cfg.AgentID)
}

func (s *Service) GetPersonaProfile(ctx context.Context, userID string) (PersonaProfile, error) {
	if s.persona == nil {
		return defaultPersonaProfile(userID, s.cfg.AgentID), nil
	}
	return s.store.GetPersonaProfile(ctx, userID, s.cfg.AgentID)
}

func (s *Service) ListPersonaCandidates(ctx context.Context, userID, sessionKey, turnID, status string, limit int) ([]PersonaUpdateCandidate, error) {
	return s.store.ListPersonaCandidates(ctx, userID, s.cfg.AgentID, sessionKey, turnID, status, limit)
}

func (s *Service) ListPersonaRevisions(ctx context.Context, userID string, limit int) ([]PersonaRevision, error) {
	return s.store.ListPersonaRevisions(ctx, userID, s.cfg.AgentID, limit)
}

func (s *Service) ApplyPersonaDirectivesSync(ctx context.Context, sessionKey, turnID, userID string) (PersonaApplyReport, error) {
	report := PersonaApplyReport{
		SessionKey: sessionKey,
		TurnID:     turnID,
		UserID:     userID,
		AgentID:    s.cfg.AgentID,
		AppliedAt:  time.Now().UnixMilli(),
	}
	if s.persona == nil || !s.cfg.PersonaSyncApply {
		return report, nil
	}
	start := time.Now()
	if err := s.persona.EmitCandidatesForTurn(ctx, sessionKey, turnID, userID, s.cfg.AgentID); err != nil {
		_ = s.store.AddMetric(ctx, "memory.persona.apply_sync.error", 1, map[string]string{
			"session_key": sessionKey,
			"user_id":     userID,
			"stage":       "emit",
		})
		return report, err
	}
	applyReport, err := s.persona.ApplyPendingForTurn(ctx, sessionKey, turnID, userID, s.cfg.AgentID)
	if err != nil {
		_ = s.store.AddMetric(ctx, "memory.persona.apply_sync.error", 1, map[string]string{
			"session_key": sessionKey,
			"user_id":     userID,
			"stage":       "apply",
		})
		return report, err
	}
	if applyReport.SessionKey == "" {
		applyReport.SessionKey = sessionKey
	}
	if applyReport.TurnID == "" {
		applyReport.TurnID = turnID
	}
	if applyReport.UserID == "" {
		applyReport.UserID = userID
	}
	if applyReport.AgentID == "" {
		applyReport.AgentID = s.cfg.AgentID
	}
	if applyReport.AppliedAt == 0 {
		applyReport.AppliedAt = time.Now().UnixMilli()
	}
	_ = s.store.AddMetric(ctx, "memory.persona.apply.attempted", float64(len(applyReport.Decisions)), map[string]string{
		"session_key": sessionKey,
		"user_id":     userID,
	})
	_ = s.store.AddMetric(ctx, "memory.persona.apply.accepted", float64(applyReport.AcceptedCount()), map[string]string{
		"session_key": sessionKey,
		"user_id":     userID,
	})
	_ = s.store.AddMetric(ctx, "memory.persona.apply.rejected", float64(applyReport.RejectedCount()), map[string]string{
		"session_key": sessionKey,
		"user_id":     userID,
	})
	_ = s.store.AddMetric(ctx, "memory.persona.apply.deferred", float64(applyReport.DeferredCount()), map[string]string{
		"session_key": sessionKey,
		"user_id":     userID,
	})
	_ = s.store.AddMetric(ctx, "memory.persona.apply_sync.latency_ms", float64(time.Since(start).Milliseconds()), map[string]string{
		"session_key": sessionKey,
		"user_id":     userID,
	})
	return applyReport, nil
}

func (s *Service) ScheduleTurnMaintenance(ctx context.Context, sessionKey, turnID, userID string) {
	now := time.Now().UnixMilli()
	_ = s.store.EnqueueJob(ctx, Job{
		ID:         maintenanceJobID(JobConsolidate, sessionKey, turnID),
		JobType:    JobConsolidate,
		SessionKey: sessionKey,
		Status:     JobPending,
		Priority:   30,
		Payload: map[string]string{
			"turn_id": turnID,
			"user_id": userID,
		},
		RunAfterMS:  now,
		CreatedAtMS: now,
		UpdatedAtMS: now,
	})
	_ = s.store.EnqueueJob(ctx, Job{
		ID:         maintenanceJobID(JobPersonaApply, sessionKey, turnID),
		JobType:    JobPersonaApply,
		SessionKey: sessionKey,
		Status:     JobPending,
		Priority:   55,
		Payload: map[string]string{
			"turn_id": turnID,
			"user_id": userID,
		},
		RunAfterMS:  now + 200,
		CreatedAtMS: now,
		UpdatedAtMS: now,
	})
	_ = s.store.EnqueueJob(ctx, Job{
		ID:         maintenanceJobID(JobEmbeddingSync, sessionKey, ""),
		JobType:    JobEmbeddingSync,
		SessionKey: sessionKey,
		Status:     JobPending,
		Priority:   60,
		Payload: map[string]string{
			"user_id": userID,
		},
		RunAfterMS:  now + 250,
		CreatedAtMS: now,
		UpdatedAtMS: now,
	})
	_ = s.store.EnqueueJob(ctx, Job{
		ID:         maintenanceJobID(JobCompact, sessionKey, ""),
		JobType:    JobCompact,
		SessionKey: sessionKey,
		Status:     JobPending,
		Priority:   80,
		Payload: map[string]string{
			"user_id": userID,
		},
		RunAfterMS:  now + 1000,
		CreatedAtMS: now,
		UpdatedAtMS: now,
	})
}

func (s *Service) ScheduleEmbeddingReindex(ctx context.Context) {
	now := time.Now().UnixMilli()
	_ = s.store.EnqueueJob(ctx, Job{
		ID:         maintenanceJobID(JobEmbeddingReindex, s.cfg.AgentID, ""),
		JobType:    JobEmbeddingReindex,
		SessionKey: s.cfg.AgentID,
		Status:     JobPending,
		Priority:   15,
		Payload: map[string]string{
			"agent_id": s.cfg.AgentID,
		},
		RunAfterMS:  now,
		CreatedAtMS: now,
		UpdatedAtMS: now,
	})
}

func (s *Service) runWorker() {
	defer s.wg.Done()

	ticker := time.NewTicker(s.cfg.WorkerPoll)
	defer ticker.Stop()

	// Run once at startup so pending jobs from prior process lifetime begin immediately.
	s.processPendingJobs()

	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.processPendingJobs()
		}
	}
}

func (s *Service) processPendingJobs() {
	const maxBatch = 32
	now := time.Now().UnixMilli()
	ctx := context.Background()
	s.runRetentionSweepIfDue(ctx, now)
	s.runFileMemorySyncIfDue(ctx, now)
	_ = s.store.RequeueExpiredJobs(ctx, now)

	leaseForMS := int64(s.cfg.WorkerLease / time.Millisecond)
	if leaseForMS <= 0 {
		leaseForMS = int64((45 * time.Second) / time.Millisecond)
	}

	for i := 0; i < maxBatch; i++ {
		job, ok, err := s.store.ClaimNextJob(ctx, time.Now().UnixMilli(), leaseForMS)
		if err != nil || !ok {
			return
		}

		if err := s.handleJob(ctx, job); err != nil {
			attempt := parseJobAttempt(job.Payload["attempt"])
			if attempt < 3 {
				nextAttempt := attempt + 1
				payload := cloneJobPayload(job.Payload)
				payload["attempt"] = strconv.Itoa(nextAttempt)
				retryAt := time.Now().Add(jobRetryBackoff(nextAttempt)).UnixMilli()
				if requeueErr := s.store.EnqueueJob(ctx, Job{
					ID:          job.ID,
					JobType:     job.JobType,
					SessionKey:  job.SessionKey,
					Status:      JobPending,
					Priority:    job.Priority,
					Payload:     payload,
					Error:       "",
					RunAfterMS:  retryAt,
					CreatedAtMS: job.CreatedAtMS,
					UpdatedAtMS: time.Now().UnixMilli(),
				}); requeueErr == nil {
					_ = s.store.AddMetric(ctx, "memory.job.retried", 1, map[string]string{
						"type":    job.JobType,
						"attempt": strconv.Itoa(nextAttempt),
					})
					continue
				}
			}
			_ = s.store.FailJob(ctx, job.ID, err.Error())
			_ = s.store.AddMetric(ctx, "memory.job.failed", 1, map[string]string{
				"type":    job.JobType,
				"attempt": strconv.Itoa(parseJobAttempt(job.Payload["attempt"])),
			})
			continue
		}
		_ = s.store.CompleteJob(ctx, job.ID)
		_ = s.store.AddMetric(ctx, "memory.job.completed", 1, map[string]string{"type": job.JobType})
	}
}

func (s *Service) runRetentionSweepIfDue(ctx context.Context, nowMS int64) {
	const minIntervalMS = int64((6 * time.Hour) / time.Millisecond)
	if s.lastRetentionSweep > 0 && nowMS-s.lastRetentionSweep < minIntervalMS {
		return
	}
	eventRetentionMS := int64(s.cfg.EventRetention / time.Millisecond)
	auditRetentionMS := int64(s.cfg.AuditRetention / time.Millisecond)
	if eventRetentionMS <= 0 && auditRetentionMS <= 0 {
		return
	}
	if err := s.store.SweepRetention(ctx, nowMS, eventRetentionMS, auditRetentionMS); err != nil {
		_ = s.store.AddMetric(ctx, "memory.retention.sweep.error", 1, nil)
		return
	}
	s.lastRetentionSweep = nowMS
	_ = s.store.AddMetric(ctx, "memory.retention.sweep.ok", 1, nil)
}

const fileMemoryKeyPrefix = "filemem:"

type fileMemorySnapshot struct {
	ModMS     int64
	SizeBytes int64
	Evergreen bool
	Keys      []string
}

type fileMemorySyncDelta struct {
	Upserts   map[string]MemoryItem
	DeleteSet map[string]struct{}
	LiveSet   map[string]struct{}
}

func (s *Service) runFileMemorySyncIfDue(ctx context.Context, nowMS int64) {
	if !s.cfg.FileMemoryEnabled {
		return
	}
	pollMS := int64(s.cfg.FileMemoryPoll / time.Millisecond)
	if pollMS <= 0 {
		pollMS = int64((15 * time.Second) / time.Millisecond)
	}
	watchDebounceMS := int64(s.cfg.FileMemoryWatchDebounce / time.Millisecond)
	if watchDebounceMS <= 0 {
		watchDebounceMS = int64((1200 * time.Millisecond) / time.Millisecond)
	}

	s.fileMemoryMu.Lock()
	dirty := s.fileMemoryDirty
	lastEventMS := s.fileMemoryEventMS
	s.fileMemoryMu.Unlock()

	dueByPoll := s.lastFileMemorySync == 0 || nowMS-s.lastFileMemorySync >= pollMS
	if s.cfg.FileMemoryWatchEnabled {
		if dirty {
			if lastEventMS > 0 && nowMS-lastEventMS < watchDebounceMS {
				return
			}
		} else if !dueByPoll {
			return
		}
	} else if !dueByPoll {
		return
	}

	store, ok := s.store.(*SQLiteStore)
	if !ok {
		return
	}

	delta, err := s.collectFileMemoryDelta(nowMS)
	if err != nil {
		_ = s.store.AddMetric(ctx, "memory.file_sync.error", 1, map[string]string{
			"reason": "scan",
		})
		return
	}
	upserted := 0
	for _, item := range delta.Upserts {
		if _, err := s.store.UpsertMemoryItem(ctx, item); err != nil {
			_ = s.store.AddMetric(ctx, "memory.file_sync.error", 1, map[string]string{
				"reason": "upsert",
			})
			return
		}
		upserted++
	}

	deleted := 0
	for key := range delta.DeleteSet {
		if err := s.store.DeleteMemoryByKey(ctx, "", s.cfg.AgentID, MemorySemanticFact, key); err != nil {
			_ = s.store.AddMetric(ctx, "memory.file_sync.error", 1, map[string]string{
				"reason": "delete",
			})
			return
		}
		deleted++
	}

	// One-time startup reconciliation to remove stale file-memory keys left over
	// from periods when this process was not running.
	if !s.fileMemoryPrimed {
		existingKeys, err := store.ListMemoryKeysByPrefix(ctx, s.cfg.AgentID, fileMemoryKeyPrefix)
		if err == nil {
			for _, key := range existingKeys {
				if _, ok := delta.LiveSet[key]; ok {
					continue
				}
				if err := s.store.DeleteMemoryByKey(ctx, "", s.cfg.AgentID, MemorySemanticFact, key); err != nil {
					_ = s.store.AddMetric(ctx, "memory.file_sync.error", 1, map[string]string{
						"reason": "startup_reconcile_delete",
					})
					return
				}
				deleted++
			}
		}
		s.fileMemoryPrimed = true
	}

	s.lastFileMemorySync = nowMS
	s.fileMemoryMu.Lock()
	s.fileMemoryDirty = false
	if s.fileMemoryEventMS == 0 {
		s.fileMemoryEventMS = nowMS
	}
	s.fileMemoryMu.Unlock()
	_ = s.store.AddMetric(ctx, "memory.file_sync.items", float64(len(delta.LiveSet)), map[string]string{
		"source": "workspace_markdown",
	})
	_ = s.store.AddMetric(ctx, "memory.file_sync.upserts", float64(upserted), map[string]string{
		"source": "workspace_markdown",
	})
	_ = s.store.AddMetric(ctx, "memory.file_sync.deletes", float64(deleted), map[string]string{
		"source": "workspace_markdown",
	})
}

func (s *Service) collectFileMemoryDelta(nowMS int64) (fileMemorySyncDelta, error) {
	delta := fileMemorySyncDelta{
		Upserts:   map[string]MemoryItem{},
		DeleteSet: map[string]struct{}{},
		LiveSet:   map[string]struct{}{},
	}
	root := strings.TrimSpace(s.cfg.FileMemoryDir)
	if root == "" {
		return delta, nil
	}
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			s.fileMemoryMu.Lock()
			for _, snapshot := range s.fileMemoryIndex {
				for _, key := range snapshot.Keys {
					delta.DeleteSet[key] = struct{}{}
				}
			}
			s.fileMemoryIndex = map[string]fileMemorySnapshot{}
			s.fileMemoryMu.Unlock()
			return delta, nil
		}
		return delta, err
	}

	prevIndex := s.snapshotFileMemoryIndex()
	nextIndex := make(map[string]fileMemorySnapshot, len(prevIndex))

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			name := strings.TrimSpace(d.Name())
			if strings.HasPrefix(name, ".") && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(d.Name()))
		if ext != ".md" && ext != ".markdown" {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.Size() <= 0 || int(info.Size()) > s.cfg.FileMemoryMaxFileBytes {
			rel, relErr := filepath.Rel(root, path)
			if relErr == nil {
				rel = filepath.ToSlash(strings.TrimSpace(rel))
				if prev, ok := prevIndex[rel]; ok {
					for _, key := range prev.Keys {
						delta.DeleteSet[key] = struct{}{}
					}
				}
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			rel = d.Name()
		}
		rel = filepath.ToSlash(strings.TrimSpace(rel))
		if rel == "" {
			return nil
		}
		modMS := info.ModTime().UnixMilli()
		if modMS <= 0 {
			modMS = nowMS
		}
		prev, hadPrev := prevIndex[rel]
		if hadPrev && prev.ModMS == modMS && prev.SizeBytes == info.Size() {
			nextIndex[rel] = prev
			for _, key := range prev.Keys {
				delta.LiveSet[key] = struct{}{}
			}
			return nil
		}

		raw, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		content := string(raw)
		evergreen := isEvergreenMemoryFile(rel, content)
		snippets := extractMarkdownMemorySnippets(content)
		newKeys := make([]string, 0, len(snippets))
		for _, snippet := range snippets {
			snippet = strings.TrimSpace(snippet)
			if snippet == "" {
				continue
			}
			key := fileMemoryKey(rel, snippet)
			newKeys = append(newKeys, key)
		}
		sort.Strings(newKeys)

		if hadPrev && prev.Evergreen == evergreen && stringSlicesEqual(prev.Keys, newKeys) {
			nextIndex[rel] = fileMemorySnapshot{
				ModMS:     modMS,
				SizeBytes: info.Size(),
				Evergreen: evergreen,
				Keys:      append([]string(nil), newKeys...),
			}
			for _, key := range newKeys {
				delta.LiveSet[key] = struct{}{}
			}
			return nil
		}

		newKeySet := make(map[string]struct{}, len(newKeys))
		for _, snippet := range snippets {
			snippet = strings.TrimSpace(snippet)
			if snippet == "" {
				continue
			}
			key := fileMemoryKey(rel, snippet)
			newKeySet[key] = struct{}{}
			delta.Upserts[key] = MemoryItem{
				ID:            "mem-" + uuid.NewString(),
				UserID:        "",
				AgentID:       s.cfg.AgentID,
				ScopeType:     MemoryScopeGlobal,
				ScopeID:       "",
				SessionKey:    "",
				Kind:          MemorySemanticFact,
				Key:           key,
				Content:       snippet,
				Confidence:    0.92,
				Weight:        1.1,
				SourceEventID: "",
				FirstSeenAtMS: modMS,
				LastSeenAtMS:  modMS,
				ExpiresAtMS:   0,
				Evergreen:     evergreen,
				Metadata: map[string]string{
					"source":    "file_memory",
					"path":      rel,
					"evergreen": strconv.FormatBool(evergreen),
				},
			}
			delta.LiveSet[key] = struct{}{}
		}
		if hadPrev {
			for _, key := range prev.Keys {
				if _, ok := newKeySet[key]; ok {
					continue
				}
				delta.DeleteSet[key] = struct{}{}
			}
		}
		nextIndex[rel] = fileMemorySnapshot{
			ModMS:     modMS,
			SizeBytes: info.Size(),
			Evergreen: evergreen,
			Keys:      append([]string(nil), newKeys...),
		}
		return nil
	})
	if err != nil {
		return delta, err
	}
	for rel, prev := range prevIndex {
		if _, ok := nextIndex[rel]; ok {
			continue
		}
		for _, key := range prev.Keys {
			delta.DeleteSet[key] = struct{}{}
		}
	}

	s.fileMemoryMu.Lock()
	s.fileMemoryIndex = nextIndex
	s.fileMemoryMu.Unlock()
	return delta, nil
}

func (s *Service) snapshotFileMemoryIndex() map[string]fileMemorySnapshot {
	s.fileMemoryMu.Lock()
	defer s.fileMemoryMu.Unlock()
	out := make(map[string]fileMemorySnapshot, len(s.fileMemoryIndex))
	for rel, snapshot := range s.fileMemoryIndex {
		out[rel] = fileMemorySnapshot{
			ModMS:     snapshot.ModMS,
			SizeBytes: snapshot.SizeBytes,
			Evergreen: snapshot.Evergreen,
			Keys:      append([]string(nil), snapshot.Keys...),
		}
	}
	return out
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func fileMemoryKey(relPath, content string) string {
	relPath = strings.ToLower(strings.TrimSpace(filepath.ToSlash(relPath)))
	hash := contentHash(content)
	return fileMemoryKeyPrefix + relPath + ":" + hash[:12]
}

func isEvergreenMemoryFile(relPath, content string) bool {
	base := strings.ToLower(strings.TrimSpace(filepath.Base(relPath)))
	switch base {
	case "memory.md", "identity.md", "profile.md", "persona.md", "about.md":
		return true
	}
	if strings.Contains(base, "evergreen") {
		return true
	}
	head := strings.ToLower(content)
	if len(head) > 400 {
		head = head[:400]
	}
	return strings.Contains(head, "evergreen: true")
}

func extractMarkdownMemorySnippets(markdown string) []string {
	markdown = strings.ReplaceAll(markdown, "\r\n", "\n")
	lines := strings.Split(markdown, "\n")
	out := make([]string, 0, 48)
	seen := map[string]struct{}{}
	inCodeBlock := false
	appendSnippet := func(raw string) {
		raw = strings.TrimSpace(raw)
		raw = strings.Trim(raw, "-* \t")
		raw = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(raw, "#"), "#"))
		if raw == "" {
			return
		}
		if len(raw) < 8 {
			return
		}
		if len(raw) > 500 {
			raw = raw[:500] + "... [trimmed]"
		}
		key := strings.ToLower(raw)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, raw)
	}

	var paragraph strings.Builder
	flushParagraph := func() {
		if paragraph.Len() == 0 {
			return
		}
		appendSnippet(paragraph.String())
		paragraph.Reset()
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inCodeBlock = !inCodeBlock
			continue
		}
		if inCodeBlock {
			continue
		}
		if trimmed == "" {
			flushParagraph()
			continue
		}
		if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") || strings.HasPrefix(trimmed, "#") {
			flushParagraph()
			appendSnippet(trimmed)
			continue
		}
		if paragraph.Len() > 0 {
			paragraph.WriteString(" ")
		}
		paragraph.WriteString(trimmed)
	}
	flushParagraph()

	if len(out) > 96 {
		out = out[:96]
	}
	return out
}

func (s *Service) handleJob(ctx context.Context, job Job) error {
	switch job.JobType {
	case JobConsolidate:
		turnID := job.Payload["turn_id"]
		userID := job.Payload["user_id"]
		if strings.TrimSpace(turnID) == "" || strings.TrimSpace(userID) == "" {
			return fmt.Errorf("invalid consolidate job payload")
		}
		if err := s.consolidator.ConsolidateTurn(ctx, job.SessionKey, turnID, userID, s.cfg.AgentID); err != nil {
			return err
		}
		return nil
	case JobPersonaApply:
		turnID := job.Payload["turn_id"]
		userID := job.Payload["user_id"]
		if strings.TrimSpace(turnID) == "" || strings.TrimSpace(userID) == "" {
			return fmt.Errorf("invalid persona apply job payload")
		}
		if s.persona == nil {
			return nil
		}
		// Ensure deterministic per-turn ordering by deriving candidates again before apply.
		// This makes persona_apply idempotent even if consolidate has not run yet.
		if err := s.persona.EmitCandidatesForTurn(ctx, job.SessionKey, turnID, userID, s.cfg.AgentID); err != nil {
			return err
		}
		_, err := s.persona.ApplyPendingForTurn(ctx, job.SessionKey, turnID, userID, s.cfg.AgentID)
		return err
	case JobCompact:
		userID := job.Payload["user_id"]
		if strings.TrimSpace(userID) == "" {
			return fmt.Errorf("invalid compact job payload")
		}
		return s.compactSessionSerialized(ctx, job.SessionKey, userID, DeriveContextBudget(s.cfg.MaxContextTokens))
	case JobEmbeddingSync:
		return s.syncSessionEmbeddingDeltas(ctx, job.SessionKey)
	case JobEmbeddingReindex:
		_, err := s.reindexEmbeddingsAtomic(ctx)
		return err
	default:
		return fmt.Errorf("unknown memory job type: %s", job.JobType)
	}
}

func maintenanceJobID(jobType, sessionKey, turnID string) string {
	h := sha1.Sum([]byte(jobType + "|" + sessionKey + "|" + turnID))
	return "job-" + hex.EncodeToString(h[:8])
}

func (s *Service) syncSessionEmbeddingDeltas(ctx context.Context, sessionKey string) error {
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return nil
	}
	store, ok := s.store.(*SQLiteStore)
	if !ok {
		return nil
	}

	model := s.primaryEmbeddingModel()
	cursor, err := store.GetSessionIndexCursor(ctx, sessionKey)
	if err != nil {
		return err
	}

	processed := 0
	maxSeen := cursor
	const batchSize = 128
	for i := 0; i < 8; i++ {
		items, err := store.ListSessionEmbeddingDeltas(ctx, sessionKey, s.cfg.AgentID, model, cursor, batchSize)
		if err != nil {
			return err
		}
		if len(items) == 0 {
			break
		}
		contents := make([]string, 0, len(items))
		for _, item := range items {
			contents = append(contents, item.Content)
		}
		modelUsed, vectors, _, embErr := s.embedBatchWithFallback(ctx, contents)
		if embErr != nil {
			return embErr
		}
		for idx, item := range items {
			if idx >= len(vectors) {
				break
			}
			if err := store.UpsertEmbedding(ctx, item.ID, modelUsed, vectors[idx]); err != nil {
				return err
			}
			processed++
			if item.LastSeenAtMS > maxSeen {
				maxSeen = item.LastSeenAtMS
			}
		}
		cursor = maxSeen
		if len(items) < batchSize {
			break
		}
	}

	if maxSeen > 0 {
		if err := store.SetSessionIndexCursor(ctx, sessionKey, maxSeen); err != nil {
			return err
		}
	}
	_ = s.store.AddMetric(ctx, "memory.embedding.sync.items", float64(processed), map[string]string{
		"session_key": sessionKey,
		"model":       model,
	})
	return nil
}

// ReindexEmbeddingsAtomic rebuilds embeddings into a temp index and swaps atomically.
func (s *Service) ReindexEmbeddingsAtomic(ctx context.Context) (EmbeddingReindexReport, error) {
	return s.reindexEmbeddingsAtomic(ctx)
}

func (s *Service) reindexEmbeddingsAtomic(ctx context.Context) (EmbeddingReindexReport, error) {
	store, ok := s.store.(*SQLiteStore)
	if !ok {
		return EmbeddingReindexReport{}, fmt.Errorf("embedding reindex is only supported by sqlite store")
	}

	fallbackHits := 0
	report, err := store.ReindexEmbeddingsAtomic(ctx, s.cfg.AgentID, func(content string) (string, []float32, error) {
		model, vector, usedFallback, err := s.embedWithFallbackNoCache(ctx, content)
		if usedFallback {
			fallbackHits++
		}
		return model, vector, err
	})
	if err != nil {
		return report, err
	}
	report.FallbackHits = fallbackHits
	_ = s.store.AddMetric(ctx, "memory.embedding.reindex.items", float64(report.IndexedItems), map[string]string{
		"agent_id": s.cfg.AgentID,
	})
	_ = s.store.AddMetric(ctx, "memory.embedding.reindex.fallback_hits", float64(report.FallbackHits), map[string]string{
		"agent_id": s.cfg.AgentID,
	})
	return report, nil
}

func (s *Service) embedWithFallback(content string) (model string, vector []float32, usedFallback bool, err error) {
	return s.embedWithFallbackCtx(context.Background(), content)
}

func (s *Service) embedWithFallbackNoCache(ctx context.Context, content string) (model string, vector []float32, usedFallback bool, err error) {
	models := s.embeddingFallbackModels
	if len(models) == 0 {
		models = []string{currentEmbeddingModel(), hashEmbeddingModel}
	}
	errs := make([]string, 0, len(models))
	for i, candidate := range models {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if s.embeddingEngine != nil {
			spec, specErr := parseEmbeddingModelSpec(candidate)
			if specErr == nil {
				if vectors, embErr := s.embeddingEngine.embedBatchRemoteAware(ctx, spec, []string{content}); embErr == nil && len(vectors) > 0 {
					return spec.Raw, vectors[0], i > 0, nil
				} else if embErr != nil {
					errs = append(errs, embErr.Error())
				}
			} else {
				errs = append(errs, specErr.Error())
			}
		}
		vec, modelID, embErr := embedTextWithModel(candidate, content)
		if embErr != nil {
			errs = append(errs, embErr.Error())
			continue
		}
		return modelID, vec, i > 0, nil
	}
	if len(errs) == 0 {
		errs = append(errs, "no embedding fallback models configured")
	}
	return "", nil, false, fmt.Errorf("embedding fallback exhausted: %s", strings.Join(errs, "; "))
}

func (s *Service) embedWithFallbackCtx(ctx context.Context, content string) (model string, vector []float32, usedFallback bool, err error) {
	model, vectors, usedFallback, err := s.embedBatchWithFallback(ctx, []string{content})
	if err != nil {
		return "", nil, false, err
	}
	if len(vectors) == 0 || len(vectors[0]) == 0 {
		return "", nil, usedFallback, fmt.Errorf("embedding fallback exhausted: no vector generated")
	}
	return model, vectors[0], usedFallback, nil
}

func (s *Service) embedBatchWithFallback(ctx context.Context, contents []string) (model string, vectors [][]float32, usedFallback bool, err error) {
	if len(contents) == 0 {
		return "", nil, false, nil
	}
	models := s.embeddingFallbackModels
	if len(models) == 0 {
		models = []string{currentEmbeddingModel(), hashEmbeddingModel}
	}
	if s.embeddingEngine != nil {
		model, vectors, err = s.embeddingEngine.EmbedBatch(ctx, models, contents)
		if err == nil {
			usedFallback = strings.TrimSpace(model) != strings.TrimSpace(models[0])
			return model, vectors, usedFallback, nil
		}
	}
	errs := make([]string, 0, len(models))
	for i, candidate := range models {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if len(contents) > 1 {
			multiVec := make([][]float32, 0, len(contents))
			modelID := ""
			ok := true
			for _, content := range contents {
				vec, localModel, embErr := embedTextWithModel(candidate, content)
				if embErr != nil {
					errs = append(errs, embErr.Error())
					ok = false
					break
				}
				if modelID == "" {
					modelID = localModel
				}
				multiVec = append(multiVec, vec)
			}
			if ok && modelID != "" {
				return modelID, multiVec, i > 0, nil
			}
			continue
		}
		vec, modelID, embErr := embedTextWithModel(candidate, contents[0])
		if embErr != nil {
			errs = append(errs, embErr.Error())
			continue
		}
		return modelID, [][]float32{vec}, i > 0, nil
	}
	if len(errs) == 0 {
		errs = append(errs, "no embedding fallback models configured")
	}
	return "", nil, false, fmt.Errorf("embedding fallback exhausted: %s", strings.Join(errs, "; "))
}

func (s *Service) primaryEmbeddingModel() string {
	models := s.embeddingFallbackModels
	if len(models) == 0 {
		return currentEmbeddingModel()
	}
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model != "" {
			return model
		}
	}
	return currentEmbeddingModel()
}

func normalizeEvent(ev Event) Event {
	if strings.TrimSpace(ev.ID) == "" {
		ev.ID = "evt-" + uuid.NewString()
	}
	if strings.TrimSpace(ev.TurnID) == "" {
		ev.TurnID = "turn-" + uuid.NewString()
	}
	if ev.CreatedAt.IsZero() {
		ev.CreatedAt = time.Now()
	}
	return ev
}

func (s *Service) appendSnapshot(ev Event) {
	if strings.TrimSpace(ev.SessionKey) == "" {
		return
	}
	s.snapshotMu.Lock()
	defer s.snapshotMu.Unlock()

	now := time.Now().UnixMilli()

	// Evict oldest session if at capacity and this is a new session.
	if _, exists := s.snapshots[ev.SessionKey]; !exists && len(s.snapshots) >= s.snapshotMaxSessions {
		oldestKey := ""
		oldestTime := int64(1<<63 - 1)
		for k, t := range s.snapshotAccess {
			if t < oldestTime {
				oldestTime = t
				oldestKey = k
			}
		}
		if oldestKey != "" {
			delete(s.snapshots, oldestKey)
			delete(s.snapshotAccess, oldestKey)
		}
	}

	stream := append(s.snapshots[ev.SessionKey], ev)
	if len(stream) > s.snapshotLimit {
		stream = stream[len(stream)-s.snapshotLimit:]
	}
	s.snapshots[ev.SessionKey] = stream
	s.snapshotAccess[ev.SessionKey] = now
}

func (s *Service) getSnapshotEvents(sessionKey string, limit int) []Event {
	if strings.TrimSpace(sessionKey) == "" {
		return nil
	}
	if limit <= 0 {
		limit = 96
	}
	s.snapshotMu.Lock()
	defer s.snapshotMu.Unlock()
	stream := s.snapshots[sessionKey]
	if len(stream) == 0 {
		return nil
	}
	s.snapshotAccess[sessionKey] = time.Now().UnixMilli()
	start := 0
	if len(stream) > limit {
		start = len(stream) - limit
	}
	out := make([]Event, len(stream[start:]))
	copy(out, stream[start:])
	return out
}

func cloneJobPayload(src map[string]string) map[string]string {
	if len(src) == 0 {
		return map[string]string{}
	}
	dst := make(map[string]string, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func parseJobAttempt(raw string) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func jobRetryBackoff(attempt int) time.Duration {
	switch attempt {
	case 1:
		return 5 * time.Second
	case 2:
		return 30 * time.Second
	default:
		return 120 * time.Second
	}
}

func mergeEventStreams(primary, secondary []Event, limit int) []Event {
	if len(primary) == 0 {
		if limit > 0 && len(secondary) > limit {
			return append([]Event(nil), secondary[len(secondary)-limit:]...)
		}
		return append([]Event(nil), secondary...)
	}
	if len(secondary) == 0 {
		if limit > 0 && len(primary) > limit {
			return append([]Event(nil), primary[len(primary)-limit:]...)
		}
		return primary
	}
	seen := map[string]struct{}{}
	out := make([]Event, 0, len(primary)+len(secondary))
	appendUnique := func(ev Event) {
		key := strings.TrimSpace(ev.ID)
		if key == "" {
			key = fmt.Sprintf("%s|%d|%s|%s", ev.TurnID, ev.Seq, ev.Role, strings.TrimSpace(ev.Content))
		}
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, ev)
	}
	for _, ev := range primary {
		appendUnique(ev)
	}
	for _, ev := range secondary {
		appendUnique(ev)
	}
	sort.SliceStable(out, func(i, j int) bool {
		ti := out[i].CreatedAt.UnixMilli()
		tj := out[j].CreatedAt.UnixMilli()
		if ti == tj {
			return out[i].Seq < out[j].Seq
		}
		return ti < tj
	})
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

func repairOrphanToolHistory(events []Event) ([]Event, int) {
	if len(events) == 0 {
		return events, 0
	}
	remainingByTurn := map[string]int{}
	out := make([]Event, 0, len(events))
	dropped := 0

	for _, ev := range events {
		turnID := strings.TrimSpace(ev.TurnID)
		if turnID == "" {
			turnID = strings.TrimSpace(ev.ID)
		}
		role := strings.ToLower(strings.TrimSpace(ev.Role))
		switch role {
		case "assistant":
			remainingByTurn[turnID] = parseToolCallCount(ev.Metadata)
			out = append(out, ev)
		case "tool":
			if strings.TrimSpace(ev.ToolCallID) == "" {
				dropped++
				continue
			}
			if remainingByTurn[turnID] <= 0 {
				dropped++
				continue
			}
			remainingByTurn[turnID]--
			out = append(out, ev)
		default:
			out = append(out, ev)
		}
	}
	return out, dropped
}

func parseToolCallCount(meta map[string]string) int {
	if len(meta) == 0 {
		return 0
	}
	raw := strings.TrimSpace(meta["tool_call_count"])
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func (s *Service) ttlFor(kind MemoryItemKind, override time.Duration) int64 {
	if override > 0 {
		return time.Now().Add(override).UnixMilli()
	}
	if s.policy == nil {
		return 0
	}
	return s.policy.TTLFor(kind)
}

func normalizeEmbeddingConfig(cfg Config) (string, []string) {
	primary := strings.TrimSpace(cfg.EmbeddingModel)
	if primary == "" {
		primary = defaultEmbeddingModel
	}
	if shouldAutoPromoteToRemoteEmbeddings(primary) {
		switch {
		case strings.TrimSpace(cfg.EmbeddingOpenAIToken) != "":
			primary = "openai:text-embedding-3-small"
		case strings.TrimSpace(cfg.EmbeddingOpenRouterKey) != "":
			primary = "openrouter:openai/text-embedding-3-small"
		}
	}

	explicitFallbackChain := len(cfg.EmbeddingFallbackModels) > 0
	chain := dedupeEmbeddingModels(cfg.EmbeddingFallbackModels)
	if len(chain) == 0 {
		chain = []string{primary}
		chain = append(chain, defaultEmbeddingModel, hashEmbeddingModel)
	}
	chain = dedupeEmbeddingModels(chain)
	hasLocalFallback := false
	for _, candidate := range chain {
		spec, err := parseEmbeddingModelSpec(candidate)
		if err == nil && spec.Provider == embeddingProviderLocal {
			hasLocalFallback = true
			break
		}
	}
	if !hasLocalFallback && !explicitFallbackChain {
		chain = dedupeEmbeddingModels(append(chain, defaultEmbeddingModel, hashEmbeddingModel))
	}
	if len(chain) == 0 {
		chain = []string{defaultEmbeddingModel, hashEmbeddingModel}
	}
	if strings.TrimSpace(primary) == "" {
		primary = chain[0]
	}
	return primary, chain
}

func dedupeEmbeddingModels(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		spec, err := parseEmbeddingModelSpec(value)
		key := strings.ToLower(value)
		if err == nil && strings.TrimSpace(spec.Raw) != "" {
			value = spec.Raw
			key = strings.ToLower(spec.Raw)
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	return out
}

func shouldAutoPromoteToRemoteEmbeddings(model string) bool {
	spec, err := parseEmbeddingModelSpec(model)
	if err != nil {
		return false
	}
	return spec.Provider == embeddingProviderLocal && (spec.Model == defaultEmbeddingModel || spec.Model == hashEmbeddingModel)
}

func dedupeStrings(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

func calibrateSignalConfidence(op ConsolidationOp) float64 {
	conf := clampConfidence(op.Confidence)
	if conf == 0 {
		switch op.Kind {
		case MemoryUserPreference:
			conf = 0.78
		case MemorySemanticFact:
			conf = 0.72
		case MemoryTaskState:
			conf = 0.62
		default:
			conf = 0.58
		}
	}
	if op.Action == "delete" {
		return 1.0
	}
	return conf
}

func containsSensitiveContent(content string) bool {
	content = strings.TrimSpace(content)
	if content == "" {
		return false
	}
	if personaSensitiveRegex.MatchString(content) {
		return true
	}
	lower := strings.ToLower(content)
	return strings.Contains(lower, "password") ||
		strings.Contains(lower, "api key") ||
		strings.Contains(lower, "private key") ||
		strings.Contains(lower, "secret token")
}

func selectHistoryWithinBudget(events []Event, tokenBudget int, estimate tokenEstimateFunc) []Message {
	if tokenBudget <= 0 {
		return nil
	}
	if estimate == nil {
		estimate = estimateMessageTokens
	}
	selected := []Event{}
	used := 0

	for i := len(events) - 1; i >= 0; i-- {
		ev := events[i]
		if ev.Role == "system" {
			continue
		}
		tokens := estimate(ev.Content)
		if used+tokens > tokenBudget && len(selected) > 0 {
			break
		}
		selected = append(selected, ev)
		used += tokens
		if len(selected) >= 48 {
			break
		}
	}

	for i, j := 0, len(selected)-1; i < j; i, j = i+1, j-1 {
		selected[i], selected[j] = selected[j], selected[i]
	}

	out := make([]Message, 0, len(selected))
	for _, ev := range selected {
		out = append(out, Message{
			Role:       ev.Role,
			Content:    ev.Content,
			ToolCallID: ev.ToolCallID,
		})
	}
	return out
}

func estimateMessageTokens(content string) int {
	runes := len([]rune(content))
	if runes == 0 {
		return 0
	}
	tokens := runes * 2 / 5
	if tokens < 8 {
		return 8
	}
	return tokens
}

type tokenEstimateFunc func(content string) int

func formatSnapshotAndRecall(snapshot SessionSnapshot, cards []MemoryCard, budgetTokens int, estimate tokenEstimateFunc) string {
	sections := []string{}
	if estimate == nil {
		estimate = estimateMessageTokens
	}
	if block := formatSessionSnapshot(snapshot, budgetTokens/2, estimate); block != "" {
		sections = append(sections, block)
	}
	if block := formatRecallCards(cards, budgetTokens, estimate); block != "" {
		sections = append(sections, block)
	}
	return strings.TrimSpace(strings.Join(sections, "\n\n"))
}

func formatSessionSnapshot(snapshot SessionSnapshot, budgetTokens int, estimate tokenEstimateFunc) string {
	if snapshot.Revision == 0 {
		return ""
	}
	if budgetTokens <= 0 {
		budgetTokens = 256
	}
	if estimate == nil {
		estimate = estimateMessageTokens
	}
	lines := []string{"## Structured Session Snapshot"}
	if strings.TrimSpace(snapshot.Summary) != "" {
		lines = append(lines, "- Summary: "+strings.TrimSpace(snapshot.Summary))
	}
	appendList := func(label string, values []string, max int) {
		if len(values) == 0 {
			return
		}
		if len(values) > max {
			values = values[:max]
		}
		lines = append(lines, "- "+label+":")
		for _, v := range values {
			lines = append(lines, "  - "+strings.TrimSpace(v))
		}
	}
	appendList("Facts", snapshot.Facts, 4)
	appendList("Preferences", snapshot.Preferences, 4)
	appendList("Open Tasks", snapshot.Tasks, 4)
	appendList("Open Loops", snapshot.OpenLoops, 4)
	appendList("Constraints", snapshot.Constraints, 4)

	used := 0
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		tokens := estimate(line)
		if used+tokens > budgetTokens && used > 0 {
			break
		}
		out = append(out, line)
		used += tokens
	}
	return strings.Join(out, "\n")
}

func formatRecallCards(cards []MemoryCard, budgetTokens int, estimate tokenEstimateFunc) string {
	if len(cards) == 0 {
		return ""
	}
	if budgetTokens <= 0 {
		budgetTokens = 512
	}
	if estimate == nil {
		estimate = estimateMessageTokens
	}
	var b strings.Builder
	b.WriteString("## Recalled Memory\n")
	used := 0
	for _, card := range cards {
		line := fmt.Sprintf("- [%s] %s", card.Kind, strings.TrimSpace(card.Content))
		tokens := estimate(line)
		if used+tokens > budgetTokens && used > 0 {
			break
		}
		b.WriteString(line)
		b.WriteString("\n")
		used += tokens
	}
	return strings.TrimSpace(b.String())
}

func deriveContinuationNotes(snapshot SessionSnapshot) []string {
	if snapshot.Revision == 0 {
		return nil
	}
	notes := []string{
		fmt.Sprintf("Continue from compacted snapshot revision %d (compaction id: %s).", snapshot.Revision, valueOr(snapshot.CompactionID, "unknown")),
	}
	for i := 0; i < len(snapshot.Tasks) && i < 2; i++ {
		task := strings.TrimSpace(snapshot.Tasks[i])
		if task == "" {
			continue
		}
		notes = append(notes, "Outstanding task: "+task)
	}
	for i := 0; i < len(snapshot.OpenLoops) && i < 2; i++ {
		loop := strings.TrimSpace(snapshot.OpenLoops[i])
		if loop == "" {
			continue
		}
		notes = append(notes, "Open loop to preserve: "+loop)
	}
	return dedupeStrings(notes)
}

func formatContinuationNotes(notes []string, budgetTokens int, estimate tokenEstimateFunc) string {
	if len(notes) == 0 {
		return ""
	}
	if budgetTokens <= 0 {
		budgetTokens = 96
	}
	if estimate == nil {
		estimate = estimateMessageTokens
	}
	lines := []string{"## Continuation Handoff"}
	used := estimate(lines[0])
	for _, note := range notes {
		line := "- " + strings.TrimSpace(note)
		tokens := estimate(line)
		if used+tokens > budgetTokens && len(lines) > 1 {
			break
		}
		lines = append(lines, line)
		used += tokens
	}
	return strings.Join(lines, "\n")
}

func valueOr(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
