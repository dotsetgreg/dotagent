package providers

import (
	"context"
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
