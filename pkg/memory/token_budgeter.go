package memory

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// TokenBudgeter provides model-aware token estimation and adapts over time
// using provider-reported prompt token usage.
type TokenBudgeter struct {
	mu sync.RWMutex

	statePath string
	models    map[string]tokenModelState

	lastPersistAtMS int64
}

type tokenModelState struct {
	Scale       float64 `json:"scale"`
	ErrorEWMA   float64 `json:"error_ewma"`
	Samples     int     `json:"samples"`
	UpdatedAtMS int64   `json:"updated_at_ms"`
}

type tokenBudgeterStateFile struct {
	Models map[string]tokenModelState `json:"models"`
}

func NewTokenBudgeter(workspace string) *TokenBudgeter {
	path := ""
	if strings.TrimSpace(workspace) != "" {
		path = filepath.Join(workspace, "state", "token_budgeter.json")
	}
	b := &TokenBudgeter{
		statePath: path,
		models:    map[string]tokenModelState{},
	}
	b.loadState()
	return b
}

// EstimateTextTokens estimates tokens for a plain text chunk.
func (b *TokenBudgeter) EstimateTextTokens(model, text string) int {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0
	}
	base := baseTextTokenEstimate(model, text)
	scale := b.scaleForModel(model)
	estimated := int(math.Ceil(float64(base) * scale))
	if estimated < 8 {
		estimated = 8
	}
	return estimated
}

// ObservePromptUsage updates model calibration from one provider usage sample.
func (b *TokenBudgeter) ObservePromptUsage(model string, estimatedPromptTokens, actualPromptTokens int) {
	if estimatedPromptTokens <= 0 || actualPromptTokens <= 0 {
		return
	}
	modelKey := normalizeModelKey(model)
	ratio := float64(actualPromptTokens) / float64(estimatedPromptTokens)
	ratio = clampFloat64(ratio, 0.45, 2.40)

	b.mu.Lock()
	st := b.models[modelKey]
	if st.Scale <= 0 {
		st.Scale = 1.0
	}
	alpha := 0.22
	if st.Samples < 8 {
		alpha = 0.35
	}
	st.Scale = clampFloat64(st.Scale*(1-alpha)+ratio*alpha, 0.62, 1.90)

	errAlpha := 0.18
	if st.Samples < 8 {
		errAlpha = 0.30
	}
	absErr := math.Abs(ratio - 1.0)
	st.ErrorEWMA = clampFloat64(st.ErrorEWMA*(1-errAlpha)+absErr*errAlpha, 0.0, 1.0)
	st.Samples++
	st.UpdatedAtMS = time.Now().UnixMilli()
	b.models[modelKey] = st

	shouldPersist := b.statePath != "" &&
		(st.Samples == 1 || st.Samples%5 == 0 || st.UpdatedAtMS-b.lastPersistAtMS >= int64((30*time.Second)/time.Millisecond))
	b.mu.Unlock()

	if shouldPersist {
		_ = b.persistState()
	}
}

// PromptSafetyFactor returns a dynamic headroom factor for prompt budgeting.
// Higher estimation error yields a tighter usable budget to reduce overflow.
func (b *TokenBudgeter) PromptSafetyFactor(model string) float64 {
	st := b.modelState(model)
	factor := 0.90
	if st.Samples < 10 {
		factor -= 0.04
	}
	factor -= math.Min(0.16, st.ErrorEWMA*0.50)
	if st.Scale > 1.08 {
		factor -= math.Min(0.07, (st.Scale-1.08)*0.30)
	}
	if st.Scale < 0.92 && st.Samples >= 12 {
		factor += math.Min(0.03, (0.92-st.Scale)*0.20)
	}
	return clampFloat64(factor, 0.68, 0.95)
}

func (b *TokenBudgeter) modelState(model string) tokenModelState {
	modelKey := normalizeModelKey(model)
	b.mu.RLock()
	defer b.mu.RUnlock()

	if st, ok := b.models[modelKey]; ok {
		return st
	}
	family := modelFamilyKey(modelKey)
	if st, ok := b.models[family]; ok {
		return st
	}
	return tokenModelState{Scale: 1.0}
}

