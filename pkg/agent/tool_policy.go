package agent

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/dotsetgreg/dotagent/pkg/config"
	"github.com/dotsetgreg/dotagent/pkg/providers"
)

type turnToolMode string

const (
	turnToolModeAuto         turnToolMode = "auto"
	turnToolModeConversation turnToolMode = "conversation"
	turnToolModeWorkspaceOps turnToolMode = "workspace_ops"
)

type turnToolPolicy struct {
	Mode           turnToolMode
	allowAll       bool
	allowedToolSet map[string]struct{}
	allowedPrefix  []string
	deniedToolSet  map[string]struct{}
	deniedPrefix   []string
}

func (p turnToolPolicy) Allows(toolName string) bool {
	if _, denied := p.deniedToolSet[toolName]; denied {
		return false
	}
	for _, prefix := range p.deniedPrefix {
		if strings.HasPrefix(toolName, prefix) {
			return false
		}
	}
	if p.allowAll {
		return true
	}
	_, ok := p.allowedToolSet[toolName]
	if ok {
		return true
	}
	for _, prefix := range p.allowedPrefix {
		if strings.HasPrefix(toolName, prefix) {
			return true
		}
	}
	return false
}

func (p turnToolPolicy) AllowedTools() []string {
	if p.allowAll {
		return nil
	}
	out := make([]string, 0, len(p.allowedToolSet))
	for name := range p.allowedToolSet {
		if _, denied := p.deniedToolSet[name]; denied {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

type toolPolicy struct {
	workspaceAbs string
	stateDirAbs  string
	providerName string
	cfg          config.ToolPolicyConfig

	mu           sync.RWMutex
	sessionModes map[string]turnToolMode
}

var workspaceCommandCueRegex = regexp.MustCompile(`(?i)\b(run|execute)\b.{0,40}\b(command|shell|terminal|bash|zsh|powershell|script|docker|git|npm|pnpm|yarn|make|go\s+test|go\s+build|go\s+run|pytest|cargo|mvn|gradle)\b`)
var workspaceToolingCueRegex = regexp.MustCompile(`(?i)\b(docker|git|npm|pnpm|yarn|go\s+test|go\s+build|go\s+run|pytest|cargo\s+test|make\s+(test|build|lint))\b`)
var workspaceFileOpCueRegex = regexp.MustCompile(`(?i)\b(read|write|edit|append|create|delete|rename|move|list|inspect|open|patch|update)\b.{0,40}\b(file|files|folder|directory|path|repo|codebase|source|project|module|package)\b`)
var shellLiteralCueRegex = regexp.MustCompile(`(?i)^\s*(ls|cat|pwd|grep|rg|sed|awk|find)\b`)

var commandTokenRegex = regexp.MustCompile(`"[^"]*"|'[^']*'|[^\s]+`)

var toolGroups = map[string][]string{
	"filesystem": {"read_file", "write_file", "list_dir", "edit_file", "append_file"},
	"shell":      {"exec", "process"},
	"web":        {"web_search", "web_fetch"},
	"messaging":  {"message"},
	"workflow":   {"cron", "spawn", "subagent", "session"},
}

func newToolPolicy(workspace, providerName string, cfg config.ToolPolicyConfig) *toolPolicy {
	workspaceAbs := ""
	stateDirAbs := ""
	if strings.TrimSpace(workspace) != "" {
		if abs, err := filepath.Abs(workspace); err == nil {
			workspaceAbs = filepath.Clean(abs)
			stateDirAbs = filepath.Join(workspaceAbs, "state")
		}
	}
	cfg.DefaultMode = string(normalizeToolMode(cfg.DefaultMode))
	if cfg.ProviderModes == nil {
		cfg.ProviderModes = map[string]string{}
	}

	return &toolPolicy{
		workspaceAbs: workspaceAbs,
		stateDirAbs:  stateDirAbs,
		providerName: strings.TrimSpace(strings.ToLower(providerName)),
		cfg:          cfg,
		sessionModes: map[string]turnToolMode{},
	}
}

func normalizeToolMode(raw string) turnToolMode {
	switch strings.TrimSpace(strings.ToLower(raw)) {
	case "workspace", "workspace_ops", "ops":
		return turnToolModeWorkspaceOps
	case "conversation", "chat":
		return turnToolModeConversation
	case "auto", "":
		return turnToolModeAuto
	default:
		return turnToolModeAuto
	}
}

func (p *toolPolicy) SetSessionMode(sessionKey string, mode turnToolMode) error {
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return fmt.Errorf("session key is required")
	}
	mode = normalizeToolMode(string(mode))
	if mode == turnToolModeAuto {
		p.ClearSessionMode(sessionKey)
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sessionModes[sessionKey] = mode
	return nil
}

func (p *toolPolicy) ClearSessionMode(sessionKey string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.sessionModes, strings.TrimSpace(sessionKey))
}

func (p *toolPolicy) SessionMode(sessionKey string) (turnToolMode, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	mode, ok := p.sessionModes[strings.TrimSpace(sessionKey)]
	return mode, ok
}

func (p *toolPolicy) baseModeForProvider() turnToolMode {
	if p == nil {
		return turnToolModeAuto
	}
	if modeRaw, ok := p.cfg.ProviderModes[p.providerName]; ok {
		return normalizeToolMode(modeRaw)
	}
	return normalizeToolMode(p.cfg.DefaultMode)
}

func (p *toolPolicy) resolveMode(sessionKey, userMessage string) turnToolMode {
	mode := p.baseModeForProvider()
	if sessionMode, ok := p.SessionMode(sessionKey); ok {
		mode = sessionMode
	}
	if mode == turnToolModeAuto {
		if looksLikeWorkspaceOperationRequest(userMessage) {
			return turnToolModeWorkspaceOps
		}
		return turnToolModeConversation
	}
	return mode
}

func (p *toolPolicy) PolicyForTurn(userMessage, sessionKey string) turnToolPolicy {
	mode := p.resolveMode(sessionKey, userMessage)
	policy := turnToolPolicy{
		Mode:           mode,
		allowAll:       mode == turnToolModeWorkspaceOps,
		allowedToolSet: map[string]struct{}{},
		allowedPrefix:  []string{},
		deniedToolSet:  map[string]struct{}{},
		deniedPrefix:   []string{},
	}

	if mode == turnToolModeConversation {
		policy.allowedToolSet["web_search"] = struct{}{}
		policy.allowedToolSet["web_fetch"] = struct{}{}
	}

	allowSet, allowPrefixes := expandToolSelectors(p.cfg.Allow)
	denySet, denyPrefixes := expandToolSelectors(p.cfg.Deny)
	if len(allowSet) > 0 {
		policy.allowAll = false
		policy.allowedToolSet = allowSet
	}
	if len(allowPrefixes) > 0 {
		policy.allowAll = false
		policy.allowedPrefix = allowPrefixes
	}
	for denied := range denySet {
		policy.deniedToolSet[denied] = struct{}{}
	}
	if len(denyPrefixes) > 0 {
		policy.deniedPrefix = denyPrefixes
	}
	return policy
}

func (p *toolPolicy) BuildSystemNote(policy turnToolPolicy) string {
	lines := []string{
		"## Tool Usage Policy (Current Turn)",
		fmt.Sprintf("- Active mode: `%s`", policy.Mode),
		"- Use recalled memory context + visible conversation history for continuity/identity answers.",
		"- Never inspect runtime state files/databases (`workspace/state/*`, `memory.db`, `state.json`) to answer conversational questions.",
	}
	if policy.Mode == turnToolModeConversation {
		allowed := policy.AllowedTools()
		if len(allowed) == 0 {
			lines = append(lines, "- No external tools are enabled for this turn.")
		} else {
			lines = append(lines, "- Enabled tools this turn: "+joinBackticked(allowed))
		}
		lines = append(lines, "- Local filesystem/shell tools are disabled unless the user explicitly requests workspace/system operations.")
		return strings.Join(lines, "\n")
	}

	if len(policy.deniedToolSet) > 0 {
		lines = append(lines, "- Blocked by policy: "+joinBackticked(sortedKeys(policy.deniedToolSet)))
	}
	lines = append(lines,
		"- Local tools are enabled because this turn appears to request workspace/system operations.",
		"- Internal runtime state paths remain protected.",
	)
	return strings.Join(lines, "\n")
}

func (p *toolPolicy) FilterDefinitions(defs []providers.ToolDefinition, policy turnToolPolicy) []providers.ToolDefinition {
	if policy.allowAll && len(policy.deniedToolSet) == 0 {
		return defs
	}
	out := make([]providers.ToolDefinition, 0, len(defs))
	for _, def := range defs {
		name := strings.TrimSpace(def.Function.Name)
		if name == "" {
			continue
		}
		if policy.Allows(name) {
			out = append(out, def)
		}
	}
	return out
}

func (p *toolPolicy) ValidateToolCall(policy turnToolPolicy, toolName string, args map[string]interface{}) (bool, string) {
	if !policy.Allows(toolName) {
		return false, fmt.Sprintf("tool %q is disabled for this turn mode (%s)", toolName, policy.Mode)
	}
	if p.touchesInternalState(toolName, args) {
		return false, "access to internal runtime state is blocked"
	}
	return true, ""
}

func (p *toolPolicy) touchesInternalState(toolName string, args map[string]interface{}) bool {
	switch toolName {
	case "read_file", "write_file", "append_file", "edit_file", "list_dir":
		path, _ := args["path"].(string)
		return p.isInternalStatePath(path)
	case "exec", "process":
		command, _ := args["command"].(string)
		return p.execTouchesInternalState(command)
	default:
		return false
	}
}

func (p *toolPolicy) execTouchesInternalState(command string) bool {
	command = strings.TrimSpace(command)
	if command == "" {
		return false
	}
	for _, token := range commandTokenRegex.FindAllString(command, -1) {
		cleaned := strings.Trim(token, "\"'`")
		if cleaned == "" {
			continue
		}
		if p.isInternalStatePath(cleaned) {
			return true
		}
	}
	lower := strings.ToLower(filepath.ToSlash(command))
	if strings.Contains(lower, "sqlite3") && strings.Contains(lower, "memory.db") {
		return true
	}
	if strings.Contains(lower, "/state/memory.db") || strings.Contains(lower, "/state/state.json") {
		return true
	}
	return false
}

func (p *toolPolicy) isInternalStatePath(raw string) bool {
	raw = strings.Trim(strings.TrimSpace(raw), "\"'`")
	if raw == "" {
		return false
	}

	lowerRaw := strings.ToLower(filepath.ToSlash(raw))
	if strings.Contains(lowerRaw, "/.dotagent/workspace/state/") || strings.HasSuffix(lowerRaw, "/.dotagent/workspace/state") {
		return true
	}
	if lowerRaw == "state/memory.db" || lowerRaw == "state/state.json" {
		return true
	}
	if strings.Contains(lowerRaw, "/state/memory.db") || strings.Contains(lowerRaw, "/state/state.json") {
		return true
	}

	if p.workspaceAbs == "" || p.stateDirAbs == "" {
		return false
	}

	resolved := raw
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(p.workspaceAbs, resolved)
	}
	absResolved, err := filepath.Abs(resolved)
	if err != nil {
		return false
	}
	return isPathWithin(filepath.Clean(absResolved), p.stateDirAbs)
}

func looksLikeWorkspaceOperationRequest(userMessage string) bool {
	msg := strings.TrimSpace(userMessage)
	if msg == "" {
		return false
	}
	if strings.HasPrefix(msg, "/") {
		return true
	}
	if strings.Contains(msg, "```") || shellLiteralCueRegex.MatchString(msg) {
		return true
	}
	return workspaceCommandCueRegex.MatchString(msg) ||
		workspaceToolingCueRegex.MatchString(msg) ||
		workspaceFileOpCueRegex.MatchString(msg)
}

func expandToolSelectors(selectors []string) (map[string]struct{}, []string) {
	out := map[string]struct{}{}
	prefixes := []string{}
	for _, raw := range selectors {
		sel := strings.TrimSpace(strings.ToLower(raw))
		if sel == "" {
			continue
		}
		if strings.HasPrefix(sel, "group:") {
			group := strings.TrimPrefix(sel, "group:")
			for _, toolName := range toolGroups[group] {
				out[toolName] = struct{}{}
			}
			continue
		}
		if strings.HasPrefix(sel, "prefix:") {
			prefix := strings.TrimSpace(strings.TrimPrefix(sel, "prefix:"))
			if prefix != "" {
				prefixes = append(prefixes, prefix)
			}
			continue
		}
		if strings.HasSuffix(sel, "*") && len(sel) > 1 {
			prefixes = append(prefixes, strings.TrimSuffix(sel, "*"))
			continue
		}
		out[sel] = struct{}{}
	}
	return out, prefixes
}

func isPathWithin(candidate, root string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(candidate))
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func sortedKeys(m map[string]struct{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func joinBackticked(items []string) string {
	if len(items) == 0 {
		return ""
	}
	quoted := make([]string, 0, len(items))
	for _, item := range items {
		quoted = append(quoted, "`"+item+"`")
	}
	return strings.Join(quoted, ", ")
}
