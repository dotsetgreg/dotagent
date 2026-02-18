package toolpacks

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/dotsetgreg/dotagent/pkg/connectors"
	"github.com/dotsetgreg/dotagent/pkg/tools"
)

const (
	defaultRootDir = "toolpacks"
	manifestFile   = "toolpack.json"
	lockFile       = "lock.json"
)

var manifestIDRegex = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
var toolNameRegex = regexp.MustCompile(`^[a-z][a-z0-9_]{1,63}$`)
var githubArchiveBaseURL = "https://codeload.github.com"
var githubAPIBaseURL = "https://api.github.com"
var githubRepoRegex = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var githubCommitSHARegex = regexp.MustCompile(`^[a-fA-F0-9]{40}$`)

var reservedToolNames = map[string]struct{}{
	"read_file":   {},
	"write_file":  {},
	"list_dir":    {},
	"edit_file":   {},
	"append_file": {},
	"exec":        {},
	"process":     {},
	"web_search":  {},
	"web_fetch":   {},
	"message":     {},
	"spawn":       {},
	"subagent":    {},
	"session":     {},
}

type Manifest struct {
	ID          string                 `json:"id"`
	Name        string                 `json:"name"`
	Version     string                 `json:"version"`
	Description string                 `json:"description"`
	Enabled     bool                   `json:"enabled"`
	Permissions []string               `json:"permissions,omitempty"`
	Connectors  []ManifestConnector    `json:"connectors,omitempty"`
	Tools       []ManifestTool         `json:"tools"`
	Metadata    map[string]interface{} `json:"metadata,omitempty"`
}

type ManifestTool struct {
	Name            string                 `json:"name"`
	Type            string                 `json:"type"` // command | mcp | openapi
	Description     string                 `json:"description"`
	CommandTemplate string                 `json:"command_template,omitempty"`
	WorkingDir      string                 `json:"working_dir,omitempty"`
	TimeoutSeconds  int                    `json:"timeout_seconds,omitempty"`
	Parameters      map[string]interface{} `json:"parameters,omitempty"`
	ConnectorID     string                 `json:"connector_id,omitempty"`
	RemoteTool      string                 `json:"remote_tool,omitempty"`
	OperationID     string                 `json:"operation_id,omitempty"`
}

type ManifestConnector struct {
	ID          string                   `json:"id"`
	Type        string                   `json:"type"` // mcp | openapi
	Description string                   `json:"description,omitempty"`
	MCP         connectors.MCPConfig     `json:"mcp,omitempty"`
	OpenAPI     connectors.OpenAPIConfig `json:"openapi,omitempty"`
}

type LockEntry struct {
	ID        string `json:"id"`
	Version   string `json:"version"`
	Source    string `json:"source"`
	DigestSHA string `json:"digest_sha256"`
	UpdatedAt string `json:"updated_at"`
}

type Manager struct {
	workspace string
	rootDir   string
	restrict  bool
}

type connectorInvokerAdapter struct {
	runtime connectors.Runtime
}

func (a connectorInvokerAdapter) Invoke(ctx context.Context, target string, args map[string]interface{}) (tools.ConnectorInvocationResult, error) {
	if a.runtime == nil {
		return tools.ConnectorInvocationResult{}, fmt.Errorf("connector runtime unavailable")
	}
	result, err := a.runtime.Invoke(ctx, target, args)
	if err != nil {
		return tools.ConnectorInvocationResult{}, err
	}
	return tools.ConnectorInvocationResult{
		Content:     result.Content,
		UserContent: result.UserContent,
		IsError:     result.IsError,
	}, nil
}

func (a connectorInvokerAdapter) Close() error {
	if a.runtime == nil {
		return nil
	}
	return a.runtime.Close()
}

func NewManager(workspace string, restrict bool) *Manager {
	root := filepath.Join(workspace, defaultRootDir)
	return &Manager{
		workspace: workspace,
		rootDir:   root,
		restrict:  restrict,
	}
}

func (m *Manager) RootDir() string {
	return m.rootDir
}

