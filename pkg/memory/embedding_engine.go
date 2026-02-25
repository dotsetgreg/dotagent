package memory

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	embeddingProviderLocal      = "local"
	embeddingProviderOpenAI     = "openai"
	embeddingProviderOpenRouter = "openrouter"
	embeddingProviderOllama     = "ollama"
)

const (
	defaultEmbeddingMaxInputTokens      = 8192
	defaultOllamaEmbeddingMaxInputToken = 2048
)

var knownEmbeddingInputLimits = map[string]int{
	"openai:text-embedding-3-small": 8192,
	"openai:text-embedding-3-large": 8192,
	"openai:text-embedding-ada-002": 8191,
	"openrouter:voyage-3":           32000,
	"openrouter:voyage-3-lite":      16000,
	"openrouter:voyage-code-3":      32000,
}

type EmbeddingEngineConfig struct {
	OpenAIToken   string
	OpenAIAPIBase string

	OpenRouterToken   string
	OpenRouterAPIBase string

	OllamaAPIBase string

	BatchSize   int
	Concurrency int
	Cache       embeddingCacheStore
}

type EmbeddingEngine struct {
	openAIToken   string
	openAIBase    string
	openRouterKey string
	openRouterAPI string
	ollamaAPI     string

	batchSize int
	sem       chan struct{}
	cache     embeddingCacheStore

	httpClient *http.Client
}

type embeddingModelSpec struct {
	Provider string
	Model    string
	Raw      string
}

type embeddingCacheStore interface {
	GetEmbeddingCacheBatch(ctx context.Context, provider, model, providerKey string, contentHashes []string) (map[string][]float32, error)
	PutEmbeddingCacheBatch(ctx context.Context, provider, model, providerKey string, vectors map[string][]float32) error
}

func NewEmbeddingEngine(cfg EmbeddingEngineConfig) *EmbeddingEngine {
	batchSize := cfg.BatchSize
	if batchSize <= 0 {
		batchSize = 96
	}
	concurrency := cfg.Concurrency
	if concurrency <= 0 {
		concurrency = 2
	}
	openAIBase := strings.TrimRight(strings.TrimSpace(cfg.OpenAIAPIBase), "/")
	if openAIBase == "" {
		openAIBase = "https://api.openai.com/v1"
	}
	openRouterBase := strings.TrimRight(strings.TrimSpace(cfg.OpenRouterAPIBase), "/")
	if openRouterBase == "" {
		openRouterBase = "https://openrouter.ai/api/v1"
	}
	ollamaBase := strings.TrimRight(strings.TrimSpace(cfg.OllamaAPIBase), "/")
	if ollamaBase == "" {
		ollamaBase = "http://127.0.0.1:11434"
	}
	return &EmbeddingEngine{
		openAIToken:   strings.TrimSpace(cfg.OpenAIToken),
		openAIBase:    openAIBase,
		openRouterKey: strings.TrimSpace(cfg.OpenRouterToken),
		openRouterAPI: openRouterBase,
		ollamaAPI:     ollamaBase,
		batchSize:     batchSize,
		sem:           make(chan struct{}, concurrency),
		cache:         cfg.Cache,
		httpClient: &http.Client{
			Timeout: 90 * time.Second,
		},
	}
}

func (e *EmbeddingEngine) EmbedBatch(ctx context.Context, modelChain []string, texts []string) (string, [][]float32, error) {
	if len(texts) == 0 {
		return "", nil, nil
	}
	if len(modelChain) == 0 {
		modelChain = []string{currentEmbeddingModel(), hashEmbeddingModel}
	}

	lastErrs := make([]string, 0, len(modelChain))
	for _, rawModel := range modelChain {
		spec, err := parseEmbeddingModelSpec(rawModel)
		if err != nil {
			lastErrs = append(lastErrs, err.Error())
			continue
		}
		vectors, err := e.embedBatchForModel(ctx, spec, texts)
		if err != nil {
			lastErrs = append(lastErrs, err.Error())
			continue
		}
		return spec.Raw, vectors, nil
	}
	if len(lastErrs) == 0 {
		lastErrs = append(lastErrs, "no embedding models configured")
	}
	return "", nil, fmt.Errorf("embedding chain failed: %s", strings.Join(lastErrs, "; "))
}

