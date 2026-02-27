package agent

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/dotsetgreg/dotagent/pkg/logger"
	"github.com/dotsetgreg/dotagent/pkg/providers"
	"github.com/dotsetgreg/dotagent/pkg/skills"
	"github.com/dotsetgreg/dotagent/pkg/tools"
)

type ContextBuilder struct {
	workspace             string
	skillsLoader          *skills.SkillsLoader
	tools                 *tools.ToolRegistry // Direct reference to tool registry
	bootstrapConflictOnce sync.Once
}

type SystemPromptMetadata struct {
	Hash              string
	BootstrapFile     string
	BootstrapConflict bool
}

func instanceRootFromWorkspace(workspace string) string {
	workspace = strings.TrimSpace(workspace)
	if workspace == "" {
		return ""
	}
	return filepath.Dir(workspace)
}

func NewContextBuilder(workspace string) *ContextBuilder {
	// builtin skills: skills directory in current project
	// Use the skills/ directory under the current working directory
	wd, _ := os.Getwd()
	builtinSkillsDir := filepath.Join(wd, "skills")
	globalSkillsDir := filepath.Join(instanceRootFromWorkspace(workspace), "skills")

	return &ContextBuilder{
		workspace:    workspace,
		skillsLoader: skills.NewSkillsLoader(workspace, globalSkillsDir, builtinSkillsDir),
	}
}

// SetToolsRegistry sets the tools registry for dynamic tool summary generation.
func (cb *ContextBuilder) SetToolsRegistry(registry *tools.ToolRegistry) {
	cb.tools = registry
}

func (cb *ContextBuilder) getIdentity() string {
	workspacePath, _ := filepath.Abs(filepath.Join(cb.workspace))
	runtime := fmt.Sprintf("%s %s, Go %s", runtime.GOOS, runtime.GOARCH, runtime.Version())

	// Build tools section dynamically
	toolsSection := cb.buildToolsSection()

	return fmt.Sprintf(`# dotagent

You are the active assistant for this workspace.
Use the dynamic persona context block as the canonical source of identity, role, and communication style.

## Runtime
%s

## Workspace
Your workspace is at: %s
- Skills: %s/skills/{skill-name}/SKILL.md

%s

## Important Rules

1. **Use tools when needed** - Use tools for external actions or verification (file edits, command execution, web lookups). Do not call tools for conversational continuity if recalled context already provides the answer.

2. **Be helpful and accurate** - When using tools, briefly explain what you're doing.

3. **Memory** - Memory capture and retrieval are automatic; use recalled memory context and current conversation state.

4. **Context honesty** - Never claim you cannot access prior messages or memory unless the current turn explicitly lacks that context and you state that limitation precisely.

5. **No runtime introspection for continuity** - Do not inspect runtime state files/databases (workspace/state/*, memory.db, state.json) to answer "do you remember" or identity-continuity questions. Use memory/context provided in the prompt.

6. **Capability clarity** - If you cannot comply with a requested style/behavior because of model/provider constraints, say that explicitly instead of denying stored persona.`,
		runtime, workspacePath, workspacePath, toolsSection)
}

func (cb *ContextBuilder) buildToolsSection() string {
	if cb.tools == nil {
		return ""
	}

	summaries := cb.tools.GetSummaries()
	if len(summaries) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("## Available Tools\n\n")
	sb.WriteString("Use tools when required by user intent or external verification. Do not use tools to infer conversational memory/identity continuity from runtime files.\n\n")
	sb.WriteString("You have access to the following tools:\n\n")
	for _, s := range summaries {
		sb.WriteString(s)
		sb.WriteString("\n")
	}

	return sb.String()
}

func (cb *ContextBuilder) BuildSystemPrompt() string {
	prompt, _ := cb.BuildSystemPromptWithMetadata()
	return prompt
}

func (cb *ContextBuilder) BuildSystemPromptWithMetadata() (string, SystemPromptMetadata) {
	meta := SystemPromptMetadata{}
	parts := []string{}

	// Core identity section
	parts = append(parts, cb.getIdentity())

	// Bootstrap files
	bootstrapContent, bootstrapFile, bootstrapConflict := cb.loadBootstrapSelection()
	if bootstrapContent != "" {
		parts = append(parts, bootstrapContent)
		meta.BootstrapFile = bootstrapFile
		meta.BootstrapConflict = bootstrapConflict
	}

	// Skills - show summary, AI can read full content with read_file tool
	skillsSummary := cb.skillsLoader.BuildSkillsSummary()
	if skillsSummary != "" {
		parts = append(parts, fmt.Sprintf(`# Skills

The following skills extend your capabilities. To use a skill, read its SKILL.md file using the read_file tool.

%s`, skillsSummary))
	}

	prompt := strings.Join(parts, "\n\n---\n\n")
	sum := sha1.Sum([]byte(prompt))
	meta.Hash = hex.EncodeToString(sum[:16])
	return prompt, meta
}

func (cb *ContextBuilder) LoadBootstrapFiles() string {
	content, _, _ := cb.loadBootstrapSelection()
	return content
}

