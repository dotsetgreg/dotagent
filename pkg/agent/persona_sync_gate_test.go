package agent

import "testing"

func TestShouldApplyPersonaSyncFastPath(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantRun bool
	}{
		{name: "empty", input: "", wantRun: false},
		{name: "normal question", input: "How do I fix this race condition?", wantRun: false},
		{name: "identity rename", input: "Your name is Luna.", wantRun: true},
		{name: "question with explicit rename cue", input: "Can you call me Greg?", wantRun: true},
		{name: "style directive", input: "Please be more concise.", wantRun: true},
		{name: "language preference", input: "Could you speak in Spanish?", wantRun: true},
		{name: "durable preference", input: "I prefer concise responses.", wantRun: true},
		{name: "session intent", input: "For this session, focus on shipping speed.", wantRun: true},
		{name: "question with weak memory word", input: "Can you remember this bug report?", wantRun: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldApplyPersonaSyncFastPath(tt.input)
			if got != tt.wantRun {
				t.Fatalf("shouldApplyPersonaSyncFastPath(%q)=%v want=%v", tt.input, got, tt.wantRun)
			}
		})
	}
}
