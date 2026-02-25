package memory

import (
	"strings"
	"testing"
)

func TestParseEmbeddingModelSpec_Ollama(t *testing.T) {
	spec, err := parseEmbeddingModelSpec("ollama:nomic-embed-text")
	if err != nil {
		t.Fatalf("parseEmbeddingModelSpec returned error: %v", err)
	}
	if spec.Provider != embeddingProviderOllama {
		t.Fatalf("expected provider %q, got %q", embeddingProviderOllama, spec.Provider)
	}
	if spec.Model != "nomic-embed-text" {
		t.Fatalf("expected model nomic-embed-text, got %q", spec.Model)
	}
	if spec.Raw != "ollama:nomic-embed-text" {
		t.Fatalf("expected canonical raw spec, got %q", spec.Raw)
	}
}

func TestNormalizeEmbeddingConfig_OllamaChainIncludesLocalFallback(t *testing.T) {
	primary, chain := normalizeEmbeddingConfig(Config{
		EmbeddingModel: "ollama:nomic-embed-text",
	})
	if primary != "ollama:nomic-embed-text" {
		t.Fatalf("unexpected primary model: %q", primary)
	}
	if len(chain) < 2 {
		t.Fatalf("expected fallback chain with local model fallback, got %v", chain)
	}
	if chain[0] != "ollama:nomic-embed-text" {
		t.Fatalf("expected first fallback entry to be primary, got %q", chain[0])
	}
	if !containsString(chain, hashEmbeddingModel) {
		t.Fatalf("expected fallback chain to include %q, got %v", hashEmbeddingModel, chain)
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func TestResolveEmbeddingMaxInputTokens(t *testing.T) {
	if got := resolveEmbeddingMaxInputTokens(embeddingModelSpec{
		Provider: embeddingProviderOpenAI,
		Model:    "text-embedding-3-small",
	}); got != 8192 {
		t.Fatalf("expected openai text-embedding-3-small limit 8192, got %d", got)
	}

	if got := resolveEmbeddingMaxInputTokens(embeddingModelSpec{
		Provider: embeddingProviderOllama,
		Model:    "nomic-embed-text",
	}); got != defaultOllamaEmbeddingMaxInputToken {
		t.Fatalf("expected ollama default limit %d, got %d", defaultOllamaEmbeddingMaxInputToken, got)
	}

	if got := resolveEmbeddingMaxInputTokens(embeddingModelSpec{
		Provider: embeddingProviderLocal,
		Model:    "dotagent-chargram-384-v1",
	}); got != 0 {
		t.Fatalf("expected local model to bypass token truncation, got %d", got)
	}
}

func TestTruncateEmbeddingInputByTokenHeuristic(t *testing.T) {
	ascii := strings.Repeat("a", 20000)
	truncated := truncateEmbeddingInputByTokenHeuristic(ascii, 256)
	if len(truncated) >= len(ascii) {
		t.Fatalf("expected long ASCII input to be truncated")
	}

	cjk := strings.Repeat("語", 5000)
	truncatedCJK := truncateEmbeddingInputByTokenHeuristic(cjk, 128)
	if len([]rune(truncatedCJK)) >= len([]rune(cjk)) {
		t.Fatalf("expected long CJK input to be truncated")
	}
	if strings.TrimSpace(truncatedCJK) == "" {
		t.Fatalf("expected truncated CJK text to remain non-empty")
	}
}