func (m *Manager) List() ([]Manifest, error) {
	if err := os.MkdirAll(m.rootDir, 0o755); err != nil {
		return nil, fmt.Errorf("create toolpacks root: %w", err)
	}
	entries, err := os.ReadDir(m.rootDir)
	if err != nil {
		return nil, fmt.Errorf("list toolpacks root: %w", err)
	}
	out := make([]Manifest, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		manifestPath := filepath.Join(m.rootDir, entry.Name(), manifestFile)
		manifest, readErr := readManifest(manifestPath)
		if readErr != nil {
			continue
		}
		if normalizeErr := validateManifest(&manifest); normalizeErr != nil {
			continue
		}
		out = append(out, manifest)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (m *Manager) LoadEnabledTools() ([]tools.Tool, error) {
	manifests, err := m.List()
	if err != nil {
		return nil, err
	}
	registered := make([]tools.Tool, 0, len(manifests))
	loadedNames := map[string]string{}
	warnings := make([]string, 0)
	for _, manifest := range manifests {
		if !manifest.Enabled {
			continue
		}
		packDir := filepath.Join(m.rootDir, filepath.Base(manifest.ID))
		connectorRuntimes, connWarnings := m.buildConnectorRuntimes(packDir, manifest)
		warnings = append(warnings, connWarnings...)
		for _, mt := range manifest.Tools {
			toolType := strings.ToLower(strings.TrimSpace(mt.Type))
			switch toolType {
			case "", "command":
				toolName := strings.TrimSpace(mt.Name)
				if toolName == "" || strings.TrimSpace(mt.CommandTemplate) == "" {
					continue
				}
				if owner, exists := loadedNames[toolName]; exists {
					warnings = append(warnings, fmt.Sprintf("%s: tool %q collides with %s; skipping", manifest.ID, toolName, owner))
					continue
				}
				registered = append(registered, tools.NewTemplateCommandTool(tools.TemplateCommandConfig{
					Name:            toolName,
					Description:     nonEmpty(mt.Description, fmt.Sprintf("ToolPack %s command tool", manifest.ID)),
					Parameters:      defaultParameters(mt.Parameters),
					CommandTemplate: mt.CommandTemplate,
					WorkingDir:      resolvePackWorkingDir(packDir, mt.WorkingDir),
					TimeoutSeconds:  mt.TimeoutSeconds,
					Workspace:       m.workspace,
					Restrict:        m.restrict,
				}))
				loadedNames[toolName] = manifest.ID
			case "mcp", "openapi":
				toolName := strings.TrimSpace(mt.Name)
				if toolName == "" {
					continue
				}
				if owner, exists := loadedNames[toolName]; exists {
					warnings = append(warnings, fmt.Sprintf("%s: tool %q collides with %s; skipping", manifest.ID, toolName, owner))
					continue
				}
				connectorID := strings.TrimSpace(mt.ConnectorID)
				runtime, ok := connectorRuntimes[connectorID]
				if !ok {
					warnings = append(warnings, fmt.Sprintf("%s: connector %q not available for tool %q", manifest.ID, connectorID, toolName))
					continue
				}
				target := strings.TrimSpace(mt.RemoteTool)
				if toolType == "openapi" {
					target = strings.TrimSpace(mt.OperationID)
				}
				if target == "" {
					target = toolName
				}
				desc := strings.TrimSpace(mt.Description)
				params := mt.Parameters
				if params == nil || len(params) == 0 {
					schemaCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
					autoDesc, autoParams, schemaErr := runtime.ToolSchema(schemaCtx, target)
					cancel()
					if schemaErr != nil {
						warnings = append(warnings, fmt.Sprintf("%s: could not load schema for %s:%s (%v)", manifest.ID, connectorID, target, schemaErr))
						continue
					}
					if desc == "" {
						desc = autoDesc
					}
					params = autoParams
				}
				registered = append(registered, tools.NewConnectorProxyTool(
					toolName,
					nonEmpty(desc, fmt.Sprintf("ToolPack %s %s connector tool", manifest.ID, toolType)),
					defaultParameters(params),
					target,
					connectorInvokerAdapter{runtime: runtime},
				))
				loadedNames[toolName] = manifest.ID
			default:
				warnings = append(warnings, fmt.Sprintf("%s: tool %q has unsupported type %q", manifest.ID, mt.Name, mt.Type))
			}
		}
	}
	if len(warnings) > 0 {
		return registered, fmt.Errorf("%d toolpack warning(s): %s", len(warnings), strings.Join(warnings, "; "))
	}
	return registered, nil
}

func defaultParameters(params map[string]interface{}) map[string]interface{} {
	if params != nil {
		return params
	}
	return map[string]interface{}{
		"type":       "object",
		"properties": map[string]interface{}{},
	}
}

func resolvePackWorkingDir(packDir, wd string) string {
	wd = strings.TrimSpace(wd)
	if wd == "" {
		return packDir
	}
	if filepath.IsAbs(wd) {
		return wd
	}
	return filepath.Join(packDir, wd)
}

func (m *Manager) buildConnectorRuntimes(packDir string, manifest Manifest) (map[string]connectors.Runtime, []string) {
	runtimes := map[string]connectors.Runtime{}
	warnings := []string{}
	for _, conn := range manifest.Connectors {
		connID := strings.TrimSpace(conn.ID)
		if connID == "" {
			continue
		}
		switch conn.Type {
		case "mcp":
			cfg := conn.MCP
			if strings.TrimSpace(cfg.WorkingDir) != "" {
				cfg.WorkingDir = resolvePackWorkingDir(packDir, cfg.WorkingDir)
			}
			rt, err := connectors.NewMCPRuntime(connID, cfg)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("%s: mcp connector %q init failed: %v", manifest.ID, connID, err))
				continue
			}
			runtimes[connID] = rt
		case "openapi":
			cfg := conn.OpenAPI
			if specPath := strings.TrimSpace(cfg.SpecPath); specPath != "" && !filepath.IsAbs(specPath) {
				cfg.SpecPath = filepath.Join(packDir, specPath)
			}
			rt, err := connectors.NewOpenAPIRuntime(connID, cfg)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("%s: openapi connector %q init failed: %v", manifest.ID, connID, err))
				continue
			}
			runtimes[connID] = rt
		default:
			warnings = append(warnings, fmt.Sprintf("%s: connector %q has unsupported type %q", manifest.ID, connID, conn.Type))
		}
	}
	return runtimes, warnings
}

