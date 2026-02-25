package channels

import (
	"strings"
	"testing"
)

func TestBuildDiscordStreamPreview_ClosesUnbalancedFence(t *testing.T) {
	preview := buildDiscordStreamPreview("```go\nfmt.Println(\"hi\")", 1600)
	if strings.Count(preview, "```")%2 != 0 {
		t.Fatalf("expected balanced markdown fences, got %q", preview)
	}
}

func TestBuildDiscordStreamPreview_TruncatesLargePayload(t *testing.T) {
	full := strings.Repeat("abcdefghijklmnopqrstuvwxyz", 120)
	preview := buildDiscordStreamPreview(full, 220)
	if len([]rune(preview)) > 240 {
		t.Fatalf("expected preview near limit, got %d runes", len([]rune(preview)))
	}
	if !strings.Contains(preview, "[stream preview truncated]") {
		t.Fatalf("expected truncation marker in preview")
	}
}
