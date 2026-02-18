package skills

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const skillLockFile = "lock.json"

var githubRawBaseURL = "https://raw.githubusercontent.com"
var githubAPIBaseURL = "https://api.github.com"
var githubRepoSegmentRegex = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
var githubCommitSHARegex = regexp.MustCompile(`^[a-fA-F0-9]{40}$`)
var installSkillNameRegex = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

type SkillInstaller struct {
	workspace string
}

type AvailableSkill struct {
	Name        string   `json:"name"`
	Repository  string   `json:"repository"`
	Description string   `json:"description"`
	Author      string   `json:"author"`
	Tags        []string `json:"tags"`
}

type BuiltinSkill struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Enabled bool   `json:"enabled"`
}

type SkillLockEntry struct {
	Name       string `json:"name"`
	Repository string `json:"repository"`
	Path       string `json:"path,omitempty"`
	Ref        string `json:"ref"`
	CommitSHA  string `json:"commit_sha"`
	Source     string `json:"source"`
	DigestSHA  string `json:"digest_sha256"`
	UpdatedAt  string `json:"updated_at"`
}

type gitHubSkillSpec struct {
	Owner string
	Repo  string
	Path  string
	Ref   string
}

func (s gitHubSkillSpec) Repository() string {
	return s.Owner + "/" + s.Repo
}

func (s gitHubSkillSpec) SkillName() string {
	if strings.TrimSpace(s.Path) == "" {
		return s.Repo
	}
	return path.Base(strings.TrimSpace(s.Path))
}

func (s gitHubSkillSpec) SkillFilePath() string {
	basePath := strings.TrimSpace(s.Path)
	if basePath == "" {
		return "SKILL.md"
	}
	return path.Join(basePath, "SKILL.md")
}

func (s gitHubSkillSpec) Source(commitSHA string) string {
	location := s.Repository()
	if strings.TrimSpace(s.Path) != "" {
		location += "/" + strings.TrimSpace(s.Path)
	}
	return fmt.Sprintf("github:%s@%s", location, strings.ToLower(strings.TrimSpace(commitSHA)))
}

func NewSkillInstaller(workspace string) *SkillInstaller {
	return &SkillInstaller{
		workspace: workspace,
	}
}

func (si *SkillInstaller) InstallFromGitHub(ctx context.Context, repo string) error {
	spec, err := parseGitHubSkillSpec(repo)
	if err != nil {
		return err
	}

	skillName := spec.SkillName()
	if !installSkillNameRegex.MatchString(skillName) {
		return fmt.Errorf("invalid skill name %q derived from %q", skillName, repo)
	}

	skillDir := filepath.Join(si.workspace, "skills", skillName)
	if _, err := os.Stat(skillDir); err == nil {
		return fmt.Errorf("skill '%s' already exists", skillName)
	}

	commitSHA, err := resolveGitHubCommitSHA(ctx, spec.Repository(), spec.Ref)
	if err != nil {
		return fmt.Errorf("resolve github ref: %w", err)
	}

	skillURL := buildRawGitHubURL(spec.Repository(), commitSHA, spec.SkillFilePath())
	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, skillURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to fetch skill: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to fetch skill: HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}
	if len(body) == 0 {
		return fmt.Errorf("fetched SKILL.md is empty")
	}

	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		return fmt.Errorf("failed to create skill directory: %w", err)
	}

	skillPath := filepath.Join(skillDir, "SKILL.md")
	if err := os.WriteFile(skillPath, body, 0o644); err != nil {
		return fmt.Errorf("failed to write skill file: %w", err)
	}

	sum := sha256.Sum256(body)
	lockEntry := SkillLockEntry{
		Name:       skillName,
		Repository: spec.Repository(),
		Path:       strings.TrimSpace(spec.Path),
		Ref:        strings.TrimSpace(spec.Ref),
		CommitSHA:  commitSHA,
		Source:     spec.Source(commitSHA),
		DigestSHA:  hex.EncodeToString(sum[:]),
		UpdatedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	if err := si.updateLockEntry(lockEntry); err != nil {
		_ = os.RemoveAll(skillDir)
		return fmt.Errorf("failed to write skill lock metadata: %w", err)
	}

	return nil
}

func (si *SkillInstaller) Uninstall(skillName string) error {
	skillName = strings.TrimSpace(skillName)
	skillDir := filepath.Join(si.workspace, "skills", skillName)

	if _, err := os.Stat(skillDir); os.IsNotExist(err) {
		return fmt.Errorf("skill '%s' not found", skillName)
	}

	if err := os.RemoveAll(skillDir); err != nil {
		return fmt.Errorf("failed to remove skill: %w", err)
	}

	if err := si.removeLockEntry(skillName); err != nil {
		return fmt.Errorf("failed to update skill lock metadata: %w", err)
	}
	return nil
}

func (si *SkillInstaller) ListAvailableSkills(ctx context.Context) ([]AvailableSkill, error) {
	url := "https://raw.githubusercontent.com/dotsetgreg/dotagent-skills/main/skills.json"

	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch skills list: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("failed to fetch skills list: HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	var skills []AvailableSkill
	if err := json.Unmarshal(body, &skills); err != nil {
		return nil, fmt.Errorf("failed to parse skills list: %w", err)
	}

	return skills, nil
}

func (si *SkillInstaller) ListBuiltinSkills() []BuiltinSkill {
	builtinSkillsDir := filepath.Join(filepath.Dir(si.workspace), "dotagent", "skills")

	entries, err := os.ReadDir(builtinSkillsDir)
	if err != nil {
		return nil
	}

	var skills []BuiltinSkill
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		skillName := entry.Name()
		skillFile := filepath.Join(builtinSkillsDir, skillName, "SKILL.md")
		if _, err := os.Stat(skillFile); err != nil {
			continue
		}
		skills = append(skills, BuiltinSkill{
			Name:    skillName,
			Path:    skillFile,
			Enabled: true,
		})
	}
	sort.SliceStable(skills, func(i, j int) bool {
		return strings.ToLower(skills[i].Name) < strings.ToLower(skills[j].Name)
	})
	return skills
}