type ConnectorHealth struct {
	PackID      string `json:"pack_id"`
	ConnectorID string `json:"connector_id"`
	Type        string `json:"type"`
	Status      string `json:"status"`
	Error       string `json:"error,omitempty"`
}

// Validate checks manifests and returns non-fatal warnings.
func (m *Manager) Validate(id string) ([]string, error) {
	if err := os.MkdirAll(m.rootDir, 0o755); err != nil {
		return nil, fmt.Errorf("create toolpacks root: %w", err)
	}
	entries, err := os.ReadDir(m.rootDir)
	if err != nil {
		return nil, err
	}
	id = strings.TrimSpace(id)
	warnings := []string{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		manifestPath := filepath.Join(m.rootDir, entry.Name(), manifestFile)
		manifest, readErr := readManifest(manifestPath)
		if readErr != nil {
			warnings = append(warnings, fmt.Sprintf("%s: manifest read failed: %v", entry.Name(), readErr))
			continue
		}
		if id != "" && manifest.ID != id {
			continue
		}
		if validateErr := validateManifest(&manifest); validateErr != nil {
			warnings = append(warnings, fmt.Sprintf("%s: manifest invalid: %v", manifest.ID, validateErr))
			continue
		}
		packDir := filepath.Join(m.rootDir, filepath.Base(manifest.ID))
		_, connWarnings := m.buildConnectorRuntimes(packDir, manifest)
		warnings = append(warnings, connWarnings...)
	}
	return warnings, nil
}

// Doctor builds configured connectors and performs runtime health checks.
func (m *Manager) Doctor(ctx context.Context, id string) ([]ConnectorHealth, error) {
	manifests, err := m.List()
	if err != nil {
		return nil, err
	}
	id = strings.TrimSpace(id)
	out := []ConnectorHealth{}
	for _, manifest := range manifests {
		if id != "" && manifest.ID != id {
			continue
		}
		packDir := filepath.Join(m.rootDir, filepath.Base(manifest.ID))
		runtimes, connWarnings := m.buildConnectorRuntimes(packDir, manifest)
		for _, warn := range connWarnings {
			out = append(out, ConnectorHealth{
				PackID: manifest.ID,
				Status: "error",
				Error:  warn,
			})
		}
		for connectorID, runtime := range runtimes {
			status := ConnectorHealth{
				PackID:      manifest.ID,
				ConnectorID: connectorID,
				Type:        runtime.Type(),
				Status:      "ok",
			}
			healthCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := runtime.Health(healthCtx)
			cancel()
			if err != nil {
				status.Status = "error"
				status.Error = err.Error()
			}
			out = append(out, status)
			_ = runtime.Close()
		}
	}
	return out, nil
}

