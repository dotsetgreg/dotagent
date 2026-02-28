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
		{name: "durable preference async only", input: "I prefer concise responses.", wantRun: false},
		{name: "session intent", input: "For this session, focus on shipping speed.", wantRun: true},
		{name: "question with weak memory word", input: "Can you remember this bug report?", wantRun: false},
		{name: "save instruction explicit", input: "Please remember this: my timezone is America/New_York", wantRun: true},
		{name: "casual statement no directive", input: "I live in Seattle and like coffee.", wantRun: false},
		{
			name:    "long prompt with incidental style phrase",
			input:   "Write a migration plan for this service and include rollback steps, safety checks, testing strategy, and communication plan. Also write in a formal tone for the document body.",
			wantRun: false,
		},
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
