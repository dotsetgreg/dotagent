package providers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dotsetgreg/dotagent/pkg/config"
)

func makeOpenAICodexToken(t *testing.T, accountID string) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payloadRaw, err := json.Marshal(map[string]any{
		openAICodexJWTClaimPath: map[string]any{
			"chatgpt_account_id": accountID,
		},
	})
	if err != nil {
		t.Fatalf("marshal codex payload: %v", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(payloadRaw)
	return header + "." + payload + ".sig"
}

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

func TestResolveOpenAICodexAuthConfig_RejectsMultipleCredentialSources(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Provider = ProviderOpenAICodex
	cfg.Providers.OpenAICodex.OAuthAccessToken = "inline-token"
	cfg.Providers.OpenAICodex.OAuthTokenFile = "/tmp/codex-auth.json"

	mode, source, err := resolveOpenAICodexAuthConfig(cfg)
	if err == nil {
		t.Fatalf("expected multi-credential configuration error")
	}
	if mode != "" || source != "" {
		t.Fatalf("expected empty mode/source on error, got mode=%q source=%q", mode, source)
	}
	if want := "multiple OpenAI Codex credential sources configured"; !strings.Contains(err.Error(), want) {
		t.Fatalf("expected error containing %q, got %v", want, err)
	}
}

func TestValidateProviderConfig_MissingCredentials_OpenAICodex(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Provider = ProviderOpenAICodex

	if err := ValidateProviderConfig(cfg); err == nil {
		t.Fatalf("expected missing credentials error for openai-codex")
	}
}

func TestCreateProvider_OpenAICodex_UsesResponsesEndpoint(t *testing.T) {
	var seenAuth string
	var seenPath string
	var seenAccountID string
	var seenBeta string
	var seenOriginator string
	var seenTools []interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		seenPath = r.URL.Path
		seenAccountID = r.Header.Get("chatgpt-account-id")
		seenBeta = r.Header.Get("OpenAI-Beta")
		seenOriginator = r.Header.Get("originator")

		var req map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if got := req["model"]; got != "gpt-5" {
			t.Fatalf("expected model gpt-5, got %v", got)
		}
		if tools, ok := req["tools"].([]interface{}); ok {
			seenTools = tools
		}
		if got, ok := req["store"].(bool); !ok || got {
			t.Fatalf("expected store=false in codex payload, got %v", req["store"])
		}
		if got, ok := req["stream"].(bool); !ok || !got {
			t.Fatalf("expected stream=true in codex payload, got %v", req["stream"])
		}
		if _, found := req["max_output_tokens"]; found {
			t.Fatalf("expected max_output_tokens removed for codex payload")
		}
		instructions, ok := req["instructions"].(string)
		if !ok || strings.TrimSpace(instructions) == "" {
			t.Fatalf("expected non-empty instructions, got %v", req["instructions"])
		}
		if strings.TrimSpace(instructions) != "system prompt" {
			t.Fatalf("expected instructions from system prompt, got %q", instructions)
		}
		if input, ok := req["input"].([]interface{}); ok {
			for _, item := range input {
				im, ok := item.(map[string]interface{})
				if !ok {
					continue
				}
				if role, _ := im["role"].(string); strings.EqualFold(strings.TrimSpace(role), "system") {
					t.Fatalf("expected system messages removed from codex input")
				}
			}
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test_1\",\"status\":\"completed\",\"output\":[{\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"read_file\",\"arguments\":\"{\\\"path\\\":\\\"README.md\\\"}\"}],\"usage\":{\"input_tokens\":6,\"output_tokens\":3,\"total_tokens\":9}}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	tokenFile := filepath.Join(t.TempDir(), "codex-token.txt")
	token := makeOpenAICodexToken(t, "acct_test_123")
	if err := os.WriteFile(tokenFile, []byte(token), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Provider = ProviderOpenAICodex
	cfg.Providers.OpenAICodex.APIBase = server.URL
	cfg.Providers.OpenAICodex.OAuthTokenFile = tokenFile

	provider, err := CreateProvider(cfg)
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}

	resp, err := provider.Chat(context.Background(), []Message{
		{Role: "system", Content: "system prompt"},
		{Role: "user", Content: "read file"},
	}, []ToolDefinition{{
		Type: "function",
		Function: ToolFunctionDefinition{
			Name:       "read_file",
			Parameters: map[string]interface{}{"type": "object"},
		},
	}}, "gpt-5", map[string]interface{}{"max_tokens": 128, "temperature": 0.3})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}

	if seenAuth != "Bearer "+token {
		t.Fatalf("expected bearer token from file, got %q", seenAuth)
	}
	if seenPath != "/codex/responses" {
		t.Fatalf("expected /codex/responses path, got %q", seenPath)
	}
	if seenAccountID != "acct_test_123" {
		t.Fatalf("expected chatgpt-account-id header, got %q", seenAccountID)
	}
	if seenBeta != "responses=experimental" {
		t.Fatalf("expected OpenAI-Beta header, got %q", seenBeta)
	}
	if seenOriginator != "dotagent" {
		t.Fatalf("expected originator header, got %q", seenOriginator)
	}
	if len(seenTools) != 1 {
		t.Fatalf("expected one tool definition in responses payload, got %d", len(seenTools))
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("expected one tool call, got %d", len(resp.ToolCalls))
	}
	if got := resp.ToolCalls[0].Name; got != "read_file" {
		t.Fatalf("expected tool call name read_file, got %q", got)
	}
	if got := resp.ToolCalls[0].Arguments["path"]; got != "README.md" {
		t.Fatalf("expected tool argument path README.md, got %v", got)
	}
}

func TestCreateProvider_OpenAICodex_SetsFallbackInstructions(t *testing.T) {
	var seenInstructions string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if inst, ok := req["instructions"].(string); ok {
			seenInstructions = strings.TrimSpace(inst)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"resp_test_fallback",
			"status":"completed",
			"output":[
				{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}
			]
		}`))
	}))
	defer server.Close()

	tokenFile := filepath.Join(t.TempDir(), "codex-token.txt")
	token := makeOpenAICodexToken(t, "acct_test_777")
	if err := os.WriteFile(tokenFile, []byte(token), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Provider = ProviderOpenAICodex
	cfg.Providers.OpenAICodex.APIBase = server.URL
	cfg.Providers.OpenAICodex.OAuthTokenFile = tokenFile

	provider, err := CreateProvider(cfg)
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	if _, err := provider.Chat(context.Background(), []Message{{Role: "user", Content: "hello"}}, nil, "gpt-5", nil); err != nil {
		t.Fatalf("chat: %v", err)
	}
	if seenInstructions == "" {
		t.Fatalf("expected fallback instructions to be set")
	}
}

func TestCreateProvider_OpenAICodex_StatefulResponseID(t *testing.T) {
	var seenPrevious string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if prev, ok := req["previous_response_id"].(string); ok {
			seenPrevious = prev
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"resp_next_1",
			"status":"completed",
			"output":[
				{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}
			]
		}`))
	}))
	defer server.Close()

	tokenFile := filepath.Join(t.TempDir(), "codex-token.txt")
	token := makeOpenAICodexToken(t, "acct_test_123")
	if err := os.WriteFile(tokenFile, []byte(token), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Provider = ProviderOpenAICodex
	cfg.Providers.OpenAICodex.APIBase = server.URL
	cfg.Providers.OpenAICodex.OAuthTokenFile = tokenFile

	provider, err := CreateProvider(cfg)
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	stateful, ok := provider.(StatefulLLMProvider)
	if !ok {
		t.Fatalf("expected openai-codex provider to implement StatefulLLMProvider")
	}

	resp, newState, err := stateful.ChatWithState(
		context.Background(),
		"resp_prev_1",
		[]Message{{Role: "user", Content: "hello"}},
		nil,
		"gpt-5",
		nil,
	)
	if err != nil {
		t.Fatalf("chat with state: %v", err)
	}
	if seenPrevious != "resp_prev_1" {
		t.Fatalf("expected previous_response_id resp_prev_1, got %q", seenPrevious)
	}
	if newState != "resp_next_1" {
		t.Fatalf("expected returned state resp_next_1, got %q", newState)
	}
	if resp.Content != "done" {
		t.Fatalf("expected content done, got %q", resp.Content)
	}
}

