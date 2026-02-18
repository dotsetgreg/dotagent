package providers

import (
	"context"
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
	if tok == "" {
		return "", fmt.Errorf("token is empty for %s", s.Source())
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
	tok := strings.TrimSpace(string(data))
	if tok == "" {
		return "", fmt.Errorf("token file %s is empty", resolved)
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