func (b *TokenBudgeter) scaleForModel(model string) float64 {
	st := b.modelState(model)
	if st.Scale <= 0 {
		return 1.0
	}
	return st.Scale
}

func (b *TokenBudgeter) loadState() {
	if strings.TrimSpace(b.statePath) == "" {
		return
	}
	raw, err := os.ReadFile(b.statePath)
	if err != nil {
		return
	}
	var file tokenBudgeterStateFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return
	}
	if len(file.Models) == 0 {
		return
	}
	b.mu.Lock()
	for k, st := range file.Models {
		if strings.TrimSpace(k) == "" {
			continue
		}
		if st.Scale <= 0 {
			st.Scale = 1.0
		}
		b.models[normalizeModelKey(k)] = st
	}
	b.mu.Unlock()
}

func (b *TokenBudgeter) persistState() error {
	if strings.TrimSpace(b.statePath) == "" {
		return nil
	}
	b.mu.RLock()
	file := tokenBudgeterStateFile{
		Models: make(map[string]tokenModelState, len(b.models)),
	}
	for k, st := range b.models {
		file.Models[k] = st
	}
	b.mu.RUnlock()

	if len(file.Models) == 0 {
		return nil
	}
	raw, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(b.statePath), 0o755); err != nil {
		return err
	}
	if err := writeBytesAtomic(b.statePath, raw, 0o600); err != nil {
		return err
	}
	b.mu.Lock()
	b.lastPersistAtMS = time.Now().UnixMilli()
	b.mu.Unlock()
	return nil
}

func writeBytesAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename temp file: %w", err)
	}
	return nil
}

func baseTextTokenEstimate(model, text string) int {
	model = normalizeModelKey(model)
	runes := utf8.RuneCountInString(text)
	if runes <= 0 {
		return 0
	}
	charsPerToken := modelCharsPerToken(model)
	words := len(tokenize(text))
	lines := strings.Count(text, "\n")
	punct := punctuationCount(text)

	estimate := float64(runes)/charsPerToken +
		float64(words)*0.08 +
		float64(lines)*0.45 +
		float64(punct)*0.025
	if estimate < 8 {
		estimate = 8
	}
	return int(math.Ceil(estimate))
}

func punctuationCount(text string) int {
	n := 0
	for _, r := range text {
		switch r {
		case '.', ',', ';', ':', '!', '?', '(', ')', '[', ']', '{', '}', '"', '\'':
			n++
		}
	}
	return n
}

func modelCharsPerToken(model string) float64 {
	switch {
	case strings.Contains(model, "gpt-5"), strings.Contains(model, "gpt-4.1"), strings.Contains(model, "o3"), strings.Contains(model, "o4"), strings.Contains(model, "openai"):
		return 3.45
	case strings.Contains(model, "claude"):
		return 3.80
	case strings.Contains(model, "gemini"):
		return 3.55
	case strings.Contains(model, "qwen"), strings.Contains(model, "deepseek"):
		return 3.30
	default:
		return 3.55
	}
}

func modelFamilyKey(model string) string {
	switch {
	case strings.Contains(model, "gpt-5"):
		return "family:gpt-5"
	case strings.Contains(model, "gpt-4.1"):
		return "family:gpt-4.1"
	case strings.Contains(model, "o3"):
		return "family:o3"
	case strings.Contains(model, "o4"):
		return "family:o4"
	case strings.Contains(model, "claude"):
		return "family:claude"
	case strings.Contains(model, "gemini"):
		return "family:gemini"
	case strings.Contains(model, "qwen"):
		return "family:qwen"
	default:
		return "family:default"
	}
}

func normalizeModelKey(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return "default"
	}
	if idx := strings.Index(model, ":"); idx > 0 {
		model = model[:idx]
	}
	if idx := strings.LastIndex(model, "/"); idx >= 0 && idx < len(model)-1 {
		// Keep provider/model keys deterministic by model suffix.
		model = model[idx+1:]
	}
	return model
}

func clampFloat64(v, min, max float64) float64 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
