package providers

import (
	"context"
	"strings"
)

const minContextWindowTokens = 16384

// ContextWindowProvider is an optional provider capability that returns the
// best-known context window for a model.
type ContextWindowProvider interface {
	ResolveContextWindow(ctx context.Context, model string) (int, error)
}

// ResolveContextWindow selects the runtime context window using provider/model
// metadata and a configured floor.
func ResolveContextWindow(ctx context.Context, provider LLMProvider, model string, configured int) (tokens int, source string) {
	best := configured
	source = "configured"
	if best < minContextWindowTokens {
		best = minContextWindowTokens
	}

	if cwProvider, ok := provider.(ContextWindowProvider); ok && cwProvider != nil {
		if resolved, err := cwProvider.ResolveContextWindow(ctx, model); err == nil && resolved > best {
			best = resolved
			source = "provider"
		}
	}

	if resolved := knownModelContextWindow(model); resolved > best {
		best = resolved
		source = "model_metadata"
	}
	if best < minContextWindowTokens {
		best = minContextWindowTokens
	}
	return best, source
}

func knownModelContextWindow(model string) int {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return 0
	}
	normalized := m
	if idx := strings.LastIndex(normalized, "/"); idx >= 0 && idx < len(normalized)-1 {
		normalized = normalized[idx+1:]
	}

	exact := map[string]int{
		"gpt-4o":                      128000,
		"gpt-4o-mini":                 128000,
		"gpt-4.1":                     1000000,
		"gpt-4.1-mini":                1000000,
		"gpt-4.1-nano":                1000000,
		"gpt-5":                       400000,
		"gpt-5-mini":                  400000,
		"gpt-5-nano":                  400000,
		"o3":                          200000,
		"o3-mini":                     200000,
		"o4-mini":                     200000,
		"claude-3-5-sonnet":           200000,
		"claude-3-7-sonnet":           200000,
		"claude-sonnet-4":             200000,
		"claude-opus-4":               200000,
		"gemini-1.5-pro":              1000000,
		"gemini-1.5-flash":            1000000,
		"gemini-2.0-flash":            1000000,
		"gemini-2.0-pro":              1000000,
		"qwen2.5-coder-32b":           131072,
		"deepseek-chat":               64000,
		"deepseek-reasoner":           64000,
		"openai/gpt-5.2":              400000,
		"openai/gpt-5":                400000,
		"openai/gpt-5-mini":           400000,
		"openai/gpt-4.1":              1000000,
		"openai/gpt-4.1-mini":         1000000,
		"anthropic/claude-3-7-sonnet": 200000,
	}
	if v, ok := exact[m]; ok {
		return v
	}
	if v, ok := exact[normalized]; ok {
		return v
	}

	switch {
	case strings.Contains(m, "gpt-4.1"), strings.Contains(m, "gpt-5"):
		return 400000
	case strings.Contains(m, "gpt-4o"):
		return 128000
	case strings.Contains(m, "claude"):
		return 200000
	case strings.Contains(m, "gemini"):
		return 1000000
	case strings.Contains(m, "qwen"), strings.Contains(m, "deepseek"):
		return 131072
	default:
		return 0
	}
}
