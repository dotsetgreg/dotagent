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
	personaAgentNameRegex     = regexp.MustCompile(`(?i)\b(?:your name is|call yourself|you are called|you're called)\s+([A-Za-z0-9][A-Za-z0-9 _\-.']{1,60}?)(?:\s+and\b|[.!?,]|$)`)
	personaTimezoneRegex      = regexp.MustCompile(`(?i)\b(?:my timezone is|i am in timezone)\s+([A-Za-z0-9_/\-+]{2,64})`)
	personaLocationRegex      = regexp.MustCompile(`(?i)\b(?:i live in|i am in|i'm in)\s+([A-Za-z0-9 _\-/]{2,80})`)
	personaLanguageRegex      = regexp.MustCompile(`(?i)\b(?:my preferred language is|respond in|speak in)\s+([A-Za-z]{2,32})`)
	personaCommStyleRegex     = regexp.MustCompile(`(?i)\b(?:be|respond|talk|write)\s+(more\s+)?(concise|detailed|formal|casual|direct|friendly)\b`)
	personaPreferenceRegex    = regexp.MustCompile(`(?i)\b(i (?:really )?(?:like|love|prefer|hate|dislike)\b[^.!?\n]*)`)
	personaGoalRegex          = regexp.MustCompile(`(?i)\b(?:my goal is|i want to|help me)\s+([^.!?\n]{4,140})`)
	personaSessionIntentRegex = regexp.MustCompile(`(?i)\b(?:right now|for this session|today)\b[:\s-]*([^.!?\n]{4,140})`)
	personaForgetRegex        = regexp.MustCompile(`(?i)\b(?:forget|remove|don't remember)\b\s+(.+)$`)
	personaDirectiveRegex     = regexp.MustCompile(`(?i)\b(?:you should|from now on|always)\s+([^.!?\n]{4,200})`)
	personaYouAreRegex        = regexp.MustCompile(`(?i)\b(?:you are|you're)\s+([^.!?\n]{3,180})`)
	personaYouHaveRegex       = regexp.MustCompile(`(?i)\b(?:you have|you've got)\s+([^.!?\n]{3,180})`)
	personaYourFieldIsRegex   = regexp.MustCompile(`(?i)\byour\s+([A-Za-z][A-Za-z0-9 _\-]{1,32})\s+is\s+([^.!?\n]{1,180})`)
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
	fileSync  PersonaFileSyncMode
	policy    *PersonaPolicyEngine

	cacheTTL time.Duration

	mu          sync.RWMutex
	promptCache map[string]promptCacheEntry
	fileHashes  map[string]string
}

