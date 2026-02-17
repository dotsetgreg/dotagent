package memory

import (
	"hash/fnv"
	"math"
	"regexp"
	"strings"
)

const (
	embeddingModel = "dotagent-hash-256-v1"
	embeddingDims  = 256
)

var tokenPattern = regexp.MustCompile(`[A-Za-z0-9_\-]+`)

func embedText(text string) []float32 {
	vec := make([]float32, embeddingDims)
	for _, token := range tokenize(text) {
		h := fnv.New64a()
		_, _ = h.Write([]byte(token))
		sum := h.Sum64()
		idx := int(sum % embeddingDims)
		sign := float32(1)
		if sum&1 == 1 {
			sign = -1
		}
		weight := float32(1 + (len(token) / 8))
		vec[idx] += sign * weight
	}
	n := vectorNorm(vec)
	if n > 0 {
		inv := float32(1.0 / n)
		for i := range vec {
			vec[i] *= inv
		}
	}
	return vec
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
