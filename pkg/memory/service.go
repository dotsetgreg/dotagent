package memory

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Config configures the memory subsystem.
type Config struct {
	Workspace            string
	AgentID              string
	EmbeddingModel       string
	MaxContextTokens     int
	MaxRecallItems       int
	CandidateLimit       int
	RetrievalCache       time.Duration
	WorkerLease          time.Duration
	WorkerPoll           time.Duration
	PersonaCardTokens    int
	PersonaExtractor     PersonaExtractionFunc
	PersonaSyncApply     bool
	PersonaFileSync      PersonaFileSyncMode
	PersonaPolicyMode    string
	PersonaMinConfidence float64
	EventRetention       time.Duration
	AuditRetention       time.Duration
}

// Service is the orchestrator for memory capture, retrieval and compaction.
type Service struct {
	cfg          Config
	store        Store
	retriever    Retriever
	consolidator Consolidator
	compactor    Compactor
	policy       Policy
	persona      *PersonaManager

	stopCh chan struct{}
	wg     sync.WaitGroup

	closeOnce sync.Once
	closeErr  error

	snapshotMu    sync.RWMutex
	snapshots     map[string][]Event
	snapshotLimit int

	lastRetentionSweep int64
}

var continuationCueRegex = regexp.MustCompile(`(?i)\b(already|earlier|before|as i (?:said|mentioned)|you (?:already )?(?:know|have)|that context|you remember|previously)\b`)

