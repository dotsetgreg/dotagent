package memory

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// SummaryFunc generates a merged summary from previous summary + transcript.
type SummaryFunc func(ctx context.Context, existingSummary, transcript string) (string, error)

type CompactionHookPayload struct {
	SessionKey    string
	UserID        string
	AgentID       string
	CompactionID  string
	Status        string
	Stage         string
	SourceCount   int
	RetainedCount int
	ArchivedCount int
	SummaryLength int
	RecoveryMode  string
	Error         string
}

type CompactionHooks struct {
	Before func(ctx context.Context, payload CompactionHookPayload)
	After  func(ctx context.Context, payload CompactionHookPayload)
}

type CompactorConfig struct {
	SummaryTimeout     time.Duration
	ChunkChars         int
	MaxTranscriptChars int
	PartialSkipChars   int
	Hooks              CompactionHooks
}

// SessionCompactor creates durable compaction artifacts and archives old events.
type SessionCompactor struct {
	store     Store
	summarize SummaryFunc
	cfg       CompactorConfig
}

func NewSessionCompactor(store Store, summarize SummaryFunc, opts ...CompactorConfig) *SessionCompactor {
	cfg := CompactorConfig{
		SummaryTimeout:     60 * time.Second,
		ChunkChars:         9000,
		MaxTranscriptChars: 48000,
		PartialSkipChars:   2600,
	}
	if len(opts) > 0 {
		opt := opts[0]
		if opt.SummaryTimeout > 0 {
			cfg.SummaryTimeout = opt.SummaryTimeout
		}
		if opt.ChunkChars > 0 {
			cfg.ChunkChars = opt.ChunkChars
		}
		if opt.MaxTranscriptChars > 0 {
			cfg.MaxTranscriptChars = opt.MaxTranscriptChars
		}
		if opt.PartialSkipChars > 0 {
			cfg.PartialSkipChars = opt.PartialSkipChars
		}
		cfg.Hooks = opt.Hooks
	}
	return &SessionCompactor{store: store, summarize: summarize, cfg: cfg}
}

