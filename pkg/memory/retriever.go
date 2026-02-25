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
	store                   Store
	policy                  Policy
	embeddingEngine         *EmbeddingEngine
	embeddingFallbackModels []string
}

type HybridRetrieverOptions struct {
	EmbeddingEngine         *EmbeddingEngine
	EmbeddingFallbackModels []string
}

type scoredCandidate struct {
	item        MemoryItem
	lexical     float64
	vector      float64
	recency     float64
	evergreen   bool
	baseScore   float64
	rerankScore float64
}

func NewHybridRetriever(store Store, policy Policy, opts ...HybridRetrieverOptions) *HybridRetriever {
	r := &HybridRetriever{store: store, policy: policy}
	if len(opts) > 0 {
		opt := opts[0]
		r.embeddingEngine = opt.EmbeddingEngine
		r.embeddingFallbackModels = dedupeEmbeddingModels(opt.EmbeddingFallbackModels)
	}
	if len(r.embeddingFallbackModels) == 0 {
		r.embeddingFallbackModels = []string{currentEmbeddingModel(), hashEmbeddingModel}
	}
	return r
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

	queryVec, embeddingModel, itemVectors, err := r.embeddingVectorsForQuery(ctx, query, candidates)
	if err != nil {
		_ = r.store.AddMetric(ctx, "memory.recall.embedding_error", 1, map[string]string{
			"session_key": opts.SessionKey,
		})
		queryVec = nil
		itemVectors = map[string][]float32{}
	}
	_ = embeddingModel

	byID := make(map[string]*scoredCandidate, len(candidates))
	for i := range candidates {
		it := candidates[i]
		s := &scoredCandidate{item: it}
		if len(queryVec) > 0 {
			if vec, ok := itemVectors[it.ID]; ok {
				s.vector = (cosineSimilarity(queryVec, vec) + 1) / 2
			}
		}
		s.evergreen = it.Evergreen
		if it.Evergreen {
			s.recency = 1
		} else {
			s.recency = recencyWeight(opts.NowMS, it.LastSeenAtMS, opts.RecencyHalfLife)
		}
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

	scored = suppressDuplicateCandidates(scored)
	if len(scored) == 0 {
		return nil, nil
	}
	selected := selectMMRDiverseCandidates(scored, itemVectors, opts.MaxCards)

	cards := make([]MemoryCard, 0, opts.MaxCards)
	for _, s := range selected {
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
	if s.evergreen {
		score += 0.08
	}
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
		r.embeddingCacheToken(),
	)
	h := sha1.Sum([]byte(payload))
	return fmt.Sprintf("recall:%s", hex.EncodeToString(h[:]))
}

type embeddingRecordReader interface {
	GetEmbeddingRecords(ctx context.Context, itemIDs []string) (map[string]EmbeddingRecord, error)
}

func (r *HybridRetriever) embeddingCacheToken() string {
	if len(r.embeddingFallbackModels) == 0 {
		return currentEmbeddingModel()
	}
	return strings.Join(r.embeddingFallbackModels, ",")
}

func (r *HybridRetriever) embeddingVectorsForQuery(ctx context.Context, query string, items []MemoryItem) ([]float32, string, map[string][]float32, error) {
	if len(items) == 0 {
		return nil, "", map[string][]float32{}, nil
	}
	if r.embeddingEngine != nil {
		model, vectors, err := r.embeddingEngine.EmbedBatch(ctx, r.embeddingFallbackModels, []string{query})
		if err != nil {
			return nil, "", nil, err
		}
		if len(vectors) == 0 || len(vectors[0]) == 0 {
			return nil, "", nil, fmt.Errorf("embedding query vector is empty")
		}
		itemVectors, err := r.ensureVectorsForModel(ctx, items, model)
		if err != nil {
			return nil, "", nil, err
		}
		return vectors[0], model, itemVectors, nil
	}

	queryVec := embedText(query)
	itemVectors, err := r.ensureVectorsLegacy(ctx, items)
	if err != nil {
		return nil, "", nil, err
	}
	return queryVec, currentEmbeddingModel(), itemVectors, nil
}

func (r *HybridRetriever) ensureVectorsLegacy(ctx context.Context, items []MemoryItem) (map[string][]float32, error) {
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

func (r *HybridRetriever) ensureVectorsForModel(ctx context.Context, items []MemoryItem, model string) (map[string][]float32, error) {
	if strings.TrimSpace(model) == "" {
		return r.ensureVectorsLegacy(ctx, items)
	}
	ids := make([]string, 0, len(items))
	contentByID := make(map[string]string, len(items))
	for _, it := range items {
		ids = append(ids, it.ID)
		contentByID[it.ID] = it.Content
	}

	out := make(map[string][]float32, len(items))
	missingIDs := make([]string, 0, len(items))
	missingTexts := make([]string, 0, len(items))

	if reader, ok := r.store.(embeddingRecordReader); ok {
		records, err := reader.GetEmbeddingRecords(ctx, ids)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			rec, exists := records[id]
			if exists && strings.EqualFold(strings.TrimSpace(rec.Model), strings.TrimSpace(model)) && len(rec.Vector) > 0 {
				out[id] = rec.Vector
				continue
			}
			missingIDs = append(missingIDs, id)
			missingTexts = append(missingTexts, contentByID[id])
		}
	} else {
		vectors, err := r.store.GetEmbeddings(ctx, ids)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			if vec, ok := vectors[id]; ok && len(vec) > 0 {
				out[id] = vec
				continue
			}
			missingIDs = append(missingIDs, id)
			missingTexts = append(missingTexts, contentByID[id])
		}
	}

	if len(missingIDs) == 0 {
		return out, nil
	}

	if r.embeddingEngine == nil {
		for i, id := range missingIDs {
			vec, modelID, err := embedTextWithModel(model, missingTexts[i])
			if err != nil {
				return nil, err
			}
			if err := r.store.UpsertEmbedding(ctx, id, modelID, vec); err != nil {
				return nil, err
			}
			out[id] = vec
		}
		return out, nil
	}

	_, vectors, err := r.embeddingEngine.EmbedBatch(ctx, []string{model}, missingTexts)
	if err != nil {
		return nil, err
	}
	if len(vectors) != len(missingIDs) {
		return nil, fmt.Errorf("embedding model %s returned %d vectors for %d inputs", model, len(vectors), len(missingIDs))
	}
	for i, id := range missingIDs {
		if err := r.store.UpsertEmbedding(ctx, id, model, vectors[i]); err != nil {
			return nil, err
		}
		out[id] = vectors[i]
	}
	return out, nil
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

func suppressDuplicateCandidates(in []*scoredCandidate) []*scoredCandidate {
	if len(in) <= 1 {
		return in
	}
	seenIdentity := map[string]struct{}{}
	seenFingerprint := map[string]struct{}{}
	seenSemantic := map[string]struct{}{}
	out := make([]*scoredCandidate, 0, len(in))
	for _, cand := range in {
		if cand == nil {
			continue
		}
		identity := strings.ToLower(strings.TrimSpace(string(cand.item.Kind) + "|" + cand.item.Key + "|" + string(cand.item.ScopeType) + "|" + cand.item.ScopeID))
		if identity != "" {
			if _, exists := seenIdentity[identity]; exists {
				continue
			}
		}

		fingerprint := contentFingerprint(cand.item.Content)
		if fingerprint != "" {
			if _, exists := seenFingerprint[fingerprint]; exists {
				continue
			}
		}
		semantic := semanticFingerprint(cand.item.Content)
		if semantic != "" {
			if _, exists := seenSemantic[semantic]; exists {
				continue
			}
		}

		isNearDup := false
		for _, existing := range out {
			if nearDuplicateContent(existing.item.Content, cand.item.Content) {
				isNearDup = true
				break
			}
		}
		if isNearDup {
			continue
		}

		out = append(out, cand)
		if identity != "" {
			seenIdentity[identity] = struct{}{}
		}
		if fingerprint != "" {
			seenFingerprint[fingerprint] = struct{}{}
		}
		if semantic != "" {
			seenSemantic[semantic] = struct{}{}
		}
	}
	return out
}

func selectMMRDiverseCandidates(scored []*scoredCandidate, vectors map[string][]float32, maxCards int) []*scoredCandidate {
	if len(scored) == 0 {
		return nil
	}
	if maxCards <= 0 {
		maxCards = 8
	}
	if len(scored) <= maxCards {
		return scored
	}

	const lambda = 0.72
	pool := append([]*scoredCandidate(nil), scored...)
	selected := make([]*scoredCandidate, 0, maxCards)

	for len(selected) < maxCards && len(pool) > 0 {
		bestIdx := 0
		bestScore := -1e9
		for i, cand := range pool {
			penalty := 0.0
			for _, chosen := range selected {
				sim := candidateSimilarity(cand, chosen, vectors)
				if sim > penalty {
					penalty = sim
				}
			}
			mmr := lambda*cand.rerankScore - (1-lambda)*penalty
			if mmr > bestScore {
				bestScore = mmr
				bestIdx = i
			}
		}
		selected = append(selected, pool[bestIdx])
		pool = append(pool[:bestIdx], pool[bestIdx+1:]...)
	}
	return selected
}

func candidateSimilarity(a, b *scoredCandidate, vectors map[string][]float32) float64 {
	if a == nil || b == nil {
		return 0
	}
	textSim := textTokenJaccard(a.item.Content, b.item.Content)
	if vectors != nil {
		va, okA := vectors[a.item.ID]
		vb, okB := vectors[b.item.ID]
		if okA && okB {
			vecSim := (cosineSimilarity(va, vb) + 1) / 2
			return 0.70*vecSim + 0.30*textSim
		}
	}
	return textSim
}

func contentFingerprint(content string) string {
	content = strings.ToLower(strings.TrimSpace(content))
	if content == "" {
		return ""
	}
	tokens := tokenize(content)
	if len(tokens) == 0 {
		return content
	}
	if len(tokens) > 16 {
		tokens = tokens[:16]
	}
	return strings.Join(tokens, "|")
}

func semanticFingerprint(content string) string {
	content = strings.TrimSpace(strings.ToLower(content))
	if content == "" {
		return ""
	}
	stop := map[string]struct{}{
		"i": {}, "my": {}, "me": {}, "the": {}, "a": {}, "an": {}, "to": {}, "in": {}, "of": {}, "and": {},
		"also": {}, "really": {}, "strongly": {}, "very": {}, "that": {}, "this": {}, "is": {}, "am": {}, "are": {},
	}
	parts := make([]string, 0, 12)
	for _, tok := range ftsTokens(content) {
		if _, skip := stop[tok]; skip {
			continue
		}
		if len(tok) <= 2 {
			continue
		}
		parts = append(parts, tok)
	}
	if len(parts) == 0 {
		return ""
	}
	sort.Strings(parts)
	if len(parts) > 8 {
		parts = parts[:8]
	}
	return strings.Join(parts, "|")
}

func nearDuplicateContent(a, b string) bool {
	a = strings.TrimSpace(strings.ToLower(a))
	b = strings.TrimSpace(strings.ToLower(b))
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	jacc := tokenJaccardNormalized(a, b)
	if jacc >= 0.70 {
		return true
	}
	return strings.Contains(a, b) || strings.Contains(b, a)
}

func tokenJaccardNormalized(a, b string) float64 {
	aTokens := duplicateTokens(a)
	bTokens := duplicateTokens(b)
	if len(aTokens) == 0 || len(bTokens) == 0 {
		return 0
	}
	aSet := make(map[string]struct{}, len(aTokens))
	for _, tok := range aTokens {
		aSet[tok] = struct{}{}
	}
	inter := 0
	union := len(aSet)
	seenB := map[string]struct{}{}
	for _, tok := range bTokens {
		if _, seen := seenB[tok]; seen {
			continue
		}
		seenB[tok] = struct{}{}
		if _, ok := aSet[tok]; ok {
			inter++
		} else {
			union++
		}
	}
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

func duplicateTokens(text string) []string {
	text = strings.ToLower(strings.TrimSpace(text))
	if text == "" {
		return nil
	}
	text = strings.ReplaceAll(text, "-", " ")
	raw := tokenize(text)
	seen := map[string]struct{}{}
	out := make([]string, 0, len(raw))
	for _, token := range raw {
		token = strings.TrimSpace(token)
		if len(token) < 2 {
			continue
		}
		if isStopwordToken(token) {
			continue
		}
		if _, ok := seen[token]; ok {
			continue
		}
		seen[token] = struct{}{}
		out = append(out, token)
	}
	return out
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
	return expandQueryTerms(query)
}
