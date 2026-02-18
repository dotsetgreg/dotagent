package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dotsetgreg/dotagent/pkg/config"
)

func TestCreateProvider_OpenRouter_DefaultSelection(t *testing.T) {
	var seenAuth string
	var seenPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		seenPath = r.URL.Path
		var req map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if got := req["model"]; got != defaultOpenRouterModel {
			t.Fatalf("expected default model %q, got %v", defaultOpenRouterModel, got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	cfg := config.DefaultConfig()
	cfg.Providers.OpenRouter.APIKey = "or-key"
	cfg.Providers.OpenRouter.APIBase = server.URL
	cfg.Agents.Defaults.Provider = ""

	provider, err := CreateProvider(cfg)
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	resp, err := provider.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, "", nil)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if resp.Content != "ok" {
		t.Fatalf("expected response content ok, got %q", resp.Content)
	}
	if seenAuth != "Bearer or-key" {
		t.Fatalf("expected openrouter auth bearer, got %q", seenAuth)
	}
	if seenPath != "/chat/completions" {
		t.Fatalf("expected /chat/completions path, got %q", seenPath)
	}
}

func TestCreateProvider_OpenAI_WithAPIKeyAndToolCalls(t *testing.T) {
	var seenAuth string
	var seenOrg string
	var seenProject string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		seenOrg = r.Header.Get("OpenAI-Organization")
		seenProject = r.Header.Get("OpenAI-Project")

		var req map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if got := req["model"]; got != "gpt-5" {
			t.Fatalf("expected model override gpt-5, got %v", got)
		}
		if _, ok := req["tools"]; !ok {
			t.Fatalf("expected tools in request")
		}
		if got, ok := req["tool_choice"]; !ok || got != "auto" {
			t.Fatalf("expected tool_choice auto, got %v", got)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices": [{
				"message": {
					"content": "",
					"tool_calls": [{
						"id": "call_1",
						"type": "function",
						"function": {
							"name": "read_file",
							"arguments": "{\"path\":\"README.md\"}"
						}
					}]
				},
				"finish_reason": "tool_calls"
			}],
			"usage": {"prompt_tokens": 12, "completion_tokens": 3, "total_tokens": 15}
		}`))
	}))
	defer server.Close()

	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Provider = ProviderOpenAI
	cfg.Providers.OpenAI.APIKey = "sk-openai"
	cfg.Providers.OpenAI.APIBase = server.URL
	cfg.Providers.OpenAI.Organization = "org_123"
	cfg.Providers.OpenAI.Project = "proj_456"

	provider, err := CreateProvider(cfg)
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}

	resp, err := provider.Chat(context.Background(), []Message{{Role: "user", Content: "read file"}}, []ToolDefinition{{
		Type: "function",
		Function: ToolFunctionDefinition{
			Name:       "read_file",
			Parameters: map[string]interface{}{"type": "object"},
		},
	}}, "gpt-5", map[string]interface{}{"max_tokens": 128, "temperature": 0.3})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(resp.ToolCalls))
	}
	if got := resp.ToolCalls[0].Name; got != "read_file" {
		t.Fatalf("expected tool name read_file, got %q", got)
	}
	if got := resp.ToolCalls[0].Arguments["path"]; got != "README.md" {
		t.Fatalf("expected tool argument path README.md, got %v", got)
	}
	if seenAuth != "Bearer sk-openai" {
		t.Fatalf("expected openai auth bearer with api key, got %q", seenAuth)
	}
	if seenOrg != "org_123" {
		t.Fatalf("expected OpenAI-Organization header, got %q", seenOrg)
	}
	if seenProject != "proj_456" {
		t.Fatalf("expected OpenAI-Project header, got %q", seenProject)
	}
}

func TestResolveOpenAIAuthConfig_RejectsMultipleCredentialSources(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token.txt")
	if err := os.WriteFile(tokenFile, []byte("from-file"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Provider = ProviderOpenAI
	cfg.Providers.OpenAI.APIKey = "api-key-wins"
	cfg.Providers.OpenAI.OAuthAccessToken = "oauth-inline"
	cfg.Providers.OpenAI.OAuthTokenFile = tokenFile

	mode, source, err := resolveOpenAIAuthConfig(cfg)
	if err == nil {
		t.Fatalf("expected multi-credential configuration error")
	}
	if mode != "" || source != "" {
		t.Fatalf("expected empty mode/source on error, got mode=%q source=%q", mode, source)
	}
	if want := "multiple OpenAI credential sources configured"; err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("expected error containing %q, got %v", want, err)
	}
}

func TestCreateProvider_OpenAI_UsesOAuthTokenFile(t *testing.T) {
	var seenAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	tokenFile := filepath.Join(t.TempDir(), "token.txt")
	if err := os.WriteFile(tokenFile, []byte("oauth-token-from-file"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Provider = ProviderOpenAI
	cfg.Providers.OpenAI.APIBase = server.URL
	cfg.Providers.OpenAI.OAuthTokenFile = tokenFile

	provider, err := CreateProvider(cfg)
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	if _, err := provider.Chat(context.Background(), []Message{{Role: "user", Content: "hello"}}, nil, "", nil); err != nil {
		t.Fatalf("chat: %v", err)
	}
	if seenAuth != "Bearer oauth-token-from-file" {
		t.Fatalf("expected oauth bearer from file, got %q", seenAuth)
	}
}

func TestCreateProvider_OpenAI_UsesOAuthTokenFile_CodexAuthJSON(t *testing.T) {
	var seenAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	tokenFile := filepath.Join(t.TempDir(), "auth.json")
	payload := `{"auth_mode":"chatgpt","tokens":{"access_token":"oauth-token-from-codex"}}`
	if err := os.WriteFile(tokenFile, []byte(payload), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Provider = ProviderOpenAI
	cfg.Providers.OpenAI.APIBase = server.URL
	cfg.Providers.OpenAI.OAuthTokenFile = tokenFile

	provider, err := CreateProvider(cfg)
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	if _, err := provider.Chat(context.Background(), []Message{{Role: "user", Content: "hello"}}, nil, "", nil); err != nil {
		t.Fatalf("chat: %v", err)
	}
	if seenAuth != "Bearer oauth-token-from-codex" {
		t.Fatalf("expected oauth bearer from codex json token file, got %q", seenAuth)
	}
}

func TestCreateProvider_UnsupportedProvider(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Provider = "does-not-exist"

	if _, err := CreateProvider(cfg); err == nil {
		t.Fatalf("expected unsupported provider error")
	}
}

func TestValidateProviderConfig_MissingCredentials(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Provider = ProviderOpenAI

	if err := ValidateProviderConfig(cfg); err == nil {
		t.Fatalf("expected missing credentials error for openai")
	}
}

func TestRegisterFactory_InvalidRegistrationDoesNotPanic(t *testing.T) {
	factoryMu.RLock()
	origFactories := make(map[string]providerFactory, len(factories))
	for k, v := range factories {
		origFactories[k] = v
	}
	origErr := registrationErr
	factoryMu.RUnlock()

	defer func() {
		factoryMu.Lock()
		factories = origFactories
		registrationErr = origErr
		factoryMu.Unlock()
	}()

	didPanic := false
	func() {
		defer func() {
			if recover() != nil {
				didPanic = true
			}
		}()
		RegisterFactory("", nil, nil, nil)
	}()
	if didPanic {
		t.Fatalf("RegisterFactory should not panic on invalid registration")
	}

	cfg := config.DefaultConfig()
	if _, err := CreateProvider(cfg); err == nil {
		t.Fatalf("expected provider creation to fail after invalid registration")
	}
}
