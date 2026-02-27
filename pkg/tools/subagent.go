package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dotsetgreg/dotagent/pkg/bus"
	"github.com/dotsetgreg/dotagent/pkg/logger"
	"github.com/dotsetgreg/dotagent/pkg/providers"
)

type SubagentTask struct {
	ID                 string `json:"id"`
	Task               string `json:"task"`
	Label              string `json:"label"`
	OriginChannel      string `json:"origin_channel"`
	OriginChatID       string `json:"origin_chat_id"`
	Status             string `json:"status"`
	Result             string `json:"result"`
	Created            int64  `json:"created"`
	Updated            int64  `json:"updated"`
	CompletedAt        int64  `json:"completed_at,omitempty"`
	CompletionNotified bool   `json:"completion_notified,omitempty"`
	LastNotifyError    string `json:"last_notify_error,omitempty"`
}

type SubagentManager struct {
	tasks                  map[string]*SubagentTask
	mu                     sync.RWMutex
	provider               providers.LLMProvider
	defaultModel           string
	bus                    *bus.MessageBus
	workspace              string
	stateRoot              string
	workspaceContext       string
	tools                  *ToolRegistry
	maxIterations          int
	contextWindow          int
	contextPruningMode     string
	contextPruningKeepLast int
	maxOverflowCompactions int
	retry                  providers.RetryConfig
	loopDetection          ToolLoopDetectionConfig
	nextID                 int
	statePath              string
	pendingResumeIDs       []string
	pendingNotifyIDs       []string
	recoveryOnce           sync.Once
}

type persistedSubagentState struct {
	Version int             `json:"version"`
	NextID  int             `json:"next_id"`
	Tasks   []*SubagentTask `json:"tasks"`
}

type SubagentLoopRuntimeOptions struct {
	ContextWindowTokens    int
	ContextPruningMode     string
	ContextPruningKeepLast int
	MaxOverflowCompactions int
	Retry                  providers.RetryConfig
	LoopDetection          ToolLoopDetectionConfig
}

const (
	subagentStateVersion = 1
	subagentStateFile    = "subagent_tasks.json"
)

func NewSubagentManager(provider providers.LLMProvider, defaultModel, workspace string, stateRoot string, bus *bus.MessageBus) *SubagentManager {
	if strings.TrimSpace(stateRoot) == "" {
		stateRoot = workspace
	}
	manager := &SubagentManager{
		tasks:                  make(map[string]*SubagentTask),
		provider:               provider,
		defaultModel:           defaultModel,
		bus:                    bus,
		workspace:              workspace,
		stateRoot:              stateRoot,
		workspaceContext:       strings.TrimSpace(workspace),
		tools:                  NewToolRegistry(),
		maxIterations:          10,
		contextWindow:          16384,
		contextPruningMode:     "off",
		contextPruningKeepLast: 5,
		maxOverflowCompactions: 2,
		retry:                  providers.DefaultRetryConfig(),
		nextID:                 1,
		statePath:              filepath.Join(stateRoot, "state", subagentStateFile),
	}
	if err := manager.loadState(); err != nil {
		logger.WarnCF("subagent", "Failed loading persisted subagent tasks", map[string]interface{}{
			"error": err.Error(),
			"path":  manager.statePath,
		})
	}
	return manager
}

func (sm *SubagentManager) ConfigureLoopRuntime(opts SubagentLoopRuntimeOptions) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if opts.ContextWindowTokens > 0 {
		sm.contextWindow = opts.ContextWindowTokens
	}
	if strings.TrimSpace(opts.ContextPruningMode) != "" {
		sm.contextPruningMode = strings.TrimSpace(opts.ContextPruningMode)
	}
	if opts.ContextPruningKeepLast > 0 {
		sm.contextPruningKeepLast = opts.ContextPruningKeepLast
	}
	if opts.MaxOverflowCompactions > 0 {
		sm.maxOverflowCompactions = opts.MaxOverflowCompactions
	}
	if opts.Retry.MaxAttempts > 0 {
		sm.retry = opts.Retry
	}
	sm.loopDetection = opts.LoopDetection
}