func normalizeBootstrapContent(in string) string {
	in = strings.ReplaceAll(in, "\r\n", "\n")
	return strings.TrimSpace(in)
}

func (cb *ContextBuilder) loadBootstrapSelection() (content string, sourceFile string, conflict bool) {
	const (
		agentsFile = "AGENTS.md"
		agentFile  = "AGENT.md"
	)

	readBootstrap := func(name string) (string, bool) {
		data, err := os.ReadFile(filepath.Join(cb.workspace, name))
		if err != nil {
			return "", false
		}
		normalized := normalizeBootstrapContent(string(data))
		if normalized == "" {
			return "", false
		}
		return normalized, true
	}

	agentsContent, hasAgents := readBootstrap(agentsFile)
	agentContent, hasAgent := readBootstrap(agentFile)
	switch {
	case hasAgents:
		content = fmt.Sprintf("## %s\n\n%s", agentsFile, agentsContent)
		sourceFile = agentsFile
	case hasAgent:
		content = fmt.Sprintf("## %s\n\n%s", agentFile, agentContent)
		sourceFile = agentFile
	default:
		return "", "", false
	}

	if hasAgents && hasAgent && agentsContent != agentContent {
		conflict = true
		cb.bootstrapConflictOnce.Do(func() {
			logger.WarnCF("agent", "Bootstrap conflict detected; applying deterministic precedence", map[string]interface{}{
				"selected":    agentsFile,
				"ignored":     agentFile,
				"workspace":   cb.workspace,
				"determinism": "enabled",
			})
		})
		notice := "## Bootstrap Notice\n\nBoth AGENTS.md and AGENT.md are present with different content. AGENTS.md is applied by deterministic precedence."
		content = notice + "\n\n" + content
	}
	return content, sourceFile, conflict
}

func (cb *ContextBuilder) BuildMessages(history []providers.Message, summary string, recalledMemory string, currentMessage string, media []string, channel, chatID string) []providers.Message {
	systemPrompt := cb.BuildSystemPrompt()
	return cb.BuildMessagesWithSystemPrompt(systemPrompt, history, summary, recalledMemory, currentMessage, media, channel, chatID)
}

func (cb *ContextBuilder) BuildMessagesWithSystemPrompt(systemPrompt string, history []providers.Message, summary string, recalledMemory string, currentMessage string, media []string, channel, chatID string) []providers.Message {
	messages := []providers.Message{}

	// Log system prompt summary for debugging (debug mode only)
	logger.DebugCF("agent", "System prompt built",
		map[string]interface{}{
			"total_chars":   len(systemPrompt),
			"total_lines":   strings.Count(systemPrompt, "\n") + 1,
			"section_count": strings.Count(systemPrompt, "\n\n---\n\n") + 1,
		})

	// Log preview of system prompt (avoid logging huge content)
	preview := systemPrompt
	if len(preview) > 500 {
		preview = preview[:500] + "... (truncated)"
	}
	logger.DebugCF("agent", "System prompt preview",
		map[string]interface{}{
			"preview": preview,
		})

	// Drop leading orphaned tool messages because providers require a matching
	// assistant tool call before any tool role message.
	for len(history) > 0 && (history[0].Role == "tool") {
		logger.DebugCF("agent", "Removing orphaned tool message from history to prevent LLM error",
			map[string]interface{}{"role": history[0].Role})
		history = history[1:]
	}

	messages = append(messages, providers.Message{
		Role:    "system",
		Content: systemPrompt,
	})
	dynamicBlocks := make([]string, 0, 3)
	if channel != "" && chatID != "" {
		dynamicBlocks = append(dynamicBlocks, fmt.Sprintf("## Current Session\nChannel: %s\nChat ID: %s", channel, chatID))
	}
	if strings.TrimSpace(summary) != "" {
		dynamicBlocks = append(dynamicBlocks, "## Summary of Previous Conversation\n\n"+strings.TrimSpace(summary))
	}
	if strings.TrimSpace(recalledMemory) != "" {
		dynamicBlocks = append(dynamicBlocks, strings.TrimSpace(recalledMemory))
	}
	if len(dynamicBlocks) > 0 {
		messages = append(messages, providers.Message{
			Role:    "system",
			Content: strings.Join(dynamicBlocks, "\n\n"),
		})
	}

	messages = append(messages, history...)

	if strings.TrimSpace(currentMessage) != "" {
		messages = append(messages, providers.Message{
			Role:    "user",
			Content: currentMessage,
		})
	}

	return messages
}

// GetSkillsInfo returns information about loaded skills.
func (cb *ContextBuilder) GetSkillsInfo() map[string]interface{} {
	allSkills := cb.skillsLoader.ListSkills()
	skillNames := make([]string, 0, len(allSkills))
	for _, s := range allSkills {
		skillNames = append(skillNames, s.Name)
	}
	return map[string]interface{}{
		"total":     len(allSkills),
		"available": len(allSkills),
		"names":     skillNames,
	}
}
