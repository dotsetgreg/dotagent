package memory

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

var (
	personaNameRegex          = regexp.MustCompile(`(?i)\b(?:my name is|call me)\s+([A-Za-z0-9][A-Za-z0-9 _\-]{1,50}?)(?:\s+and\b|[.!?,]|$)`)
	personaTimezoneRegex      = regexp.MustCompile(`(?i)\b(?:my timezone is|i am in timezone)\s+([A-Za-z0-9_/\-+]{2,64})`)
	personaLocationRegex      = regexp.MustCompile(`(?i)\b(?:i live in|i am in|i'm in)\s+([A-Za-z0-9 _\-/]{2,80})`)
	personaLanguageRegex      = regexp.MustCompile(`(?i)\b(?:my preferred language is|respond in|speak in)\s+([A-Za-z]{2,32})`)
	personaCommStyleRegex     = regexp.MustCompile(`(?i)\b(?:be|respond|talk|write)\s+(more\s+)?(concise|detailed|formal|casual|direct|friendly)\b`)
	personaPreferenceRegex    = regexp.MustCompile(`(?i)\b(i (?:really )?(?:like|love|prefer|hate|dislike)\b[^.!?\n]*)`)
	personaGoalRegex          = regexp.MustCompile(`(?i)\b(?:my goal is|i want to|help me)\s+([^.!?\n]{4,140})`)
	personaSessionIntentRegex = regexp.MustCompile(`(?i)\b(?:right now|for this session|today)\b[:\s-]*([^.!?\n]{4,140})`)
	personaForgetRegex        = regexp.MustCompile(`(?i)\b(?:forget|remove|don't remember)\b\s+(.+)$`)
	personaDirectiveRegex     = regexp.MustCompile(`(?i)\b(?:you should|from now on|always)\s+([^.!?\n]{4,200})`)
	personaSensitiveRegex     = regexp.MustCompile(`(?i)(api[_ -]?key|password|secret|token|private key|ssh-rsa|-----BEGIN|sk-[A-Za-z0-9]{12,}|ghp_[A-Za-z0-9]{20,})`)
)

type promptCacheEntry struct {
	prompt      string
	expiresAtMS int64
}

type PersonaManager struct {
	store     Store
	workspace string
	extractor PersonaExtractionFunc

	cacheTTL time.Duration

	mu          sync.RWMutex
	promptCache map[string]promptCacheEntry
	fileHashes  map[string]string
}

func NewPersonaManager(store Store, workspace string, extractor PersonaExtractionFunc) *PersonaManager {
	return &PersonaManager{
		store:       store,
		workspace:   workspace,
		extractor:   extractor,
		cacheTTL:    45 * time.Second,
		promptCache: map[string]promptCacheEntry{},
		fileHashes:  map[string]string{},
	}
}

func (pm *PersonaManager) EmitCandidatesForTurn(ctx context.Context, sessionKey, turnID, userID, agentID string) error {
	turnEvents, err := pm.loadTurnEvents(ctx, sessionKey, turnID)
	if err != nil {
		return err
	}
	if len(turnEvents) == 0 {
		return nil
	}

	profile, err := pm.store.GetPersonaProfile(ctx, userID, agentID)
	if err != nil {
		return err
	}
	if profile.UserID == "" {
		profile = defaultPersonaProfile(userID, agentID)
	}

	heuristics := pm.extractHeuristicCandidates(turnEvents, sessionKey, turnID, userID, agentID)

	llmCandidates := []PersonaUpdateCandidate{}
	if pm.extractor != nil {
		req := PersonaExtractionRequest{
			UserID:          userID,
			AgentID:         agentID,
			SessionKey:      sessionKey,
			TurnID:          turnID,
			Transcript:      buildTurnTranscript(turnEvents),
			ExistingProfile: profile,
		}
		if extracted, extErr := pm.extractor(ctx, req); extErr == nil {
			for _, c := range extracted {
				c.Source = "llm"
				llmCandidates = append(llmCandidates, c)
			}
		}
	}

	candidates := pm.normalizeCandidates(append(heuristics, llmCandidates...), sessionKey, turnID, userID, agentID)
	if len(candidates) == 0 {
		return nil
	}
	if err := pm.store.InsertPersonaCandidates(ctx, candidates); err != nil {
		return err
	}
	_ = pm.store.AddMetric(ctx, "memory.persona.candidates.enqueued", float64(len(candidates)), map[string]string{
		"user_id": userID,
	})
	return nil
}

