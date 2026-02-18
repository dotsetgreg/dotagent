package tools

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var commandTemplatePlaceholderRegex = regexp.MustCompile(`\{\{\s*([a-zA-Z0-9_]+)\s*\}\}`)

type TemplateCommandTool struct {
	name            string
	description     string
	parameters      map[string]interface{}
	commandTemplate string
	workingDir      string
	exec            *ExecTool
}

type TemplateCommandConfig struct {
	Name            string
	Description     string
	Parameters      map[string]interface{}
	CommandTemplate string
	WorkingDir      string
	TimeoutSeconds  int
	Workspace       string
	Restrict        bool
}

func NewTemplateCommandTool(cfg TemplateCommandConfig) *TemplateCommandTool {
	execTool := NewExecTool(cfg.Workspace, cfg.Restrict)
	if cfg.TimeoutSeconds > 0 {
		execTool.SetTimeout(time.Duration(cfg.TimeoutSeconds) * time.Second)
	}
	if cfg.Parameters == nil {
		cfg.Parameters = map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		}
	}
	return &TemplateCommandTool{
		name:            cfg.Name,
		description:     cfg.Description,
		parameters:      cfg.Parameters,
		commandTemplate: cfg.CommandTemplate,
		workingDir:      cfg.WorkingDir,
		exec:            execTool,
	}
}

func (t *TemplateCommandTool) Name() string {
	return t.name
}

func (t *TemplateCommandTool) Description() string {
	return t.description
}

func (t *TemplateCommandTool) Parameters() map[string]interface{} {
	return t.parameters
}

func (t *TemplateCommandTool) Execute(ctx context.Context, args map[string]interface{}) *ToolResult {
	command, err := renderCommandTemplate(t.commandTemplate, args)
	if err != nil {
		return ErrorResult(err.Error())
	}
	execArgs := map[string]interface{}{
		"command": command,
	}
	if strings.TrimSpace(t.workingDir) != "" {
		execArgs["working_dir"] = t.workingDir
	}
	return t.exec.Execute(ctx, execArgs)
}

func renderCommandTemplate(template string, args map[string]interface{}) (string, error) {
	template = strings.TrimSpace(template)
	if template == "" {
		return "", fmt.Errorf("command template is empty")
	}
	out := commandTemplatePlaceholderRegex.ReplaceAllStringFunc(template, func(match string) string {
		keyMatches := commandTemplatePlaceholderRegex.FindStringSubmatch(match)
		if len(keyMatches) != 2 {
			return ""
		}
		key := keyMatches[1]
		raw, ok := args[key]
		if !ok {
			// sentinel marker for missing arg; handled below
			return "<<missing:" + key + ">>"
		}
		return shellQuote(renderTemplateValue(raw))
	})
	if missing := findMissingTemplateArg(out); missing != "" {
		return "", fmt.Errorf("missing required template argument: %s", missing)
	}
	return out, nil
}

func findMissingTemplateArg(rendered string) string {
	start := strings.Index(rendered, "<<missing:")
	if start < 0 {
		return ""
	}
	end := strings.Index(rendered[start:], ">>")
	if end < 0 {
		return "unknown"
	}
	token := rendered[start : start+end+2]
	token = strings.TrimPrefix(token, "<<missing:")
	token = strings.TrimSuffix(token, ">>")
	return token
}

func renderTemplateValue(v interface{}) string {
	switch tv := v.(type) {
	case string:
		return tv
	case float64:
		if tv == float64(int64(tv)) {
			return strconv.FormatInt(int64(tv), 10)
		}
		return strconv.FormatFloat(tv, 'f', -1, 64)
	case bool:
		if tv {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprintf("%v", tv)
	}
}

func shellQuote(raw string) string {
	if raw == "" {
		return "''"
	}
	escaped := strings.ReplaceAll(raw, `'`, `'\''`)
	return "'" + escaped + "'"
}
