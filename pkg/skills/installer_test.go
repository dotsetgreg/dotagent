package skills

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseGitHubSkillSpec(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantRepo  string
		wantPath  string
		wantRef   string
		wantSkill string
		wantErr   bool
	}{
		{
			name:      "repo only",
			input:     "owner/repo",
			wantRepo:  "owner/repo",
			wantPath:  "",
			wantRef:   "main",
			wantSkill: "repo",
		},
		{
			name:      "repo path and ref",
			input:     "owner/repo/skills/weather@release-1",
			wantRepo:  "owner/repo",
			wantPath:  "skills/weather",
			wantRef:   "release-1",
			wantSkill: "weather",
		},
		{
			name:    "invalid path traversal",
			input:   "owner/repo/../secret",
			wantErr: true,
		},
		{
			name:    "invalid repo format",
			input:   "owneronly",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := parseGitHubSkillSpec(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseGitHubSkillSpec failed: %v", err)
			}
			if got := spec.Repository(); got != tc.wantRepo {
				t.Fatalf("repo mismatch: got %q want %q", got, tc.wantRepo)
			}
			if spec.Path != tc.wantPath {
				t.Fatalf("path mismatch: got %q want %q", spec.Path, tc.wantPath)
			}
			if spec.Ref != tc.wantRef {
				t.Fatalf("ref mismatch: got %q want %q", spec.Ref, tc.wantRef)
			}
			if got := spec.SkillName(); got != tc.wantSkill {
				t.Fatalf("skill name mismatch: got %q want %q", got, tc.wantSkill)
			}
		})
	}
}

func TestSkillInstaller_InstallFromGitHub_PinsCommitAndWritesLock(t *testing.T) {
	const commitSHA = "0123456789abcdef0123456789abcdef01234567"
	const skillBody = "# weather\nuse this skill"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/owner/repo/commits/main":
			_, _ = w.Write([]byte(`{"sha":"` + commitSHA + `"}`))
			return
		case "/owner/repo/" + commitSHA + "/skills/weather/SKILL.md":
			_, _ = w.Write([]byte(skillBody))
			return
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	origAPI := githubAPIBaseURL
	origRaw := githubRawBaseURL
	githubAPIBaseURL = server.URL
	githubRawBaseURL = server.URL
	defer func() {
		githubAPIBaseURL = origAPI
		githubRawBaseURL = origRaw
	}()

	workspace := t.TempDir()
	installer := NewSkillInstaller(workspace)

	if err := installer.InstallFromGitHub(context.Background(), "owner/repo/skills/weather"); err != nil {
		t.Fatalf("InstallFromGitHub failed: %v", err)
	}

	skillPath := filepath.Join(workspace, "skills", "weather", "SKILL.md")
	raw, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("read installed skill: %v", err)
	}
	if strings.TrimSpace(string(raw)) != strings.TrimSpace(skillBody) {
		t.Fatalf("installed skill mismatch")
	}

	lockPath := filepath.Join(workspace, "skills", skillLockFile)
	lockRaw, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("read lock file: %v", err)
	}
	var locks []SkillLockEntry
	if err := json.Unmarshal(lockRaw, &locks); err != nil {
		t.Fatalf("parse lock file: %v", err)
	}
	if len(locks) != 1 {
		t.Fatalf("expected one lock entry, got %d", len(locks))
	}
	entry := locks[0]
	if entry.CommitSHA != commitSHA {
		t.Fatalf("expected pinned commit %q, got %q", commitSHA, entry.CommitSHA)
	}
	if !strings.Contains(entry.Source, "@"+commitSHA) {
		t.Fatalf("expected source to include pinned commit, got %q", entry.Source)
	}
}

func TestSkillInstaller_UninstallRemovesLockEntry(t *testing.T) {
	workspace := t.TempDir()
	skillDir := filepath.Join(workspace, "skills", "weather")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir skill dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write skill file: %v", err)
	}

	lockPath := filepath.Join(workspace, "skills", skillLockFile)
	locks := []SkillLockEntry{
		{Name: "weather", Repository: "owner/repo", Ref: "main", CommitSHA: "abcd"},
	}
	lockRaw, _ := json.MarshalIndent(locks, "", "  ")
	if err := os.WriteFile(lockPath, lockRaw, 0o644); err != nil {
		t.Fatalf("write lock file: %v", err)
	}

	installer := NewSkillInstaller(workspace)
	if err := installer.Uninstall("weather"); err != nil {
		t.Fatalf("Uninstall failed: %v", err)
	}

	if _, err := os.Stat(skillDir); !os.IsNotExist(err) {
		t.Fatalf("expected skill directory to be removed")
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("expected lock file to be removed when last entry is deleted")
	}
}

func TestSkillInstaller_ListBuiltinSkills_ReturnsEntriesWithoutPrinting(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}

	builtinDir := filepath.Join(root, "dotagent", "skills", "builtin-a")
	if err := os.MkdirAll(builtinDir, 0o755); err != nil {
		t.Fatalf("mkdir builtin dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(builtinDir, "SKILL.md"), []byte("# builtin"), 0o644); err != nil {
		t.Fatalf("write builtin skill: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "dotagent", "skills", "invalid"), 0o755); err != nil {
		t.Fatalf("mkdir invalid builtin dir: %v", err)
	}

	installer := NewSkillInstaller(workspace)
	stdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	defer func() {
		os.Stdout = stdout
	}()
	skills := installer.ListBuiltinSkills()
	_ = w.Close()
	var printed bytes.Buffer
	_, _ = printed.ReadFrom(r)
	_ = r.Close()

	if printed.Len() != 0 {
		t.Fatalf("expected no stdout output, got %q", printed.String())
	}
	if len(skills) != 1 {
		t.Fatalf("expected one builtin skill, got %d", len(skills))
	}
	if skills[0].Name != "builtin-a" {
		t.Fatalf("expected builtin skill name builtin-a, got %q", skills[0].Name)
	}
	if !skills[0].Enabled {
		t.Fatalf("expected builtin skill to be enabled")
	}
}
