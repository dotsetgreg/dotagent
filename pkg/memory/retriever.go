package memory

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// HybridRetriever blends lexical and vector recall with recency scoring.
type HybridRetriever struct {
	store  Store
	policy Policy
}

func NewHybridRetriever(store Store, policy Policy) *HybridRetriever {
	return &HybridRetriever{store: store, policy: policy}
}

func (r *HybridRetriever) Recall(ctx context.Context, query string, opts RetrievalOptions) ([]MemoryCard, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	intent := detectQueryIntent(query)
	if opts.NowMS == 0 {
		opts.NowMS = time.Now().UnixMilli()
	}
	if opts.MaxCards <= 0 {
		opts.MaxCards = 8
	}
	if opts.CandidateLimit <= 0 {
		opts.CandidateLimit = 80
	}
	if opts.MinScore <= 0 {
		opts.MinScore = 0.32
	}
	if opts.CacheTTL <= 0 {
		opts.CacheTTL = 20 * time.Second
	}
	if opts.RecencyHalfLife <= 0 {
		opts.RecencyHalfLife = 14 * 24 * time.Hour
	}
	if !opts.IncludeSession && !opts.IncludeGlobal {
		return nil, nil
	}

	cacheKey := r.cacheKey(query, opts)
	if raw, ok, err := r.store.GetRetrievalCache(ctx, cacheKey, opts.NowMS); err == nil && ok {
		cards := []MemoryCard{}
		if json.Unmarshal([]byte(raw), &cards) == nil {
			_ = r.store.AddMetric(ctx, "memory.recall.cache_hit", 1, map[string]string{"session_key": opts.SessionKey})
			return cards, nil
		}
	}

	ftsQuery := buildFTSQuery(query)
	lexicalItems, err := r.store.SearchMemoryFTS(ctx, opts.UserID, opts.AgentID, opts.SessionKey, ftsQuery, opts.CandidateLimit)
	if err != nil {
		return nil, err
	}

	vectorCandidates, err := r.store.ListMemoryCandidates(ctx, opts.UserID, opts.AgentID, opts.SessionKey, opts.CandidateLimit)
	if err != nil {
		return nil, err
	}
	lexicalItems = filterItemsByScope(lexicalItems, opts.SessionKey, opts.IncludeSession, opts.IncludeGlobal)
	vectorCandidates = filterItemsByScope(vectorCandidates, opts.SessionKey, opts.IncludeSession, opts.IncludeGlobal)

	queryVec := embedText(query)
	itemVectors, err := r.ensureVectors(ctx, vectorCandidates)
	if err != nil {
		return nil, err
	}

	type scored struct {
		item    MemoryItem
		lexical float64
		vector  float64
		recency float64
		score   float64
	}

	scores := map[string]*scored{}
	for i := range vectorCandidates {
		it := vectorCandidates[i]
		s := &scored{item: it}
		if vec, ok := itemVectors[it.ID]; ok {
			s.vector = (cosineSimilarity(queryVec, vec) + 1.0) / 2.0
		}
		deltaMS := float64(opts.NowMS - it.LastSeenAtMS)
		if deltaMS < 0 {
			deltaMS = 0
		}
		halfLife := float64(opts.RecencyHalfLife / time.Millisecond)
		if halfLife <= 0 {
			halfLife = float64((14 * 24 * time.Hour) / time.Millisecond)
		}
		s.recency = math.Exp(-math.Ln2 * deltaMS / halfLife)
		scores[it.ID] = s
	}

	for rank, it := range lexicalItems {
		s, ok := scores[it.ID]
		if !ok {
			s = &scored{item: it}
			scores[it.ID] = s
		}
		s.lexical = 1.0 - (float64(rank) / float64(len(lexicalItems)+1))
	}

	ordered := make([]*scored, 0, len(scores))
	for _, s := range scores {
		lexicalWeight := 0.45
		vectorWeight := 0.45
		recencyWeight := 0.10
		switch intent {
		case "task":
			lexicalWeight = 0.40
			vectorWeight = 0.35
			recencyWeight = 0.25
		case "preference":
			lexicalWeight = 0.38
			vectorWeight = 0.42
			recencyWeight = 0.20
		case "identity", "style":
			lexicalWeight = 0.48
			vectorWeight = 0.42
			recencyWeight = 0.10
		}

		s.score = lexicalWeight*s.lexical + vectorWeight*s.vector + recencyWeight*s.recency

		switch intent {
		case "task":
			if s.item.Kind == MemoryTaskState {
				s.score += 0.18
			}
		case "preference":
			if s.item.Kind == MemoryUserPreference {
				s.score += 0.18
			}
		case "identity", "style":
			if s.item.Kind == MemorySemanticFact || s.item.Kind == MemoryProcedural {
				s.score += 0.14
			}
		}
		if s.item.Weight > 0 {
			s.score *= math.Min(1.5, 0.9+0.1*s.item.Weight)
		}
		if s.score >= opts.MinScore {
			ordered = append(ordered, s)
		}
	}

	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].score == ordered[j].score {
			return ordered[i].item.LastSeenAtMS > ordered[j].item.LastSeenAtMS
		}
		return ordered[i].score > ordered[j].score
	})

	cards := make([]MemoryCard, 0, opts.MaxCards)
	for _, s := range ordered {
		card := MemoryCard{
			ID:         s.item.ID,
			Kind:       s.item.Kind,
			Content:    s.item.Content,
			Score:      s.score,
			Confidence: s.item.Confidence,
			RecencyMS:  s.item.LastSeenAtMS,
			Source:     s.item.SourceEventID,
		}
		if r.policy != nil && !r.policy.ShouldRecall(card) {
			continue
		}
		cards = append(cards, card)
		if len(cards) >= opts.MaxCards {
			break
		}
	}

	if raw, err := json.Marshal(cards); err == nil {
		expires := opts.NowMS + int64(opts.CacheTTL/time.Millisecond)
		_ = r.store.PutRetrievalCache(ctx, cacheKey, string(raw), expires)
	}
	_ = r.store.AddMetric(ctx, "memory.recall.cache_miss", 1, map[string]string{"session_key": opts.SessionKey})

	return cards, nil
}

