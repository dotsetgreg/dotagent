package memory

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Config configures the memory subsystem.
type Config struct {
	Workspace        string
	AgentID          string
	MaxContextTokens int
	MaxRecallItems   int
	CandidateLimit   int
	RetrievalCache   time.Duration
	WorkerLease      time.Duration
	WorkerPoll       time.Duration
}

// Service is the orchestrator for memory capture, retrieval and compaction.
type Service struct {
	cfg          Config
	store        Store
	retriever    Retriever
	consolidator Consolidator
	compactor    Compactor
	policy       Policy

	stopCh chan struct{}
	wg     sync.WaitGroup

	closeOnce sync.Once
	closeErr  error
}

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

	dbPath := filepath.Join(cfg.Workspace, "state", "memory.db")
	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		return nil, err
	}
	policy := NewDefaultPolicy()

	svc := &Service{
		cfg:          cfg,
		store:        store,
		policy:       policy,
		retriever:    NewHybridRetriever(store, policy),
		consolidator: NewHeuristicConsolidator(store, policy),
		compactor:    NewSessionCompactor(store, summarize),
		stopCh:       make(chan struct{}),
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

func (s *Service) AppendEvent(ctx context.Context, ev Event) error {
	return s.store.AppendEvent(ctx, ev)
}

func (s *Service) BuildPromptContext(ctx context.Context, sessionKey, userID, query string, maxTokens int) (PromptContext, error) {
	if maxTokens <= 0 {
		maxTokens = s.cfg.MaxContextTokens
	}
	budget := DeriveContextBudget(maxTokens)

	summary, err := s.store.GetSessionSummary(ctx, sessionKey)
	if err != nil {
		return PromptContext{}, err
	}

	events, err := s.store.ListRecentEvents(ctx, sessionKey, 96, false)
	if err != nil {
		return PromptContext{}, err
	}
	history := selectHistoryWithinBudget(events, budget.ThreadTokens)

	recallCards, err := s.retriever.Recall(ctx, query, RetrievalOptions{
		SessionKey:      sessionKey,
		UserID:          userID,
		AgentID:         s.cfg.AgentID,
		MaxCards:        s.cfg.MaxRecallItems,
		CandidateLimit:  s.cfg.CandidateLimit,
		MinScore:        0.32,
		CacheTTL:        s.cfg.RetrievalCache,
		NowMS:           time.Now().UnixMilli(),
		IncludeSession:  true,
		IncludeGlobal:   true,
		RecencyHalfLife: 14 * 24 * time.Hour,
	})
	if err != nil {
		return PromptContext{}, err
	}
	_ = s.store.AddMetric(ctx, "memory.recall.cards", float64(len(recallCards)), map[string]string{
		"session_key": sessionKey,
		"user_id":     userID,
	})
	_ = s.store.AddMetric(ctx, "memory.context.history_messages", float64(len(history)), map[string]string{
		"session_key": sessionKey,
		"user_id":     userID,
	})

	return PromptContext{
		History:      history,
		Summary:      summary,
		RecallCards:  recallCards,
		RecallPrompt: formatRecallCards(recallCards, budget.MemoryTokens),
		Budget:       budget,
	}, nil
}

func (s *Service) ForceCompact(ctx context.Context, sessionKey, userID string, maxTokens int) error {
	budget := DeriveContextBudget(maxTokens)
	return s.compactor.CompactSession(ctx, sessionKey, userID, s.cfg.AgentID, budget)
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

func (s *Service) handleJob(ctx context.Context, job Job) error {
	switch job.JobType {
	case JobConsolidate:
		turnID := job.Payload["turn_id"]
		userID := job.Payload["user_id"]
		if strings.TrimSpace(turnID) == "" || strings.TrimSpace(userID) == "" {
			return fmt.Errorf("invalid consolidate job payload")
		}
		return s.consolidator.ConsolidateTurn(ctx, job.SessionKey, turnID, userID, s.cfg.AgentID)
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