func (pm *PersonaManager) ApplyPendingForTurn(ctx context.Context, sessionKey, turnID, userID, agentID string) error {
	profile, err := pm.store.GetPersonaProfile(ctx, userID, agentID)
	if err != nil {
		return err
	}
	if profile.UserID == "" {
		profile = defaultPersonaProfile(userID, agentID)
	}

	// Reverse import: manual edits to persona files are merged back into canonical profile.
	merged, changed, impErr := pm.importProfileFromFiles(ctx, profile, sessionKey, turnID)
	if impErr == nil && changed {
		profile = merged
	}

	candidates, err := pm.store.ListPersonaCandidates(ctx, userID, agentID, sessionKey, turnID, personaCandidatePending, 64)
	if err != nil {
		return err
	}
	if len(candidates) == 0 {
		return pm.renderProfileFiles(profile)
	}

	accepted := 0
	rejected := 0
	deferred := 0

	for _, cand := range candidates {
		next, changed, oldValue, newValue := applyCandidate(profile, cand)
		if !changed {
			_ = pm.store.UpdatePersonaCandidateStatus(ctx, cand.ID, personaCandidateRejected, "no_change", "", 0)
			rejected++
			continue
		}

		if reason := pm.rejectReason(cand, oldValue, newValue); reason != "" {
			_ = pm.store.UpdatePersonaCandidateStatus(ctx, cand.ID, personaCandidateRejected, reason, "", 0)
			rejected++
			continue
		}

		hits, _ := pm.store.BumpPersonaSignal(ctx, userID, agentID, cand.FieldPath, hashLower(newValue), time.Now().UnixMilli())
		if required := requiredEvidenceHits(cand.FieldPath); hits < required {
			_ = pm.store.UpdatePersonaCandidateStatus(ctx, cand.ID, personaCandidateDeferred, fmt.Sprintf("insufficient_evidence_%d_of_%d", hits, required), "", 0)
			deferred++
			continue
		}

		next.UpdatedAtMS = time.Now().UnixMilli()
		next.Revision = profile.Revision + 1
		revision := PersonaRevision{
			ID:                "prv-" + uuid.NewString(),
			UserID:            userID,
			AgentID:           agentID,
			SessionKey:        sessionKey,
			TurnID:            turnID,
			CandidateID:       cand.ID,
			FieldPath:         cand.FieldPath,
			Operation:         cand.Operation,
			OldValue:          oldValue,
			NewValue:          newValue,
			Confidence:        cand.Confidence,
			Evidence:          cand.Evidence,
			Reason:            "candidate_applied",
			Source:            cand.Source,
			ProfileBeforeJSON: profileToJSON(profile),
			ProfileAfterJSON:  profileToJSON(next),
			CreatedAtMS:       time.Now().UnixMilli(),
		}

		memoryOps := mapCandidateToMemoryOps(cand)
		if err := pm.store.ApplyPersonaMutation(ctx, next, cand, revision, memoryOps); err != nil {
			reason := "apply_failed"
			if msg := strings.TrimSpace(err.Error()); msg != "" {
				reason = truncateForMetadata("apply_failed:"+msg, 180)
			}
			_ = pm.store.UpdatePersonaCandidateStatus(ctx, cand.ID, personaCandidateRejected, reason, "", 0)
			rejected++
			continue
		}

		profile = next
		accepted++
		pm.invalidatePromptCache(userID, agentID)
	}

	_ = pm.store.AddMetric(ctx, "memory.persona.candidates.accepted", float64(accepted), map[string]string{"user_id": userID})
	_ = pm.store.AddMetric(ctx, "memory.persona.candidates.rejected", float64(rejected), map[string]string{"user_id": userID})
	_ = pm.store.AddMetric(ctx, "memory.persona.candidates.deferred", float64(deferred), map[string]string{"user_id": userID})

	return pm.renderProfileFiles(profile)
}

