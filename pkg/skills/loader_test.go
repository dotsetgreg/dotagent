package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSkillsInfoValidate(t *testing.T) {
	testcases := []struct {
		name        string
		skillName   string
		description string
		wantErr     bool
		errContains []string
	}{
		{
			name:        "valid-skill",
			skillName:   "valid-skill",
			description: "a valid skill description",
			wantErr:     false,
		},
		{
			name:        "empty-name",
			skillName:   "",
			description: "description without name",
			wantErr:     true,
			errContains: []string{"name is required"},
		},
		{
			name:        "empty-description",
			skillName:   "skill-without-description",
			description: "",
			wantErr:     true,
			errContains: []string{"description is required"},
		},
		{
			name:        "empty-both",
			skillName:   "",
			description: "",
			wantErr:     true,
			errContains: []string{"name is required", "description is required"},
		},
		{
			name:        "name-with-spaces",
			skillName:   "skill with spaces",
			description: "invalid name with spaces",
			wantErr:     true,
			errContains: []string{"name must be alphanumeric with hyphens"},
		},
		{
			name:        "name-with-underscore",
			skillName:   "skill_underscore",
			description: "invalid name with underscore",
			wantErr:     true,
			errContains: []string{"name must be alphanumeric with hyphens"},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			info := SkillInfo{
				Name:        tc.skillName,
				Description: tc.description,
			}
			err := info.validate()
			if tc.wantErr {
				assert.Error(t, err)
				for _, msg := range tc.errContains {
					assert.ErrorContains(t, err, msg)
				}
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestSkillsLoader_ListSkills_UsesDirectoryNameWhenFrontmatterNameMissing(t *testing.T) {
	workspace := t.TempDir()
	skillDir := filepath.Join(workspace, "skills", "weather-helper")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir skill dir: %v", err)
	}
	content := `---
description: Weather utility helper
---
# Weather Helper
`
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("write skill file: %v", err)
	}

	loader := NewSkillsLoader(workspace, "", "")
	skills := loader.ListSkills()
	if len(skills) != 1 {
		t.Fatalf("expected one skill, got %d", len(skills))
	}
	if skills[0].Name != "weather-helper" {
		t.Fatalf("expected fallback name weather-helper, got %q", skills[0].Name)
	}
	if skills[0].Description != "Weather utility helper" {
		t.Fatalf("expected description from frontmatter, got %q", skills[0].Description)
	}
}

func TestSkillsLoader_LoadSkill_StripsOnlyLeadingFrontmatter(t *testing.T) {
	workspace := t.TempDir()
	skillDir := filepath.Join(workspace, "skills", "demo-skill")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir skill dir: %v", err)
	}
	content := `---
name: demo-skill
description: Demo skill
---
line one
---
line three
`
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("write skill file: %v", err)
	}

	loader := NewSkillsLoader(workspace, "", "")
	body, ok := loader.LoadSkill("demo-skill")
	if !ok {
		t.Fatalf("expected skill to load")
	}
	if strings.Contains(body, "name: demo-skill") {
		t.Fatalf("expected frontmatter to be stripped, got %q", body)
	}
	if !strings.Contains(body, "line one\n---\nline three") {
		t.Fatalf("expected body separator to remain, got %q", body)
	}
}
