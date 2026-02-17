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
	keepTurns, retainedCount := selectTurnsToKeep(events, keepLatest)
	archiveStrategy := "turns"
	toArchive := make([]Event, 0, len(events))
	for _, ev := range events {
		turnID := strings.TrimSpace(ev.TurnID)
		if turnID == "" {
			turnID = ev.ID
		}
		if _, ok := keepTurns[turnID]; ok {
			continue
		}
		toArchive = append(toArchive, ev)
	}
	// Fallback for large single-turn sessions: retain an event cap even when turn-aware
	// retention would otherwise keep everything.
	if len(toArchive) == 0 && len(events) > keepLatest {
		archiveStrategy = "events_fallback"
		retainedCount = keepLatest
		toArchive = append(toArchive, events[:len(events)-keepLatest]...)
	}
	if len(toArchive) == 0 {
		return nil
	}

	compactionID, err := c.store.StartCompaction(ctx, sessionKey, sourceCount, retainedCount, map[string]string{
		"phase":            "started",
		"source_count":     fmt.Sprintf("%d", sourceCount),
		"retain_count":     fmt.Sprintf("%d", retainedCount),
		"archive_strategy": archiveStrategy,
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
	if err := c.store.UpsertSessionSnapshot(ctx, buildSessionSnapshot(sessionKey, compactionID, summary, toArchive)); err != nil {
		_ = c.store.FailCompaction(ctx, compactionID, err.Error())
		return err
	}

	var archivedCount int
	if archiveStrategy == "turns" {
		keepTurnIDs := make([]string, 0, len(keepTurns))
		for turnID := range keepTurns {
			keepTurnIDs = append(keepTurnIDs, turnID)
		}
		archivedCount, err = c.store.ArchiveEventsExceptTurns(ctx, sessionKey, keepTurnIDs)
	} else {
		archivedCount, err = c.store.ArchiveEventsBefore(ctx, sessionKey, keepLatest)
	}
	if err != nil {
		_ = c.store.FailCompaction(ctx, compactionID, err.Error())
		return err
	}

	if err := c.store.CheckpointCompaction(ctx, compactionID, map[string]string{
		"phase":            "archived",
		"archived_count":   fmt.Sprintf("%d", archivedCount),
		"archive_strategy": archiveStrategy,
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

func selectTurnsToKeep(events []Event, keepLatest int) (map[string]struct{}, int) {
	keep := map[string]struct{}{}
	if keepLatest <= 0 || len(events) == 0 {
		return keep, 0
	}
	keptEvents := 0
	for i := len(events) - 1; i >= 0; i-- {
		ev := events[i]
		turnID := strings.TrimSpace(ev.TurnID)
		if turnID == "" {
			turnID = ev.ID
		}
		if turnID == "" {
			continue
		}
		if _, ok := keep[turnID]; ok {
			keptEvents++
			continue
		}
		if keptEvents >= keepLatest && len(keep) > 0 {
			break
		}
		keep[turnID] = struct{}{}
		keptEvents++
	}
	return keep, keptEvents
}

func buildSessionSnapshot(sessionKey, compactionID, summary string, events []Event) SessionSnapshot {
	facts := []string{}
	prefs := []string{}
	tasks := []string{}
	openLoops := []string{}
	constraints := []string{}

	add := func(dst *[]string, value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		key := strings.ToLower(value)
		for _, existing := range *dst {
			if strings.ToLower(existing) == key {
				return
			}
		}
		*dst = append(*dst, value)
	}

	for _, ev := range events {
		if ev.Role != "user" {
			continue
		}
		content := strings.TrimSpace(ev.Content)
		if content == "" {
			continue
		}
		for _, fact := range ExtractFactSignals(content) {
			if isPreferencePhrase(fact) {
				add(&prefs, fact)
			} else {
				add(&facts, fact)
			}
		}
		lower := strings.ToLower(content)
		if strings.Contains(lower, "need to") ||
			strings.Contains(lower, "todo") ||
			strings.Contains(lower, "deadline") ||
			strings.Contains(lower, "remind me") {
			add(&tasks, content)
		}
		if strings.Contains(lower, "can't") ||
			strings.Contains(lower, "cannot") ||
			strings.Contains(lower, "must") ||
			strings.Contains(lower, "requirement") ||
			strings.Contains(lower, "constraint") {
			add(&constraints, content)
		}
		if strings.Contains(content, "?") && len(openLoops) < 8 {
			add(&openLoops, content)
		}
	}

	return SessionSnapshot{
		SessionKey:   sessionKey,
		CreatedAtMS:  nowMS(),
		Facts:        facts,
		Preferences:  prefs,
		Tasks:        tasks,
		OpenLoops:    openLoops,
		Constraints:  constraints,
		Summary:      strings.TrimSpace(summary),
		CompactionID: compactionID,
	}
}