func (e *EmbeddingEngine) embedBatchForModel(ctx context.Context, spec embeddingModelSpec, texts []string) ([][]float32, error) {
	texts = normalizeEmbeddingInputs(spec, texts)
	providerKey := e.providerKeyFingerprint(spec.Provider)
	contentHashes := make([]string, len(texts))
	for i, text := range texts {
		contentHashes[i] = contentHash(text)
	}

	cached := map[string][]float32{}
	if e.cache != nil {
		cacheMap, err := e.cache.GetEmbeddingCacheBatch(ctx, spec.Provider, spec.Model, providerKey, contentHashes)
		if err == nil && len(cacheMap) > 0 {
			cached = cacheMap
		}
	}

	out := make([][]float32, len(texts))
	missingIdx := make([]int, 0, len(texts))
	missingTexts := make([]string, 0, len(texts))
	missingHashes := make([]string, 0, len(texts))

	for i, hash := range contentHashes {
		if vec, ok := cached[hash]; ok && len(vec) > 0 {
			out[i] = vec
			continue
		}
		missingIdx = append(missingIdx, i)
		missingTexts = append(missingTexts, texts[i])
		missingHashes = append(missingHashes, hash)
	}

	if len(missingIdx) == 0 {
		return out, nil
	}

	missingVectors, err := e.embedBatchRemoteAware(ctx, spec, missingTexts)
	if err != nil {
		return nil, err
	}
	if len(missingVectors) != len(missingIdx) {
		return nil, fmt.Errorf("embedding provider %s returned %d vectors for %d inputs", spec.Raw, len(missingVectors), len(missingIdx))
	}

	cacheEntries := map[string][]float32{}
	for i, idx := range missingIdx {
		out[idx] = missingVectors[i]
		cacheEntries[missingHashes[i]] = missingVectors[i]
	}

	if e.cache != nil && len(cacheEntries) > 0 {
		_ = e.cache.PutEmbeddingCacheBatch(ctx, spec.Provider, spec.Model, providerKey, cacheEntries)
	}
	return out, nil
}

func normalizeEmbeddingInputs(spec embeddingModelSpec, texts []string) []string {
	if len(texts) == 0 {
		return nil
	}
	out := make([]string, len(texts))
	maxTokens := resolveEmbeddingMaxInputTokens(spec)
	for i, text := range texts {
		out[i] = truncateEmbeddingInputByTokenHeuristic(text, maxTokens)
	}
	return out
}

func resolveEmbeddingMaxInputTokens(spec embeddingModelSpec) int {
	provider := strings.ToLower(strings.TrimSpace(spec.Provider))
	model := strings.ToLower(strings.TrimSpace(spec.Model))
	if provider == embeddingProviderLocal {
		return 0
	}
	if provider == embeddingProviderOllama {
		return defaultOllamaEmbeddingMaxInputToken
	}
	if v, ok := knownEmbeddingInputLimits[provider+":"+model]; ok && v > 0 {
		return v
	}
	if provider == embeddingProviderOpenRouter {
		switch {
		case strings.Contains(model, "voyage-3"):
			return 32000
		case strings.Contains(model, "voyage"):
			return 16000
		case strings.Contains(model, "text-embedding-3"):
			return 8192
		default:
			return defaultEmbeddingMaxInputTokens
		}
	}
	if provider == embeddingProviderOpenAI {
		if strings.Contains(model, "text-embedding-ada-002") {
			return 8191
		}
		return defaultEmbeddingMaxInputTokens
	}
	return defaultEmbeddingMaxInputTokens
}

func truncateEmbeddingInputByTokenHeuristic(text string, maxTokens int) string {
	text = strings.TrimSpace(text)
	if text == "" || maxTokens <= 0 {
		return text
	}
	runes := []rune(text)
	if len(runes) == 0 {
		return text
	}
	ratio := nonASCIIRatio(runes)
	charsPerToken := 4.0
	switch {
	case ratio >= 0.35:
		charsPerToken = 1.25
	case ratio >= 0.15:
		charsPerToken = 2.0
	}
	estimatedTokens := int(float64(len(runes))/charsPerToken) + 1
	if estimatedTokens <= maxTokens {
		return text
	}
	maxRunes := int(float64(maxTokens) * charsPerToken)
	if maxRunes < 128 {
		maxRunes = 128
	}
	if maxRunes >= len(runes) {
		return text
	}
	return strings.TrimSpace(string(runes[:maxRunes]))
}

func nonASCIIRatio(runes []rune) float64 {
	if len(runes) == 0 {
		return 0
	}
	nonASCII := 0
	for _, r := range runes {
		if r > 127 {
			nonASCII++
		}
	}
	return float64(nonASCII) / float64(len(runes))
}

