package memory

import "testing"

func TestDetectQueryIntentAvoidsSubstringFalsePositives(t *testing.T) {
	tests := []struct {
		query string
		want  string
	}{
		{query: "this seems unlikely to work", want: "general"},
		{query: "what do you remember about me", want: "identity"},
		{query: "what do i prefer", want: "preference"},
		{query: "remind me what tasks are open", want: "task"},
	}
	for _, tt := range tests {
		if got := detectQueryIntent(tt.query); got != tt.want {
			t.Fatalf("detectQueryIntent(%q)=%q want %q", tt.query, got, tt.want)
		}
	}
}

func TestNormalizePersonaLanguage(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{input: "English", want: "english"},
		{input: "en-US", want: "en-us"},
		{input: "detail", want: ""},
	}
	for _, tt := range tests {
		if got := normalizePersonaLanguage(tt.input); got != tt.want {
			t.Fatalf("normalizePersonaLanguage(%q)=%q want %q", tt.input, got, tt.want)
		}
	}
}

func TestMapForgetTargetPath(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{input: "my preferences", want: "user.preferences"},
		{input: "my timezone", want: "user.timezone"},
		{input: "my name", want: "user.name"},
		{input: "that thing", want: ""},
	}
	for _, tt := range tests {
		if got := mapForgetTargetPath(tt.input); got != tt.want {
			t.Fatalf("mapForgetTargetPath(%q)=%q want %q", tt.input, got, tt.want)
		}
	}
}
