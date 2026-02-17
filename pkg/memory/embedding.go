package memory

import (
	"hash/fnv"
	"math"
	"regexp"
	"strings"
	"sync/atomic"
)

type Embedder interface {
	ModelID() string
	Embed(text string) []float32
}

const (
	defaultEmbeddingModel = "dotagent-chargram-384-v1"
	hashEmbeddingModel    = "dotagent-hash-256-v1"
)

var tokenPattern = regexp.MustCompile(`[A-Za-z0-9_\-]+`)

type hashEmbedder struct {
	dims    int
	modelID string
}

func (e *hashEmbedder) ModelID() string { return e.modelID }

func (e *hashEmbedder) Embed(text string) []float32 {
	vec := make([]float32, e.dims)
	for _, token := range tokenize(text) {
		h := fnv.New64a()
		_, _ = h.Write([]byte(token))
		sum := h.Sum64()
		idx := int(sum % uint64(e.dims))
		sign := float32(1)
		if sum&1 == 1 {
			sign = -1
		}
		weight := float32(1 + (len(token) / 8))
		vec[idx] += sign * weight
	}
	normalizeVector(vec)
	return vec
}

type chargramEmbedder struct {
	dims    int
	modelID string
}

func (e *chargramEmbedder) ModelID() string { return e.modelID }

func (e *chargramEmbedder) Embed(text string) []float32 {
	vec := make([]float32, e.dims)
	normalized := strings.ToLower(strings.TrimSpace(text))
	if normalized == "" {
		return vec
	}
	window := "#" + normalized + "#"
	for i := 0; i+3 <= len(window); i++ {
		gram := window[i : i+3]
		h := fnv.New64a()
		_, _ = h.Write([]byte(gram))
		sum := h.Sum64()
		idx := int(sum % uint64(e.dims))
		vec[idx] += 1
	}
	for _, token := range tokenize(normalized) {
		h := fnv.New64a()
		_, _ = h.Write([]byte("tok:" + token))
		sum := h.Sum64()
		idx := int(sum % uint64(e.dims))
		vec[idx] += 1.25
	}
	normalizeVector(vec)
	return vec
}

type embedderState struct {
	embedder Embedder
}

var activeEmbedder atomic.Pointer[embedderState]

func init() {
	SetEmbedderByName(defaultEmbeddingModel)
}

func SetEmbedderByName(name string) {
	name = strings.ToLower(strings.TrimSpace(name))
	switch name {
	case "", defaultEmbeddingModel, "chargram", "chargram-384":
		SetEmbedder(&chargramEmbedder{dims: 384, modelID: defaultEmbeddingModel})
	case hashEmbeddingModel, "hash", "hash-256":
		SetEmbedder(&hashEmbedder{dims: 256, modelID: hashEmbeddingModel})
	default:
		SetEmbedder(&chargramEmbedder{dims: 384, modelID: defaultEmbeddingModel})
	}
}

func SetEmbedder(embedder Embedder) {
	if embedder == nil {
		embedder = &chargramEmbedder{dims: 384, modelID: defaultEmbeddingModel}
	}
	activeEmbedder.Store(&embedderState{embedder: embedder})
}

func currentEmbedder() Embedder {
	st := activeEmbedder.Load()
	if st == nil || st.embedder == nil {
		def := &chargramEmbedder{dims: 384, modelID: defaultEmbeddingModel}
		activeEmbedder.Store(&embedderState{embedder: def})
		return def
	}
	return st.embedder
}

func currentEmbeddingModel() string {
	return currentEmbedder().ModelID()
}

func embedText(text string) []float32 {
	return currentEmbedder().Embed(text)
}

func tokenize(text string) []string {
	text = strings.ToLower(text)
	matches := tokenPattern.FindAllString(text, -1)
	if len(matches) == 0 {
		return []string{text}
	}
	return matches
}

func vectorNorm(vec []float32) float64 {
	if len(vec) == 0 {
		return 0
	}
	var sum float64
	for _, v := range vec {
		sum += float64(v * v)
	}
	return math.Sqrt(sum)
}

func normalizeVector(vec []float32) {
	n := vectorNorm(vec)
	if n == 0 {
		return
	}
	inv := float32(1.0 / n)
	for i := range vec {
		vec[i] *= inv
	}
}

func cosineSimilarity(a, b []float32) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	var dot float64
	for i := 0; i < n; i++ {
		dot += float64(a[i] * b[i])
	}
	return dot
}