func (pm *PersonaManager) BuildPrompt(ctx context.Context, userID, agentID, sessionIntent string, budgetTokens int) (string, error) {
	profile, err := pm.store.GetPersonaProfile(ctx, userID, agentID)
	if err != nil {
		return "", err
	}
	if profile.UserID == "" {
		profile = defaultPersonaProfile(userID, agentID)
	}
	merged, changed, _ := pm.importProfileFromFiles(ctx, profile, "", "")
	if changed {
		profile = merged
	}

	intent := detectQueryIntent(sessionIntent)
	cacheKey := fmt.Sprintf("%s|%s|%d|%s|%d", userID, agentID, profile.Revision, intent, budgetTokens)
	now := time.Now().UnixMilli()

	pm.mu.RLock()
	if cached, ok := pm.promptCache[cacheKey]; ok && cached.expiresAtMS > now {
		pm.mu.RUnlock()
		_ = pm.store.AddMetric(ctx, "memory.persona.prompt_cache_hit", 1, map[string]string{"user_id": userID})
		return cached.prompt, nil
	}
	pm.mu.RUnlock()

	lines := []string{
		"## Active Persona",
		"",
		"### Core Identity",
		fmt.Sprintf("- Agent name: %s", nonEmpty(profile.Identity.AgentName, "DotAgent")),
		fmt.Sprintf("- Role: %s", nonEmpty(profile.Identity.Role, "Personal AI assistant")),
		fmt.Sprintf("- Purpose: %s", nonEmpty(profile.Identity.Purpose, "Deliver practical, concise, reliable help.")),
	}
	if len(profile.Identity.Goals) > 0 {
		lines = append(lines, "- Goals:")
		for _, g := range dedupeNonEmpty(profile.Identity.Goals) {
			lines = append(lines, "  - "+g)
		}
	}
	if len(profile.Identity.Boundaries) > 0 {
		lines = append(lines, "- Boundaries:")
		for _, b := range dedupeNonEmpty(profile.Identity.Boundaries) {
			lines = append(lines, "  - "+b)
		}
	}

	lines = append(lines, "",
		"### Soul and Communication Style",
		fmt.Sprintf("- Voice: %s", nonEmpty(profile.Soul.Voice, "Grounded and direct")),
		fmt.Sprintf("- Communication style: %s", nonEmpty(profile.Soul.Communication, "Concise by default")),
	)
	if len(profile.Soul.Values) > 0 {
		lines = append(lines, "- Values:")
		for _, v := range dedupeNonEmpty(profile.Soul.Values) {
			lines = append(lines, "  - "+v)
		}
	}

	lines = append(lines, "",
		"### User Profile and Preferences",
		fmt.Sprintf("- User name: %s", nonEmpty(profile.User.Name, "(unknown)")),
		fmt.Sprintf("- Timezone: %s", nonEmpty(profile.User.Timezone, "(unknown)")),
		fmt.Sprintf("- Location: %s", nonEmpty(profile.User.Location, "(unknown)")),
		fmt.Sprintf("- Language: %s", nonEmpty(profile.User.Language, "(unknown)")),
		fmt.Sprintf("- Preferred communication style: %s", nonEmpty(profile.User.CommunicationStyle, "(unspecified)")),
	)
	if len(profile.User.Goals) > 0 {
		lines = append(lines, "- User goals:")
		for _, g := range dedupeNonEmpty(profile.User.Goals) {
			lines = append(lines, "  - "+g)
		}
	}
	if len(profile.User.Preferences) > 0 {
		keys := make([]string, 0, len(profile.User.Preferences))
		for k := range profile.User.Preferences {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		lines = append(lines, "- Preferences:")
		for _, k := range keys {
			lines = append(lines, fmt.Sprintf("  - %s: %s", k, profile.User.Preferences[k]))
		}
	}

	finalIntent := strings.TrimSpace(sessionIntent)
	if finalIntent == "" {
		finalIntent = profile.User.SessionIntent
	}
	if finalIntent != "" {
		lines = append(lines, "",
			"### Current Session Intent",
			"- "+finalIntent,
		)
	}

	maxTokens := budgetTokens
	if maxTokens <= 0 {
		maxTokens = 480
	}
	trimmed := []string{}
	used := 0
	for _, line := range lines {
		t := estimateMessageTokens(line)
		if used+t > maxTokens && used > 0 {
			break
		}
		trimmed = append(trimmed, line)
		used += t
	}
	prompt := strings.TrimSpace(strings.Join(trimmed, "\n"))

	pm.mu.Lock()
	pm.promptCache[cacheKey] = promptCacheEntry{
		prompt:      prompt,
		expiresAtMS: now + int64(pm.cacheTTL/time.Millisecond),
	}
	pm.mu.Unlock()
	_ = pm.store.AddMetric(ctx, "memory.persona.prompt_cache_miss", 1, map[string]string{"user_id": userID})

	return prompt, nil
}

func (pm *PersonaManager) RollbackLastRevision(ctx context.Context, userID, agentID string) error {
	revs, err := pm.store.ListPersonaRevisions(ctx, userID, agentID, 1)
	if err != nil {
		return err
	}
	if len(revs) == 0 {
		return nil
	}
	profile, err := pm.store.RollbackPersonaToRevision(ctx, userID, agentID, revs[0].ID)
	if err != nil {
		return err
	}
	pm.invalidatePromptCache(userID, agentID)
	return pm.renderProfileFiles(profile)
}

func (pm *PersonaManager) loadTurnEvents(ctx context.Context, sessionKey, turnID string) ([]Event, error) {
	events, err := pm.store.ListRecentEvents(ctx, sessionKey, 128, false)
	if err != nil {
		return nil, err
	}
	out := make([]Event, 0, 8)
	for _, ev := range events {
		if ev.TurnID == turnID {
			out = append(out, ev)
		}
	}
	return out, nil
}

func buildTurnTranscript(events []Event) string {
	var b strings.Builder
	for _, ev := range events {
		if ev.Role != "user" && ev.Role != "assistant" {
			continue
		}
		line := strings.TrimSpace(ev.Content)
		if line == "" {
			continue
		}
		if len(line) > 350 {
			line = line[:350] + "..."
		}
		b.WriteString(ev.Role)
		b.WriteString(": ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

func (pm *PersonaManager) extractHeuristicCandidates(events []Event, sessionKey, turnID, userID, agentID string) []PersonaUpdateCandidate {
	out := []PersonaUpdateCandidate{}
	for _, ev := range events {
		if ev.Role != "user" {
			continue
		}
		content := strings.TrimSpace(ev.Content)
		if content == "" {
			continue
		}

		for _, m := range personaNameRegex.FindAllStringSubmatch(content, -1) {
			if len(m) < 2 {
				continue
			}
			out = append(out, newCandidate(sessionKey, turnID, userID, agentID, ev.ID, "user.name", "set", m[1], 0.86, content, "heuristic"))
		}
		for _, m := range personaTimezoneRegex.FindAllStringSubmatch(content, -1) {
			if len(m) < 2 {
				continue
			}
			out = append(out, newCandidate(sessionKey, turnID, userID, agentID, ev.ID, "user.timezone", "set", m[1], 0.82, content, "heuristic"))
		}
		for _, m := range personaLocationRegex.FindAllStringSubmatch(content, -1) {
			if len(m) < 2 {
				continue
			}
			out = append(out, newCandidate(sessionKey, turnID, userID, agentID, ev.ID, "user.location", "set", m[1], 0.74, content, "heuristic"))
		}
		for _, m := range personaLanguageRegex.FindAllStringSubmatch(content, -1) {
			if len(m) < 2 {
				continue
			}
			out = append(out, newCandidate(sessionKey, turnID, userID, agentID, ev.ID, "user.language", "set", strings.ToLower(m[1]), 0.72, content, "heuristic"))
		}
		for _, m := range personaCommStyleRegex.FindAllStringSubmatch(content, -1) {
			if len(m) < 3 {
				continue
			}
			style := strings.ToLower(strings.TrimSpace(m[2]))
			out = append(out, newCandidate(sessionKey, turnID, userID, agentID, ev.ID, "user.communication_style", "set", style, 0.71, content, "heuristic"))
		}
		for _, m := range personaPreferenceRegex.FindAllStringSubmatch(content, -1) {
			if len(m) < 2 {
				continue
			}
			pref := strings.TrimSpace(m[1])
			key := "pref_" + shortStableSlug(pref)
			out = append(out, newCandidate(sessionKey, turnID, userID, agentID, ev.ID, "user.preferences."+key, "set", pref, 0.68, content, "heuristic"))
		}
		for _, m := range personaGoalRegex.FindAllStringSubmatch(content, -1) {
			if len(m) < 2 {
				continue
			}
			out = append(out, newCandidate(sessionKey, turnID, userID, agentID, ev.ID, "user.goals", "append", strings.TrimSpace(m[1]), 0.66, content, "heuristic"))
		}
		for _, m := range personaSessionIntentRegex.FindAllStringSubmatch(content, -1) {
			if len(m) < 2 {
				continue
			}
			out = append(out, newCandidate(sessionKey, turnID, userID, agentID, ev.ID, "user.session_intent", "set", strings.TrimSpace(m[1]), 0.69, content, "heuristic"))
		}
		for _, m := range personaDirectiveRegex.FindAllStringSubmatch(content, -1) {
			if len(m) < 2 {
				continue
			}
			out = append(out, newCandidate(sessionKey, turnID, userID, agentID, ev.ID, "soul.behavioral_rules", "append", strings.TrimSpace(m[1]), 0.64, content, "heuristic"))
		}
		for _, m := range personaForgetRegex.FindAllStringSubmatch(content, -1) {
			if len(m) < 2 {
				continue
			}
			target := strings.ToLower(strings.TrimSpace(m[1]))
			switch {
			case strings.Contains(target, "name"):
				out = append(out, newCandidate(sessionKey, turnID, userID, agentID, ev.ID, "user.name", "delete", "", 0.88, content, "heuristic"))
			case strings.Contains(target, "timezone"):
				out = append(out, newCandidate(sessionKey, turnID, userID, agentID, ev.ID, "user.timezone", "delete", "", 0.88, content, "heuristic"))
			case strings.Contains(target, "location"):
				out = append(out, newCandidate(sessionKey, turnID, userID, agentID, ev.ID, "user.location", "delete", "", 0.88, content, "heuristic"))
			case strings.Contains(target, "preference"):
				out = append(out, newCandidate(sessionKey, turnID, userID, agentID, ev.ID, "user.preferences", "delete", "", 0.75, content, "heuristic"))
			}
		}
	}
	return out
}

func newCandidate(sessionKey, turnID, userID, agentID, sourceEventID, fieldPath, op, value string, confidence float64, evidence, source string) PersonaUpdateCandidate {
	return PersonaUpdateCandidate{
		ID:            "pcd-" + uuid.NewString(),
		UserID:        userID,
		AgentID:       agentID,
		SessionKey:    sessionKey,
		TurnID:        turnID,
		SourceEventID: sourceEventID,
		FieldPath:     strings.ToLower(strings.TrimSpace(fieldPath)),
		Operation:     strings.ToLower(strings.TrimSpace(op)),
		Value:         strings.TrimSpace(value),
		Confidence:    confidence,
		Evidence:      strings.TrimSpace(evidence),
		Source:        strings.TrimSpace(source),
		Status:        personaCandidatePending,
		CreatedAtMS:   time.Now().UnixMilli(),
	}
}

func (pm *PersonaManager) normalizeCandidates(in []PersonaUpdateCandidate, sessionKey, turnID, userID, agentID string) []PersonaUpdateCandidate {
	out := make([]PersonaUpdateCandidate, 0, len(in))
	seen := map[string]struct{}{}

	for _, c := range in {
		c.FieldPath = strings.ToLower(strings.TrimSpace(c.FieldPath))
		if c.FieldPath == "" || !isAllowedPersonaPath(c.FieldPath) {
			continue
		}
		c.Operation = strings.ToLower(strings.TrimSpace(c.Operation))
		if c.Operation == "" {
			c.Operation = "set"
		}
		if c.Operation != "set" && c.Operation != "append" && c.Operation != "delete" {
			continue
		}
		if c.Confidence <= 0 {
			c.Confidence = 0.6
		}
		if c.Confidence > 1 {
			c.Confidence = 1
		}
		if c.ID == "" {
			c.ID = "pcd-" + uuid.NewString()
		}
		if c.UserID == "" {
			c.UserID = userID
		}
		if c.AgentID == "" {
			c.AgentID = agentID
		}
		if c.SessionKey == "" {
			c.SessionKey = sessionKey
		}
		if c.TurnID == "" {
			c.TurnID = turnID
		}
		if c.Source == "" {
			c.Source = "heuristic"
		}
		if c.Status == "" {
			c.Status = personaCandidatePending
		}
		if c.CreatedAtMS == 0 {
			c.CreatedAtMS = time.Now().UnixMilli()
		}
		c.Value = strings.TrimSpace(c.Value)
		key := strings.ToLower(c.FieldPath + "|" + c.Operation + "|" + c.Value)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, c)
	}

	return out
}

func applyCandidate(profile PersonaProfile, cand PersonaUpdateCandidate) (PersonaProfile, bool, string, string) {
	next := profile.clone()
	oldValue := readField(profile, cand.FieldPath)
	switch cand.Operation {
	case "delete":
		deleteField(&next, cand.FieldPath)
	case "append":
		appendField(&next, cand.FieldPath, cand.Value)
	default:
		setField(&next, cand.FieldPath, cand.Value)
	}
	newValue := readField(next, cand.FieldPath)
	return next, strings.TrimSpace(oldValue) != strings.TrimSpace(newValue), oldValue, newValue
}

func (pm *PersonaManager) rejectReason(cand PersonaUpdateCandidate, oldValue, newValue string) string {
	if cand.Confidence < 0.52 {
		return "low_confidence"
	}
	if personaSensitiveRegex.MatchString(cand.Value) || personaSensitiveRegex.MatchString(cand.Evidence) {
		return "sensitive_data"
	}
	if isStableField(cand.FieldPath) && strings.TrimSpace(oldValue) != "" && strings.TrimSpace(oldValue) != strings.TrimSpace(newValue) {
		if cand.Confidence < 0.86 && !isExplicitCorrection(cand.Evidence) && !isExplicitStableSetter(cand.FieldPath, cand.Evidence) {
			return "stable_field_contradiction"
		}
	}
	if strings.TrimSpace(newValue) == "" && cand.Operation != "delete" {
		return "empty_value"
	}
	return ""
}

func requiredEvidenceHits(fieldPath string) int {
	if isUnstableField(fieldPath) {
		return 2
	}
	return 1
}

func isUnstableField(fieldPath string) bool {
	return strings.HasPrefix(fieldPath, "user.preferences.") || fieldPath == "user.communication_style" || fieldPath == "user.session_intent"
}

func isStableField(fieldPath string) bool {
	switch fieldPath {
	case "user.name", "user.timezone", "user.location", "identity.agent_name":
		return true
	default:
		return false
	}
}

func isExplicitCorrection(evidence string) bool {
	ev := strings.ToLower(evidence)
	return strings.Contains(ev, "actually") ||
		strings.Contains(ev, "correction") ||
		strings.Contains(ev, "instead") ||
		strings.Contains(ev, "don't call me") ||
		strings.Contains(ev, "call me")
}

func isExplicitStableSetter(fieldPath, evidence string) bool {
	ev := strings.ToLower(evidence)
	switch fieldPath {
	case "user.name":
		return strings.Contains(ev, "my name is") || strings.Contains(ev, "call me")
	case "user.timezone":
		return strings.Contains(ev, "my timezone is") || strings.Contains(ev, "i am in timezone")
	case "user.location":
		return strings.Contains(ev, "i live in") || strings.Contains(ev, "i am in") || strings.Contains(ev, "i'm in")
	default:
		return false
	}
}

func isAllowedPersonaPath(path string) bool {
	if path == "" {
		return false
	}
	allowedExact := map[string]struct{}{
		"identity.agent_name":      {},
		"identity.role":            {},
		"identity.purpose":         {},
		"identity.goals":           {},
		"identity.boundaries":      {},
		"soul.voice":               {},
		"soul.communication_style": {},
		"soul.values":              {},
		"soul.behavioral_rules":    {},
		"user.name":                {},
		"user.timezone":            {},
		"user.location":            {},
		"user.language":            {},
		"user.communication_style": {},
		"user.goals":               {},
		"user.session_intent":      {},
		"user.preferences":         {},
	}
	if _, ok := allowedExact[path]; ok {
		return true
	}
	return strings.HasPrefix(path, "user.preferences.")
}

func setField(profile *PersonaProfile, path, value string) {
	value = strings.TrimSpace(value)
	switch {
	case path == "identity.agent_name":
		profile.Identity.AgentName = value
	case path == "identity.role":
		profile.Identity.Role = value
	case path == "identity.purpose":
		profile.Identity.Purpose = value
	case path == "soul.voice":
		profile.Soul.Voice = value
	case path == "soul.communication_style":
		profile.Soul.Communication = value
	case path == "user.name":
		profile.User.Name = value
	case path == "user.timezone":
		profile.User.Timezone = value
	case path == "user.location":
		profile.User.Location = value
	case path == "user.language":
		profile.User.Language = value
	case path == "user.communication_style":
		profile.User.CommunicationStyle = value
	case path == "user.session_intent":
		profile.User.SessionIntent = value
	case strings.HasPrefix(path, "user.preferences."):
		if profile.User.Preferences == nil {
			profile.User.Preferences = map[string]string{}
		}
		key := strings.TrimPrefix(path, "user.preferences.")
		if value == "" {
			delete(profile.User.Preferences, key)
		} else {
			profile.User.Preferences[key] = value
		}
	}
}

func appendField(profile *PersonaProfile, path, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	switch path {
	case "identity.goals":
		profile.Identity.Goals = append(profile.Identity.Goals, value)
		profile.Identity.Goals = dedupeNonEmpty(profile.Identity.Goals)
	case "identity.boundaries":
		profile.Identity.Boundaries = append(profile.Identity.Boundaries, value)
		profile.Identity.Boundaries = dedupeNonEmpty(profile.Identity.Boundaries)
	case "soul.values":
		profile.Soul.Values = append(profile.Soul.Values, value)
		profile.Soul.Values = dedupeNonEmpty(profile.Soul.Values)
	case "soul.behavioral_rules":
		profile.Soul.BehavioralRules = append(profile.Soul.BehavioralRules, value)
		profile.Soul.BehavioralRules = dedupeNonEmpty(profile.Soul.BehavioralRules)
	case "user.goals":
		profile.User.Goals = append(profile.User.Goals, value)
		profile.User.Goals = dedupeNonEmpty(profile.User.Goals)
	default:
		setField(profile, path, value)
	}
}

func deleteField(profile *PersonaProfile, path string) {
	switch {
	case path == "user.name":
		profile.User.Name = ""
	case path == "user.timezone":
		profile.User.Timezone = ""
	case path == "user.location":
		profile.User.Location = ""
	case path == "user.language":
		profile.User.Language = ""
	case path == "user.communication_style":
		profile.User.CommunicationStyle = ""
	case path == "user.session_intent":
		profile.User.SessionIntent = ""
	case path == "user.preferences":
		profile.User.Preferences = map[string]string{}
	case strings.HasPrefix(path, "user.preferences."):
		key := strings.TrimPrefix(path, "user.preferences.")
		delete(profile.User.Preferences, key)
	case path == "identity.goals":
		profile.Identity.Goals = nil
	case path == "identity.boundaries":
		profile.Identity.Boundaries = nil
	case path == "soul.values":
		profile.Soul.Values = nil
	case path == "soul.behavioral_rules":
		profile.Soul.BehavioralRules = nil
	}
}

func readField(profile PersonaProfile, path string) string {
	switch {
	case path == "identity.agent_name":
		return profile.Identity.AgentName
	case path == "identity.role":
		return profile.Identity.Role
	case path == "identity.purpose":
		return profile.Identity.Purpose
	case path == "identity.goals":
		return strings.Join(profile.Identity.Goals, " | ")
	case path == "identity.boundaries":
		return strings.Join(profile.Identity.Boundaries, " | ")
	case path == "soul.voice":
		return profile.Soul.Voice
	case path == "soul.communication_style":
		return profile.Soul.Communication
	case path == "soul.values":
		return strings.Join(profile.Soul.Values, " | ")
	case path == "soul.behavioral_rules":
		return strings.Join(profile.Soul.BehavioralRules, " | ")
	case path == "user.name":
		return profile.User.Name
	case path == "user.timezone":
		return profile.User.Timezone
	case path == "user.location":
		return profile.User.Location
	case path == "user.language":
		return profile.User.Language
	case path == "user.communication_style":
		return profile.User.CommunicationStyle
	case path == "user.goals":
		return strings.Join(profile.User.Goals, " | ")
	case path == "user.session_intent":
		return profile.User.SessionIntent
	case path == "user.preferences":
		if len(profile.User.Preferences) == 0 {
			return ""
		}
		keys := make([]string, 0, len(profile.User.Preferences))
		for k := range profile.User.Preferences {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+"="+profile.User.Preferences[k])
		}
		return strings.Join(parts, " | ")
	case strings.HasPrefix(path, "user.preferences."):
		return profile.User.Preferences[strings.TrimPrefix(path, "user.preferences.")]
	default:
		return ""
	}
}

func mapCandidateToMemoryOps(cand PersonaUpdateCandidate) []ConsolidationOp {
	if cand.FieldPath == "" {
		return nil
	}
	baseKey := "persona/" + cand.FieldPath
	if cand.Operation == "delete" {
		return []ConsolidationOp{
			{
				Action:     "delete",
				Kind:       MemorySemanticFact,
				Key:        baseKey,
				Confidence: 1.0,
				Content:    "",
			},
		}
	}
	kind := MemorySemanticFact
	if strings.HasPrefix(cand.FieldPath, "user.preferences.") || cand.FieldPath == "user.communication_style" {
		kind = MemoryUserPreference
	}
	content := fmt.Sprintf("Persona %s: %s", cand.FieldPath, cand.Value)
	key := fmt.Sprintf("%s/%s", baseKey, hashLower(cand.Value)[:12])
	return []ConsolidationOp{
		{
			Action:     "upsert",
			Kind:       kind,
			Key:        key,
			Content:    content,
			Confidence: cand.Confidence,
			Metadata: map[string]string{
				"source":   "persona",
				"field":    cand.FieldPath,
				"op":       cand.Operation,
				"evidence": truncateForMetadata(cand.Evidence, 120),
			},
		},
	}
}

func truncateForMetadata(in string, max int) string {
	in = strings.TrimSpace(in)
	if max <= 0 || len(in) <= max {
		return in
	}
	return in[:max] + "..."
}

func (pm *PersonaManager) invalidatePromptCache(userID, agentID string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	for key := range pm.promptCache {
		if strings.HasPrefix(key, userID+"|"+agentID+"|") {
			delete(pm.promptCache, key)
		}
	}
}

func detectQueryIntent(query string) string {
	q := strings.ToLower(strings.TrimSpace(query))
	switch {
	case strings.Contains(q, "prefer") || strings.Contains(q, "like") || strings.Contains(q, "favorite"):
		return "preference"
	case strings.Contains(q, "name") || strings.Contains(q, "who am i") || strings.Contains(q, "timezone") || strings.Contains(q, "profile"):
		return "identity"
	case strings.Contains(q, "style") || strings.Contains(q, "tone") || strings.Contains(q, "respond"):
		return "style"
	case strings.Contains(q, "task") || strings.Contains(q, "todo") || strings.Contains(q, "deadline") || strings.Contains(q, "remind"):
		return "task"
	default:
		return "general"
	}
}

func shortStableSlug(in string) string {
	in = strings.ToLower(strings.TrimSpace(in))
	in = regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(in, "_")
	in = strings.Trim(in, "_")
	if len(in) > 24 {
		in = in[:24]
	}
	if in == "" {
		return hashLower(in)[:10]
	}
	return in
}

func hashLower(in string) string {
	sum := sha1.Sum([]byte(strings.ToLower(strings.TrimSpace(in))))
	return hex.EncodeToString(sum[:])
}

func nonEmpty(v, fallback string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return fallback
	}
	return v
}

func dedupeNonEmpty(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		k := strings.ToLower(v)
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, v)
	}
	return out
}