// SetTools sets the tool registry for subagent execution.
// If not set, subagent will have access to the provided tools.
func (sm *SubagentManager) SetTools(tools *ToolRegistry) {
	sm.mu.Lock()
	sm.tools = tools
	sm.mu.Unlock()
	sm.recoveryOnce.Do(func() {
		go sm.recoverPendingState()
	})
}

// RegisterTool registers a tool for subagent execution.
func (sm *SubagentManager) RegisterTool(tool Tool) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if err := sm.tools.Register(tool); err != nil {
		logger.ErrorCF("subagent", "Failed registering subagent tool", map[string]interface{}{
			"tool":  tool.Name(),
			"error": err.Error(),
		})
	}
}

// SetWorkspaceContext injects workspace/tools context into subagent prompts.
func (sm *SubagentManager) SetWorkspaceContext(workspaceContext string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.workspaceContext = strings.TrimSpace(workspaceContext)
}

// pruneCompleted removes completed/failed/cancelled tasks older than 30 minutes.
// Must be called while sm.mu write lock is held.
func (sm *SubagentManager) pruneCompleted() {
	cutoff := time.Now().Add(-30 * time.Minute).UnixMilli()
	for id, task := range sm.tasks {
		if task.Status == "running" || task.Status == "queued_resume" {
			continue
		}
		compareTS := task.Updated
		if compareTS == 0 {
			compareTS = task.Created
		}
		if compareTS < cutoff {
			delete(sm.tasks, id)
		}
	}
}

func (sm *SubagentManager) Spawn(ctx context.Context, task, label, originChannel, originChatID string, callback AsyncCallback) (string, error) {
	sm.mu.Lock()
	sm.pruneCompleted()

	now := time.Now().UnixMilli()
	taskID := fmt.Sprintf("subagent-%d", sm.nextID)
	sm.nextID++

	subagentTask := &SubagentTask{
		ID:                 taskID,
		Task:               task,
		Label:              label,
		OriginChannel:      originChannel,
		OriginChatID:       originChatID,
		Status:             "running",
		Created:            now,
		Updated:            now,
		CompletionNotified: true,
	}
	sm.tasks[taskID] = subagentTask
	if err := sm.persistStateLocked(); err != nil {
		logger.WarnCF("subagent", "Failed persisting spawned subagent task", map[string]interface{}{
			"task_id": taskID,
			"error":   err.Error(),
		})
	}
	sm.mu.Unlock()

	// Start task in background with context cancellation support
	go sm.runTask(ctx, taskID, callback)

	if label != "" {
		return fmt.Sprintf("Spawned subagent '%s' for task: %s", label, task), nil
	}
	return fmt.Sprintf("Spawned subagent for task: %s", task), nil
}

