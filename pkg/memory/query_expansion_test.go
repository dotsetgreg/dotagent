package memory

import "testing"

func TestExpandQueryTerms_RemovesStopWordsAndKeepsKeywords(t *testing.T) {
	terms := expandQueryTerms("what was that thing we discussed about the API design")
	if len(terms) == 0 {
		t.Fatalf("expected expanded terms")
	}
	for _, term := range terms {
		if term == "the" || term == "about" || term == "thing" {
			t.Fatalf("expected stopword filtering, found %q", term)
		}
	}
	foundAPI := false
	for _, term := range terms {
		if term == "api" || term == "api design" {
			foundAPI = true
			break
		}
	}
	if !foundAPI {
		t.Fatalf("expected API keyword to survive expansion: %#v", terms)
	}
}

func TestExpandQueryTerms_CJKExpansion(t *testing.T) {
	terms := expandQueryTerms("之前讨论的接口设计")
	if len(terms) == 0 {
		t.Fatalf("expected CJK terms")
	}
	foundBigram := false
	for _, term := range terms {
		if term == "接口" || term == "设计" {
			foundBigram = true
			break
		}
	}
	if !foundBigram {
		t.Fatalf("expected CJK bigrams in terms: %#v", terms)
	}
}

func TestExpandQueryTerms_FallbackWhenStopwordsWouldEmptyTerms(t *testing.T) {
	terms := expandQueryTerms("the and or to")
	if len(terms) == 0 {
		t.Fatalf("expected fallback terms for stopword-only query")
	}
}