func (pm *PersonaManager) importProfileFromFiles(ctx context.Context, profile PersonaProfile, sessionKey, turnID string) (PersonaProfile, bool, error) {
	identityPath := filepath.Join(pm.workspace, "IDENTITY.md")
	soulPath := filepath.Join(pm.workspace, "SOUL.md")
	userPath := filepath.Join(pm.workspace, "USER.md")

	identityRaw, _ := os.ReadFile(identityPath)
	soulRaw, _ := os.ReadFile(soulPath)
	userRaw, _ := os.ReadFile(userPath)

	hash := hashLower(string(identityRaw) + "|" + string(soulRaw) + "|" + string(userRaw))
	key := profile.UserID + "|" + profile.AgentID

	pm.mu.RLock()
	prevHash := pm.fileHashes[key]
	pm.mu.RUnlock()
	if prevHash == hash {
		return profile, false, nil
	}

	updated := profile.clone()
	changed := false

	if mergeIdentityMarkdown(&updated, string(identityRaw)) {
		changed = true
	}
	if mergeSoulMarkdown(&updated, string(soulRaw)) {
		changed = true
	}
	if mergeUserMarkdown(&updated, string(userRaw)) {
		changed = true
	}

	pm.mu.Lock()
	pm.fileHashes[key] = hash
	pm.mu.Unlock()

	if !changed {
		return profile, false, nil
	}
	updated.Revision = profile.Revision + 1
	updated.UpdatedAtMS = time.Now().UnixMilli()
	rev := PersonaRevision{
		ID:                "prv-" + uuid.NewString(),
		UserID:            profile.UserID,
		AgentID:           profile.AgentID,
		SessionKey:        sessionKey,
		TurnID:            turnID,
		CandidateID:       "",
		FieldPath:         "persona.files",
		Operation:         "merge",
		OldValue:          "(file import)",
		NewValue:          "(file import)",
		Confidence:        1.0,
		Evidence:          "workspace markdown sync",
		Reason:            "file_import",
		Source:            "file_import",
		ProfileBeforeJSON: profileToJSON(profile),
		ProfileAfterJSON:  profileToJSON(updated),
		CreatedAtMS:       time.Now().UnixMilli(),
	}
	if err := pm.store.UpsertPersonaProfile(ctx, updated); err != nil {
		return profile, false, err
	}
	if err := pm.store.InsertPersonaRevision(ctx, rev); err != nil {
		return profile, false, err
	}
	pm.invalidatePromptCache(profile.UserID, profile.AgentID)
	return updated, true, nil
}

