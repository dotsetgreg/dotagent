package memory

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// SummaryFunc generates a merged summary from previous summary + transcript.
type SummaryFunc func(ctx context.Context, existingSummary, transcript string) (string, error)

// SessionCompactor creates durable compaction artifacts and archives old events.
type SessionCompactor struct {
	store     Store
	summarize SummaryFunc
}

func NewSessionCompactor(store Store, summarize SummaryFunc) *SessionCompactor {
	return &SessionCompactor{store: store, summarize: summarize}
}

func (c *SessionCompactor) CompactSession(ctx context.Context, sessionKey, userID, agentID string, budget ContextBudget) error {
	events, err := c.store.ListRecentEvents(ctx, sessionKey, 320, false)
	if err != nil {
		return err
	}
	if len(events) < 24 {
		return nil
	}

	est := estimateEventTokens(events)
	threshold := int(float64(budget.ThreadTokens) * 0.85)
	if est <= threshold {
		return nil
	}

	keepLatest := 16
	if budget.ThreadTokens > 0 {
		candidate := budget.ThreadTokens / 280
		if candidate > keepLatest {
			keepLatest = candidate
		}
	}
	if keepLatest < 10 {
		keepLatest = 10
	}
	if keepLatest > 40 {
		keepLatest = 40
	}
	if len(events) <= keepLatest {
		return nil
	}

	sourceCount := len(events)
	toArchive := events[:len(events)-keepLatest]

	compactionID, err := c.store.StartCompaction(ctx, sessionKey, sourceCount, keepLatest, map[string]string{
		"phase":        "started",
		"source_count": fmt.Sprintf("%d", sourceCount),
	})
	if err != nil {
		return err
	}

	existingSummary, _ := c.store.GetSessionSummary(ctx, sessionKey)
	transcript := buildCompactionTranscript(toArchive)

	summary := ""
	if c.summarize != nil {
		summary, err = c.summarize(ctx, existingSummary, transcript)
		if err != nil {
			_ = c.store.FailCompaction(ctx, compactionID, err.Error())
			return err
		}
	}
	if strings.TrimSpace(summary) == "" {
		summary = fallbackSummary(existingSummary, toArchive)
	}

	if err := c.store.CheckpointCompaction(ctx, compactionID, map[string]string{
		"phase":          "summary_ready",
		"summary_length": fmt.Sprintf("%d", len(summary)),
	}); err != nil {
		_ = c.store.FailCompaction(ctx, compactionID, err.Error())
		return err
	}

	if err := c.store.SetSessionSummary(ctx, sessionKey, summary); err != nil {
		_ = c.store.FailCompaction(ctx, compactionID, err.Error())
		return err
	}

	archivedCount, err := c.store.ArchiveEventsBefore(ctx, sessionKey, keepLatest)
	if err != nil {
		_ = c.store.FailCompaction(ctx, compactionID, err.Error())
		return err
	}

	if err := c.store.CheckpointCompaction(ctx, compactionID, map[string]string{
		"phase":          "archived",
		"archived_count": fmt.Sprintf("%d", archivedCount),
	}); err != nil {
		_ = c.store.FailCompaction(ctx, compactionID, err.Error())
		return err
	}

	if err := c.store.CompleteCompaction(ctx, compactionID, summary); err != nil {
		return err
	}
	_ = c.store.AddMetric(ctx, "memory.compaction.archived_events", float64(archivedCount), map[string]string{"session_key": sessionKey})
	return nil
}

func estimateEventTokens(events []Event) int {
	chars := 0
	for _, ev := range events {
		chars += len([]rune(ev.Content))
	}
	if chars == 0 {
		return 0
	}
	return chars * 2 / 5
}

func buildCompactionTranscript(events []Event) string {
	var b strings.Builder
	for _, ev := range events {
		if ev.Role != "user" && ev.Role != "assistant" {
			continue
		}
		content := strings.TrimSpace(ev.Content)
		if content == "" {
			continue
		}
		if len(content) > 400 {
			content = content[:400] + "..."
		}
		b.WriteString(ev.Role)
		b.WriteString(": ")
		b.WriteString(content)
		b.WriteString("\n")
	}
	return b.String()
}

func fallbackSummary(existing string, events []Event) string {
	parts := []string{}
	if strings.TrimSpace(existing) != "" {
		parts = append(parts, strings.TrimSpace(existing))
	}
	if len(events) > 0 {
		start := events[0].CreatedAt.Format(time.RFC3339)
		end := events[len(events)-1].CreatedAt.Format(time.RFC3339)
		parts = append(parts, fmt.Sprintf("Compacted conversation window %s - %s (%d events).", start, end, len(events)))
	}

	bulletCount := 0
	for _, ev := range events {
		if ev.Role != "user" {
			continue
		}
		line := strings.TrimSpace(ev.Content)
		if line == "" {
			continue
		}
		if len(line) > 160 {
			line = line[:160] + "..."
		}
		parts = append(parts, "- User topic: "+line)
		bulletCount++
		if bulletCount >= 6 {
			break
		}
	}

	return strings.Join(parts, "\n")
}
