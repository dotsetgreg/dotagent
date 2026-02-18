package providers

import (
	"encoding/json"
	"testing"
)

func TestBuildResponsesInput_ConvertsToolMessages(t *testing.T) {
	input := buildResponsesInput([]Message{
		{Role: "system", Content: "system prompt"},
		{Role: "user", Content: "read file"},
		{
			Role:    "assistant",
			Content: "",
			ToolCalls: []ToolCall{
				{
					ID:   "call_1",
					Type: "function",
					Function: &FunctionCall{
						Name:      "read_file",
						Arguments: `{"path":"README.md"}`,
					},
				},
			},
		},
		{Role: "tool", ToolCallID: "call_1", Content: `{"ok":true}`},
	})

	foundFunctionCall := false
	foundFunctionCallOutput := false
	for _, item := range input {
		itemType, _ := item["type"].(string)
		switch itemType {
		case "function_call":
			foundFunctionCall = true
			if got := item["call_id"]; got != "call_1" {
				t.Fatalf("expected function call id call_1, got %v", got)
			}
			if got := item["name"]; got != "read_file" {
				t.Fatalf("expected function call name read_file, got %v", got)
			}
		case "function_call_output":
			foundFunctionCallOutput = true
			if got := item["call_id"]; got != "call_1" {
				t.Fatalf("expected function_call_output call_id call_1, got %v", got)
			}
		}
	}

	if !foundFunctionCall {
		t.Fatalf("expected function_call item in responses input")
	}
	if !foundFunctionCallOutput {
		t.Fatalf("expected function_call_output item in responses input")
	}
}

func TestBuildResponsesInput_AssistantUsesOutputText(t *testing.T) {
	input := buildResponsesInput([]Message{
		{Role: "assistant", Content: "assistant reply"},
	})
	if len(input) != 1 {
		t.Fatalf("expected one input item, got %d", len(input))
	}
	content, ok := input[0]["content"].([]map[string]interface{})
	if !ok || len(content) == 0 {
		t.Fatalf("expected assistant content array")
	}
	if got, _ := content[0]["type"].(string); got != "output_text" {
		t.Fatalf("expected assistant content type output_text, got %q", got)
	}
}

func TestParseResponsesResponse_ParsesToolCallsAndText(t *testing.T) {
	body := []byte(`{
		"id":"resp_1",
		"status":"completed",
		"output_text":"summary text",
		"output":[
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"assistant text"}]},
			{"type":"function_call","call_id":"call_1","name":"read_file","arguments":"{\"path\":\"README.md\"}"}
		],
		"usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14}
	}`)

	parsed, err := parseResponsesResponse(body)
	if err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if parsed.ResponseID != "resp_1" {
		t.Fatalf("expected response id resp_1, got %q", parsed.ResponseID)
	}
	if parsed.Response.FinishReason != "tool_calls" {
		t.Fatalf("expected finish reason tool_calls, got %q", parsed.Response.FinishReason)
	}
	if len(parsed.Response.ToolCalls) != 1 {
		t.Fatalf("expected one tool call, got %d", len(parsed.Response.ToolCalls))
	}
	if got := parsed.Response.ToolCalls[0].Arguments["path"]; got != "README.md" {
		t.Fatalf("expected tool args path README.md, got %v", got)
	}
	if parsed.Response.Content == "" {
		t.Fatalf("expected non-empty parsed text content")
	}
	if parsed.Response.Usage == nil {
		t.Fatalf("expected usage info")
	}
	if parsed.Response.Usage.TotalTokens != 14 {
		t.Fatalf("expected total_tokens 14, got %d", parsed.Response.Usage.TotalTokens)
	}
}

func TestToResponsesTools(t *testing.T) {
	tools := toResponsesTools([]ToolDefinition{
		{
			Type: "function",
			Function: ToolFunctionDefinition{
				Name:        "read_file",
				Description: "read a file",
				Parameters: map[string]interface{}{
					"type": "object",
				},
			},
		},
	})
	if len(tools) != 1 {
		t.Fatalf("expected one converted tool, got %d", len(tools))
	}

	raw, err := json.Marshal(tools[0])
	if err != nil {
		t.Fatalf("marshal converted tool: %v", err)
	}
	if string(raw) == "" {
		t.Fatalf("expected non-empty marshaled tool payload")
	}
}

func TestParseResponsesStreamBody_CompletionEvent(t *testing.T) {
	body := []byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_stream_1\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}],\"usage\":{\"input_tokens\":3,\"output_tokens\":2,\"total_tokens\":5}}}\n\ndata: [DONE]\n\n")
	parsed, err := parseResponsesStreamBody(body)
	if err != nil {
		t.Fatalf("parse stream body: %v", err)
	}
	if parsed.ResponseID != "resp_stream_1" {
		t.Fatalf("expected response id resp_stream_1, got %q", parsed.ResponseID)
	}
	if got := parsed.Response.Content; got != "hello" {
		t.Fatalf("expected content hello, got %q", got)
	}
}

func TestParseResponsesStreamBody_DeltaFallback(t *testing.T) {
	body := []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\" world\"}\n\ndata: [DONE]\n\n")
	parsed, err := parseResponsesStreamBody(body)
	if err != nil {
		t.Fatalf("parse stream body fallback: %v", err)
	}
	if got := parsed.Response.Content; got != "hello world" {
		t.Fatalf("expected fallback content hello world, got %q", got)
	}
}
