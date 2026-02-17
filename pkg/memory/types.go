package memory

import "time"

// Session captures persistent per-conversation state.
type Session struct {
	SessionKey         string
	Channel            string
	ChatID             string
	UserID             string
	CreatedAtMS        int64
	UpdatedAtMS        int64
	MessageCount       int
	Summary            string
	LastConsolidatedMS int64
}

// Event is the canonical append-only conversation record.
type Event struct {
	ID         string
	SessionKey string
	TurnID     string
	Seq        int
	Role       string
	Content    string
	ToolCallID string
	ToolName   string
	Metadata   map[string]string
	CreatedAt  time.Time
	Archived   bool
}

// MemoryItemKind classifies long-term memories.
type MemoryItemKind string

const (
	MemorySemanticFact   MemoryItemKind = "semantic_fact"
	MemoryUserPreference MemoryItemKind = "user_preference"
	MemoryEpisodic       MemoryItemKind = "episodic_summary"
	MemoryTaskState      MemoryItemKind = "task_state"
	MemoryProcedural     MemoryItemKind = "procedural"
)

// MemoryItem is a consolidated memory entry in the canonical store.
type MemoryItem struct {
	ID            string
	UserID        string
	AgentID       string
	SessionKey    string
	Kind          MemoryItemKind
	Key           string
	Content       string
	Confidence    float64
	Weight        float64
	SourceEventID string
	FirstSeenAtMS int64
	LastSeenAtMS  int64
	ExpiresAtMS   int64
	DeletedAtMS   int64
	Metadata      map[string]string
}

// MemoryLink relates memory items (entity graph edges).
type MemoryLink struct {
	ID          string
	FromItemID  string
	ToItemID    string
	Relation    string
	Weight      float64
	CreatedAtMS int64
}

// MemoryCard is an LLM-facing recall unit.
type MemoryCard struct {
	ID         string
	Kind       MemoryItemKind
	Content    string
	Score      float64
	Confidence float64
	RecencyMS  int64
	Source     string
}

// RetrievalOptions controls memory recall behavior.
type RetrievalOptions struct {
	SessionKey      string
	UserID          string
	AgentID         string
	MaxCards        int
	CandidateLimit  int
	MinScore        float64
	CacheTTL        time.Duration
	NowMS           int64
	IncludeSession  bool
	IncludeGlobal   bool
	RecencyHalfLife time.Duration
}

// ConsolidationOp represents one memory update decision.
type ConsolidationOp struct {
	Action      string
	Kind        MemoryItemKind
	Key         string
	Content     string
	Confidence  float64
	SourceEvent string
	TTL         time.Duration
	Metadata    map[string]string
}

// PromptContext is the memory context assembled for each LLM turn.
type PromptContext struct {
	History      []Message
	Summary      string
	RecallCards  []MemoryCard
	RecallPrompt string
	Budget       ContextBudget
}

// Message is provider-agnostic prompt message representation.
type Message struct {
	Role       string
	Content    string
	ToolCallID string
}

// ContextBudget controls token allocation per prompt section.
type ContextBudget struct {
	TotalTokens   int
	SystemTokens  int
	ThreadTokens  int
	MemoryTokens  int
	SummaryTokens int
}

// JobType values for background memory workers.
const (
	JobConsolidate = "consolidate"
	JobCompact     = "compact"
)

// JobStatus values.
const (
	JobPending   = "pending"
	JobRunning   = "running"
	JobCompleted = "completed"
	JobFailed    = "failed"
)

// Job is a durable background memory task.
type Job struct {
	ID            string
	JobType       string
	SessionKey    string
	Status        string
	Priority      int
	Payload       map[string]string
	Error         string
	RunAfterMS    int64
	LeaseUntilMS  int64
	CreatedAtMS   int64
	UpdatedAtMS   int64
	CompletedAtMS int64
}