func TestResolveOpenAICodexResponsesEndpoint(t *testing.T) {
	cases := []struct {
		base string
		want string
	}{
		{base: "https://chatgpt.com/backend-api", want: "https://chatgpt.com/backend-api/codex/responses"},
		{base: "https://chatgpt.com/backend-api/", want: "https://chatgpt.com/backend-api/codex/responses"},
		{base: "https://chatgpt.com/backend-api/codex", want: "https://chatgpt.com/backend-api/codex/responses"},
		{base: "https://chatgpt.com/backend-api/codex/responses", want: "https://chatgpt.com/backend-api/codex/responses"},
	}
	for _, tc := range cases {
		got := resolveOpenAICodexResponsesEndpoint(tc.base)
		if got != tc.want {
			t.Fatalf("endpoint for %q = %q, want %q", tc.base, got, tc.want)
		}
	}
}

func TestExtractOpenAICodexAccountID(t *testing.T) {
	token := makeOpenAICodexToken(t, "acct_test_42")
	got, err := extractOpenAICodexAccountID(token)
	if err != nil {
		t.Fatalf("extract account id: %v", err)
	}
	if got != "acct_test_42" {
		t.Fatalf("account id = %q, want acct_test_42", got)
	}
	if _, err := extractOpenAICodexAccountID("not-a-jwt"); err == nil {
		t.Fatalf("expected invalid token format error")
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
