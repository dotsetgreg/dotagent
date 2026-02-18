package memory

import "context"

// Store provides durable persistence for all memory state.
type Store interface {
	Close() error
	EnsureSession(ctx context.Context, sessionKey, channel, chatID, userID string) error
	GetSession(ctx context.Context, sessionKey string) (Session, error)
	ListSessions(ctx context.Context, userID string, limit int) ([]Session, error)
	MarkSessionConsolidated(ctx context.Context, sessionKey string, atMS int64) error
	GetSessionSummary(ctx context.Context, sessionKey string) (string, error)
	SetSessionSummary(ctx context.Context, sessionKey, summary string) error
	GetSessionProviderState(ctx context.Context, sessionKey string) (string, error)
	SetSessionProviderState(ctx context.Context, sessionKey, stateID string) error
	GetLatestSessionSnapshot(ctx context.Context, sessionKey string) (SessionSnapshot, error)
	UpsertSessionSnapshot(ctx context.Context, snap SessionSnapshot) error
	AppendEvent(ctx context.Context, ev Event) error
	AppendUserEventAndMemories(ctx context.Context, ev Event, userID, agentID string, ops []ConsolidationOp) (memoryCount int, err error)
	ListRecentEvents(ctx context.Context, sessionKey string, limit int, includeArchived bool) ([]Event, error)
	ArchiveEventsBefore(ctx context.Context, sessionKey string, keepLatest int) (archivedCount int, err error)
	ArchiveEventsExceptTurns(ctx context.Context, sessionKey string, keepTurnIDs []string) (archivedCount int, err error)
	StartCompaction(ctx context.Context, sessionKey string, sourceCount, retainedCount int, checkpoint map[string]string) (string, error)
	CheckpointCompaction(ctx context.Context, compactionID string, checkpoint map[string]string) error
	CompleteCompaction(ctx context.Context, compactionID, summary string) error
	FailCompaction(ctx context.Context, compactionID, errMsg string) error

	UpsertMemoryItem(ctx context.Context, item MemoryItem) (MemoryItem, error)
	DeleteMemoryByKey(ctx context.Context, userID, agentID string, kind MemoryItemKind, key string) error
	ListMemoryCandidates(ctx context.Context, userID, agentID, sessionKey string, limit int) ([]MemoryItem, error)
	SearchMemoryFTS(ctx context.Context, userID, agentID, sessionKey, query string, limit int) ([]MemoryItem, error)
	UpsertMemoryLink(ctx context.Context, link MemoryLink) error
	ListMemoryLinks(ctx context.Context, itemID string, limit int) ([]MemoryLink, error)
	ListMemoryObservations(ctx context.Context, itemID string, limit int) ([]MemoryObservation, error)

	UpsertEmbedding(ctx context.Context, itemID, model string, vector []float32) error
	GetEmbeddings(ctx context.Context, itemIDs []string) (map[string][]float32, error)

	GetRetrievalCache(ctx context.Context, key string, nowMS int64) (string, bool, error)
	PutRetrievalCache(ctx context.Context, key, value string, expiresAtMS int64) error

	EnqueueJob(ctx context.Context, job Job) error
	ClaimNextJob(ctx context.Context, nowMS, leaseForMS int64) (Job, bool, error)
	CompleteJob(ctx context.Context, id string) error
	FailJob(ctx context.Context, id, errMsg string) error
	RequeueExpiredJobs(ctx context.Context, nowMS int64) error
	SweepRetention(ctx context.Context, nowMS, eventRetentionMS, auditRetentionMS int64) error

	AddMetric(ctx context.Context, metric string, value float64, labels map[string]string) error

	GetPersonaProfile(ctx context.Context, userID, agentID string) (PersonaProfile, error)
	UpsertPersonaProfile(ctx context.Context, profile PersonaProfile) error
	InsertPersonaCandidates(ctx context.Context, candidates []PersonaUpdateCandidate) error
	ListPersonaCandidates(ctx context.Context, userID, agentID, sessionKey, turnID, status string, limit int) ([]PersonaUpdateCandidate, error)
	UpdatePersonaCandidateStatus(ctx context.Context, id, status, reason, revisionID string, appliedAtMS int64) error
	BumpPersonaSignal(ctx context.Context, userID, agentID, fieldPath, valueHash string, atMS int64) (int, error)
	InsertPersonaRevision(ctx context.Context, rev PersonaRevision) error
	ListPersonaRevisions(ctx context.Context, userID, agentID string, limit int) ([]PersonaRevision, error)
	ApplyPersonaMutation(ctx context.Context, profile PersonaProfile, candidate PersonaUpdateCandidate, revision PersonaRevision, memoryOps []ConsolidationOp) error
	RollbackPersonaToRevision(ctx context.Context, userID, agentID, revisionID string) (PersonaProfile, error)
}

// Retriever recalls memories for prompt construction.
type Retriever interface {
	Recall(ctx context.Context, query string, opts RetrievalOptions) ([]MemoryCard, error)
}

// Consolidator extracts structured long-term memories from turns.
type Consolidator interface {
	ConsolidateTurn(ctx context.Context, sessionKey, turnID, userID, agentID string) error
}

// Compactor generates compaction artifacts and archives old thread events.
type Compactor interface {
	CompactSession(ctx context.Context, sessionKey, userID, agentID string, budget ContextBudget) error
}

// Policy controls capture/retention/selection behavior.
type Policy interface {
	ShouldCapture(ev Event) bool
	TTLFor(kind MemoryItemKind) int64
	MinConfidence(kind MemoryItemKind) float64
	ShouldRecall(card MemoryCard) bool
}
