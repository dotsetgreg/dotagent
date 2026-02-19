package memory

import (
	"regexp"
	"strings"
	"time"
)

var (
	prefRegex                         = regexp.MustCompile(`(?i)\b(i (?:really )?(?:like|love|prefer|hate|dislike)\b[^.!?\n]*)`)
	identityRegex                     = regexp.MustCompile(`(?i)\b(?:my name is|call me)\s+([A-Za-z0-9 _\-]{2,50})`)
	timezoneRegex                     = regexp.MustCompile(`(?i)\b(?:my timezone is|timezone is|time zone is)\s+([A-Za-z0-9_\-/:+ ]{2,80})`)
	taskStateRegex                    = regexp.MustCompile(`(?i)\b(remind me|schedule|todo|task|deadline)\b([^.!?\n]*)`)
	forgetRegex                       = regexp.MustCompile(`(?i)\b(?:please\s+)?(?:forget|remove)\b(?:\s+(?:that|this|about))?\s+(.+)$`)
	extractionLikelyQuestionLeadRegex = regexp.MustCompile(`(?i)^\s*(?:what|why|how|when|where|who|can|could|would|do|does|did|is|are|am|if|whether)\b`)
	extractionPersistenceCueRegex     = regexp.MustCompile(`(?i)\b(?:remember|note|save|store|track|my name is|my timezone is|call me)\b`)

	firstPersonVerbFactRegex = regexp.MustCompile(`(?i)\b(i (?:am|i'm|have|had|use|used|work on|work with|build|built|maintain|maintained|live in|lived in|read|reading|need|needed|want|wanted|prefer|like|love|hate|dislike|got|keep|own|run|study|studied|mod|modded|modified)\b[^.!?\n]{4,180})`)
	sentenceSplitRegex       = regexp.MustCompile(`[.!?\n;]+`)
	firstPersonLeadRegex     = regexp.MustCompile(`(?i)^(?:i|i'm|i am|my)\b`)
	hedgedLeadRegex          = regexp.MustCompile(`(?i)^i (?:think|guess|wonder|hope|suppose|feel)\b`)
)

func extractUserContentUpsertOps(content, sourceEventID string) []ConsolidationOp {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}
	skipFactCapture := isLikelyQuestionForMemory(content) && !extractionPersistenceCueRegex.MatchString(content)

	ops := []ConsolidationOp{}
	seen := map[string]struct{}{}
	addOp := func(op ConsolidationOp) {
		key := string(op.Kind) + "|" + op.Key
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		ops = append(ops, op)
	}

	if !skipFactCapture {
		for _, m := range prefRegex.FindAllStringSubmatch(content, -1) {
			if len(m) < 2 {
				continue
			}
			entry := normalizeEntityPhrase(m[1])
			if entry == "" {
				continue
			}
			addOp(ConsolidationOp{
				Action:      "upsert",
				Kind:        MemoryUserPreference,
				Key:         contentKey("pref", entry),
				Content:     entry,
				Confidence:  0.8,
				SourceEvent: sourceEventID,
				Metadata:    map[string]string{"source_role": "user", "extractor": "preference"},
			})
		}

		for _, m := range identityRegex.FindAllStringSubmatch(content, -1) {
			if len(m) < 2 {
				continue
			}
			identity := normalizeEntityPhrase(m[1])
			if len(identity) < 2 {
				continue
			}
			addOp(ConsolidationOp{
				Action:      "upsert",
				Kind:        MemorySemanticFact,
				Key:         "identity/name",
				Content:     "User identity hint: " + identity,
				Confidence:  0.75,
				SourceEvent: sourceEventID,
				Metadata:    map[string]string{"source_role": "user", "extractor": "identity"},
			})
		}

		for _, m := range timezoneRegex.FindAllStringSubmatch(content, -1) {
			if len(m) < 2 {
				continue
			}
			tz := normalizeEntityPhrase(m[1])
			if tz == "" {
				continue
			}
			addOp(ConsolidationOp{
				Action:      "upsert",
				Kind:        MemorySemanticFact,
				Key:         "profile/timezone_or_location",
				Content:     "User timezone/location: " + tz,
				Confidence:  0.7,
				SourceEvent: sourceEventID,
				Metadata:    map[string]string{"source_role": "user", "extractor": "timezone"},
			})
		}

		for _, phrase := range ExtractFactSignals(content) {
			kind := MemorySemanticFact
			confidence := 0.72
			keyPrefix := "fact"
			if isPreferencePhrase(phrase) {
				kind = MemoryUserPreference
				confidence = 0.76
				keyPrefix = "pref_fact"
			}
			addOp(ConsolidationOp{
				Action:      "upsert",
				Kind:        kind,
				Key:         contentKey(keyPrefix, phrase),
				Content:     phrase,
				Confidence:  confidence,
				SourceEvent: sourceEventID,
				Metadata:    map[string]string{"source_role": "user", "extractor": "first_person_fact"},
			})
		}
	}

	for _, m := range taskStateRegex.FindAllStringSubmatch(content, -1) {
		if len(m) < 2 {
			continue
		}
		task := normalizeEntityPhrase(strings.Join(m[1:], " "))
		if task == "" {
			continue
		}
		addOp(ConsolidationOp{
			Action:      "upsert",
			Kind:        MemoryTaskState,
			Key:         contentKey("task", task),
			Content:     "Open task intent: " + task,
			Confidence:  0.6,
			SourceEvent: sourceEventID,
			TTL:         14 * 24 * time.Hour,
			Metadata:    map[string]string{"source_role": "user", "extractor": "task"},
		})
	}

	return ops
}

