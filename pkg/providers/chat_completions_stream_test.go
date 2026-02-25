package providers

import (
	"strings"
	"testing"
)

func TestParseChatCompletionsStreamResponse_TextDeltas(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"hello "},"finish_reason":""}]}`,
		``,
		`data: {"choices":[{"delta":{"content":"world"},"finish_reason":"stop"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	var deltas []string
	resp, err := parseChatCompletionsStreamResponse(strings.NewReader(stream), func(delta string) {
		deltas = append(deltas, delta)
	})
	if err != nil {
		t.Fatalf("parse stream: %v", err)
	}
	if resp.Content != "hello world" {
		t.Fatalf("expected merged content, got %q", resp.Content)
	}
	if resp.FinishReason != "stop" {
		t.Fatalf("expected stop finish reason, got %q", resp.FinishReason)
	}
	if len(deltas) != 2 {
		t.Fatalf("expected 2 deltas, got %d", len(deltas))
	}
}

func TestParseChatCompletionsStreamResponse_ToolCalls(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"search","arguments":"{\"q\":\"hello"}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":" world\"}"}}]},"finish_reason":"tool_calls"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	resp, err := parseChatCompletionsStreamResponse(strings.NewReader(stream), nil)
	if err != nil {
		t.Fatalf("parse stream: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].Name != "search" {
		t.Fatalf("expected tool name search, got %q", resp.ToolCalls[0].Name)
	}
	if resp.FinishReason != "tool_calls" {
		t.Fatalf("expected tool_calls finish reason, got %q", resp.FinishReason)
	}
	if got := resp.ToolCalls[0].Arguments["q"]; got != "hello world" {
		t.Fatalf("expected reconstructed args, got %#v", got)
	}
}