func (c *SessionCompactor) CompactSession(ctx context.Context, sessionKey, userID, agentID string, budget ContextBudget) (retErr error) {
	events, err := c.store.ListRecentEvents(ctx, sessionKey, 320, false)
	if err != nil {
		return err
	}
	if len(events) < 24 {
		return nil
	}

	est := estimateEventTokens(events)
	threshold := int(float64(budget.ThreadTokens) * 0.85)
	if int(float64(est)*1.10) <= threshold {
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

	hookPayload := CompactionHookPayload{
		SessionKey:    sessionKey,
		UserID:        userID,
		AgentID:       agentID,
		CompactionID:  compactionID,
		Status:        "running",
		Stage:         "started",
		SourceCount:   sourceCount,
		RetainedCount: retainedCount,
	}
	c.emitBeforeHook(ctx, hookPayload)
	defer func() {
		if retErr != nil {
			hookPayload.Status = "failed"
			hookPayload.Error = retErr.Error()
		} else {
			hookPayload.Status = "completed"
		}
		c.emitAfterHook(ctx, hookPayload)
	}()

	existingSummary, _ := c.store.GetSessionSummary(ctx, sessionKey)
	summary, recoveryMode, sumErr := c.summarizeWithRecovery(ctx, existingSummary, toArchive)
	if sumErr != nil {
		_ = c.store.FailCompaction(ctx, compactionID, sumErr.Error())
		hookPayload.Stage = "summary_failed"
		retErr = sumErr
		return retErr
	}
	if strings.TrimSpace(summary) == "" {
		summary = fallbackSummary(existingSummary, toArchive)
		recoveryMode = "heuristic_emergency"
	}
	hookPayload.RecoveryMode = recoveryMode
	hookPayload.SummaryLength = len(summary)
	hookPayload.Stage = "summary_ready"

	if err := c.store.CheckpointCompaction(ctx, compactionID, map[string]string{
		"phase":          "summary_ready",
		"summary_length": fmt.Sprintf("%d", len(summary)),
		"recovery_mode":  recoveryMode,
	}); err != nil {
		_ = c.store.FailCompaction(ctx, compactionID, err.Error())
		retErr = err
		return retErr
	}

	if err := c.store.SetSessionSummary(ctx, sessionKey, summary); err != nil {
		_ = c.store.FailCompaction(ctx, compactionID, err.Error())
		retErr = err
		return retErr
	}
	snapshot, snapErr := buildSessionSnapshot(ctx, c.store, sessionKey, userID, agentID, compactionID, summary, toArchive)
	if snapErr != nil {
		_ = c.store.FailCompaction(ctx, compactionID, snapErr.Error())
		retErr = snapErr
		return retErr
	}
	if err := c.store.UpsertSessionSnapshot(ctx, snapshot); err != nil {
		_ = c.store.FailCompaction(ctx, compactionID, err.Error())
		retErr = err
		return retErr
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
		retErr = err
		return retErr
	}
	hookPayload.ArchivedCount = archivedCount
	hookPayload.Stage = "archived"

	if err := c.store.CheckpointCompaction(ctx, compactionID, map[string]string{
		"phase":            "archived",
		"archived_count":   fmt.Sprintf("%d", archivedCount),
		"archive_strategy": archiveStrategy,
		"recovery_mode":    recoveryMode,
	}); err != nil {
		_ = c.store.FailCompaction(ctx, compactionID, err.Error())
		retErr = err
		return retErr
	}

	if err := c.store.CompleteCompaction(ctx, compactionID, summary); err != nil {
		retErr = err
		return retErr
	}
	_ = c.store.AddMetric(ctx, "memory.compaction.archived_events", float64(archivedCount), map[string]string{
		"session_key": sessionKey,
		"mode":        recoveryMode,
	})
	return nil
}

func estimateEventTokens(events []Event) int {
	total := 0
	for _, ev := range events {
		words := len(strings.Fields(ev.Content))
		tokens := words * 4 / 3
		if tokens < 8 {
			tokens = 8
		}
		total += tokens
	}
	return total
}

func (c *SessionCompactor) emitBeforeHook(ctx context.Context, payload CompactionHookPayload) {
	if c == nil || c.cfg.Hooks.Before == nil {
		return
	}
	c.cfg.Hooks.Before(ctx, payload)
}

func (c *SessionCompactor) emitAfterHook(ctx context.Context, payload CompactionHookPayload) {
	if c == nil || c.cfg.Hooks.After == nil {
		return
	}
	c.cfg.Hooks.After(ctx, payload)
}

func (c *SessionCompactor) summarizeWithRecovery(ctx context.Context, existingSummary string, events []Event) (summary string, mode string, err error) {
	fullTranscript := buildCompactionTranscript(events, c.cfg.MaxTranscriptChars, c.cfg.PartialSkipChars, false)
	if strings.TrimSpace(fullTranscript) != "" {
		summary, err = c.summarizeTranscript(ctx, existingSummary, fullTranscript)
		if err == nil && strings.TrimSpace(summary) != "" {
			return strings.TrimSpace(summary), "full", nil
		}
	}

	partialTranscript := buildCompactionTranscript(events, c.cfg.MaxTranscriptChars, c.cfg.PartialSkipChars, true)
	if strings.TrimSpace(partialTranscript) != "" {
		summary, err = c.summarizeTranscript(ctx, existingSummary, partialTranscript)
		if err == nil && strings.TrimSpace(summary) != "" {
			return strings.TrimSpace(summary), "partial_skip_oversized", nil
		}
	}

	// Multi-stage fallback: summarize transcript chunks incrementally.
	if strings.TrimSpace(fullTranscript) != "" {
		summary, err = c.summarizeChunked(ctx, existingSummary, fullTranscript)
		if err == nil && strings.TrimSpace(summary) != "" {
			return strings.TrimSpace(summary), "chunked", nil
		}
	}
	return "", "heuristic_emergency", nil
}

func (c *SessionCompactor) summarizeTranscript(ctx context.Context, existingSummary, transcript string) (string, error) {
	if c == nil || c.summarize == nil {
		return "", nil
	}
	transcript = strings.TrimSpace(transcript)
	if transcript == "" {
		return "", nil
	}
	timeout := c.cfg.SummaryTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	result, err := c.summarize(callCtx, existingSummary, transcript)
	if err != nil {
		return "", err
	}
	if callCtx.Err() != nil {
		return "", callCtx.Err()
	}
	return strings.TrimSpace(result), nil
}

func (c *SessionCompactor) summarizeChunked(ctx context.Context, existingSummary, transcript string) (string, error) {
	chunkChars := c.cfg.ChunkChars
	if chunkChars <= 0 {
		chunkChars = 9000
	}
	chunks := splitTranscriptByChars(transcript, chunkChars)
	if len(chunks) == 0 {
		return "", nil
	}
	running := strings.TrimSpace(existingSummary)
	for _, chunk := range chunks {
		next, err := c.summarizeTranscript(ctx, running, chunk)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(next) != "" {
			running = strings.TrimSpace(next)
		}
	}
	return running, nil
}

func splitTranscriptByChars(transcript string, chunkChars int) []string {
	transcript = strings.TrimSpace(transcript)
	if transcript == "" {
		return nil
	}
	if chunkChars <= 0 || len(transcript) <= chunkChars {
		return []string{transcript}
	}
	lines := strings.Split(transcript, "\n")
	chunks := make([]string, 0, (len(transcript)/chunkChars)+1)
	var current strings.Builder
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if current.Len() > 0 && current.Len()+len(line)+1 > chunkChars {
			chunks = append(chunks, current.String())
			current.Reset()
		}
		if current.Len() > 0 {
			current.WriteString("\n")
		}
		current.WriteString(line)
	}
	if current.Len() > 0 {
		chunks = append(chunks, current.String())
	}
	return chunks
}

