package memory

import (
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

var unicodeQueryTokenPattern = regexp.MustCompile(`[\p{L}\p{N}_-]+`)

var multilingualStopwords = buildMultilingualStopwords()

func buildMultilingualStopwords() map[string]struct{} {
	terms := []string{
		// English
		"the", "a", "an", "and", "or", "but", "to", "of", "in", "on", "for", "with", "as", "at", "by", "from", "is", "are", "was", "were", "be", "been", "being", "it", "that", "this", "these", "those", "my", "your", "our", "their", "about", "thing", "what", "which", "who", "whom", "when", "where", "why", "how",
		// Spanish
		"el", "la", "los", "las", "un", "una", "unos", "unas", "y", "o", "de", "del", "en", "para", "con", "como", "es", "son", "fue", "mi", "tu", "su",
		// Portuguese
		"os", "as", "um", "uma", "uns", "umas", "e", "ou", "do", "da", "dos", "das", "no", "na", "nos", "nas", "com", "por", "seu", "sua",
		// Arabic common particles
		"و", "في", "من", "على", "الى", "إلى", "عن", "هذا", "هذه", "ذلك", "التي", "الذي", "مع",
	}
	out := make(map[string]struct{}, len(terms))
	for _, term := range terms {
		out[term] = struct{}{}
	}
	return out
}

func expandQueryTerms(query string) []string {
	query = strings.TrimSpace(strings.ToLower(query))
	if query == "" {
		return nil
	}

	baseTokens := unicodeQueryTokenPattern.FindAllString(query, -1)
	if len(baseTokens) == 0 {
		return nil
	}

	terms := make([]string, 0, len(baseTokens)*3)
	latinTerms := make([]string, 0, len(baseTokens))
	for _, token := range baseTokens {
		token = strings.TrimSpace(strings.ToLower(token))
		if token == "" {
			continue
		}
		if containsCJK(token) {
			terms = append(terms, cjkNgrams(token, 2, 3)...)
			continue
		}
		if isHangulWord(token) {
			token = stripKoreanParticle(token)
		}
		if len(token) < 2 || isStopwordToken(token) {
			continue
		}
		terms = append(terms, token)
		latinTerms = append(latinTerms, token)
		if singular := singularizeToken(token); singular != token && len(singular) >= 3 {
			terms = append(terms, singular)
		}
	}

	// Add lightweight phrase expansion for better FTS recall.
	for i := 0; i+1 < len(latinTerms) && i < 6; i++ {
		terms = append(terms, latinTerms[i]+" "+latinTerms[i+1])
	}
	if len(terms) == 0 {
		for _, token := range baseTokens {
			token = strings.TrimSpace(strings.ToLower(token))
			if token == "" {
				continue
			}
			// Keep at least one lexical anchor to avoid zero-term lookups.
			if containsCJK(token) {
				terms = append(terms, cjkNgrams(token, 1, 2)...)
				continue
			}
			if len(token) >= 2 {
				terms = append(terms, token)
			}
		}
	}

	return dedupeQueryTerms(terms)
}

func dedupeQueryTerms(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(strings.ToLower(value))
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if len(out[i]) == len(out[j]) {
			return out[i] < out[j]
		}
		return len(out[i]) > len(out[j])
	})
	return out
}

func isStopwordToken(token string) bool {
	if token == "" {
		return true
	}
	_, ok := multilingualStopwords[token]
	return ok
}

func singularizeToken(token string) string {
	switch {
	case strings.HasSuffix(token, "ies") && len(token) > 4:
		return token[:len(token)-3] + "y"
	case strings.HasSuffix(token, "es") && len(token) > 4:
		return token[:len(token)-2]
	case strings.HasSuffix(token, "s") && len(token) > 3:
		return token[:len(token)-1]
	default:
		return token
	}
}

func containsCJK(token string) bool {
	for _, r := range token {
		if unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana) {
			return true
		}
	}
	return false
}

func isHangulWord(token string) bool {
	for _, r := range token {
		if unicode.In(r, unicode.Hangul) {
			return true
		}
	}
	return false
}

func stripKoreanParticle(token string) string {
	particles := []string{"은", "는", "이", "가", "을", "를", "에", "에서", "와", "과", "으로", "로"}
	for _, suffix := range particles {
		if strings.HasSuffix(token, suffix) {
			candidate := strings.TrimSuffix(token, suffix)
			if utf8.RuneCountInString(candidate) >= 2 {
				return candidate
			}
		}
	}
	return token
}

func cjkNgrams(text string, minN, maxN int) []string {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) == 0 {
		return nil
	}
	if minN <= 0 {
		minN = 1
	}
	if maxN < minN {
		maxN = minN
	}
	out := make([]string, 0, len(runes)*2)
	for n := minN; n <= maxN; n++ {
		if len(runes) < n {
			continue
		}
		for i := 0; i+n <= len(runes); i++ {
			out = append(out, string(runes[i:i+n]))
		}
	}
	return out
}
