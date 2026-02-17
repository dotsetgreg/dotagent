package memory

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

const (
	personaCandidatePending  = "pending"
	personaCandidateApplied  = "applied"
	personaCandidateRejected = "rejected"
	personaCandidateDeferred = "deferred"
)

type PersonaFileSyncMode string

const (
	PersonaFileSyncExportOnly   PersonaFileSyncMode = "export_only"
	PersonaFileSyncImportExport PersonaFileSyncMode = "import_export"
	PersonaFileSyncDisabled     PersonaFileSyncMode = "disabled"
)

func NormalizePersonaFileSyncMode(raw string) PersonaFileSyncMode {
	switch PersonaFileSyncMode(strings.ToLower(strings.TrimSpace(raw))) {
	case PersonaFileSyncImportExport:
		return PersonaFileSyncImportExport
	case PersonaFileSyncDisabled:
		return PersonaFileSyncDisabled
	default:
		return PersonaFileSyncExportOnly
	}
}

// PersonaProfile is the canonical user-facing personalization document.
// It is the single durable source of truth for persona state.
type PersonaProfile struct {
	UserID   string `json:"user_id"`
	AgentID  string `json:"agent_id"`
	Revision int64  `json:"revision"`

	UpdatedAtMS int64 `json:"updated_at_ms"`

	Identity PersonaIdentity `json:"identity"`
	Soul     PersonaSoul     `json:"soul"`
	User     PersonaUser     `json:"user"`
}

type PersonaIdentity struct {
	AgentName  string            `json:"agent_name"`
	Role       string            `json:"role"`
	Purpose    string            `json:"purpose"`
	Goals      []string          `json:"goals"`
	Boundaries []string          `json:"boundaries"`
	Attributes map[string]string `json:"attributes"`
}

type PersonaSoul struct {
	Voice           string            `json:"voice"`
	Communication   string            `json:"communication_style"`
	Values          []string          `json:"values"`
	BehavioralRules []string          `json:"behavioral_rules"`
	Attributes      map[string]string `json:"attributes"`
}

type PersonaUser struct {
	Name               string            `json:"name"`
	Timezone           string            `json:"timezone"`
	Location           string            `json:"location"`
	Language           string            `json:"language"`
	CommunicationStyle string            `json:"communication_style"`
	Goals              []string          `json:"goals"`
	Preferences        map[string]string `json:"preferences"`
	SessionIntent      string            `json:"session_intent"`
	Attributes         map[string]string `json:"attributes"`
}

func defaultPersonaProfile(userID, agentID string) PersonaProfile {
	now := time.Now().UnixMilli()
	return PersonaProfile{
		UserID:      strings.TrimSpace(userID),
		AgentID:     strings.TrimSpace(agentID),
		Revision:    1,
		UpdatedAtMS: now,
		Identity: PersonaIdentity{
			AgentName: "DotAgent",
			Role:      "Personal AI assistant",
			Purpose:   "Deliver practical, concise, reliable help.",
			Goals: []string{
				"Keep responses actionable and accurate",
				"Preserve useful context and preferences over time",
			},
			Boundaries: []string{
				"Never fabricate actions",
				"Never expose or retain sensitive secrets",
			},
			Attributes: map[string]string{},
		},
		Soul: PersonaSoul{
			Voice:         "Grounded, direct, and helpful",
			Communication: "Concise by default; detail on request",
			Values: []string{
				"Accuracy",
				"Clarity",
				"User control",
			},
			BehavioralRules: []string{
				"State assumptions explicitly",
				"Prefer deterministic and testable behavior",
			},
			Attributes: map[string]string{},
		},
		User: PersonaUser{
			Preferences: map[string]string{},
			Attributes:  map[string]string{},
		},
	}
}

func (p PersonaProfile) clone() PersonaProfile {
	out := p
	out.Identity.Goals = append([]string{}, p.Identity.Goals...)
	out.Identity.Boundaries = append([]string{}, p.Identity.Boundaries...)
	out.Soul.Values = append([]string{}, p.Soul.Values...)
	out.Soul.BehavioralRules = append([]string{}, p.Soul.BehavioralRules...)
	out.Identity.Attributes = map[string]string{}
	for k, v := range p.Identity.Attributes {
		out.Identity.Attributes[k] = v
	}
	out.Soul.Attributes = map[string]string{}
	for k, v := range p.Soul.Attributes {
		out.Soul.Attributes[k] = v
	}
	out.User.Goals = append([]string{}, p.User.Goals...)
	out.User.Preferences = map[string]string{}
	for k, v := range p.User.Preferences {
		out.User.Preferences[k] = v
	}
	out.User.Attributes = map[string]string{}
	for k, v := range p.User.Attributes {
		out.User.Attributes[k] = v
	}
	return out
}

type PersonaDecision string

const (
	PersonaDecisionAccepted PersonaDecision = "accepted"
	PersonaDecisionRejected PersonaDecision = "rejected"
	PersonaDecisionDeferred PersonaDecision = "deferred"
)

