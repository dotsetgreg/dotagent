package tools

import (
	"context"
	"sync/atomic"
)

// Tool is the interface that all tools must implement.
type Tool interface {
	Name() string
	Description() string
	Parameters() map[string]interface{}
	Execute(ctx context.Context, args map[string]interface{}) *ToolResult
}

// ContextualTool is an optional interface that tools can implement
// to receive the current message context (channel, chatID)
type ContextualTool interface {
	Tool
	SetContext(channel, chatID string)
}

// AsyncCallback is a function type that async tools use to notify completion.
// When an async tool finishes its work, it calls this callback with the result.
//
// The ctx parameter allows the callback to be canceled if the agent is shutting down.
// The result parameter contains the tool's execution result.
//
// Example usage in an async tool:
//
//	func (t *MyAsyncTool) Execute(ctx context.Context, args map[string]interface{}) *ToolResult {
//	    // Start async work in background
//	    go func() {
//	        result := doAsyncWork()
//	        if t.callback != nil {
//	            t.callback(ctx, result)
//	        }
//	    }()
//	    return AsyncResult("Async task started")
//	}
type AsyncCallback func(ctx context.Context, result *ToolResult)

// AsyncTool is an optional interface that tools can implement to support
// asynchronous execution with completion callbacks.
//
// Async tools return immediately with an AsyncResult, then notify completion
// via the callback set by SetCallback.
//
// This is useful for:
// - Long-running operations that shouldn't block the agent loop
// - Subagent spawns that complete independently
// - Background tasks that need to report results later
//
// Example:
//
//	type SpawnTool struct {
//	    callback AsyncCallback
//	}
//
//	func (t *SpawnTool) SetCallback(cb AsyncCallback) {
//	    t.callback = cb
//	}
//
//	func (t *SpawnTool) Execute(ctx context.Context, args map[string]interface{}) *ToolResult {
//	    go t.runSubagent(ctx, args)
//	    return AsyncResult("Subagent spawned, will report back")
//	}
type AsyncTool interface {
	Tool
	// SetCallback registers a callback function to be invoked when the async operation completes.
	// The callback will be called from a goroutine and should handle thread-safety if needed.
	SetCallback(cb AsyncCallback)
}

// ClosableTool is an optional interface for tools that hold runtime resources
// and require explicit teardown when the agent stops.
type ClosableTool interface {
	Tool
	Close() error
}

type toolExecutionContext struct {
	channel       string
	chatID        string
	asyncCallback AsyncCallback
}

type toolExecutionContextKey struct{}

// withToolExecutionContext annotates a call context with per-execution metadata.
func withToolExecutionContext(ctx context.Context, channel, chatID string, asyncCallback AsyncCallback) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if existing, ok := toolExecutionContextFromContext(ctx); ok {
		if channel == "" {
			channel = existing.channel
		}
		if chatID == "" {
			chatID = existing.chatID
		}
		if asyncCallback == nil {
			asyncCallback = existing.asyncCallback
		}
	}
	execCtx := toolExecutionContext{
		channel:       channel,
		chatID:        chatID,
		asyncCallback: asyncCallback,
	}
	return context.WithValue(ctx, toolExecutionContextKey{}, execCtx)
}

func toolExecutionContextFromContext(ctx context.Context) (toolExecutionContext, bool) {
	if ctx == nil {
		return toolExecutionContext{}, false
	}
	execCtx, ok := ctx.Value(toolExecutionContextKey{}).(toolExecutionContext)
	return execCtx, ok
}

func channelChatFromContext(ctx context.Context) (string, string) {
	execCtx, ok := toolExecutionContextFromContext(ctx)
	if !ok {
		return "", ""
	}
	return execCtx.channel, execCtx.chatID
}

func asyncCallbackFromContext(ctx context.Context) AsyncCallback {
	execCtx, ok := toolExecutionContextFromContext(ctx)
	if !ok {
		return nil
	}
	return execCtx.asyncCallback
}

// ExecutionRoundState tracks per-agent-run round state in a request-scoped way.
type ExecutionRoundState struct {
	messageSent atomic.Bool
}

func NewExecutionRoundState() *ExecutionRoundState {
	return &ExecutionRoundState{}
}

func (s *ExecutionRoundState) MarkMessageSent() {
	if s == nil {
		return
	}
	s.messageSent.Store(true)
}

func (s *ExecutionRoundState) MessageSent() bool {
	if s == nil {
		return false
	}
	return s.messageSent.Load()
}

type executionRoundStateKey struct{}

// WithExecutionRoundState adds per-round state to context.
func WithExecutionRoundState(ctx context.Context, state *ExecutionRoundState) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if state == nil {
		return ctx
	}
	return context.WithValue(ctx, executionRoundStateKey{}, state)
}

func executionRoundStateFromContext(ctx context.Context) *ExecutionRoundState {
	if ctx == nil {
		return nil
	}
	state, _ := ctx.Value(executionRoundStateKey{}).(*ExecutionRoundState)
	return state
}

func markMessageSentInContext(ctx context.Context) {
	if state := executionRoundStateFromContext(ctx); state != nil {
		state.MarkMessageSent()
	}
}

func ToolToSchema(tool Tool) map[string]interface{} {
	return map[string]interface{}{
		"type": "function",
		"function": map[string]interface{}{
			"name":        tool.Name(),
			"description": tool.Description(),
			"parameters":  tool.Parameters(),
		},
	}
}
