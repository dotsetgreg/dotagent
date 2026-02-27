package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseOllamaContextWindow_FromModelInfo(t *testing.T) {
	body := []byte(`{
		"model_info": {
			"llama.context_length": 32768
		}
	}`)
	got, err := parseOllamaContextWindow(body)
	if err != nil {
		t.Fatalf("parse ollama context window: %v", err)
	}
	if got != 32768 {
		t.Fatalf("expected 32768, got %d", got)
	}
}

func TestParseOllamaContextWindow_FallbackToNumCtx(t *testing.T) {
	body := []byte(`{
		"model_info": {},
		"parameters": "num_ctx 8192\nstop <|eot_id|>"
	}`)
	got, err := parseOllamaContextWindow(body)
	if err != nil {
		t.Fatalf("parse ollama context window: %v", err)
	}
	if got != 8192 {
		t.Fatalf("expected 8192, got %d", got)
	}
}

func TestParseOllamaContextWindow_MissingMetadata(t *testing.T) {
	body := []byte(`{"model_info":{},"parameters":"stop end"}`)
	if _, err := parseOllamaContextWindow(body); err == nil {
		t.Fatalf("expected parse failure when metadata is missing")
	}
}

func TestResolveContextWindow_OllamaUsesShowEndpoint(t *testing.T) {
	var seenPath string
	var seenModel string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		var payload map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if model, ok := payload["model"].(string); ok {
			seenModel = model
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model_info":{"llama.context_length":65536}}`))
	}))
	defer server.Close()

	provider, err := newChatCompletionsProvider(ProviderOllama, server.URL+"/v1", "llama3.2", "", NewNoAuth(), nil)
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}

	got, err := provider.ResolveContextWindow(context.Background(), "llama3.2")
	if err != nil {
		t.Fatalf("resolve context window: %v", err)
	}
	if got != 65536 {
		t.Fatalf("expected 65536, got %d", got)
	}
	if seenPath != "/api/show" {
		t.Fatalf("expected /api/show path, got %q", seenPath)
	}
	if seenModel != "llama3.2" {
		t.Fatalf("expected model llama3.2 in request, got %q", seenModel)
	}
}
