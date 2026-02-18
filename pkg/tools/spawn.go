package tools

import (
	"context"
	"fmt"
	"sync"
)

type SpawnTool struct {
	manager       *SubagentManager
	originChannel string
	originChatID  string
	callback      AsyncCallback // For async completion notification
	mu            sync.RWMutex
}

func NewSpawnTool(manager *SubagentManager) *SpawnTool {
	return &SpawnTool{
		manager:       manager,
		originChannel: "cli",
		originChatID:  "direct",
	}
}

// SetCallback implements AsyncTool interface for async completion notification
func (t *SpawnTool) SetCallback(cb AsyncCallback) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.callback = cb
}

func (t *SpawnTool) Name() string {
	return "spawn"
}

func (t *SpawnTool) Description() string {
	return "Spawn a subagent to handle a task in the background. Use this for complex or time-consuming tasks that can run independently. The subagent will complete the task and report back when done."
}

func (t *SpawnTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"task": map[string]interface{}{
				"type":        "string",
				"description": "The task for subagent to complete",
			},
			"label": map[string]interface{}{
				"type":        "string",
				"description": "Optional short label for the task (for display)",
			},
		},
		"required": []string{"task"},
	}
}

func (t *SpawnTool) SetContext(channel, chatID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.originChannel = channel
	t.originChatID = chatID
}

func (t *SpawnTool) Execute(ctx context.Context, args map[string]interface{}) *ToolResult {
	task, ok := args["task"].(string)
	if !ok {
		return ErrorResult("task is required")
	}

	label, _ := args["label"].(string)

	if t.manager == nil {
		return ErrorResult("Subagent manager not configured")
	}

	originChannel, originChatID := channelChatFromContext(ctx)
	callback := asyncCallbackFromContext(ctx)

	t.mu.RLock()
	if originChannel == "" {
		originChannel = t.originChannel
	}
	if originChatID == "" {
		originChatID = t.originChatID
	}
	if callback == nil {
		callback = t.callback
	}
	t.mu.RUnlock()

	// Pass callback to manager for async completion notification
	result, err := t.manager.Spawn(ctx, task, label, originChannel, originChatID, callback)
	if err != nil {
		return ErrorResult(fmt.Sprintf("failed to spawn subagent: %v", err))
	}

	// Return AsyncResult since the task runs in background
	return AsyncResult(result)
}
