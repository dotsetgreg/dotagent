package memory

import "time"

// DefaultPolicy implements sensible memory capture/retention rules.
type DefaultPolicy struct{}

func NewDefaultPolicy() *DefaultPolicy { return &DefaultPolicy{} }

func (p *DefaultPolicy) ShouldCapture(ev Event) bool {
	if ev.Role != "user" && ev.Role != "assistant" {
		return false
	}
	return len(ev.Content) >= 6
}

func (p *DefaultPolicy) TTLFor(kind MemoryItemKind) int64 {
	now := time.Now().UnixMilli()
	switch kind {
	case MemoryEpisodic:
		return now + int64((30*24*time.Hour)/time.Millisecond)
	case MemoryTaskState:
		return now + int64((14*24*time.Hour)/time.Millisecond)
	default:
		return 0
	}
}

func (p *DefaultPolicy) MinConfidence(kind MemoryItemKind) float64 {
	switch kind {
	case MemorySemanticFact, MemoryUserPreference:
		return 0.55
	case MemoryTaskState:
		return 0.5
	default:
		return 0.45
	}
}

func (p *DefaultPolicy) ShouldRecall(card MemoryCard) bool {
	return card.Score >= 0.3 && card.Confidence >= 0.4
}
