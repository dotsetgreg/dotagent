package providers

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStaticTokenSource_RejectsPlaceholderToken(t *testing.T) {
	src := NewStaticTokenSource("<OPENAI_OAUTH_TOKEN>", "providers.openai.oauth_access_token")
	if _, err := src.Token(context.Background()); err == nil {
		t.Fatalf("expected placeholder token to be rejected")
	}
}

func TestStaticTokenSource_RejectsEnvReferenceToken(t *testing.T) {
	src := NewStaticTokenSource("${OPENAI_OAUTH_TOKEN}", "providers.openai.oauth_access_token")
	if _, err := src.Token(context.Background()); err == nil {
		t.Fatalf("expected env reference token to be rejected")
	}
}

func TestFileTokenSource_PlainTokenFile(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token.txt")
	if err := os.WriteFile(tokenFile, []byte("oauth-token-123"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	src := NewFileTokenSource(tokenFile)
	got, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	if got != "oauth-token-123" {
		t.Fatalf("expected plain token, got %q", got)
	}
}

func TestFileTokenSource_CodexAuthJSON(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "auth.json")
	payload := `{"auth_mode":"chatgpt","tokens":{"access_token":"oauth-from-codex"}}`
	if err := os.WriteFile(tokenFile, []byte(payload), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	src := NewFileTokenSource(tokenFile)
	got, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	if got != "oauth-from-codex" {
		t.Fatalf("expected token from codex json, got %q", got)
	}
}

func TestFileTokenSource_JSONMissingAccessToken(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "auth.json")
	payload := `{"tokens":{"refresh_token":"rt_123"}}`
	if err := os.WriteFile(tokenFile, []byte(payload), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	src := NewFileTokenSource(tokenFile)
	_, err := src.Token(context.Background())
	if err == nil {
		t.Fatalf("expected missing access token error")
	}
	if !strings.Contains(err.Error(), "missing access_token") {
		t.Fatalf("expected missing access_token message, got %v", err)
	}
}

func TestNoAuth_ModeAndApply(t *testing.T) {
	auth := NewNoAuth()
	if got := auth.Mode(); got != authModeNone {
		t.Fatalf("expected mode %q, got %q", authModeNone, got)
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer preserve-me")

	if err := auth.Apply(context.Background(), req); err != nil {
		t.Fatalf("apply no-auth: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer preserve-me" {
		t.Fatalf("no-auth should not mutate authorization header, got %q", got)
	}
}