func (sm *SubagentManager) runTask(ctx context.Context, taskID string, callback AsyncCallback) {
	sm.mu.Lock()
	task := sm.tasks[taskID]
	if task == nil {
		sm.mu.Unlock()
		return
	}
	now := time.Now().UnixMilli()
	task.Status = "running"
	if task.Created == 0 {
		task.Created = now
	}
	task.Updated = now
	task.CompletedAt = 0
	task.CompletionNotified = true
	task.LastNotifyError = ""
	workspaceContext := sm.workspaceContext
	originChannel := task.OriginChannel
	originChatID := task.OriginChatID
	taskPrompt := task.Task
	label := task.Label
	if err := sm.persistStateLocked(); err != nil {
		logger.WarnCF("subagent", "Failed persisting running state", map[string]interface{}{
			"task_id": taskID,
			"error":   err.Error(),
		})
	}
	sm.mu.Unlock()

	// Build system prompt for subagent
	systemPrompt := buildSubagentSystemPrompt(workspaceContext)

	messages := []providers.Message{
		{
			Role:    "system",
			Content: systemPrompt,
		},
		{
			Role:    "user",
			Content: taskPrompt,
		},
	}

	// Check if context is already cancelled before starting
	select {
	case <-ctx.Done():
		sm.mu.Lock()
		if existing := sm.tasks[taskID]; existing != nil {
			existing.Status = "cancelled"
			existing.Result = "Task cancelled before execution"
			existing.Updated = time.Now().UnixMilli()
			existing.CompletedAt = existing.Updated
			existing.CompletionNotified = true
			existing.LastNotifyError = ""
			_ = sm.persistStateLocked()
		}
		sm.mu.Unlock()
		return
	default:
	}

	// Run tool loop with access to tools
	sm.mu.RLock()
	tools := sm.tools
	maxIter := sm.maxIterations
	contextWindow := sm.contextWindow
	contextPruningMode := sm.contextPruningMode
	contextPruningKeepLast := sm.contextPruningKeepLast
	maxOverflowCompactions := sm.maxOverflowCompactions
	retryCfg := sm.retry
	loopDetection := sm.loopDetection
	sm.mu.RUnlock()
	initialMessages := cloneSubagentMessages(messages)

	loopResult, err := RunToolLoop(ctx, ToolLoopConfig{
		Provider:               sm.provider,
		Model:                  sm.defaultModel,
		Tools:                  tools,
		MaxIterations:          maxIter,
		ContextWindowTokens:    contextWindow,
		ContextPruningMode:     contextPruningMode,
		ContextPruningKeepLast: contextPruningKeepLast,
		MaxOverflowCompactions: maxOverflowCompactions,
		Retry:                  retryCfg,
		LoopDetection:          loopDetection,
		LLMOptions: map[string]any{
			"max_tokens":  4096,
			"temperature": 0.7,
		},
		RebuildContext: func(ctx context.Context) ([]providers.Message, error) {
			return cloneSubagentMessages(initialMessages), nil
		},
	}, messages, originChannel, originChatID)

	var result *ToolResult
	now = time.Now().UnixMilli()
	if err != nil {
		result = &ToolResult{
			ForLLM:  fmt.Sprintf("Error: %v", err),
			ForUser: "",
			Silent:  false,
			IsError: true,
			Async:   false,
			Err:     err,
		}
		// Check if it was cancelled
		if ctx.Err() != nil {
			result.ForLLM = "Task cancelled during execution"
		}
	} else {
		result = &ToolResult{
			ForLLM:  fmt.Sprintf("Subagent '%s' completed (iterations: %d): %s", label, loopResult.Iterations, loopResult.Content),
			ForUser: loopResult.Content,
			Silent:  false,
			IsError: false,
			Async:   false,
		}
	}

	sm.mu.Lock()
	task = sm.tasks[taskID]
	if task != nil {
		task.Result = result.ForLLM
		if err != nil {
			task.Status = "failed"
			if ctx.Err() != nil {
				task.Status = "cancelled"
			}
		} else {
			task.Status = "completed"
		}
		task.Updated = now
		task.CompletedAt = now
		task.CompletionNotified = false
		task.LastNotifyError = ""
		if persistErr := sm.persistStateLocked(); persistErr != nil {
			logger.WarnCF("subagent", "Failed persisting finished subagent task", map[string]interface{}{
				"task_id": taskID,
				"error":   persistErr.Error(),
			})
		}
	}
	taskSnapshot := cloneSubagentTask(task)
	sm.mu.Unlock()

	if callback != nil && result != nil {
		callback(ctx, result)
	}
	if taskSnapshot == nil {
		return
	}

	publishErr := sm.publishCompletionAnnouncement(*taskSnapshot)
	sm.mu.Lock()
	if existing := sm.tasks[taskID]; existing != nil {
		if publishErr == nil {
			existing.CompletionNotified = true
			existing.LastNotifyError = ""
		} else {
			existing.CompletionNotified = false
			existing.LastNotifyError = publishErr.Error()
			sm.addPendingNotifyLocked(taskID)
		}
		existing.Updated = time.Now().UnixMilli()
		if persistErr := sm.persistStateLocked(); persistErr != nil {
			logger.WarnCF("subagent", "Failed persisting completion notification state", map[string]interface{}{
				"task_id": taskID,
				"error":   persistErr.Error(),
			})
		}
	}
	sm.mu.Unlock()
}