func readManifest(path string) (Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, err
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("parse manifest: %w", err)
	}
	if strings.TrimSpace(manifest.ID) == "" {
		manifest.ID = strings.TrimSuffix(filepath.Base(filepath.Dir(path)), string(filepath.Separator))
	}
	return manifest, nil
}

func validateManifest(manifest *Manifest) error {
	if manifest == nil {
		return fmt.Errorf("manifest is nil")
	}
	manifest.ID = strings.TrimSpace(strings.ToLower(manifest.ID))
	manifest.Name = strings.TrimSpace(manifest.Name)
	manifest.Version = strings.TrimSpace(manifest.Version)
	manifest.Description = strings.TrimSpace(manifest.Description)
	if manifest.ID == "" {
		return fmt.Errorf("manifest id is required")
	}
	if !manifestIDRegex.MatchString(manifest.ID) {
		return fmt.Errorf("manifest id %q is invalid", manifest.ID)
	}
	if manifest.Name == "" {
		return fmt.Errorf("manifest name is required")
	}
	if manifest.Version == "" {
		return fmt.Errorf("manifest version is required")
	}
	if len(manifest.Tools) == 0 {
		return fmt.Errorf("manifest tools must not be empty")
	}

	connectorByID := map[string]ManifestConnector{}
	for i := range manifest.Connectors {
		conn := &manifest.Connectors[i]
		conn.ID = strings.TrimSpace(strings.ToLower(conn.ID))
		conn.Type = strings.TrimSpace(strings.ToLower(conn.Type))
		conn.Description = strings.TrimSpace(conn.Description)
		if conn.ID == "" {
			return fmt.Errorf("connector[%d] id is required", i)
		}
		if !toolNameRegex.MatchString(conn.ID) {
			return fmt.Errorf("connector[%d] id %q is invalid", i, conn.ID)
		}
		if conn.Type != "mcp" && conn.Type != "openapi" {
			return fmt.Errorf("connector[%d] type %q is unsupported", i, conn.Type)
		}
		if _, exists := connectorByID[conn.ID]; exists {
			return fmt.Errorf("connector[%d] id %q is duplicated", i, conn.ID)
		}
		switch conn.Type {
		case "mcp":
			if strings.TrimSpace(conn.MCP.Transport) == "" {
				// Default transport selection happens at runtime; this is fine.
			}
		case "openapi":
			if strings.TrimSpace(conn.OpenAPI.SpecPath) == "" && strings.TrimSpace(conn.OpenAPI.SpecURL) == "" {
				return fmt.Errorf("connector[%d] openapi requires spec_path or spec_url", i)
			}
		}
		connectorByID[conn.ID] = *conn
	}

	seen := map[string]struct{}{}
	for i := range manifest.Tools {
		tool := &manifest.Tools[i]
		tool.Name = strings.TrimSpace(strings.ToLower(tool.Name))
		tool.Type = strings.TrimSpace(strings.ToLower(tool.Type))
		tool.Description = strings.TrimSpace(tool.Description)
		tool.CommandTemplate = strings.TrimSpace(tool.CommandTemplate)
		tool.WorkingDir = strings.TrimSpace(tool.WorkingDir)
		tool.ConnectorID = strings.TrimSpace(strings.ToLower(tool.ConnectorID))
		tool.RemoteTool = strings.TrimSpace(tool.RemoteTool)
		tool.OperationID = strings.TrimSpace(tool.OperationID)
		if tool.Type == "" {
			tool.Type = "command"
		}
		if tool.Name == "" {
			return fmt.Errorf("tool[%d] name is required", i)
		}
		if !toolNameRegex.MatchString(tool.Name) {
			return fmt.Errorf("tool[%d] name %q is invalid", i, tool.Name)
		}
		if _, reserved := reservedToolNames[tool.Name]; reserved {
			return fmt.Errorf("tool[%d] name %q collides with built-in tool", i, tool.Name)
		}
		if _, dup := seen[tool.Name]; dup {
			return fmt.Errorf("tool[%d] name %q is duplicated in manifest", i, tool.Name)
		}
		seen[tool.Name] = struct{}{}
		switch tool.Type {
		case "command":
			if tool.CommandTemplate == "" {
				return fmt.Errorf("tool[%d] command_template is required for command tools", i)
			}
		case "mcp":
			if tool.ConnectorID == "" {
				return fmt.Errorf("tool[%d] connector_id is required for mcp tools", i)
			}
			conn, ok := connectorByID[tool.ConnectorID]
			if !ok {
				return fmt.Errorf("tool[%d] references unknown connector_id %q", i, tool.ConnectorID)
			}
			if conn.Type != "mcp" {
				return fmt.Errorf("tool[%d] connector_id %q type mismatch: expected mcp, got %s", i, tool.ConnectorID, conn.Type)
			}
		case "openapi":
			if tool.ConnectorID == "" {
				return fmt.Errorf("tool[%d] connector_id is required for openapi tools", i)
			}
			conn, ok := connectorByID[tool.ConnectorID]
			if !ok {
				return fmt.Errorf("tool[%d] references unknown connector_id %q", i, tool.ConnectorID)
			}
			if conn.Type != "openapi" {
				return fmt.Errorf("tool[%d] connector_id %q type mismatch: expected openapi, got %s", i, tool.ConnectorID, conn.Type)
			}
		default:
			return fmt.Errorf("tool[%d] type %q is unsupported", i, tool.Type)
		}
		if tool.TimeoutSeconds < 0 {
			return fmt.Errorf("tool[%d] timeout_seconds must be >= 0", i)
		}
	}
	return nil
}

