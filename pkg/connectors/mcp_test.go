package connectors

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestMCPRuntime_StreamableHTTPInvoke(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&req)
		method, _ := req["method"].(string)
		id := req["id"]
		switch method {
		case "initialize":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": map[string]interface{}{}})
		case "notifications/initialized":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": map[string]interface{}{}})
		case "tools/list":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      id,
				"result": map[string]interface{}{
					"tools": []map[string]interface{}{
						{
							"name":        "echo",
							"description": "Echo tool",
							"inputSchema": map[string]interface{}{
								"type": "object",
								"properties": map[string]interface{}{
									"msg": map[string]interface{}{"type": "string"},
								},
							},
						},
					},
				},
			})
		case "tools/call":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      id,
				"result": map[string]interface{}{
					"isError": false,
					"content": []map[string]interface{}{
						{"type": "text", "text": "mcp-http-ok"},
					},
				},
			})
		default:
			http.Error(w, "unexpected method", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	rt, err := NewMCPRuntime("mcp-http", MCPConfig{
		Transport: "streamable_http",
		URL:       server.URL,
	})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	if err := rt.Health(context.Background()); err != nil {
		t.Fatalf("health: %v", err)
	}
	_, schema, err := rt.ToolSchema(context.Background(), "echo")
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	props, _ := schema["properties"].(map[string]interface{})
	if _, ok := props["msg"]; !ok {
		t.Fatalf("schema missing msg property: %v", schema)
	}
	out, err := rt.Invoke(context.Background(), "echo", map[string]interface{}{"msg": "hello"})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if out.IsError {
		t.Fatalf("unexpected invoke error result: %+v", out)
	}
	if !strings.Contains(out.Content, "mcp-http-ok") {
		t.Fatalf("unexpected invoke content: %s", out.Content)
	}
}

func TestMCPRuntime_StreamableHTTPInvoke_EnvURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&req)
		method, _ := req["method"].(string)
		id := req["id"]
		switch method {
		case "initialize":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": map[string]interface{}{}})
		case "notifications/initialized":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": map[string]interface{}{}})
		case "tools/list":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      id,
				"result": map[string]interface{}{
					"tools": []map[string]interface{}{
						{
							"name": "echo",
							"inputSchema": map[string]interface{}{
								"type":       "object",
								"properties": map[string]interface{}{},
							},
						},
					},
				},
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      id,
				"result":  map[string]interface{}{},
			})
		}
	}))
	defer server.Close()

	t.Setenv("DOTAGENT_TEST_MCP_URL", server.URL)
	rt, err := NewMCPRuntime("mcp-http-env", MCPConfig{
		Transport: "streamable_http",
		URL:       "env:DOTAGENT_TEST_MCP_URL",
	})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	if err := rt.Health(context.Background()); err != nil {
		t.Fatalf("health: %v", err)
	}
}

func TestMCPRuntime_StdioInvoke(t *testing.T) {
	if os.Getenv("GO_WANT_MCP_HELPER") == "1" {
		return
	}
	rt, err := NewMCPRuntime("mcp-stdio", MCPConfig{
		Transport: "stdio",
		Command:   os.Args[0],
		Args:      []string{"-test.run=TestMCPHelperProcess", "--"},
		Env: map[string]string{
			"GO_WANT_MCP_HELPER": "1",
		},
	})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	defer rt.Close()

	if err := rt.Health(context.Background()); err != nil {
		t.Fatalf("health: %v", err)
	}
	out, err := rt.Invoke(context.Background(), "echo", map[string]interface{}{"msg": "x"})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if out.IsError {
		t.Fatalf("unexpected invoke error: %+v", out)
	}
	if !strings.Contains(out.Content, "mcp-stdio-ok") {
		t.Fatalf("unexpected content: %s", out.Content)
	}
}

func TestMCPHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_MCP_HELPER") != "1" {
		return
	}
	reader := bufio.NewReader(os.Stdin)
	for {
		frame, err := readMCPFrameWithContext(context.Background(), reader, nil)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			return
		}
		var req map[string]interface{}
		if err := json.Unmarshal(frame, &req); err != nil {
			continue
		}
		method, _ := req["method"].(string)
		id := req["id"]
		switch method {
		case "initialize":
			_ = writeMCPFrame(os.Stdout, map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      id,
				"result":  map[string]interface{}{},
			})
		case "notifications/initialized":
			// Notification has no response.
		case "tools/list":
			_ = writeMCPFrame(os.Stdout, map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      id,
				"result": map[string]interface{}{
					"tools": []map[string]interface{}{
						{
							"name":        "echo",
							"description": "Echo stdio tool",
							"inputSchema": map[string]interface{}{
								"type":       "object",
								"properties": map[string]interface{}{},
							},
						},
					},
				},
			})
		case "tools/call":
			_ = writeMCPFrame(os.Stdout, map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      id,
				"result": map[string]interface{}{
					"isError": false,
					"content": []map[string]interface{}{
						{"type": "text", "text": "mcp-stdio-ok"},
					},
				},
			})
		default:
			_ = writeMCPFrame(os.Stdout, map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      id,
				"error": map[string]interface{}{
					"code":    -32601,
					"message": "method not found",
				},
			})
		}
	}
}

func TestReadMCPFrameWithContext_Cancel(t *testing.T) {
	pr, pw := io.Pipe()
	defer pr.Close()
	defer pw.Close()
	reader := bufio.NewReader(pr)

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()

	called := false
	start := time.Now()
	_, err := readMCPFrameWithContext(ctx, reader, func() {
		called = true
		_ = pr.Close()
		_ = pw.Close()
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}
	if !called {
		t.Fatalf("expected onCancel callback to be called")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("cancellation should not hang")
	}
}