func (r *HybridRetriever) ensureVectors(ctx context.Context, items []MemoryItem) (map[string][]float32, error) {
	ids := make([]string, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ID)
	}

	vectors, err := r.store.GetEmbeddings(ctx, ids)
	if err != nil {
		return nil, err
	}

	for _, it := range items {
		if _, ok := vectors[it.ID]; ok {
			continue
		}
		vec := embedText(it.Content)
		if err := r.store.UpsertEmbedding(ctx, it.ID, embeddingModel, vec); err != nil {
			return nil, err
		}
		vectors[it.ID] = vec
	}

	return vectors, nil
}

func (r *HybridRetriever) cacheKey(query string, opts RetrievalOptions) string {
	recencySec := int64(opts.RecencyHalfLife / time.Second)
	payload := fmt.Sprintf("%s|%s|%s|%s|%d|%d|%.3f|%t|%t|%d",
		strings.ToLower(strings.TrimSpace(query)),
		opts.SessionKey,
		opts.UserID,
		opts.AgentID,
		opts.MaxCards,
		opts.CandidateLimit,
		opts.MinScore,
		opts.IncludeSession,
		opts.IncludeGlobal,
		recencySec,
	)
	h := sha1.Sum([]byte(payload))
	return fmt.Sprintf("recall:%s", hex.EncodeToString(h[:]))
}

func buildFTSQuery(query string) string {
	tokens := tokenize(query)
	if len(tokens) == 0 {
		return query
	}
	for i := range tokens {
		tokens[i] = strings.ReplaceAll(tokens[i], "\"", "")
	}
	if len(tokens) == 1 {
		return tokens[0]
	}
	return strings.Join(tokens, " OR ")
}

func filterItemsByScope(items []MemoryItem, sessionKey string, includeSession, includeGlobal bool) []MemoryItem {
	if len(items) == 0 {
		return nil
	}
	filtered := make([]MemoryItem, 0, len(items))
	for _, it := range items {
		if it.SessionKey == "" {
			if includeGlobal {
				filtered = append(filtered, it)
			}
			continue
		}
		if includeSession && (sessionKey == "" || it.SessionKey == sessionKey) {
			filtered = append(filtered, it)
		}
	}
	return filtered
}
