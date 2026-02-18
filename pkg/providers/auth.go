package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const (
	authModeAPIKey      = "api_key"
	authModeBearerToken = "bearer_token"
)

// TokenSource returns bearer material for request auth.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
	Source() string
}

type staticTokenSource struct {
	token  string
	source string
}

func NewStaticTokenSource(token, source string) TokenSource {
	return &staticTokenSource{
		token:  strings.TrimSpace(token),
		source: strings.TrimSpace(source),
	}
}

func (s *staticTokenSource) Token(context.Context) (string, error) {
	tok := strings.TrimSpace(s.token)
	if err := validateTokenLiteral(tok, s.Source()); err != nil {
		return "", err
	}
	return tok, nil
}

func (s *staticTokenSource) Source() string {
	if s.source != "" {
		return s.source
	}
	return "static"
}

type fileTokenSource struct {
	path string
}

func NewFileTokenSource(path string) TokenSource {
	return &fileTokenSource{path: strings.TrimSpace(path)}
}

func (s *fileTokenSource) Token(context.Context) (string, error) {
	resolved := expandHome(strings.TrimSpace(s.path))
	if resolved == "" {
		return "", fmt.Errorf("token file path is empty")
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return "", fmt.Errorf("read token file %s: %w", resolved, err)
	}
	tok, err := parseTokenFileContent(data, resolved)
	if err != nil {
		return "", err
	}
	return tok, nil
}

func (s *fileTokenSource) Source() string {
	resolved := expandHome(strings.TrimSpace(s.path))
	if resolved != "" {
		return resolved
	}
	return "token_file"
}

// AuthStrategy applies request auth for provider HTTP calls.
type AuthStrategy interface {
	Mode() string
	Apply(ctx context.Context, req *http.Request) error
}

type apiKeyAuth struct {
	source TokenSource
}

func NewAPIKeyAuth(source TokenSource) AuthStrategy {
	return &apiKeyAuth{source: source}
}

func (a *apiKeyAuth) Mode() string {
	return authModeAPIKey
}

func (a *apiKeyAuth) Apply(ctx context.Context, req *http.Request) error {
	return applyBearerAuth(ctx, req, a.source)
}

type bearerTokenAuth struct {
	source TokenSource
}

func NewBearerTokenAuth(source TokenSource) AuthStrategy {
	return &bearerTokenAuth{source: source}
}

func (a *bearerTokenAuth) Mode() string {
	return authModeBearerToken
}

func (a *bearerTokenAuth) Apply(ctx context.Context, req *http.Request) error {
	return applyBearerAuth(ctx, req, a.source)
}

func applyBearerAuth(ctx context.Context, req *http.Request, source TokenSource) error {
	if source == nil {
		return fmt.Errorf("auth token source is nil")
	}
	tok, err := source.Token(ctx)
	if err != nil {
		return fmt.Errorf("resolve auth token: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	return nil
}

func expandHome(path string) string {
	path = strings.TrimSpace(path)
	if path == "" || path[0] != '~' {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	return path
}

func parseTokenFileContent(data []byte, source string) (string, error) {
	raw := strings.TrimSpace(string(data))
	if raw == "" {
		return "", fmt.Errorf("token file %s is empty", source)
	}

	// Support both plain-token files and Codex/OpenAI OAuth auth.json format.
	if strings.HasPrefix(raw, "{") || strings.HasPrefix(raw, "[") {
		token, err := extractTokenFromJSON(raw)
		if err != nil {
			return "", fmt.Errorf("parse token JSON from %s: %w", source, err)
		}
		if err := validateTokenLiteral(token, source); err != nil {
			return "", err
		}
		return token, nil
	}

	if err := validateTokenLiteral(raw, source); err != nil {
		return "", err
	}
	return raw, nil
}

func extractTokenFromJSON(raw string) (string, error) {
	var payload struct {
		AccessToken string `json:"access_token"`
		Tokens      struct {
			AccessToken string `json:"access_token"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return "", err
	}

	token := strings.TrimSpace(payload.AccessToken)
	if token == "" {
		token = strings.TrimSpace(payload.Tokens.AccessToken)
	}
	if token == "" {
		return "", fmt.Errorf("missing access_token (expected access_token or tokens.access_token)")
	}
	return token, nil
}

func validateTokenLiteral(tok, source string) error {
	token := strings.TrimSpace(tok)
	if token == "" {
		return fmt.Errorf("token is empty for %s", source)
	}
	if strings.HasPrefix(token, "<") && strings.HasSuffix(token, ">") {
		return fmt.Errorf("token for %s appears to be a placeholder template", source)
	}
	if strings.HasPrefix(token, "$") || strings.Contains(token, "${") {
		return fmt.Errorf("token for %s appears to be an unresolved environment reference", source)
	}
	if strings.ContainsAny(token, " \t\r\n") {
		return fmt.Errorf("token for %s must be a single value without whitespace", source)
	}
	return nil
}