func NewPersonaManager(store Store, workspace string, extractor PersonaExtractionFunc, fileSync PersonaFileSyncMode, policy *PersonaPolicyEngine) *PersonaManager {
	if fileSync == "" {
		fileSync = PersonaFileSyncExportOnly
	}
	if policy == nil {
		policy = NewPersonaPolicyEngine(PersonaPolicyConfig{
			Mode:          "balanced",
			MinConfidence: 0.52,
		})
	}
	return &PersonaManager{
		store:       store,
		workspace:   workspace,
		extractor:   extractor,
		fileSync:    fileSync,
		policy:      policy,
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

func (pm *PersonaManager) ApplyPendingForTurn(ctx context.Context, sessionKey, turnID, userID, agentID string) (PersonaApplyReport, error) {
	report := PersonaApplyReport{
		SessionKey: sessionKey,
		TurnID:     turnID,
		UserID:     userID,
		AgentID:    agentID,
		AppliedAt:  time.Now().UnixMilli(),
	}

	profile, err := pm.store.GetPersonaProfile(ctx, userID, agentID)
	if err != nil {
		return report, err
	}
	if profile.UserID == "" {
		profile = defaultPersonaProfile(userID, agentID)
	}

	// Reverse import: manual edits to persona files are merged back into canonical profile.
	if pm.fileSync == PersonaFileSyncImportExport {
		merged, changed, impErr := pm.importProfileFromFiles(ctx, profile, sessionKey, turnID)
		if impErr == nil && changed {
			profile = merged
		}
	}

	candidates, err := pm.store.ListPersonaCandidates(ctx, userID, agentID, sessionKey, turnID, personaCandidatePending, 64)
	if err != nil {
		return report, err
	}
	if len(candidates) == 0 {
		return report, pm.renderProfileFiles(profile)
	}

	accepted := 0
	rejected := 0
	deferred := 0

	for _, cand := range candidates {
		next, changed, oldValue, newValue := applyCandidate(profile, cand)
		if !changed {
			_ = pm.store.UpdatePersonaCandidateStatus(ctx, cand.ID, personaCandidateRejected, "no_change", "", 0)
			report.Decisions = append(report.Decisions, PersonaCandidateDecision{
				CandidateID: cand.ID,
				FieldPath:   cand.FieldPath,
				Operation:   cand.Operation,
				Value:       cand.Value,
				Confidence:  cand.Confidence,
				Decision:    PersonaDecisionRejected,
				ReasonCode:  "no_change",
				Source:      cand.Source,
			})
			rejected++
			continue
		}

		if reason := pm.rejectReason(cand, oldValue, newValue); reason != "" {
			_ = pm.store.UpdatePersonaCandidateStatus(ctx, cand.ID, personaCandidateRejected, reason, "", 0)
			report.Decisions = append(report.Decisions, PersonaCandidateDecision{
				CandidateID: cand.ID,
				FieldPath:   cand.FieldPath,
				Operation:   cand.Operation,
				Value:       cand.Value,
				Confidence:  cand.Confidence,
				Decision:    PersonaDecisionRejected,
				ReasonCode:  reason,
				Source:      cand.Source,
			})
			if reason == PersonaReasonStableFieldConflict {
				_ = pm.store.AddMetric(ctx, "memory.persona.conflict_detected", 1, map[string]string{
					"user_id":    userID,
					"field_path": cand.FieldPath,
				})
			}
			rejected++
			continue
		}

		hits, _ := pm.store.BumpPersonaSignal(ctx, userID, agentID, cand.FieldPath, hashLower(newValue), time.Now().UnixMilli())
		if required := requiredEvidenceHits(cand.FieldPath); hits < required {
			reason := fmt.Sprintf("insufficient_evidence_%d_of_%d", hits, required)
			_ = pm.store.UpdatePersonaCandidateStatus(ctx, cand.ID, personaCandidateDeferred, reason, "", 0)
			report.Decisions = append(report.Decisions, PersonaCandidateDecision{
				CandidateID: cand.ID,
				FieldPath:   cand.FieldPath,
				Operation:   cand.Operation,
				Value:       cand.Value,
				Confidence:  cand.Confidence,
				Decision:    PersonaDecisionDeferred,
				ReasonCode:  reason,
				Source:      cand.Source,
			})
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
			report.Decisions = append(report.Decisions, PersonaCandidateDecision{
				CandidateID: cand.ID,
				FieldPath:   cand.FieldPath,
				Operation:   cand.Operation,
				Value:       cand.Value,
				Confidence:  cand.Confidence,
				Decision:    PersonaDecisionRejected,
				ReasonCode:  reason,
				Source:      cand.Source,
			})
			rejected++
			continue
		}

		profile = next
		report.Decisions = append(report.Decisions, PersonaCandidateDecision{
			CandidateID: cand.ID,
			FieldPath:   cand.FieldPath,
			Operation:   cand.Operation,
			Value:       cand.Value,
			Confidence:  cand.Confidence,
			Decision:    PersonaDecisionAccepted,
			ReasonCode:  PersonaReasonAllowed,
			Source:      cand.Source,
		})
		accepted++
		pm.invalidatePromptCache(userID, agentID)
	}

	_ = pm.store.AddMetric(ctx, "memory.persona.candidates.accepted", float64(accepted), map[string]string{"user_id": userID})
	_ = pm.store.AddMetric(ctx, "memory.persona.candidates.rejected", float64(rejected), map[string]string{"user_id": userID})
	_ = pm.store.AddMetric(ctx, "memory.persona.candidates.deferred", float64(deferred), map[string]string{"user_id": userID})

	return report, pm.renderProfileFiles(profile)
}

func (pm *PersonaManager) BuildPrompt(ctx context.Context, userID, agentID, sessionIntent string, budgetTokens int) (string, error) {
	profile, err := pm.store.GetPersonaProfile(ctx, userID, agentID)
	if err != nil {
		return "", err
	}
	if profile.UserID == "" {
		profile = defaultPersonaProfile(userID, agentID)
	}
	if pm.fileSync == PersonaFileSyncImportExport {
		merged, changed, _ := pm.importProfileFromFiles(ctx, profile, "", "")
		if changed {
			profile = merged
		}
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
	if len(profile.Identity.Attributes) > 0 {
		keys := make([]string, 0, len(profile.Identity.Attributes))
		for k := range profile.Identity.Attributes {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		lines = append(lines, "- Identity attributes:")
		for _, k := range keys {
			lines = append(lines, fmt.Sprintf("  - %s: %s", k, profile.Identity.Attributes[k]))
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
	if len(profile.Soul.Attributes) > 0 {
		keys := make([]string, 0, len(profile.Soul.Attributes))
		for k := range profile.Soul.Attributes {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		lines = append(lines, "- Soul attributes:")
		for _, k := range keys {
			lines = append(lines, fmt.Sprintf("  - %s: %s", k, profile.Soul.Attributes[k]))
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
	if len(profile.User.Attributes) > 0 {
		keys := make([]string, 0, len(profile.User.Attributes))
		for k := range profile.User.Attributes {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		lines = append(lines, "- User attributes:")
		for _, k := range keys {
			lines = append(lines, fmt.Sprintf("  - %s: %s", k, profile.User.Attributes[k]))
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
		for _, m := range personaAgentNameRegex.FindAllStringSubmatch(content, -1) {
			if len(m) < 2 {
				continue
			}
			out = append(out, newCandidate(sessionKey, turnID, userID, agentID, ev.ID, "identity.agent_name", "set", m[1], 0.9, content, "heuristic"))
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
		for _, m := range personaYourFieldIsRegex.FindAllStringSubmatch(content, -1) {
			if len(m) < 3 {
				continue
			}
			field := strings.TrimSpace(m[1])
			value := strings.TrimSpace(m[2])
			if field == "" || value == "" {
				continue
			}
			fieldPath, op := mapPersonaDirectiveField(field)
			out = append(out, newCandidate(sessionKey, turnID, userID, agentID, ev.ID, fieldPath, op, value, 0.82, content, "heuristic"))
		}
		for _, m := range personaYouAreRegex.FindAllStringSubmatch(content, -1) {
			if len(m) < 2 {
				continue
			}
			for _, cand := range deriveYouAreCandidates(sessionKey, turnID, userID, agentID, ev.ID, strings.TrimSpace(m[1]), content) {
				out = append(out, cand)
			}
		}
		for _, m := range personaYouHaveRegex.FindAllStringSubmatch(content, -1) {
			if len(m) < 2 {
				continue
			}
			trait := strings.TrimSpace(m[1])
			if trait == "" {
				continue
			}
			key := "appearance_" + shortStableSlug(trait)
			out = append(out, newCandidate(sessionKey, turnID, userID, agentID, ev.ID, "identity.attributes."+key, "set", trait, 0.7, content, "heuristic"))
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
	if pm.policy == nil {
		pm.policy = NewPersonaPolicyEngine(PersonaPolicyConfig{
			Mode:          "balanced",
			MinConfidence: 0.52,
		})
	}
	allowed, reason := pm.policy.Evaluate(cand, oldValue, newValue)
	if allowed {
		return ""
	}
	return reason
}

func requiredEvidenceHits(fieldPath string) int {
	if isUnstableField(fieldPath) {
		return 2
	}
	return 1
}

func isUnstableField(fieldPath string) bool {
	return strings.HasPrefix(fieldPath, "user.preferences.") ||
		fieldPath == "user.communication_style" ||
		fieldPath == "user.session_intent"
}

func isStableField(fieldPath string) bool {
	switch fieldPath {
	case "user.name", "user.timezone", "user.location", "identity.agent_name", "identity.role", "identity.purpose":
		return true
	default:
		return false
	}
}

func isExplicitOverride(fieldPath, evidence string) bool {
	ev := strings.ToLower(evidence)
	if strings.Contains(ev, "actually") ||
		strings.Contains(ev, "correction") ||
		strings.Contains(ev, "instead") ||
		strings.Contains(ev, "from now on") {
		return true
	}
	switch fieldPath {
	case "user.name":
		return strings.Contains(ev, "my name is") || strings.Contains(ev, "call me") || strings.Contains(ev, "don't call me")
	case "identity.agent_name":
		return strings.Contains(ev, "your name is") || strings.Contains(ev, "call yourself") || strings.Contains(ev, "you are called")
	case "user.timezone":
		return strings.Contains(ev, "my timezone is") || strings.Contains(ev, "i am in timezone")
	case "user.location":
		return strings.Contains(ev, "i live in") || strings.Contains(ev, "i am in") || strings.Contains(ev, "i'm in")
	case "identity.role":
		return strings.Contains(ev, "you are") || strings.Contains(ev, "you're") || strings.Contains(ev, "your role is")
	case "identity.purpose":
		return strings.Contains(ev, "your purpose is")
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
		"identity.attributes":      {},
		"soul.attributes":          {},
		"user.attributes":          {},
	}
	if _, ok := allowedExact[path]; ok {
		return true
	}
	return strings.HasPrefix(path, "user.preferences.") ||
		strings.HasPrefix(path, "identity.attributes.") ||
		strings.HasPrefix(path, "soul.attributes.") ||
		strings.HasPrefix(path, "user.attributes.")
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
	case path == "identity.goals":
		profile.Identity.Goals = splitDelimitedList(value)
	case path == "identity.boundaries":
		profile.Identity.Boundaries = splitDelimitedList(value)
	case path == "soul.voice":
		profile.Soul.Voice = value
	case path == "soul.communication_style":
		profile.Soul.Communication = value
	case path == "soul.values":
		profile.Soul.Values = splitDelimitedList(value)
	case path == "soul.behavioral_rules":
		profile.Soul.BehavioralRules = splitDelimitedList(value)
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
	case path == "user.goals":
		profile.User.Goals = splitDelimitedList(value)
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
	case strings.HasPrefix(path, "identity.attributes."):
		if profile.Identity.Attributes == nil {
			profile.Identity.Attributes = map[string]string{}
		}
		key := strings.TrimPrefix(path, "identity.attributes.")
		if value == "" {
			delete(profile.Identity.Attributes, key)
		} else {
			profile.Identity.Attributes[key] = value
		}
	case strings.HasPrefix(path, "soul.attributes."):
		if profile.Soul.Attributes == nil {
			profile.Soul.Attributes = map[string]string{}
		}
		key := strings.TrimPrefix(path, "soul.attributes.")
		if value == "" {
			delete(profile.Soul.Attributes, key)
		} else {
			profile.Soul.Attributes[key] = value
		}
	case strings.HasPrefix(path, "user.attributes."):
		if profile.User.Attributes == nil {
			profile.User.Attributes = map[string]string{}
		}
		key := strings.TrimPrefix(path, "user.attributes.")
		if value == "" {
			delete(profile.User.Attributes, key)
		} else {
			profile.User.Attributes[key] = value
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
	case path == "identity.agent_name":
		profile.Identity.AgentName = ""
	case path == "identity.role":
		profile.Identity.Role = ""
	case path == "identity.purpose":
		profile.Identity.Purpose = ""
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
	case path == "identity.attributes":
		profile.Identity.Attributes = map[string]string{}
	case strings.HasPrefix(path, "identity.attributes."):
		delete(profile.Identity.Attributes, strings.TrimPrefix(path, "identity.attributes."))
	case path == "soul.voice":
		profile.Soul.Voice = ""
	case path == "soul.communication_style":
		profile.Soul.Communication = ""
	case path == "soul.values":
		profile.Soul.Values = nil
	case path == "soul.behavioral_rules":
		profile.Soul.BehavioralRules = nil
	case path == "soul.attributes":
		profile.Soul.Attributes = map[string]string{}
	case strings.HasPrefix(path, "soul.attributes."):
		delete(profile.Soul.Attributes, strings.TrimPrefix(path, "soul.attributes."))
	case path == "user.goals":
		profile.User.Goals = nil
	case path == "user.attributes":
		profile.User.Attributes = map[string]string{}
	case strings.HasPrefix(path, "user.attributes."):
		delete(profile.User.Attributes, strings.TrimPrefix(path, "user.attributes."))
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
	case path == "identity.attributes":
		return joinMapPairs(profile.Identity.Attributes)
	case strings.HasPrefix(path, "identity.attributes."):
		return profile.Identity.Attributes[strings.TrimPrefix(path, "identity.attributes.")]
	case path == "soul.attributes":
		return joinMapPairs(profile.Soul.Attributes)
	case strings.HasPrefix(path, "soul.attributes."):
		return profile.Soul.Attributes[strings.TrimPrefix(path, "soul.attributes.")]
	case path == "user.attributes":
		return joinMapPairs(profile.User.Attributes)
	case strings.HasPrefix(path, "user.attributes."):
		return profile.User.Attributes[strings.TrimPrefix(path, "user.attributes.")]
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

func mapPersonaDirectiveField(field string) (string, string) {
	field = strings.ToLower(strings.TrimSpace(field))
	switch field {
	case "name", "agent name", "assistant name":
		return "identity.agent_name", "set"
	case "role":
		return "identity.role", "set"
	case "purpose", "mission":
		return "identity.purpose", "set"
	case "voice":
		return "soul.voice", "set"
	case "tone", "communication style", "style":
		return "soul.communication_style", "set"
	case "boundary", "boundaries":
		return "identity.boundaries", "append"
	case "rule", "rules":
		return "soul.behavioral_rules", "append"
	case "user name":
		return "user.name", "set"
	case "timezone", "time zone":
		return "user.timezone", "set"
	case "location":
		return "user.location", "set"
	default:
		return "identity.attributes." + shortStableSlug(field), "set"
	}
}

func deriveYouAreCandidates(sessionKey, turnID, userID, agentID, sourceEventID, clause, evidence string) []PersonaUpdateCandidate {
	clause = strings.TrimSpace(clause)
	if clause == "" {
		return nil
	}
	lower := strings.ToLower(clause)
	out := []PersonaUpdateCandidate{}
	switch {
	case strings.Contains(lower, "assistant") || strings.Contains(lower, "copilot") || strings.Contains(lower, "agent"):
		out = append(out, newCandidate(sessionKey, turnID, userID, agentID, sourceEventID, "identity.role", "set", clause, 0.84, evidence, "heuristic"))
	case strings.Contains(lower, "concise") || strings.Contains(lower, "detailed") || strings.Contains(lower, "formal") || strings.Contains(lower, "casual") || strings.Contains(lower, "direct") || strings.Contains(lower, "friendly"):
		out = append(out, newCandidate(sessionKey, turnID, userID, agentID, sourceEventID, "soul.communication_style", "set", clause, 0.8, evidence, "heuristic"))
	default:
		key := "self_description"
		if strings.Contains(lower, "you have") || strings.Contains(lower, "hair") || strings.Contains(lower, "eyes") || strings.Contains(lower, "look") {
			key = "appearance"
		}
		out = append(out, newCandidate(sessionKey, turnID, userID, agentID, sourceEventID, "identity.attributes."+key, "set", clause, 0.72, evidence, "heuristic"))
	}
	if strings.Contains(lower, "flirty") || strings.Contains(lower, "vulgar") || strings.Contains(lower, "casual") {
		out = append(out, newCandidate(sessionKey, turnID, userID, agentID, sourceEventID, "soul.behavioral_rules", "append", clause, 0.72, evidence, "heuristic"))
	}
	return out
}

func splitDelimitedList(in string) []string {
	in = strings.TrimSpace(in)
	if in == "" {
		return nil
	}
	parts := strings.FieldsFunc(in, func(r rune) bool {
		return r == '|' || r == ',' || r == ';'
	})
	if len(parts) == 0 {
		return nil
	}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return dedupeNonEmpty(out)
}

func joinMapPairs(values map[string]string) string {
	if len(values) == 0 {
		return ""
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+values[k])
	}
	return strings.Join(parts, " | ")
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
	if pm.fileSync == PersonaFileSyncDisabled {
		return nil
	}
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
	lines = append(lines, "", "## Attributes")
	if len(profile.Identity.Attributes) == 0 {
		lines = append(lines, "- (none yet)")
	} else {
		keys := make([]string, 0, len(profile.Identity.Attributes))
		for k := range profile.Identity.Attributes {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			lines = append(lines, fmt.Sprintf("- %s: %s", k, profile.Identity.Attributes[k]))
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
	lines = append(lines, "", "## Attributes")
	if len(profile.Soul.Attributes) == 0 {
		lines = append(lines, "- (none yet)")
	} else {
		keys := make([]string, 0, len(profile.Soul.Attributes))
		for k := range profile.Soul.Attributes {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			lines = append(lines, fmt.Sprintf("- %s: %s", k, profile.Soul.Attributes[k]))
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
	lines = append(lines, "", "## Attributes")
	if len(profile.User.Attributes) == 0 {
		lines = append(lines, "- (none yet)")
	} else {
		keys := make([]string, 0, len(profile.User.Attributes))
		for k := range profile.User.Attributes {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			lines = append(lines, fmt.Sprintf("- %s: %s", k, profile.User.Attributes[k]))
		}
	}
	return strings.Join(lines, "\n")
}

func parseMarkdownSections(raw string) (map[string]string, map[string][]string, map[string]map[string]string) {
	values := map[string]string{}
	lists := map[string][]string{}
	sectionPairs := map[string]map[string]string{}
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
				v := strings.TrimSpace(item[idx+1:])
				values[k] = v
				if section != "" {
					if sectionPairs[section] == nil {
						sectionPairs[section] = map[string]string{}
					}
					sectionPairs[section][k] = v
				}
			} else if section != "" {
				lists[section] = append(lists[section], item)
			}
			continue
		}
		if section != "" {
			values[section] = t
		}
	}
	return values, lists, sectionPairs
}

func mergeIdentityMarkdown(profile *PersonaProfile, raw string) bool {
	values, lists, sectionPairs := parseMarkdownSections(raw)
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
	if attrs := sectionPairs["attributes"]; len(attrs) > 0 {
		if profile.Identity.Attributes == nil {
			profile.Identity.Attributes = map[string]string{}
		}
		for k, v := range attrs {
			if v == "" || isTemplatePlaceholder(v) {
				continue
			}
			if profile.Identity.Attributes[k] != v {
				profile.Identity.Attributes[k] = v
				changed = true
			}
		}
	}
	return changed
}

func mergeSoulMarkdown(profile *PersonaProfile, raw string) bool {
	values, lists, sectionPairs := parseMarkdownSections(raw)
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
	if attrs := sectionPairs["attributes"]; len(attrs) > 0 {
		if profile.Soul.Attributes == nil {
			profile.Soul.Attributes = map[string]string{}
		}
		for k, v := range attrs {
			if v == "" || isTemplatePlaceholder(v) {
				continue
			}
			if profile.Soul.Attributes[k] != v {
				profile.Soul.Attributes[k] = v
				changed = true
			}
		}
	}
	return changed
}

func mergeUserMarkdown(profile *PersonaProfile, raw string) bool {
	values, lists, sectionPairs := parseMarkdownSections(raw)
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

	if nextPrefs := sectionPairs["preferences"]; len(nextPrefs) > 0 {
		if profile.User.Preferences == nil {
			profile.User.Preferences = map[string]string{}
		}
		for k, v := range nextPrefs {
			if k == "" || v == "" || isTemplatePlaceholder(v) {
				continue
			}
			if profile.User.Preferences[k] != v {
				profile.User.Preferences[k] = v
				changed = true
			}
		}
	}
	if attrs := sectionPairs["attributes"]; len(attrs) > 0 {
		if profile.User.Attributes == nil {
			profile.User.Attributes = map[string]string{}
		}
		for k, v := range attrs {
			if k == "" || v == "" || isTemplatePlaceholder(v) {
				continue
			}
			if profile.User.Attributes[k] != v {
				profile.User.Attributes[k] = v
				changed = true
			}
		}
	}
	return changed
}

func isTemplatePlaceholder(v string) bool {
	v = strings.TrimSpace(strings.ToLower(v))
	return v == "" || strings.Contains(v, "(optional)") || strings.Contains(v, "(none yet)") || strings.Contains(v, "(unspecified)") || strings.Contains(v, "(your")
}