func (sm *SubagentManager) recoverPendingState() {
	sm.mu.Lock()
	resumeIDs := append([]string(nil), sm.pendingResumeIDs...)
	notifyIDs := append([]string(nil), sm.pendingNotifyIDs...)
	sm.pendingResumeIDs = nil
	sm.pendingNotifyIDs = nil
	sm.mu.Unlock()

	for _, taskID := range resumeIDs {
		go sm.runTask(context.Background(), taskID, nil)
	}
	for _, taskID := range notifyIDs {
		go sm.retryPendingAnnouncement(taskID)
	}
}

func (sm *SubagentManager) retryPendingAnnouncement(taskID string) {
	sm.mu.RLock()
	task := cloneSubagentTask(sm.tasks[taskID])
	sm.mu.RUnlock()
	if task == nil {
		return
	}
	err := sm.publishCompletionAnnouncement(*task)
	sm.mu.Lock()
	defer sm.mu.Unlock()
	existing := sm.tasks[taskID]
	if existing == nil {
		return
	}
	if err == nil {
		existing.CompletionNotified = true
		existing.LastNotifyError = ""
	} else {
		existing.CompletionNotified = false
		existing.LastNotifyError = err.Error()
		sm.addPendingNotifyLocked(taskID)
	}
	existing.Updated = time.Now().UnixMilli()
	_ = sm.persistStateLocked()
}

func (sm *SubagentManager) publishCompletionAnnouncement(task SubagentTask) error {
	if sm.bus == nil {
		return nil
	}
	announceContent := fmt.Sprintf("Task '%s' completed.\n\nResult:\n%s", task.Label, task.Result)
	msg := bus.InboundMessage{
		Channel:  "system",
		SenderID: fmt.Sprintf("subagent:%s", task.ID),
		ChatID:   fmt.Sprintf("%s:%s", task.OriginChannel, task.OriginChatID),
		Content:  announceContent,
	}
	var lastErr error
	backoff := []time.Duration{0, 200 * time.Millisecond, 800 * time.Millisecond}
	for attempt, wait := range backoff {
		if wait > 0 {
			time.Sleep(wait)
		}
		if err := sm.bus.PublishInbound(msg); err == nil {
			return nil
		} else {
			lastErr = err
			logger.WarnCF("subagent", "Failed to publish subagent completion", map[string]interface{}{
				"task_id": task.ID,
				"attempt": attempt + 1,
				"error":   err.Error(),
			})
		}
	}
	return lastErr
}

func (sm *SubagentManager) GetTask(taskID string) (*SubagentTask, bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	task, ok := sm.tasks[taskID]
	if !ok {
		return nil, false
	}
	return cloneSubagentTask(task), true
}

func (sm *SubagentManager) ListTasks() []*SubagentTask {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	tasks := make([]*SubagentTask, 0, len(sm.tasks))
	for _, task := range sm.tasks {
		tasks = append(tasks, cloneSubagentTask(task))
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].Created < tasks[j].Created })
	return tasks
}

func cloneSubagentTask(task *SubagentTask) *SubagentTask {
	if task == nil {
		return nil
	}
	cp := *task
	return &cp
}