func writeManifest(path string, manifest Manifest) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func (m *Manager) Enable(id string, enabled bool) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("toolpack id is required")
	}
	dir := filepath.Join(m.rootDir, filepath.Base(id))
	manifestPath := filepath.Join(dir, manifestFile)
	manifest, err := readManifest(manifestPath)
	if err != nil {
		return fmt.Errorf("load manifest %s: %w", id, err)
	}
	if err := validateManifest(&manifest); err != nil {
		return fmt.Errorf("invalid manifest %s: %w", id, err)
	}
	manifest.Enabled = enabled
	if err := writeManifest(manifestPath, manifest); err != nil {
		return fmt.Errorf("write manifest %s: %w", id, err)
	}
	if err := m.updateLock(manifest, "local:"+manifestPath, manifestPath); err != nil {
		return fmt.Errorf("update lock %s: %w", id, err)
	}
	return nil
}

func (m *Manager) Remove(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("toolpack id is required")
	}
	dir := filepath.Join(m.rootDir, filepath.Base(id))
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return fmt.Errorf("toolpack %s not found", id)
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return m.removeLock(id)
}

func (m *Manager) InstallFromPath(src string) (Manifest, error) {
	src = strings.TrimSpace(src)
	if src == "" {
		return Manifest{}, fmt.Errorf("source path is required")
	}
	srcAbs, err := filepath.Abs(src)
	if err != nil {
		return Manifest{}, err
	}
	manifest, err := readManifest(filepath.Join(srcAbs, manifestFile))
	if err != nil {
		return Manifest{}, fmt.Errorf("read source manifest: %w", err)
	}
	if err := validateManifest(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("validate source manifest: %w", err)
	}
	targetDir := filepath.Join(m.rootDir, filepath.Base(manifest.ID))
	if err := os.RemoveAll(targetDir); err != nil {
		return Manifest{}, fmt.Errorf("clear target dir: %w", err)
	}
	if err := copyDir(srcAbs, targetDir); err != nil {
		return Manifest{}, fmt.Errorf("copy toolpack: %w", err)
	}
	if err := m.updateLock(manifest, "path:"+srcAbs, filepath.Join(targetDir, manifestFile)); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func (m *Manager) InstallFromGitHub(ctx context.Context, repo string) (Manifest, error) {
	spec, err := parseGitHubRepoSpec(repo)
	if err != nil {
		return Manifest{}, err
	}
	commitSHA, err := resolveGitHubCommitSHA(ctx, spec.Repo, spec.Ref)
	if err != nil {
		return Manifest{}, fmt.Errorf("resolve github ref: %w", err)
	}
	repoRoot, err := downloadAndExtractGitHubRepo(ctx, spec.Repo, commitSHA)
	if err != nil {
		return Manifest{}, err
	}
	defer os.RemoveAll(repoRoot)

	manifestPath, err := selectToolpackManifestPath(repoRoot)
	if err != nil {
		return Manifest{}, err
	}
	manifest, err := readManifest(manifestPath)
	if err != nil {
		return Manifest{}, fmt.Errorf("read remote manifest: %w", err)
	}
	if err := validateManifest(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("validate remote manifest: %w", err)
	}

	targetDir := filepath.Join(m.rootDir, filepath.Base(manifest.ID))
	if err := os.RemoveAll(targetDir); err != nil {
		return Manifest{}, fmt.Errorf("clear target dir: %w", err)
	}
	srcDir := filepath.Dir(manifestPath)
	if err := copyDir(srcDir, targetDir); err != nil {
		return Manifest{}, fmt.Errorf("copy toolpack from github archive: %w", err)
	}
	targetManifestPath := filepath.Join(targetDir, manifestFile)
	if _, err := os.Stat(targetManifestPath); err != nil {
		return Manifest{}, fmt.Errorf("installed toolpack missing manifest at %s", targetManifestPath)
	}
	source := fmt.Sprintf("github:%s@%s", spec.Repo, strings.ToLower(strings.TrimSpace(commitSHA)))
	if err := m.updateLock(manifest, source, targetManifestPath); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func (m *Manager) updateLock(manifest Manifest, source, manifestPath string) error {
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	entry := LockEntry{
		ID:        manifest.ID,
		Version:   manifest.Version,
		Source:    source,
		DigestSHA: hex.EncodeToString(sum[:]),
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	locks, err := m.lockEntries()
	if err != nil {
		return err
	}
	next := make([]LockEntry, 0, len(locks)+1)
	replaced := false
	for _, lock := range locks {
		if lock.ID == entry.ID {
			next = append(next, entry)
			replaced = true
			continue
		}
		next = append(next, lock)
	}
	if !replaced {
		next = append(next, entry)
	}
	sort.SliceStable(next, func(i, j int) bool { return next[i].ID < next[j].ID })
	return m.writeLockEntries(next)
}

func (m *Manager) lockEntries() ([]LockEntry, error) {
	lockPath := filepath.Join(m.rootDir, lockFile)
	raw, err := os.ReadFile(lockPath)
	if err != nil {
		if os.IsNotExist(err) {
			return []LockEntry{}, nil
		}
		return nil, err
	}
	locks := []LockEntry{}
	if err := json.Unmarshal(raw, &locks); err != nil {
		return nil, err
	}
	return locks, nil
}

func (m *Manager) GetLock(id string) (LockEntry, bool, error) {
	locks, err := m.lockEntries()
	if err != nil {
		return LockEntry{}, false, err
	}
	for _, lock := range locks {
		if lock.ID == strings.TrimSpace(id) {
			return lock, true, nil
		}
	}
	return LockEntry{}, false, nil
}

func (m *Manager) removeLock(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil
	}
	locks, err := m.lockEntries()
	if err != nil {
		return err
	}
	next := make([]LockEntry, 0, len(locks))
	for _, entry := range locks {
		if entry.ID == id {
			continue
		}
		next = append(next, entry)
	}
	return m.writeLockEntries(next)
}

func (m *Manager) writeLockEntries(entries []LockEntry) error {
	lockPath := filepath.Join(m.rootDir, lockFile)
	if len(entries) == 0 {
		if err := os.Remove(lockPath); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	raw, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(lockPath, raw, 0o644)
}

func downloadAndExtractGitHubRepo(ctx context.Context, repo, ref string) (string, error) {
	archiveURL := fmt.Sprintf("%s/%s/zip/%s", strings.TrimRight(githubArchiveBaseURL, "/"), repo, url.PathEscape(strings.TrimSpace(ref)))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, archiveURL, nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 25 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download github archive: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024*1024))
	if err != nil {
		return "", fmt.Errorf("read github archive: %w", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("open github archive zip: %w", err)
	}
	root, err := os.MkdirTemp("", "dotagent-toolpack-github-*")
	if err != nil {
		return "", err
	}
	for _, zf := range zr.File {
		rel, skip := zipEntryRelativePath(zf.Name)
		if skip {
			continue
		}
		target := filepath.Join(root, rel)
		if !pathWithin(target, root) {
			_ = os.RemoveAll(root)
			return "", fmt.Errorf("zip entry escapes extraction root: %s", zf.Name)
		}
		mode := zf.Mode()
		if mode&os.ModeSymlink != 0 {
			_ = os.RemoveAll(root)
			return "", fmt.Errorf("symlinks are not allowed in github toolpack archives: %s", zf.Name)
		}
		if zf.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				_ = os.RemoveAll(root)
				return "", err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			_ = os.RemoveAll(root)
			return "", err
		}
		in, err := zf.Open()
		if err != nil {
			_ = os.RemoveAll(root)
			return "", err
		}
		fileMode := mode.Perm()
		if fileMode == 0 {
			fileMode = 0o644
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fileMode)
		if err != nil {
			_ = in.Close()
			_ = os.RemoveAll(root)
			return "", err
		}
		if _, err := io.Copy(out, in); err != nil {
			_ = out.Close()
			_ = in.Close()
			_ = os.RemoveAll(root)
			return "", err
		}
		_ = out.Close()
		_ = in.Close()
	}
	return root, nil
}

type gitHubRepoSpec struct {
	Repo string
	Ref  string
}

func parseGitHubRepoSpec(raw string) (gitHubRepoSpec, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return gitHubRepoSpec{}, fmt.Errorf("repo is required")
	}
	spec := gitHubRepoSpec{
		Repo: raw,
		Ref:  "main",
	}
	slashPos := strings.Index(raw, "/")
	atPos := strings.LastIndex(raw, "@")
	if atPos > slashPos {
		spec.Repo = strings.TrimSpace(raw[:atPos])
		spec.Ref = strings.TrimSpace(raw[atPos+1:])
	}
	spec.Repo = strings.TrimSpace(spec.Repo)
	spec.Ref = strings.TrimSpace(spec.Ref)
	if spec.Ref == "" {
		return gitHubRepoSpec{}, fmt.Errorf("github ref cannot be empty; use owner/repo or owner/repo@ref")
	}
	if !githubRepoRegex.MatchString(spec.Repo) {
		return gitHubRepoSpec{}, fmt.Errorf("invalid github repository format %q (expected owner/repo or owner/repo@ref)", raw)
	}
	return spec, nil
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

func zipEntryRelativePath(raw string) (string, bool) {
	raw = strings.TrimSpace(strings.TrimPrefix(filepath.ToSlash(raw), "/"))
	if raw == "" {
		return "", true
	}
	parts := strings.Split(raw, "/")
	if len(parts) < 2 {
		return "", true
	}
	rel := filepath.Clean(filepath.FromSlash(strings.Join(parts[1:], "/")))
	if rel == "." || rel == string(filepath.Separator) {
		return "", true
	}
	if strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
		return "", true
	}
	return rel, false
}

func pathWithin(candidate, root string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(candidate))
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func selectToolpackManifestPath(repoRoot string) (string, error) {
	candidates := make([]string, 0, 2)
	err := filepath.Walk(repoRoot, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info == nil || info.IsDir() {
			return nil
		}
		if info.Name() == manifestFile {
			candidates = append(candidates, path)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("github archive did not contain %s", manifestFile)
	}
	rootManifest := filepath.Join(repoRoot, manifestFile)
	for _, candidate := range candidates {
		if filepath.Clean(candidate) == filepath.Clean(rootManifest) {
			return candidate, nil
		}
	}
	if len(candidates) == 1 {
		return candidates[0], nil
	}
	sort.Strings(candidates)
	preview := candidates
	if len(preview) > 5 {
		preview = preview[:5]
	}
	return "", fmt.Errorf("github repo contains multiple toolpack manifests; choose a dedicated repo or local path install (found: %s)", strings.Join(preview, ", "))
}

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlinks are not supported in toolpacks: %s", path)
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, in)
		return err
	})
}

func nonEmpty(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}