func buildCompactionTranscript(events []Event, maxTranscriptChars, partialSkipChars int, skipOversized bool) string {
	var b strings.Builder
	if maxTranscriptChars <= 0 {
		maxTranscriptChars = 48000
	}
	if partialSkipChars <= 0 {
		partialSkipChars = 2600
	}
	for _, ev := range events {
		role := normalizeCompactionRole(ev.Role)
		if role == "" {
			continue
		}
		content := strings.TrimSpace(ev.Content)
		if content == "" {
			continue
		}
		if skipOversized && len(content) > partialSkipChars {
			continue
		}
		if role == "tool" {
			content = stripToolResultDetails(content, partialSkipChars)
		} else if len(content) > 900 {
			content = content[:900] + "... [truncated]"
		}
		line := role + ": " + content + "\n"
		if b.Len()+len(line) > maxTranscriptChars {
			break
		}
		b.WriteString(role)
		b.WriteString(": ")
		b.WriteString(content)
		b.WriteString("\n")
	}
	return b.String()
}

func normalizeCompactionRole(role string) string {
	role = strings.ToLower(strings.TrimSpace(role))
	switch role {
	case "user", "assistant", "tool":
		return role
	default:
		return ""
	}
}

func stripToolResultDetails(content string, maxChars int) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return ""
	}
	if maxChars <= 0 {
		maxChars = 2600
	}

	lower := strings.ToLower(content)
	if strings.Contains(lower, "```") {
		content = strings.ReplaceAll(content, "```", "")
	}
	if isLikelyStructuredPayload(lower) {
		lines := strings.Split(content, "\n")
		head := make([]string, 0, 12)
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			head = append(head, line)
			if len(head) >= 12 {
				break
			}
		}
		content = strings.Join(head, "\n")
		content += "\n[tool payload details stripped for compaction]"
	}
	if len(content) > maxChars {
		content = content[:maxChars] + "... [tool output truncated]"
	}
	return content
}

func isLikelyStructuredPayload(lower string) bool {
	if lower == "" {
		return false
	}
	if strings.Contains(lower, "{") && strings.Contains(lower, "}") {
		return true
	}
	if strings.Contains(lower, "[") && strings.Contains(lower, "]") {
		return true
	}
	return strings.Contains(lower, "\"status\"") ||
		strings.Contains(lower, "\"result\"") ||
		strings.Contains(lower, "\"stdout\"") ||
		strings.Contains(lower, "exit_code")
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

func buildSessionSnapshot(ctx context.Context, store Store, sessionKey, userID, agentID, compactionID, summary string, events []Event) (SessionSnapshot, error) {
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

	items, err := store.ListMemoryCandidates(ctx, userID, agentID, sessionKey, 256)
	if err != nil {
		return SessionSnapshot{}, err
	}
	items = filterItemsByScope(items, sessionKey, userID, true, true, false)
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].LastSeenAtMS > items[j].LastSeenAtMS
	})

	for _, item := range items {
		content := strings.TrimSpace(item.Content)
		if content == "" {
			continue
		}
		switch item.Kind {
		case MemoryUserPreference:
			add(&prefs, content)
		case MemoryTaskState:
			add(&tasks, content)
		case MemorySemanticFact, MemoryProcedural:
			add(&facts, content)
		}
		normalizedContent, _ := normalizeIntentQuery(content)
		if containsAnyIntentPhrase(normalizedContent, []string{"can't", "cannot", "must", "requirement", "constraint"}) {
			add(&constraints, content)
		}
		if strings.Contains(content, "?") && len(openLoops) < 8 {
			add(&openLoops, content)
		}
	}

	// Preserve unresolved question continuity from compacted user turns.
	for _, ev := range events {
		if ev.Role != "user" {
			continue
		}
		content := strings.TrimSpace(ev.Content)
		if content == "" {
			continue
		}
		if strings.Contains(content, "?") && len(openLoops) < 8 {
			add(&openLoops, content)
		}
		normalizedContent, _ := normalizeIntentQuery(content)
		if containsAnyIntentPhrase(normalizedContent, []string{"need to", "todo", "follow up", "pending", "still need", "finish"}) {
			add(&tasks, content)
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
	}, nil
}