func (sm *SubagentManager) loadState() error {
	data, err := os.ReadFile(sm.statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var state persistedSubagentState
	if err := json.Unmarshal(data, &state); err != nil {
		return err
	}

	sm.mu.Lock()
	defer sm.mu.Unlock()

	sm.tasks = make(map[string]*SubagentTask, len(state.Tasks))
	sm.nextID = 1
	if state.NextID > 0 {
		sm.nextID = state.NextID
	}
	sm.pendingResumeIDs = nil
	sm.pendingNotifyIDs = nil

	needsRewrite := false
	for _, task := range state.Tasks {
		cp := cloneSubagentTask(task)
		if cp == nil || strings.TrimSpace(cp.ID) == "" {
			continue
		}
		if cp.Updated == 0 {
			cp.Updated = cp.Created
		}
		if cp.Created == 0 {
			cp.Created = cp.Updated
		}
		switch cp.Status {
		case "running":
			cp.Status = "queued_resume"
			cp.Result = "Resuming after restart"
			cp.Updated = time.Now().UnixMilli()
			sm.pendingResumeIDs = append(sm.pendingResumeIDs, cp.ID)
			needsRewrite = true
		case "queued_resume":
			sm.pendingResumeIDs = append(sm.pendingResumeIDs, cp.ID)
		case "completed", "failed", "cancelled":
		default:
			cp.Status = "failed"
			cp.Result = "Task state recovered with invalid status; marked as failed"
			cp.Updated = time.Now().UnixMilli()
			cp.CompletedAt = cp.Updated
			cp.CompletionNotified = true
			needsRewrite = true
		}
		// Legacy entries (pre-notification metadata) should not be retried.
		if cp.CompletedAt == 0 && (cp.Status == "completed" || cp.Status == "failed" || cp.Status == "cancelled") {
			cp.CompletionNotified = true
		}
		if (cp.Status == "completed" || cp.Status == "failed" || cp.Status == "cancelled") &&
			!cp.CompletionNotified &&
			strings.TrimSpace(cp.OriginChannel) != "" &&
			strings.TrimSpace(cp.OriginChatID) != "" {
			sm.pendingNotifyIDs = append(sm.pendingNotifyIDs, cp.ID)
		}
		sm.tasks[cp.ID] = cp
		if n := parseSubagentNumericID(cp.ID); n >= sm.nextID {
			sm.nextID = n + 1
		}
	}
	sm.pruneCompleted()
	if sm.nextID <= 0 {
		sm.nextID = 1
	}
	if needsRewrite {
		_ = sm.persistStateLocked()
	}
	return nil
}

func (sm *SubagentManager) persistStateLocked() error {
	state := persistedSubagentState{
		Version: subagentStateVersion,
		NextID:  sm.nextID,
		Tasks:   make([]*SubagentTask, 0, len(sm.tasks)),
	}
	for _, task := range sm.tasks {
		state.Tasks = append(state.Tasks, cloneSubagentTask(task))
	}
	sort.Slice(state.Tasks, func(i, j int) bool { return state.Tasks[i].Created < state.Tasks[j].Created })
	return writeJSONFileAtomic(sm.statePath, state)
}

func writeJSONFileAtomic(path string, value interface{}) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("path is required")
	}
	payload, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, payload, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func parseSubagentNumericID(id string) int {
	const prefix = "subagent-"
	if !strings.HasPrefix(id, prefix) {
		return -1
	}
	n, err := strconv.Atoi(strings.TrimPrefix(id, prefix))
	if err != nil {
		return -1
	}
	return n
}

func (sm *SubagentManager) addPendingNotifyLocked(taskID string) {
	for _, existing := range sm.pendingNotifyIDs {
		if existing == taskID {
			return
		}
	}
	sm.pendingNotifyIDs = append(sm.pendingNotifyIDs, taskID)
}

// SubagentTool executes a subagent task synchronously and returns the result.
// Unlike SpawnTool which runs tasks asynchronously, SubagentTool waits for completion
// and returns the result directly in the ToolResult.
type SubagentTool struct {
	manager       *SubagentManager
	originChannel string
	originChatID  string
	mu            sync.RWMutex
}

func NewSubagentTool(manager *SubagentManager) *SubagentTool {
	return &SubagentTool{
		manager:       manager,
		originChannel: "cli",
		originChatID:  "direct",
	}
}

func (t *SubagentTool) Name() string {
	return "subagent"
}

func (t *SubagentTool) Description() string {
	return "Execute a subagent task synchronously and return the result. Use this for delegating specific tasks to an independent agent instance. Returns execution summary to user and full details to LLM."
}

