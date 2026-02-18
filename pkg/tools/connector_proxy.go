package tools

import (
	"context"
	"fmt"
	"strings"
)

// ConnectorInvocationResult is a normalized connector tool execution result.
type ConnectorInvocationResult struct {
	Content     string
	UserContent string
	IsError     bool
}

// ConnectorInvoker is the minimal runtime contract required by connector-backed tools.
type ConnectorInvoker interface {
	Invoke(ctx context.Context, target string, args map[string]interface{}) (ConnectorInvocationResult, error)
	Close() error
}

// ConnectorProxyTool binds a local tool name to a remote connector target.
type ConnectorProxyTool struct {
	name        string
	description string
	parameters  map[string]interface{}
	target      string
	invoker     ConnectorInvoker
}

func NewConnectorProxyTool(name, description string, parameters map[string]interface{}, target string, invoker ConnectorInvoker) *ConnectorProxyTool {
	if parameters == nil {
		parameters = map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		}
	}
	return &ConnectorProxyTool{
		name:        strings.TrimSpace(name),
		description: strings.TrimSpace(description),
		parameters:  parameters,
		target:      strings.TrimSpace(target),
		invoker:     invoker,
	}
}

func (t *ConnectorProxyTool) Name() string {
	return t.name
}

func (t *ConnectorProxyTool) Description() string {
	if t.description == "" {
		return "Connector-backed tool"
	}
	return t.description
}

func (t *ConnectorProxyTool) Parameters() map[string]interface{} {
	return t.parameters
}

func (t *ConnectorProxyTool) Execute(ctx context.Context, args map[string]interface{}) *ToolResult {
	if t.invoker == nil {
		return ErrorResult("connector runtime is unavailable")
	}
	result, err := t.invoker.Invoke(ctx, t.target, args)
	if err != nil {
		return ErrorResult(fmt.Sprintf("connector invoke failed: %v", err)).WithError(err)
	}
	content := strings.TrimSpace(result.Content)
	if result.IsError {
		if content == "" {
			content = "connector invocation failed"
		}
		return ErrorResult(content)
	}
	if strings.TrimSpace(result.UserContent) != "" {
		return &ToolResult{
			ForLLM:  valueOr(content, result.UserContent),
			ForUser: result.UserContent,
		}
	}
	if content == "" {
		content = "Connector invocation completed."
	}
	return UserResult(content)
}

func (t *ConnectorProxyTool) Close() error {
	if t.invoker == nil {
		return nil
	}
	return t.invoker.Close()
}

func valueOr(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}
