package memory

import "strings"

const (
	PersonaReasonAllowed             = "allowed"
	PersonaReasonLowConfidence       = "low_confidence"
	PersonaReasonSensitiveData       = "sensitive_data"
	PersonaReasonEmptyValue          = "empty_value"
	PersonaReasonStableFieldConflict = "stable_field_conflict"
	PersonaReasonValueTooLong        = "value_too_long"
)

type PersonaPolicyConfig struct {
	Mode          string
	MinConfidence float64
}

type PersonaPolicyEngine struct {
	cfg PersonaPolicyConfig
}

func NewPersonaPolicyEngine(cfg PersonaPolicyConfig) *PersonaPolicyEngine {
	mode := strings.ToLower(strings.TrimSpace(cfg.Mode))
	switch mode {
	case "strict", "balanced", "permissive":
	default:
		mode = "balanced"
	}
	cfg.Mode = mode
	if cfg.MinConfidence <= 0 {
		cfg.MinConfidence = 0.52
	}
	if cfg.MinConfidence > 1 {
		cfg.MinConfidence = 1
	}
	return &PersonaPolicyEngine{cfg: cfg}
}

func (p *PersonaPolicyEngine) Evaluate(cand PersonaUpdateCandidate, oldValue, newValue string) (bool, string) {
	confidence := clampConfidence(cand.Confidence)
	if confidence < p.cfg.MinConfidence {
		return false, PersonaReasonLowConfidence
	}
	if personaSensitiveRegex.MatchString(cand.Value) || personaSensitiveRegex.MatchString(cand.Evidence) {
		return false, PersonaReasonSensitiveData
	}

	trimmedNew := strings.TrimSpace(newValue)
	if trimmedNew == "" && cand.Operation != "delete" {
		return false, PersonaReasonEmptyValue
	}
	if len(trimmedNew) > 260 && !strings.Contains(cand.FieldPath, ".attributes.") {
		return false, PersonaReasonValueTooLong
	}

	trimmedOld := strings.TrimSpace(oldValue)
	if isStableField(cand.FieldPath) && trimmedOld != "" && trimmedOld != trimmedNew {
		threshold := p.conflictThreshold()
		if confidence < threshold && !isExplicitOverride(cand.FieldPath, cand.Evidence) {
			return false, PersonaReasonStableFieldConflict
		}
	}

	return true, PersonaReasonAllowed
}

func (p *PersonaPolicyEngine) conflictThreshold() float64 {
	switch p.cfg.Mode {
	case "strict":
		return 0.9
	case "permissive":
		return 0.7
	default:
		return 0.82
	}
}
