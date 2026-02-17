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

// HybridRetriever blends lexical and embedding similarity with recency and reranking.
type HybridRetriever struct {
	store  Store
	policy Policy
}

type scoredCandidate struct {
	item        MemoryItem
	lexical     float64
	vector      float64
	recency     float64
	baseScore   float64
	rerankScore float64
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
	if !opts.IncludeSession && !opts.IncludeUser && !opts.IncludeGlobal {
		opts.IncludeSession = true
		opts.IncludeUser = true
		opts.IncludeGlobal = true
	}

	cacheKey := r.cacheKey(query, opts)
	if raw, ok, err := r.store.GetRetrievalCache(ctx, cacheKey, opts.NowMS); err == nil && ok {
		cards := []MemoryCard{}
		if json.Unmarshal([]byte(raw), &cards) == nil {
			_ = r.store.AddMetric(ctx, "memory.recall.cache_hit", 1, map[string]string{"session_key": opts.SessionKey})
			return cards, nil
		}
	}

	candidates, err := r.store.ListMemoryCandidates(ctx, opts.UserID, opts.AgentID, opts.SessionKey, opts.CandidateLimit)
	if err != nil {
		return nil, err
	}
	candidates = filterItemsByScope(candidates, opts.SessionKey, opts.UserID, opts.IncludeSession, opts.IncludeUser, opts.IncludeGlobal)
	if len(candidates) == 0 {
		_ = r.store.AddMetric(ctx, "memory.recall.empty_candidates", 1, map[string]string{"session_key": opts.SessionKey})
		return nil, nil
	}

	lexicalItems := r.lexicalCandidates(ctx, query, opts)
	if len(lexicalItems) == 0 {
		lexicalItems = rankLexicalFallback(candidates, query, opts.CandidateLimit)
	}

	queryVec := embedText(query)
	itemVectors, err := r.ensureVectors(ctx, candidates)
	if err != nil {
		return nil, err
	}

	byID := make(map[string]*scoredCandidate, len(candidates))
	for i := range candidates {
		it := candidates[i]
		s := &scoredCandidate{item: it}
		if vec, ok := itemVectors[it.ID]; ok {
			s.vector = (cosineSimilarity(queryVec, vec) + 1) / 2
		}
		s.recency = recencyWeight(opts.NowMS, it.LastSeenAtMS, opts.RecencyHalfLife)
		byID[it.ID] = s
	}

	for rank, it := range lexicalItems {
		s, ok := byID[it.ID]
		if !ok {
			s = &scoredCandidate{item: it}
			byID[it.ID] = s
		}
		s.lexical = 1.0 - (float64(rank) / float64(len(lexicalItems)+1))
	}

	scored := make([]*scoredCandidate, 0, len(byID))
	for _, s := range byID {
		s.baseScore = r.baseScore(intent, s)
		if s.baseScore < opts.MinScore {
			continue
		}
		s.rerankScore = r.rerankScore(query, intent, s)
		scored = append(scored, s)
	}
	if len(scored) == 0 {
		return nil, nil
	}

	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].rerankScore == scored[j].rerankScore {
			if scored[i].baseScore == scored[j].baseScore {
				return scored[i].item.LastSeenAtMS > scored[j].item.LastSeenAtMS
			}
			return scored[i].baseScore > scored[j].baseScore
		}
		return scored[i].rerankScore > scored[j].rerankScore
	})

	cards := make([]MemoryCard, 0, opts.MaxCards)
	for _, s := range scored {
		card := MemoryCard{
			ID:         s.item.ID,
			Kind:       s.item.Kind,
			Content:    s.item.Content,
			Score:      s.rerankScore,
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

	if raw, mErr := json.Marshal(cards); mErr == nil {
		expires := opts.NowMS + int64(opts.CacheTTL/time.Millisecond)
		_ = r.store.PutRetrievalCache(ctx, cacheKey, string(raw), expires)
	}
	_ = r.store.AddMetric(ctx, "memory.recall.cache_miss", 1, map[string]string{"session_key": opts.SessionKey})
	return cards, nil
}

func (r *HybridRetriever) lexicalCandidates(ctx context.Context, query string, opts RetrievalOptions) []MemoryItem {
	ftsQuery := buildFTSQuery(query)
	if ftsQuery == "" {
		return nil
	}
	found, err := r.store.SearchMemoryFTS(ctx, opts.UserID, opts.AgentID, opts.SessionKey, ftsQuery, opts.CandidateLimit)
	if err != nil {
		_ = r.store.AddMetric(ctx, "memory.recall.fts_error", 1, map[string]string{
			"session_key": opts.SessionKey,
		})
		return nil
	}
	return filterItemsByScope(found, opts.SessionKey, opts.UserID, opts.IncludeSession, opts.IncludeUser, opts.IncludeGlobal)
}

func (r *HybridRetriever) baseScore(intent string, s *scoredCandidate) float64 {
	lexicalWeight := 0.45
	vectorWeight := 0.45
	recencyW := 0.10
	switch intent {
	case "task":
		lexicalWeight = 0.40
		vectorWeight = 0.35
		recencyW = 0.25
	case "preference":
		lexicalWeight = 0.38
		vectorWeight = 0.42
		recencyW = 0.20
	case "identity", "style":
		lexicalWeight = 0.48
		vectorWeight = 0.42
		recencyW = 0.10
	}
	score := lexicalWeight*s.lexical + vectorWeight*s.vector + recencyW*s.recency
	switch intent {
	case "task":
		if s.item.Kind == MemoryTaskState {
			score += 0.18
		}
	case "preference":
		if s.item.Kind == MemoryUserPreference {
			score += 0.18
		}
	case "identity", "style":
		if s.item.Kind == MemorySemanticFact || s.item.Kind == MemoryProcedural {
			score += 0.14
		}
	}
	if s.item.Weight > 0 {
		score *= math.Min(1.5, 0.9+0.1*s.item.Weight)
	}
	return score
}

func (r *HybridRetriever) rerankScore(query, intent string, s *scoredCandidate) float64 {
	score := s.baseScore
	overlap := textTokenJaccard(query, s.item.Content)
	score += overlap * 0.20
	if strings.Contains(strings.ToLower(s.item.Content), strings.ToLower(query)) {
		score += 0.08
	}
	switch intent {
	case "task":
		if s.item.Kind == MemoryTaskState {
			score += 0.05
		}
	case "preference":
		if s.item.Kind == MemoryUserPreference {
			score += 0.05
		}
	case "identity", "style":
		if s.item.Kind == MemorySemanticFact || s.item.Kind == MemoryProcedural {
			score += 0.03
		}
	}
	return score
}

func recencyWeight(nowMS, seenMS int64, halfLife time.Duration) float64 {
	deltaMS := float64(nowMS - seenMS)
	if deltaMS < 0 {
		deltaMS = 0
	}
	hl := float64(halfLife / time.Millisecond)
	if hl <= 0 {
		hl = float64((14 * 24 * time.Hour) / time.Millisecond)
	}
	return math.Exp(-math.Ln2 * deltaMS / hl)
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
	model := currentEmbeddingModel()
	for _, it := range items {
		if _, ok := vectors[it.ID]; ok {
			continue
		}
		vec := embedText(it.Content)
		if err := r.store.UpsertEmbedding(ctx, it.ID, model, vec); err != nil {
			return nil, err
		}
		vectors[it.ID] = vec
	}
	return vectors, nil
}

func (r *HybridRetriever) cacheKey(query string, opts RetrievalOptions) string {
	recencySec := int64(opts.RecencyHalfLife / time.Second)
	payload := fmt.Sprintf("%s|%s|%s|%s|%d|%d|%.3f|%t|%t|%t|%d|%s",
		strings.ToLower(strings.TrimSpace(query)),
		opts.SessionKey,
		opts.UserID,
		opts.AgentID,
		opts.MaxCards,
		opts.CandidateLimit,
		opts.MinScore,
		opts.IncludeSession,
		opts.IncludeUser,
		opts.IncludeGlobal,
		recencySec,
		currentEmbeddingModel(),
	)
	h := sha1.Sum([]byte(payload))
	return fmt.Sprintf("recall:%s", hex.EncodeToString(h[:]))
}

func buildFTSQuery(query string) string {
	tokens := ftsTokens(query)
	if len(tokens) == 0 {
		return ""
	}
	quoted := make([]string, 0, len(tokens))
	for _, tok := range tokens {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		tok = strings.ReplaceAll(tok, `"`, `""`)
		quoted = append(quoted, `"`+tok+`"`)
	}
	return strings.Join(quoted, " OR ")
}

func filterItemsByScope(items []MemoryItem, sessionKey, userID string, includeSession, includeUser, includeGlobal bool) []MemoryItem {
	if len(items) == 0 {
		return nil
	}
	filtered := make([]MemoryItem, 0, len(items))
	for _, it := range items {
		normalizeMemoryScope(&it)
		switch it.ScopeType {
		case MemoryScopeSession:
			if includeSession && strings.TrimSpace(sessionKey) != "" && it.ScopeID == sessionKey {
				filtered = append(filtered, it)
			}
		case MemoryScopeUser:
			if includeUser && strings.TrimSpace(userID) != "" && it.ScopeID == userID {
				filtered = append(filtered, it)
			}
		case MemoryScopeGlobal:
			if includeGlobal {
				filtered = append(filtered, it)
			}
		}
	}
	return filtered
}

func rankLexicalFallback(items []MemoryItem, query string, limit int) []MemoryItem {
	if len(items) == 0 {
		return nil
	}
	if limit <= 0 {
		limit = 20
	}
	terms := ftsTokens(query)
	if len(terms) == 0 {
		terms = tokenize(query)
	}
	type itemScore struct {
		item  MemoryItem
		score int
	}
	scored := make([]itemScore, 0, len(items))
	for _, it := range items {
		lower := strings.ToLower(it.Content)
		score := 0
		for _, term := range terms {
			if term != "" && strings.Contains(lower, term) {
				score++
			}
		}
		if strings.Contains(lower, strings.ToLower(strings.TrimSpace(query))) {
			score += 2
		}
		if score > 0 {
			scored = append(scored, itemScore{item: it, score: score})
		}
	}
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].score == scored[j].score {
			return scored[i].item.LastSeenAtMS > scored[j].item.LastSeenAtMS
		}
		return scored[i].score > scored[j].score
	})
	out := make([]MemoryItem, 0, limit)
	for _, s := range scored {
		out = append(out, s.item)
		if len(out) >= limit {
			break
		}
	}
	return out
}

func ftsTokens(query string) []string {
	raw := tokenize(query)
	if len(raw) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(raw)*2)
	for _, tok := range raw {
		for _, part := range strings.FieldsFunc(tok, func(r rune) bool {
			return !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
		}) {
			part = strings.TrimSpace(strings.ToLower(part))
			if len(part) < 2 {
				continue
			}
			if _, ok := seen[part]; ok {
				continue
			}
			seen[part] = struct{}{}
			out = append(out, part)
		}
	}
	return out
}
