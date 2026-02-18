package tools

import (
	"context"
	"errors"
	"testing"
)

type mockConnectorInvoker struct {
	result ConnectorInvocationResult
	err    error
	closed bool
}

func (m *mockConnectorInvoker) Invoke(ctx context.Context, target string, args map[string]interface{}) (ConnectorInvocationResult, error) {
	return m.result, m.err
}

func (m *mockConnectorInvoker) Close() error {
	m.closed = true
	return nil
}

func TestConnectorProxyTool_Execute(t *testing.T) {
	inv := &mockConnectorInvoker{
		result: ConnectorInvocationResult{Content: "ok"},
	}
	tool := NewConnectorProxyTool("remote_echo", "desc", nil, "echo", inv)
	res := tool.Execute(context.Background(), map[string]interface{}{"msg": "x"})
	if res.IsError {
		t.Fatalf("expected success, got error: %s", res.ForLLM)
	}
	if res.ForLLM == "" {
		t.Fatalf("expected non-empty ForLLM")
	}
}

func TestConnectorProxyTool_ExecuteError(t *testing.T) {
	inv := &mockConnectorInvoker{
		err: errors.New("boom"),
	}
	tool := NewConnectorProxyTool("remote_echo", "desc", nil, "echo", inv)
	res := tool.Execute(context.Background(), nil)
	if !res.IsError {
		t.Fatalf("expected error result")
	}
}

func TestConnectorProxyTool_Close(t *testing.T) {
	inv := &mockConnectorInvoker{}
	tool := NewConnectorProxyTool("remote_echo", "desc", nil, "echo", inv)
	if err := tool.Close(); err != nil {
		t.Fatalf("close failed: %v", err)
	}
	if !inv.closed {
		t.Fatalf("expected invoker close to be called")
	}
}
