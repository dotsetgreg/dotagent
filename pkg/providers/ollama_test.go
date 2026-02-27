package providers

import "testing"

func TestNormalizeOllamaAPIBase(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{raw: "http://127.0.0.1:11434", want: "http://127.0.0.1:11434/v1"},
		{raw: "http://127.0.0.1:11434/", want: "http://127.0.0.1:11434/v1"},
		{raw: "http://127.0.0.1:11434/v1", want: "http://127.0.0.1:11434/v1"},
		{raw: "http://127.0.0.1:11434/v1/chat/completions", want: "http://127.0.0.1:11434/v1"},
	}
	for _, tc := range cases {
		got, err := normalizeOllamaAPIBase(tc.raw)
		if err != nil {
			t.Fatalf("normalize %q: %v", tc.raw, err)
		}
		if got != tc.want {
			t.Fatalf("normalize %q = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestNormalizeOllamaAPIBase_RejectsInvalidURL(t *testing.T) {
	if _, err := normalizeOllamaAPIBase("127.0.0.1:11434"); err == nil {
		t.Fatalf("expected invalid url to fail normalization")
	}
}
