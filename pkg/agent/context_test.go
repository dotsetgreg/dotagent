package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadBootstrapFiles_PrefersAgentMD(t *testing.T) {
	ws := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(ws, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	write("AGENT.md", "agent-current")
	write("AGENTS.md", "agent-legacy")
	write("IDENTITY.md", "identity")
	write("SOUL.md", "soul")
	write("USER.md", "user")

	cb := NewContextBuilder(ws)
	out := cb.LoadBootstrapFiles()

	if !strings.Contains(out, "agent-current") {
		t.Fatalf("expected AGENT.md content to be loaded")
	}
	if strings.Contains(out, "agent-legacy") {
		t.Fatalf("expected AGENTS.md content to be ignored when AGENT.md exists")
	}
	if strings.Contains(out, "IDENTITY.md") || strings.Contains(out, "SOUL.md") || strings.Contains(out, "USER.md") {
		t.Fatalf("expected only AGENT bootstrap content, got: %q", out)
	}
}

func TestLoadBootstrapFiles_FallbacksToAgentsMD(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "AGENTS.md"), []byte("legacy-agent"), 0o644); err != nil {
		t.Fatalf("write AGENTS.md: %v", err)
	}
	cb := NewContextBuilder(ws)
	out := cb.LoadBootstrapFiles()
	if !strings.Contains(out, "legacy-agent") {
		t.Fatalf("expected AGENTS.md fallback content to load")
	}
}