// ExtractFactSignals emits normalized first-person factual statements from user text.
// This is intentionally topic-agnostic and designed for reuse by memory and context checks.
func ExtractFactSignals(content string) []string {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}
	seen := map[string]struct{}{}
	out := []string{}
	add := func(value string) {
		value = normalizeEntityPhrase(value)
		if value == "" {
			return
		}
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}

	for _, m := range prefRegex.FindAllStringSubmatch(content, -1) {
		if len(m) >= 2 {
			add(m[1])
		}
	}
	for _, m := range firstPersonVerbFactRegex.FindAllStringSubmatch(content, -1) {
		if len(m) >= 2 {
			add(m[1])
		}
	}
	for _, clause := range extractFirstPersonClauses(content) {
		add(clause)
	}

	if len(out) > 16 {
		out = out[:16]
	}
	return out
}

func extractFirstPersonClauses(content string) []string {
	parts := sentenceSplitRegex.Split(content, -1)
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = normalizeEntityPhrase(part)
		if part == "" {
			continue
		}
		lower := strings.ToLower(part)
		if len(lower) < 8 {
			continue
		}
		if !firstPersonLeadRegex.MatchString(lower) {
			continue
		}
		if hedgedLeadRegex.MatchString(lower) {
			continue
		}
		out = append(out, part)
	}
	if len(out) > 20 {
		out = out[:20]
	}
	return out
}

func isPreferencePhrase(phrase string) bool {
	lower := strings.ToLower(phrase)
	return strings.Contains(lower, " like ") ||
		strings.Contains(lower, " love ") ||
		strings.Contains(lower, " prefer ") ||
		strings.Contains(lower, " dislike ") ||
		strings.Contains(lower, " hate ")
}

func normalizeEntityPhrase(in string) string {
	in = strings.TrimSpace(in)
	if in == "" {
		return ""
	}
	in = strings.Trim(in, " .,!?:;\"'")
	if len(in) < 2 {
		return ""
	}
	if len(in) > 180 {
		in = strings.TrimSpace(in[:180])
	}
	return in
}

func isLikelyQuestionForMemory(content string) bool {
	content = strings.TrimSpace(content)
	if content == "" {
		return false
	}
	if strings.Contains(content, "?") {
		return true
	}
	return extractionLikelyQuestionLeadRegex.MatchString(content)
}