type PersonaCandidateDecision struct {
	CandidateID string          `json:"candidate_id"`
	FieldPath   string          `json:"field_path"`
	Operation   string          `json:"operation"`
	Value       string          `json:"value"`
	Confidence  float64         `json:"confidence"`
	Decision    PersonaDecision `json:"decision"`
	ReasonCode  string          `json:"reason_code"`
	Source      string          `json:"source"`
}

type PersonaApplyReport struct {
	SessionKey string                     `json:"session_key"`
	TurnID     string                     `json:"turn_id"`
	UserID     string                     `json:"user_id"`
	AgentID    string                     `json:"agent_id"`
	Decisions  []PersonaCandidateDecision `json:"decisions"`
	AppliedAt  int64                      `json:"applied_at_ms"`
}

func (r PersonaApplyReport) AcceptedCount() int {
	total := 0
	for _, d := range r.Decisions {
		if d.Decision == PersonaDecisionAccepted {
			total++
		}
	}
	return total
}

func (r PersonaApplyReport) RejectedCount() int {
	total := 0
	for _, d := range r.Decisions {
		if d.Decision == PersonaDecisionRejected {
			total++
		}
	}
	return total
}

func (r PersonaApplyReport) DeferredCount() int {
	total := 0
	for _, d := range r.Decisions {
		if d.Decision == PersonaDecisionDeferred {
			total++
		}
	}
	return total
}

func (r PersonaApplyReport) HasAccepted() bool {
	return r.AcceptedCount() > 0
}

// PersonaUpdateCandidate is a proposed profile mutation extracted from a turn.
// Candidates are evaluated and applied asynchronously.
type PersonaUpdateCandidate struct {
	ID            string `json:"id"`
	UserID        string `json:"user_id"`
	AgentID       string `json:"agent_id"`
	SessionKey    string `json:"session_key"`
	TurnID        string `json:"turn_id"`
	SourceEventID string `json:"source_event_id"`

	FieldPath  string  `json:"field_path"`
	Operation  string  `json:"operation"` // set | append | delete
	Value      string  `json:"value"`
	Confidence float64 `json:"confidence"`
	Evidence   string  `json:"evidence"`
	Source     string  `json:"source"` // heuristic | llm | file_import

	Status            string `json:"status"`
	RejectedReason    string `json:"rejected_reason"`
	AppliedRevisionID string `json:"applied_revision_id"`

	CreatedAtMS int64 `json:"created_at_ms"`
	AppliedAtMS int64 `json:"applied_at_ms"`
}

// PersonaRevision is an immutable audit record for every profile mutation.
type PersonaRevision struct {
	ID          string `json:"id"`
	UserID      string `json:"user_id"`
	AgentID     string `json:"agent_id"`
	SessionKey  string `json:"session_key"`
	TurnID      string `json:"turn_id"`
	CandidateID string `json:"candidate_id"`

	FieldPath  string  `json:"field_path"`
	Operation  string  `json:"operation"`
	OldValue   string  `json:"old_value"`
	NewValue   string  `json:"new_value"`
	Confidence float64 `json:"confidence"`
	Evidence   string  `json:"evidence"`
	Reason     string  `json:"reason"`
	Source     string  `json:"source"`

	ProfileBeforeJSON string `json:"profile_before_json"`
	ProfileAfterJSON  string `json:"profile_after_json"`
	CreatedAtMS       int64  `json:"created_at_ms"`
}

type PersonaExtractionRequest struct {
	UserID          string
	AgentID         string
	SessionKey      string
	TurnID          string
	Transcript      string
	ExistingProfile PersonaProfile
}

// PersonaExtractionFunc is the service-level extraction callback.
type PersonaExtractionFunc func(ctx context.Context, req PersonaExtractionRequest) ([]PersonaUpdateCandidate, error)

func profileToJSON(profile PersonaProfile) string {
	raw, err := json.Marshal(profile)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

func profileFromJSON(raw string, userID, agentID string) PersonaProfile {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" {
		return defaultPersonaProfile(userID, agentID)
	}
	var p PersonaProfile
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return defaultPersonaProfile(userID, agentID)
	}
	if strings.TrimSpace(p.UserID) == "" {
		p.UserID = userID
	}
	if strings.TrimSpace(p.AgentID) == "" {
		p.AgentID = agentID
	}
	if p.Identity.Attributes == nil {
		p.Identity.Attributes = map[string]string{}
	}
	if p.Soul.Attributes == nil {
		p.Soul.Attributes = map[string]string{}
	}
	if p.User.Preferences == nil {
		p.User.Preferences = map[string]string{}
	}
	if p.User.Attributes == nil {
		p.User.Attributes = map[string]string{}
	}
	if p.Revision <= 0 {
		p.Revision = 1
	}
	if p.UpdatedAtMS <= 0 {
		p.UpdatedAtMS = time.Now().UnixMilli()
	}
	return p
}