func (t *SubagentTool) Parameters() map[string]interface{} {
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

func (t *SubagentTool) SetContext(channel, chatID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.originChannel = channel
	t.originChatID = chatID
}

func (t *SubagentTool) Execute(ctx context.Context, args map[string]interface{}) *ToolResult {
	task, ok := args["task"].(string)
	if !ok {
		return ErrorResult("task is required").WithError(fmt.Errorf("task parameter is required"))
	}

	label, _ := args["label"].(string)

	if t.manager == nil {
		return ErrorResult("Subagent manager not configured").WithError(fmt.Errorf("manager is nil"))
	}

	originChannel, originChatID := channelChatFromContext(ctx)
	t.mu.RLock()
	if originChannel == "" {
		originChannel = t.originChannel
	}
	if originChatID == "" {
		originChatID = t.originChatID
	}
	t.mu.RUnlock()

	// Build messages for subagent
	t.manager.mu.RLock()
	workspaceContext := t.manager.workspaceContext
	t.manager.mu.RUnlock()
	messages := []providers.Message{
		{
			Role:    "system",
			Content: buildSubagentSystemPrompt(workspaceContext),
		},
		{
			Role:    "user",
			Content: task,
		},
	}

	// Use RunToolLoop to execute with tools (same as async SpawnTool)
	sm := t.manager
	sm.mu.RLock()
	tools := sm.tools
	maxIter := sm.maxIterations
	contextWindow := sm.contextWindow
	contextPruningMode := sm.contextPruningMode
	contextPruningKeepLast := sm.contextPruningKeepLast
	maxOverflowCompactions := sm.maxOverflowCompactions
	retryCfg := sm.retry
	loopDetection := sm.loopDetection
	sm.mu.RUnlock()
	initialMessages := cloneSubagentMessages(messages)

	loopResult, err := RunToolLoop(ctx, ToolLoopConfig{
		Provider:               sm.provider,
		Model:                  sm.defaultModel,
		Tools:                  tools,
		MaxIterations:          maxIter,
		ContextWindowTokens:    contextWindow,
		ContextPruningMode:     contextPruningMode,
		ContextPruningKeepLast: contextPruningKeepLast,
		MaxOverflowCompactions: maxOverflowCompactions,
		Retry:                  retryCfg,
		LoopDetection:          loopDetection,
		LLMOptions: map[string]any{
			"max_tokens":  4096,
			"temperature": 0.7,
		},
		RebuildContext: func(ctx context.Context) ([]providers.Message, error) {
			return cloneSubagentMessages(initialMessages), nil
		},
	}, messages, originChannel, originChatID)

	if err != nil {
		return ErrorResult(fmt.Sprintf("Subagent execution failed: %v", err)).WithError(err)
	}

	// ForUser: Brief summary for user (truncated if too long)
	userContent := loopResult.Content
	maxUserLen := 500
	if len(userContent) > maxUserLen {
		userContent = userContent[:maxUserLen] + "..."
	}

	// ForLLM: Full execution details
	labelStr := label
	if labelStr == "" {
		labelStr = "(unnamed)"
	}
	llmContent := fmt.Sprintf("Subagent task completed:\nLabel: %s\nIterations: %d\nResult: %s",
		labelStr, loopResult.Iterations, loopResult.Content)

	return &ToolResult{
		ForLLM:  llmContent,
		ForUser: userContent,
		Silent:  false,
		IsError: false,
		Async:   false,
	}
}

func buildSubagentSystemPrompt(workspaceContext string) string {
	base := []string{
		"You are a subagent. Complete the given task independently and report the result.",
		"You have access to tools - use them as needed to complete your task.",
		"After completing the task, provide a clear summary of what was done.",
	}
	workspaceContext = strings.TrimSpace(workspaceContext)
	if workspaceContext != "" {
		base = append(base, "## Workspace Context\n"+workspaceContext)
	}
	return strings.Join(base, "\n\n")
}

func cloneSubagentMessages(messages []providers.Message) []providers.Message {
	if len(messages) == 0 {
		return nil
	}
	out := make([]providers.Message, len(messages))
	copy(out, messages)
	return out
}
