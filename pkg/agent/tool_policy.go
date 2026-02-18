package agent

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/dotsetgreg/dotagent/pkg/providers"
)

type turnToolMode string

const (
	turnToolModeConversation turnToolMode = "conversation"
	turnToolModeWorkspaceOps turnToolMode = "workspace_ops"
)

type turnToolPolicy struct {
	Mode           turnToolMode
	allowAll       bool
	allowedToolSet map[string]struct{}
}

func (p turnToolPolicy) Allows(toolName string) bool {
	if p.allowAll {
		return true
	}
	_, ok := p.allowedToolSet[toolName]
	return ok
}

func (p turnToolPolicy) AllowedTools() []string {
	if p.allowAll {
		return nil
	}
	out := make([]string, 0, len(p.allowedToolSet))
	for name := range p.allowedToolSet {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

type toolPolicy struct {
	workspaceAbs string
	stateDirAbs  string
}

var workspaceCommandCueRegex = regexp.MustCompile(`(?i)\b(run|execute)\b.{0,40}\b(command|shell|terminal|bash|zsh|powershell|script|docker|git|npm|pnpm|yarn|make|go\s+test|go\s+build|go\s+run|pytest|cargo|mvn|gradle)\b`)
var workspaceToolingCueRegex = regexp.MustCompile(`(?i)\b(docker|git|npm|pnpm|yarn|go\s+test|go\s+build|go\s+run|pytest|cargo\s+test|make\s+(test|build|lint))\b`)
var workspaceFileOpCueRegex = regexp.MustCompile(`(?i)\b(read|write|edit|append|create|delete|rename|move|list|inspect|open|patch|update)\b.{0,40}\b(file|files|folder|directory|path|repo|codebase|source|project|module|package)\b`)
var shellLiteralCueRegex = regexp.MustCompile(`(?i)^\s*(ls|cat|pwd|grep|rg|sed|awk|find)\b`)

var commandTokenRegex = regexp.MustCompile(`"[^"]*"|'[^']*'|[^\s]+`)

func newToolPolicy(workspace string) *toolPolicy {
	workspaceAbs := ""
	stateDirAbs := ""
	if strings.TrimSpace(workspace) != "" {
		if abs, err := filepath.Abs(workspace); err == nil {
			workspaceAbs = filepath.Clean(abs)
			stateDirAbs = filepath.Join(workspaceAbs, "state")
		}
	}
	return &toolPolicy{
		workspaceAbs: workspaceAbs,
		stateDirAbs:  stateDirAbs,
	}
}

func (p *toolPolicy) PolicyForTurn(userMessage string) turnToolPolicy {
	if looksLikeWorkspaceOperationRequest(userMessage) {
		return turnToolPolicy{
			Mode:     turnToolModeWorkspaceOps,
			allowAll: true,
		}
	}

	// Conversation mode: keep local system tools off to avoid runtime-state
	// introspection and force continuity responses through memory context.
	return turnToolPolicy{
		Mode: turnToolModeConversation,
		allowedToolSet: map[string]struct{}{
			"web_search": {},
			"web_fetch":  {},
		},
	}
}

func (p *toolPolicy) BuildSystemNote(policy turnToolPolicy) string {
	lines := []string{
		"## Tool Usage Policy (Current Turn)",
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
		lines = append(lines, "- Local filesystem/shell/hardware tools are disabled unless the user explicitly requests workspace/system operations.")
		return strings.Join(lines, "\n")
	}

	lines = append(lines,
		"- Local tools are enabled because this turn appears to request workspace/system operations.",
		"- Internal runtime state paths remain protected.",
	)
	return strings.Join(lines, "\n")
}

func (p *toolPolicy) FilterDefinitions(defs []providers.ToolDefinition, policy turnToolPolicy) []providers.ToolDefinition {
	if policy.allowAll {
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
	case "exec":
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