func parseGitHubSkillSpec(raw string) (gitHubSkillSpec, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return gitHubSkillSpec{}, fmt.Errorf("repo is required")
	}

	specPart := raw
	ref := "main"
	slashPos := strings.Index(raw, "/")
	atPos := strings.LastIndex(raw, "@")
	if atPos > slashPos {
		specPart = strings.TrimSpace(raw[:atPos])
		ref = strings.TrimSpace(raw[atPos+1:])
	}
	if strings.TrimSpace(ref) == "" {
		return gitHubSkillSpec{}, fmt.Errorf("github ref cannot be empty; use owner/repo[/path] or owner/repo[/path]@ref")
	}

	parts := strings.Split(specPart, "/")
	if len(parts) < 2 {
		return gitHubSkillSpec{}, fmt.Errorf("invalid github repository format %q (expected owner/repo[/path][@ref])", raw)
	}
	owner := strings.TrimSpace(parts[0])
	repo := strings.TrimSpace(parts[1])
	if !githubRepoSegmentRegex.MatchString(owner) || !githubRepoSegmentRegex.MatchString(repo) {
		return gitHubSkillSpec{}, fmt.Errorf("invalid github repository format %q (expected owner/repo[/path][@ref])", raw)
	}

	subPath := strings.TrimSpace(strings.Join(parts[2:], "/"))
	subPath = strings.Trim(subPath, "/")
	if strings.Contains(subPath, "\\") {
		return gitHubSkillSpec{}, fmt.Errorf("invalid github path %q", subPath)
	}
	if subPath != "" {
		cleaned := path.Clean(subPath)
		if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.Contains(cleaned, "/../") {
			return gitHubSkillSpec{}, fmt.Errorf("invalid github path %q", subPath)
		}
		subPath = cleaned
	}

	return gitHubSkillSpec{
		Owner: owner,
		Repo:  repo,
		Path:  subPath,
		Ref:   ref,
	}, nil
}

func resolveGitHubCommitSHA(ctx context.Context, repo, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if githubCommitSHARegex.MatchString(ref) {
		return strings.ToLower(ref), nil
	}
	commitURL := fmt.Sprintf("%s/repos/%s/commits/%s", strings.TrimRight(githubAPIBaseURL, "/"), strings.TrimSpace(repo), url.PathEscape(ref))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, commitURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", fmt.Errorf("github commit lookup failed: HTTP %d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var payload struct {
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32*1024)).Decode(&payload); err != nil {
		return "", fmt.Errorf("decode github commit response: %w", err)
	}
	sha := strings.ToLower(strings.TrimSpace(payload.SHA))
	if !githubCommitSHARegex.MatchString(sha) {
		return "", fmt.Errorf("github commit lookup returned invalid sha %q", payload.SHA)
	}
	return sha, nil
}

func buildRawGitHubURL(repo, ref, filePath string) string {
	repo = strings.TrimSpace(repo)
	ref = strings.TrimSpace(ref)
	filePath = strings.Trim(filePath, "/")

	segments := strings.Split(filePath, "/")
	escaped := make([]string, 0, len(segments))
	for _, segment := range segments {
		if strings.TrimSpace(segment) == "" {
			continue
		}
		escaped = append(escaped, url.PathEscape(segment))
	}

	return fmt.Sprintf("%s/%s/%s/%s", strings.TrimRight(githubRawBaseURL, "/"), repo, url.PathEscape(ref), strings.Join(escaped, "/"))
}

func (si *SkillInstaller) lockPath() string {
	return filepath.Join(si.workspace, "skills", skillLockFile)
}

func (si *SkillInstaller) lockEntries() ([]SkillLockEntry, error) {
	lockPath := si.lockPath()
	raw, err := os.ReadFile(lockPath)
	if err != nil {
		if os.IsNotExist(err) {
			return []SkillLockEntry{}, nil
		}
		return nil, err
	}

	entries := []SkillLockEntry{}
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

func (si *SkillInstaller) writeLockEntries(entries []SkillLockEntry) error {
	lockPath := si.lockPath()
	if len(entries) == 0 {
		if err := os.Remove(lockPath); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return err
	}

	raw, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(lockPath, raw, 0o644)
}

func (si *SkillInstaller) updateLockEntry(entry SkillLockEntry) error {
	entries, err := si.lockEntries()
	if err != nil {
		return err
	}

	next := make([]SkillLockEntry, 0, len(entries)+1)
	replaced := false
	for _, existing := range entries {
		if existing.Name == entry.Name {
			next = append(next, entry)
			replaced = true
			continue
		}
		next = append(next, existing)
	}
	if !replaced {
		next = append(next, entry)
	}

	sort.SliceStable(next, func(i, j int) bool {
		return strings.ToLower(next[i].Name) < strings.ToLower(next[j].Name)
	})
	return si.writeLockEntries(next)
}

func (si *SkillInstaller) removeLockEntry(skillName string) error {
	skillName = strings.TrimSpace(skillName)
	if skillName == "" {
		return nil
	}

	entries, err := si.lockEntries()
	if err != nil {
		return err
	}

	next := make([]SkillLockEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.Name == skillName {
			continue
		}
		next = append(next, entry)
	}
	return si.writeLockEntries(next)
}
