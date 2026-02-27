package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadBootstrapFiles_PrefersAgentsMDAndEmitsConflictNotice(t *testing.T) {
	ws := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(ws, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	write("AGENTS.md", "agent-preferred")
	write("AGENT.md", "agent-secondary")
	write("IDENTITY.md", "identity")
	write("SOUL.md", "soul")
	write("USER.md", "user")

	cb := NewContextBuilder(ws)
	out := cb.LoadBootstrapFiles()

	if !strings.Contains(out, "agent-preferred") {
		t.Fatalf("expected AGENTS.md content to be loaded")
	}
	if strings.Contains(out, "agent-secondary") {
		t.Fatalf("expected AGENT.md content to be ignored when AGENTS.md exists")
	}
	if !strings.Contains(out, "Bootstrap Notice") {
		t.Fatalf("expected conflict notice when AGENT.md and AGENTS.md diverge")
	}
	if strings.Contains(out, "IDENTITY.md") || strings.Contains(out, "SOUL.md") || strings.Contains(out, "USER.md") {
		t.Fatalf("expected only AGENT bootstrap content, got: %q", out)
	}
}

func TestLoadBootstrapFiles_FallbacksToAgentMD(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "AGENT.md"), []byte("legacy-agent"), 0o644); err != nil {
		t.Fatalf("write AGENT.md: %v", err)
	}
	cb := NewContextBuilder(ws)
	out := cb.LoadBootstrapFiles()
	if !strings.Contains(out, "legacy-agent") {
		t.Fatalf("expected AGENT.md fallback content to load")
	}
}

func TestBuildSystemPrompt_DoesNotHardcodeAssistantName(t *testing.T) {
	ws := t.TempDir()
	cb := NewContextBuilder(ws)
	prompt := cb.BuildSystemPrompt()
	if strings.Contains(strings.ToLower(prompt), "you are dotagent") {
		t.Fatalf("expected system prompt to avoid hardcoded assistant identity, got: %s", prompt)
	}
}

func TestBuildSystemPrompt_DeterministicHash(t *testing.T) {
	ws := t.TempDir()
	cb := NewContextBuilder(ws)
	first, meta1 := cb.BuildSystemPromptWithMetadata()
	second, meta2 := cb.BuildSystemPromptWithMetadata()
	if first != second {
		t.Fatalf("expected deterministic prompt output")
	}
	if meta1.Hash == "" || meta2.Hash == "" || meta1.Hash != meta2.Hash {
		t.Fatalf("expected stable prompt hash, got %q vs %q", meta1.Hash, meta2.Hash)
	}
}