func NewService(cfg Config, summarize SummaryFunc) (*Service, error) {
	if strings.TrimSpace(cfg.Workspace) == "" {
		return nil, fmt.Errorf("memory workspace is required")
	}
	if cfg.AgentID == "" {
		cfg.AgentID = "dotagent"
	}
	if cfg.MaxContextTokens <= 0 {
		cfg.MaxContextTokens = 8192
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
	SetEmbedderByName(cfg.EmbeddingModel)

	dbPath := filepath.Join(cfg.Workspace, "state", "memory.db")
	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		return nil, err
	}
	policy := NewDefaultPolicy()

	personaPolicy := NewPersonaPolicyEngine(PersonaPolicyConfig{
		Mode:          cfg.PersonaPolicyMode,
		MinConfidence: cfg.PersonaMinConfidence,
	})

	svc := &Service{
		cfg:           cfg,
		store:         store,
		policy:        policy,
		retriever:     NewHybridRetriever(store, policy),
		consolidator:  NewHeuristicConsolidator(store, policy),
		compactor:     NewSessionCompactor(store, summarize),
		persona:       NewPersonaManager(store, cfg.Workspace, cfg.PersonaExtractor, cfg.PersonaFileSync, personaPolicy),
		stopCh:        make(chan struct{}),
		snapshots:     map[string][]Event{},
		snapshotLimit: 128,
	}

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
	budget := DeriveContextBudget(maxTokens)
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
	}
	if strings.TrimSpace(summary) == "" && strings.TrimSpace(snapshot.Summary) != "" {
		summary = strings.TrimSpace(snapshot.Summary)
	}
	continuity.HasSummary = strings.TrimSpace(summary) != ""

	events, err := s.store.ListRecentEvents(ctx, sessionKey, 96, false)
	if err != nil {
		degradedReasons = append(degradedReasons, "history")
		events = s.getSnapshotEvents(sessionKey, 96)
	} else {
		events = mergeEventStreams(events, s.getSnapshotEvents(sessionKey, 96), 96)
	}
	if len(events) == 0 {
		events = s.getSnapshotEvents(sessionKey, 96)
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
	budget = DeriveAdaptiveContextBudget(maxTokens, BudgetSignals{
		RecentEventCount: len(events),
		Query:            query,
		HasSummary:       continuity.HasSummary,
		HasRecall:        continuity.HasRecall,
	})
	history := selectHistoryWithinBudget(events, budget.ThreadTokens)
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

	recallPrompt := formatSnapshotAndRecall(snapshot, recallCards, budget.MemoryTokens)
	if personaPrompt != "" {
		if recallPrompt != "" {
			recallPrompt = personaPrompt + "\n\n" + recallPrompt
		} else {
			recallPrompt = personaPrompt
		}
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

	hasContinuityArtifacts := continuity.HasHistory || continuity.HasSummary || continuity.HasRecall
	if continuity.HasPriorTurns && !hasContinuityArtifacts {
		_ = s.store.AddMetric(ctx, "memory.context.fail_closed", 1, map[string]string{
			"session_key": sessionKey,
			"user_id":     userID,
			"reason":      "no_continuity_artifacts",
		})
		return PromptContext{}, ErrContinuityUnavailable
	}
	if continuity.HasPriorTurns && continuationCueRegex.MatchString(query) && !hasContinuityArtifacts {
		_ = s.store.AddMetric(ctx, "memory.context.fail_closed", 1, map[string]string{
			"session_key": sessionKey,
			"user_id":     userID,
			"reason":      "continuation_cue_without_context",
		})
		return PromptContext{}, ErrContinuityUnavailable
	}

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

func (s *Service) GetProviderState(ctx context.Context, sessionKey string) (string, error) {
	return s.store.GetSessionProviderState(ctx, sessionKey)
}

func (s *Service) SetProviderState(ctx context.Context, sessionKey, stateID string) error {
	return s.store.SetSessionProviderState(ctx, sessionKey, stateID)
}

func (s *Service) ForceCompact(ctx context.Context, sessionKey, userID string, maxTokens int) error {
	budget := DeriveContextBudget(maxTokens)
	return s.compactor.CompactSession(ctx, sessionKey, userID, s.cfg.AgentID, budget)
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
			_ = s.store.FailJob(ctx, job.ID, err.Error())
			_ = s.store.AddMetric(ctx, "memory.job.failed", 1, map[string]string{"type": job.JobType})
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
		if s.persona != nil {
			return s.persona.EmitCandidatesForTurn(ctx, job.SessionKey, turnID, userID, s.cfg.AgentID)
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
		return s.compactor.CompactSession(ctx, job.SessionKey, userID, s.cfg.AgentID, DeriveContextBudget(s.cfg.MaxContextTokens))
	default:
		return fmt.Errorf("unknown memory job type: %s", job.JobType)
	}
}

func maintenanceJobID(jobType, sessionKey, turnID string) string {
	h := sha1.Sum([]byte(jobType + "|" + sessionKey + "|" + turnID))
	return "job-" + hex.EncodeToString(h[:8])
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
	stream := append(s.snapshots[ev.SessionKey], ev)
	if len(stream) > s.snapshotLimit {
		stream = stream[len(stream)-s.snapshotLimit:]
	}
	s.snapshots[ev.SessionKey] = stream
}

func (s *Service) getSnapshotEvents(sessionKey string, limit int) []Event {
	if strings.TrimSpace(sessionKey) == "" {
		return nil
	}
	if limit <= 0 {
		limit = 96
	}
	s.snapshotMu.RLock()
	defer s.snapshotMu.RUnlock()
	stream := s.snapshots[sessionKey]
	if len(stream) == 0 {
		return nil
	}
	start := 0
	if len(stream) > limit {
		start = len(stream) - limit
	}
	out := make([]Event, len(stream[start:]))
	copy(out, stream[start:])
	return out
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

func (s *Service) ttlFor(kind MemoryItemKind, override time.Duration) int64 {
	if override > 0 {
		return time.Now().Add(override).UnixMilli()
	}
	if s.policy == nil {
		return 0
	}
	return s.policy.TTLFor(kind)
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

func selectHistoryWithinBudget(events []Event, tokenBudget int) []Message {
	if tokenBudget <= 0 {
		return nil
	}
	selected := []Event{}
	used := 0

	for i := len(events) - 1; i >= 0; i-- {
		ev := events[i]
		if ev.Role == "system" {
			continue
		}
		tokens := estimateMessageTokens(ev.Content)
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

func formatSnapshotAndRecall(snapshot SessionSnapshot, cards []MemoryCard, budgetTokens int) string {
	sections := []string{}
	if block := formatSessionSnapshot(snapshot, budgetTokens/2); block != "" {
		sections = append(sections, block)
	}
	if block := formatRecallCards(cards, budgetTokens); block != "" {
		sections = append(sections, block)
	}
	return strings.TrimSpace(strings.Join(sections, "\n\n"))
}

func formatSessionSnapshot(snapshot SessionSnapshot, budgetTokens int) string {
	if snapshot.Revision == 0 {
		return ""
	}
	if budgetTokens <= 0 {
		budgetTokens = 256
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
		tokens := estimateMessageTokens(line)
		if used+tokens > budgetTokens && used > 0 {
			break
		}
		out = append(out, line)
		used += tokens
	}
	return strings.Join(out, "\n")
}

func formatRecallCards(cards []MemoryCard, budgetTokens int) string {
	if len(cards) == 0 {
		return ""
	}
	if budgetTokens <= 0 {
		budgetTokens = 512
	}
	var b strings.Builder
	b.WriteString("## Recalled Memory\n")
	used := 0
	for _, card := range cards {
		line := fmt.Sprintf("- [%s] %s", card.Kind, strings.TrimSpace(card.Content))
		tokens := estimateMessageTokens(line)
		if used+tokens > budgetTokens && used > 0 {
			break
		}
		b.WriteString(line)
		b.WriteString("\n")
		used += tokens
	}
	return strings.TrimSpace(b.String())
}