func (pm *PersonaManager) renderProfileFiles(profile PersonaProfile) error {
	if strings.TrimSpace(pm.workspace) == "" {
		return nil
	}
	if err := os.MkdirAll(pm.workspace, 0o755); err != nil {
		return err
	}
	files := map[string]string{
		"IDENTITY.md": renderIdentityMarkdown(profile),
		"SOUL.md":     renderSoulMarkdown(profile),
		"USER.md":     renderUserMarkdown(profile),
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(pm.workspace, name), []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func renderIdentityMarkdown(profile PersonaProfile) string {
	lines := []string{
		"# Identity",
		"",
		"## Name",
		nonEmpty(profile.Identity.AgentName, "DotAgent"),
		"",
		"## Role",
		nonEmpty(profile.Identity.Role, "Personal AI assistant"),
		"",
		"## Purpose",
		nonEmpty(profile.Identity.Purpose, "Deliver practical, concise, reliable help."),
		"",
		"## Goals",
	}
	goals := dedupeNonEmpty(profile.Identity.Goals)
	if len(goals) == 0 {
		lines = append(lines, "- Keep responses actionable and accurate")
	} else {
		for _, g := range goals {
			lines = append(lines, "- "+g)
		}
	}
	lines = append(lines, "", "## Boundaries")
	boundaries := dedupeNonEmpty(profile.Identity.Boundaries)
	if len(boundaries) == 0 {
		lines = append(lines, "- Never fabricate actions")
	} else {
		for _, b := range boundaries {
			lines = append(lines, "- "+b)
		}
	}
	return strings.Join(lines, "\n")
}

func renderSoulMarkdown(profile PersonaProfile) string {
	lines := []string{
		"# Soul",
		"",
		"## Voice",
		nonEmpty(profile.Soul.Voice, "Grounded, direct, and helpful"),
		"",
		"## Communication Style",
		nonEmpty(profile.Soul.Communication, "Concise by default; detail on request"),
		"",
		"## Values",
	}
	values := dedupeNonEmpty(profile.Soul.Values)
	if len(values) == 0 {
		lines = append(lines, "- Accuracy", "- Clarity", "- User control")
	} else {
		for _, v := range values {
			lines = append(lines, "- "+v)
		}
	}
	lines = append(lines, "", "## Behavioral Rules")
	rules := dedupeNonEmpty(profile.Soul.BehavioralRules)
	if len(rules) == 0 {
		lines = append(lines, "- State assumptions explicitly")
	} else {
		for _, r := range rules {
			lines = append(lines, "- "+r)
		}
	}
	return strings.Join(lines, "\n")
}

func renderUserMarkdown(profile PersonaProfile) string {
	lines := []string{
		"# User",
		"",
		"## Name",
		nonEmpty(profile.User.Name, "(optional)"),
		"",
		"## Timezone",
		nonEmpty(profile.User.Timezone, "(optional)"),
		"",
		"## Location",
		nonEmpty(profile.User.Location, "(optional)"),
		"",
		"## Language",
		nonEmpty(profile.User.Language, "(optional)"),
		"",
		"## Communication Style",
		nonEmpty(profile.User.CommunicationStyle, "(unspecified)"),
		"",
		"## Goals",
	}
	userGoals := dedupeNonEmpty(profile.User.Goals)
	if len(userGoals) == 0 {
		lines = append(lines, "- (none yet)")
	} else {
		for _, g := range userGoals {
			lines = append(lines, "- "+g)
		}
	}
	lines = append(lines, "", "## Preferences")
	if len(profile.User.Preferences) == 0 {
		lines = append(lines, "- (none yet)")
	} else {
		keys := make([]string, 0, len(profile.User.Preferences))
		for k := range profile.User.Preferences {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			lines = append(lines, fmt.Sprintf("- %s: %s", k, profile.User.Preferences[k]))
		}
	}
	if strings.TrimSpace(profile.User.SessionIntent) != "" {
		lines = append(lines, "", "## Current Session Intent", profile.User.SessionIntent)
	}
	return strings.Join(lines, "\n")
}

func parseMarkdownSections(raw string) (map[string]string, map[string][]string) {
	values := map[string]string{}
	lists := map[string][]string{}
	section := ""
	lines := strings.Split(raw, "\n")
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		if strings.HasPrefix(t, "#") {
			section = strings.ToLower(strings.TrimSpace(strings.TrimLeft(t, "#")))
			section = strings.ReplaceAll(section, " ", "_")
			continue
		}
		if strings.HasPrefix(t, "- ") {
			item := strings.TrimSpace(strings.TrimPrefix(t, "- "))
			if idx := strings.Index(item, ":"); idx > 0 {
				k := strings.ToLower(strings.TrimSpace(item[:idx]))
				k = strings.ReplaceAll(k, " ", "_")
				values[k] = strings.TrimSpace(item[idx+1:])
			} else if section != "" {
				lists[section] = append(lists[section], item)
			}
			continue
		}
		if section != "" {
			values[section] = t
		}
	}
	return values, lists
}

func mergeIdentityMarkdown(profile *PersonaProfile, raw string) bool {
	values, lists := parseMarkdownSections(raw)
	changed := false
	applyScalar := func(dst *string, key string) {
		if v := strings.TrimSpace(values[key]); v != "" && !isTemplatePlaceholder(v) && *dst != v {
			*dst = v
			changed = true
		}
	}
	applyScalar(&profile.Identity.AgentName, "name")
	applyScalar(&profile.Identity.Role, "role")
	if v := strings.TrimSpace(values["description"]); v != "" && !isTemplatePlaceholder(v) && profile.Identity.Purpose != v {
		profile.Identity.Purpose = v
		changed = true
	}
	applyScalar(&profile.Identity.Purpose, "purpose")

	if items := dedupeNonEmpty(append(lists["goals"], lists["purpose"]...)); len(items) > 0 {
		if strings.Join(profile.Identity.Goals, "|") != strings.Join(items, "|") {
			profile.Identity.Goals = items
			changed = true
		}
	}
	if items := dedupeNonEmpty(lists["boundaries"]); len(items) > 0 {
		if strings.Join(profile.Identity.Boundaries, "|") != strings.Join(items, "|") {
			profile.Identity.Boundaries = items
			changed = true
		}
	}
	return changed
}

func mergeSoulMarkdown(profile *PersonaProfile, raw string) bool {
	values, lists := parseMarkdownSections(raw)
	changed := false
	if v := strings.TrimSpace(values["voice"]); v != "" && !isTemplatePlaceholder(v) && profile.Soul.Voice != v {
		profile.Soul.Voice = v
		changed = true
	}
	if v := strings.TrimSpace(values["communication_style"]); v != "" && !isTemplatePlaceholder(v) && profile.Soul.Communication != v {
		profile.Soul.Communication = v
		changed = true
	}
	if items := dedupeNonEmpty(lists["values"]); len(items) > 0 {
		if strings.Join(profile.Soul.Values, "|") != strings.Join(items, "|") {
			profile.Soul.Values = items
			changed = true
		}
	}
	if items := dedupeNonEmpty(append(lists["behavioral_rules"], lists["personality"]...)); len(items) > 0 {
		if strings.Join(profile.Soul.BehavioralRules, "|") != strings.Join(items, "|") {
			profile.Soul.BehavioralRules = items
			changed = true
		}
	}
	return changed
}

func mergeUserMarkdown(profile *PersonaProfile, raw string) bool {
	values, lists := parseMarkdownSections(raw)
	changed := false
	setIf := func(dst *string, key string) {
		if v := strings.TrimSpace(values[key]); v != "" && !isTemplatePlaceholder(v) && *dst != v {
			*dst = v
			changed = true
		}
	}
	setIf(&profile.User.Name, "name")
	setIf(&profile.User.Timezone, "timezone")
	setIf(&profile.User.Location, "location")
	setIf(&profile.User.Language, "language")
	setIf(&profile.User.CommunicationStyle, "communication_style")
	setIf(&profile.User.SessionIntent, "current_session_intent")

	if items := dedupeNonEmpty(append(lists["goals"], lists["learning_goals"]...)); len(items) > 0 {
		if strings.Join(profile.User.Goals, "|") != strings.Join(items, "|") {
			profile.User.Goals = items
			changed = true
		}
	}

	if prefLines := lists["preferences"]; len(prefLines) > 0 {
		nextPrefs := map[string]string{}
		for _, line := range prefLines {
			if idx := strings.Index(line, ":"); idx > 0 {
				k := strings.ToLower(strings.TrimSpace(line[:idx]))
				v := strings.TrimSpace(line[idx+1:])
				if k != "" && v != "" && !isTemplatePlaceholder(v) {
					nextPrefs[k] = v
				}
			}
		}
		if len(nextPrefs) > 0 {
			if profile.User.Preferences == nil {
				profile.User.Preferences = map[string]string{}
			}
			for k, v := range nextPrefs {
				if profile.User.Preferences[k] != v {
					profile.User.Preferences[k] = v
					changed = true
				}
			}
		}
	}
	return changed
}

func isTemplatePlaceholder(v string) bool {
	v = strings.TrimSpace(strings.ToLower(v))
	return v == "" || strings.Contains(v, "(optional)") || strings.Contains(v, "(none yet)") || strings.Contains(v, "(unspecified)") || strings.Contains(v, "(your")
}
