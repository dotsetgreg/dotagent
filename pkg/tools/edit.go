package tools

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// EditFileTool edits a file by replacing old_text with new_text.
// The old_text must exist exactly in the file.
type EditFileTool struct {
	allowedDir string
	restrict   bool
}

// NewEditFileTool creates a new EditFileTool with optional directory restriction.
func NewEditFileTool(allowedDir string, restrict bool) *EditFileTool {
	return &EditFileTool{
		allowedDir: allowedDir,
		restrict:   restrict,
	}
}

func (t *EditFileTool) Name() string {
	return "edit_file"
}

func (t *EditFileTool) Description() string {
	return "Edit a file by replacing old_text with new_text. Use match_index when old_text appears multiple times."
}

func (t *EditFileTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{
				"type":        "string",
				"description": "The file path to edit",
			},
			"old_text": map[string]interface{}{
				"type":        "string",
				"description": "The exact text to find and replace",
			},
			"new_text": map[string]interface{}{
				"type":        "string",
				"description": "The text to replace with",
			},
			"match_index": map[string]interface{}{
				"type":        "integer",
				"description": "Optional 1-based occurrence index when old_text appears multiple times",
			},
		},
		"required": []string{"path", "old_text", "new_text"},
	}
}

func (t *EditFileTool) Execute(ctx context.Context, args map[string]interface{}) *ToolResult {
	path, ok := args["path"].(string)
	if !ok {
		return ErrorResult("path is required")
	}

	oldText, ok := args["old_text"].(string)
	if !ok {
		return ErrorResult("old_text is required")
	}

	newText, ok := args["new_text"].(string)
	if !ok {
		return ErrorResult("new_text is required")
	}

	resolvedPath, err := validatePath(path, t.allowedDir, t.restrict)
	if err != nil {
		return ErrorResult(err.Error())
	}

	if _, err := os.Stat(resolvedPath); os.IsNotExist(err) {
		return ErrorResult(fmt.Sprintf("file not found: %s", path))
	}

	content, err := os.ReadFile(resolvedPath)
	if err != nil {
		return ErrorResult(fmt.Sprintf("failed to read file: %v", err))
	}

	contentStr := string(content)

	if !strings.Contains(contentStr, oldText) {
		return ErrorResult("old_text not found in file. Make sure it matches exactly")
	}

	count := strings.Count(contentStr, oldText)
	matchIndex, err := readOptionalInt(args, "match_index", 0)
	if err != nil {
		return ErrorResult(err.Error())
	}
	if count > 1 {
		if matchIndex <= 0 {
			return ErrorResult(fmt.Sprintf("old_text appears %d times. Provide match_index (1-%d) to choose which occurrence to replace", count, count))
		}
		if matchIndex > count {
			return ErrorResult(fmt.Sprintf("match_index %d is out of range; old_text appears %d times", matchIndex, count))
		}
	} else if matchIndex > 1 {
		return ErrorResult("match_index is out of range for a single match")
	}
	targetIdx := firstMatchOffset(contentStr, oldText, matchIndex)
	if targetIdx < 0 {
		return ErrorResult("failed to resolve selected match_index")
	}
	newContent := contentStr[:targetIdx] + newText + contentStr[targetIdx+len(oldText):]

	if err := os.WriteFile(resolvedPath, []byte(newContent), 0644); err != nil {
		return ErrorResult(fmt.Sprintf("failed to write file: %v", err))
	}

	return SilentResult(fmt.Sprintf("File edited: %s", path))
}

func firstMatchOffset(content, oldText string, matchIndex int) int {
	if matchIndex <= 0 {
		matchIndex = 1
	}
	searchFrom := 0
	for i := 1; i <= matchIndex; i++ {
		next := strings.Index(content[searchFrom:], oldText)
		if next < 0 {
			return -1
		}
		abs := searchFrom + next
		if i == matchIndex {
			return abs
		}
		searchFrom = abs + len(oldText)
	}
	return -1
}

type AppendFileTool struct {
	workspace string
	restrict  bool
}

func NewAppendFileTool(workspace string, restrict bool) *AppendFileTool {
	return &AppendFileTool{workspace: workspace, restrict: restrict}
}

func (t *AppendFileTool) Name() string {
	return "append_file"
}

func (t *AppendFileTool) Description() string {
	return "Append content to the end of a file"
}

func (t *AppendFileTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{
				"type":        "string",
				"description": "The file path to append to",
			},
			"content": map[string]interface{}{
				"type":        "string",
				"description": "The content to append",
			},
		},
		"required": []string{"path", "content"},
	}
}

func (t *AppendFileTool) Execute(ctx context.Context, args map[string]interface{}) *ToolResult {
	path, ok := args["path"].(string)
	if !ok {
		return ErrorResult("path is required")
	}

	content, ok := args["content"].(string)
	if !ok {
		return ErrorResult("content is required")
	}

	resolvedPath, err := validatePath(path, t.workspace, t.restrict)
	if err != nil {
		return ErrorResult(err.Error())
	}

	f, err := os.OpenFile(resolvedPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return ErrorResult(fmt.Sprintf("failed to open file: %v", err))
	}
	defer f.Close()

	if _, err := f.WriteString(content); err != nil {
		return ErrorResult(fmt.Sprintf("failed to append to file: %v", err))
	}

	return SilentResult(fmt.Sprintf("Appended to %s", path))
}