func (e *EmbeddingEngine) embedBatchRemoteAware(ctx context.Context, spec embeddingModelSpec, texts []string) ([][]float32, error) {
	switch spec.Provider {
	case embeddingProviderLocal:
		embedder, _, ok := newEmbedderByName(spec.Model)
		if !ok {
			return nil, fmt.Errorf("unknown local embedding model %s", spec.Model)
		}
		out := make([][]float32, len(texts))
		for i, text := range texts {
			out[i] = embedder.Embed(text)
		}
		return out, nil
	case embeddingProviderOpenAI:
		if e.openAIToken == "" {
			return nil, fmt.Errorf("openai embedding unavailable: missing token")
		}
		return e.embedViaHTTP(ctx, e.openAIBase+"/embeddings", e.openAIToken, spec.Model, texts)
	case embeddingProviderOpenRouter:
		if e.openRouterKey == "" {
			return nil, fmt.Errorf("openrouter embedding unavailable: missing api key")
		}
		return e.embedViaHTTP(ctx, e.openRouterAPI+"/embeddings", e.openRouterKey, spec.Model, texts)
	case embeddingProviderOllama:
		return e.embedViaOllama(ctx, spec.Model, texts)
	default:
		return nil, fmt.Errorf("unsupported embedding provider %s", spec.Provider)
	}
}

func (e *EmbeddingEngine) embedViaHTTP(ctx context.Context, endpoint, token, model string, texts []string) ([][]float32, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return nil, fmt.Errorf("embedding endpoint is empty")
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, fmt.Errorf("embedding model is empty")
	}
	if len(texts) == 0 {
		return nil, nil
	}

	e.acquire()
	defer e.release()

	batchSize := e.batchSize
	if batchSize <= 0 {
		batchSize = len(texts)
	}
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += batchSize {
		end := start + batchSize
		if end > len(texts) {
			end = len(texts)
		}
		chunk := texts[start:end]
		vecs, err := e.embedHTTPChunk(ctx, endpoint, token, model, chunk)
		if err != nil {
			return nil, err
		}
		out = append(out, vecs...)
	}
	return out, nil
}

func (e *EmbeddingEngine) embedHTTPChunk(ctx context.Context, endpoint, token, model string, texts []string) ([][]float32, error) {
	body := map[string]any{
		"model": model,
		"input": texts,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal embedding request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("create embedding request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send embedding request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read embedding response: %w", err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("embedding request failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var payload struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &payload); err != nil {
		return nil, fmt.Errorf("parse embedding response: %w", err)
	}
	if len(payload.Data) == 0 {
		return nil, fmt.Errorf("embedding response returned no vectors")
	}

	out := make([][]float32, len(texts))
	for i, item := range payload.Data {
		idx := item.Index
		if idx < 0 || idx >= len(out) {
			idx = i
		}
		vec := item.Embedding
		if len(vec) == 0 {
			continue
		}
		out[idx] = vec
	}
	for i := range out {
		if len(out[i]) == 0 {
			return nil, fmt.Errorf("embedding response missing vector for input index %d", i)
		}
	}
	return out, nil
}

func (e *EmbeddingEngine) embedViaOllama(ctx context.Context, model string, texts []string) ([][]float32, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, fmt.Errorf("ollama embedding model is empty")
	}
	base := strings.TrimRight(strings.TrimSpace(e.ollamaAPI), "/")
	if base == "" {
		base = "http://127.0.0.1:11434"
	}
	if len(texts) == 0 {
		return nil, nil
	}

	e.acquire()
	defer e.release()

	batchVectors, batchErr := e.embedViaOllamaBatchEndpoint(ctx, base, model, texts)
	if batchErr == nil {
		return batchVectors, nil
	}

	singleVectors, singleErr := e.embedViaOllamaLegacyEndpoint(ctx, base, model, texts)
	if singleErr == nil {
		return singleVectors, nil
	}
	return nil, fmt.Errorf("ollama embeddings failed: batch endpoint error: %v; legacy endpoint error: %v", batchErr, singleErr)
}

func (e *EmbeddingEngine) embedViaOllamaBatchEndpoint(ctx context.Context, base, model string, texts []string) ([][]float32, error) {
	body := map[string]any{
		"model": model,
		"input": texts,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal ollama batch request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/embed", bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("create ollama batch request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send ollama batch request: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read ollama batch response: %w", err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("ollama batch request failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var payload struct {
		Embeddings [][]float32 `json:"embeddings"`
		Embedding  []float32   `json:"embedding"`
		Error      string      `json:"error"`
	}
	if err := json.Unmarshal(respBody, &payload); err != nil {
		return nil, fmt.Errorf("parse ollama batch response: %w", err)
	}
	if msg := strings.TrimSpace(payload.Error); msg != "" {
		return nil, fmt.Errorf("ollama batch response error: %s", msg)
	}
	if len(payload.Embeddings) == 0 && len(payload.Embedding) > 0 {
		payload.Embeddings = [][]float32{payload.Embedding}
	}
	if len(payload.Embeddings) != len(texts) {
		return nil, fmt.Errorf("ollama batch endpoint returned %d vectors for %d inputs", len(payload.Embeddings), len(texts))
	}
	for i, vec := range payload.Embeddings {
		if len(vec) == 0 {
			return nil, fmt.Errorf("ollama batch endpoint missing vector for input index %d", i)
		}
	}
	return payload.Embeddings, nil
}

func (e *EmbeddingEngine) embedViaOllamaLegacyEndpoint(ctx context.Context, base, model string, texts []string) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for i, text := range texts {
		body := map[string]any{
			"model":  model,
			"prompt": text,
		}
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal ollama legacy request: %w", err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/embeddings", bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("create ollama legacy request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := e.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("send ollama legacy request: %w", err)
		}
		respBody, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read ollama legacy response: %w", readErr)
		}
		if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
			return nil, fmt.Errorf("ollama legacy request failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
		}
		var payload struct {
			Embedding []float32 `json:"embedding"`
			Error     string    `json:"error"`
		}
		if err := json.Unmarshal(respBody, &payload); err != nil {
			return nil, fmt.Errorf("parse ollama legacy response: %w", err)
		}
		if msg := strings.TrimSpace(payload.Error); msg != "" {
			return nil, fmt.Errorf("ollama legacy response error: %s", msg)
		}
		if len(payload.Embedding) == 0 {
			return nil, fmt.Errorf("ollama legacy endpoint missing vector for input index %d", i)
		}
		out = append(out, payload.Embedding)
	}
	return out, nil
}

func parseEmbeddingModelSpec(raw string) (embeddingModelSpec, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return embeddingModelSpec{}, fmt.Errorf("embedding model is empty")
	}
	lower := strings.ToLower(value)
	if strings.Contains(lower, ":") {
		parts := strings.SplitN(lower, ":", 2)
		provider := strings.TrimSpace(parts[0])
		model := strings.TrimSpace(parts[1])
		if provider == "" || model == "" {
			return embeddingModelSpec{}, fmt.Errorf("invalid embedding model spec %q", raw)
		}
		switch provider {
		case embeddingProviderOpenAI, embeddingProviderOpenRouter:
			return embeddingModelSpec{Provider: provider, Model: model, Raw: provider + ":" + model}, nil
		case embeddingProviderOllama:
			return embeddingModelSpec{Provider: provider, Model: model, Raw: provider + ":" + model}, nil
		case embeddingProviderLocal:
			if _, canonical, ok := newEmbedderByName(model); ok {
				return embeddingModelSpec{Provider: embeddingProviderLocal, Model: canonical, Raw: canonical}, nil
			}
			return embeddingModelSpec{}, fmt.Errorf("unknown local embedding model %q", model)
		default:
			return embeddingModelSpec{}, fmt.Errorf("unknown embedding provider %q", provider)
		}
	}
	if _, canonical, ok := newEmbedderByName(value); ok {
		return embeddingModelSpec{Provider: embeddingProviderLocal, Model: canonical, Raw: canonical}, nil
	}
	// Unqualified non-local models default to OpenAI.
	return embeddingModelSpec{Provider: embeddingProviderOpenAI, Model: value, Raw: embeddingProviderOpenAI + ":" + value}, nil
}

func contentHash(text string) string {
	normalized := strings.TrimSpace(strings.ToLower(text))
	sum := sha1.Sum([]byte(normalized))
	return hex.EncodeToString(sum[:])
}

func (e *EmbeddingEngine) providerKeyFingerprint(provider string) string {
	key := ""
	switch provider {
	case embeddingProviderOpenAI:
		key = e.openAIToken
	case embeddingProviderOpenRouter:
		key = e.openRouterKey
	case embeddingProviderOllama:
		key = e.ollamaAPI
	default:
		key = provider
	}
	if key == "" {
		key = provider
	}
	sum := sha1.Sum([]byte(key))
	return hex.EncodeToString(sum[:8])
}

func (e *EmbeddingEngine) acquire() {
	e.sem <- struct{}{}
}

func (e *EmbeddingEngine) release() {
	select {
	case <-e.sem:
	default:
	}
}
