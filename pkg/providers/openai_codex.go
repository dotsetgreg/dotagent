package providers

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strings"

	"github.com/dotsetgreg/dotagent/pkg/config"
)

const (
	defaultOpenAICodexAPIBase      = "https://chatgpt.com/backend-api"
	defaultOpenAICodexModel        = "gpt-5"
	openAICodexJWTClaimPath        = "https://api.openai.com/auth"
	defaultOpenAICodexInstructions = "You are a helpful assistant."
)

func init() {
	RegisterFactory(ProviderOpenAICodex, newOpenAICodexProviderFromConfig, validateOpenAICodexConfig, openAICodexCredentialStatus)
}

func validateOpenAICodexConfig(cfg *config.Config) error {
	if cfg == nil {
		return fmt.Errorf("config is required")
	}
	mode, source, err := resolveOpenAICodexAuthConfig(cfg)
	if err != nil {
		return err
	}
	if err := validateOAuthTokenFileSource(mode, source, "OpenAI Codex"); err != nil {
		return err
	}
	return nil
}

func openAICodexCredentialStatus(cfg *config.Config) (bool, string) {
	if cfg == nil {
		return false, ""
	}
	mode, _, err := resolveOpenAICodexAuthConfig(cfg)
	if err != nil {
		return false, ""
	}
	return true, mode
}

func newOpenAICodexProviderFromConfig(cfg *config.Config) (LLMProvider, error) {
	if err := validateOpenAICodexConfig(cfg); err != nil {
		return nil, err
	}
	auth, err := resolveOpenAICodexAuthStrategy(cfg)
	if err != nil {
		return nil, err
	}

	apiBase := strings.TrimSpace(cfg.Providers.OpenAICodex.APIBase)
	if apiBase == "" {
		apiBase = defaultOpenAICodexAPIBase
	}

	return newResponsesProviderWithOptions(
		ProviderOpenAICodex,
		apiBase,
		defaultOpenAICodexModel,
		strings.TrimSpace(cfg.Providers.OpenAICodex.Proxy),
		auth,
		nil,
		&responsesProviderOptions{
			buildEndpoint: resolveOpenAICodexResponsesEndpoint,
			beforeMarshal: func(body map[string]interface{}) {
				delete(body, "max_output_tokens")
				delete(body, "temperature")
				body["store"] = false
				body["stream"] = true
				ensureOpenAICodexInstructions(body)
			},
			beforeSend: decorateOpenAICodexRequest,
		},
	)
}

func resolveOpenAICodexAuthStrategy(cfg *config.Config) (AuthStrategy, error) {
	mode, source, err := resolveOpenAICodexAuthConfig(cfg)
	if err != nil {
		return nil, err
	}
	switch mode {
	case "oauth_access_token":
		return NewBearerTokenAuth(NewStaticTokenSource(source, "providers.openai_codex.oauth_access_token")), nil
	case "oauth_token_file":
		return NewBearerTokenAuth(NewFileTokenSource(source)), nil
	default:
		return nil, fmt.Errorf("unsupported OpenAI Codex auth mode %q", mode)
	}
}

func resolveOpenAICodexAuthConfig(cfg *config.Config) (mode string, source string, err error) {
	if cfg == nil {
		return "", "", fmt.Errorf("config is required")
	}

	candidates := make([]credentialCandidate, 0, 2)
	if token := strings.TrimSpace(cfg.Providers.OpenAICodex.OAuthAccessToken); token != "" {
		candidates = append(candidates, credentialCandidate{
			mode:   "oauth_access_token",
			source: token,
			field:  "providers.openai_codex.oauth_access_token",
		})
	}
	if tokenFile := strings.TrimSpace(cfg.Providers.OpenAICodex.OAuthTokenFile); tokenFile != "" {
		candidates = append(candidates, credentialCandidate{
			mode:   "oauth_token_file",
			source: tokenFile,
			field:  "providers.openai_codex.oauth_token_file",
		})
	}

	return selectSingleCredential(
		candidates,
		"OpenAI Codex credentials are required (set providers.openai_codex.oauth_access_token or providers.openai_codex.oauth_token_file)",
		"multiple OpenAI Codex credential sources configured",
	)
}

func resolveOpenAICodexResponsesEndpoint(apiBase string) string {
	base := strings.TrimRight(strings.TrimSpace(apiBase), "/")
	if base == "" {
		return ""
	}
	if strings.HasSuffix(base, "/codex/responses") {
		return base
	}
	if strings.HasSuffix(base, "/codex") {
		return base + "/responses"
	}
	return base + "/codex/responses"
}

func decorateOpenAICodexRequest(req *http.Request) error {
	if req == nil {
		return fmt.Errorf("request is nil")
	}
	token, err := extractBearerToken(req.Header.Get("Authorization"))
	if err != nil {
		return err
	}
	accountID, err := extractOpenAICodexAccountID(token)
	if err != nil {
		return err
	}

	req.Header.Set("chatgpt-account-id", accountID)
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	req.Header.Set("originator", "dotagent")
	req.Header.Set("User-Agent", openAICodexUserAgent())
	req.Header.Set("Accept", "application/json")
	return nil
}

func extractBearerToken(authHeader string) (string, error) {
	header := strings.TrimSpace(authHeader)
	if header == "" {
		return "", fmt.Errorf("missing Authorization header")
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(strings.ToLower(header), strings.ToLower(prefix)) {
		return "", fmt.Errorf("unsupported Authorization header format")
	}
	token := strings.TrimSpace(header[len(prefix):])
	if token == "" {
		return "", fmt.Errorf("bearer token is empty")
	}
	return token, nil
}

func extractOpenAICodexAccountID(token string) (string, error) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("invalid OpenAI Codex token format")
	}
	payloadRaw, err := decodeBase64URL(parts[1])
	if err != nil {
		return "", fmt.Errorf("decode OpenAI Codex token payload: %w", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(payloadRaw, &payload); err != nil {
		return "", fmt.Errorf("parse OpenAI Codex token payload: %w", err)
	}
	rawClaim, ok := payload[openAICodexJWTClaimPath]
	if !ok {
		return "", fmt.Errorf("missing %q claim in OpenAI Codex token", openAICodexJWTClaimPath)
	}
	authClaim, ok := rawClaim.(map[string]any)
	if !ok {
		return "", fmt.Errorf("invalid %q claim in OpenAI Codex token", openAICodexJWTClaimPath)
	}
	accountID, _ := authClaim["chatgpt_account_id"].(string)
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return "", fmt.Errorf("missing chatgpt_account_id in OpenAI Codex token")
	}
	return accountID, nil
}

func decodeBase64URL(raw string) ([]byte, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, fmt.Errorf("empty base64url segment")
	}
	if decoded, err := base64.RawURLEncoding.DecodeString(trimmed); err == nil {
		return decoded, nil
	}
	decoded, err := base64.URLEncoding.DecodeString(trimmed)
	if err != nil {
		return nil, err
	}
	return decoded, nil
}

func openAICodexUserAgent() string {
	return fmt.Sprintf("dotagent (%s %s; %s)", runtime.GOOS, runtime.GOARCH, runtime.Version())
}

func ensureOpenAICodexInstructions(body map[string]interface{}) {
	if body == nil {
		return
	}
	if current, ok := body["instructions"].(string); ok && strings.TrimSpace(current) != "" {
		return
	}

	inputItems := normalizeResponsesInputItems(body["input"])
	if len(inputItems) == 0 {
		body["instructions"] = defaultOpenAICodexInstructions
		return
	}

	filtered := make([]map[string]interface{}, 0, len(inputItems))
	instructionParts := make([]string, 0, 2)
	for _, item := range inputItems {
		role := strings.ToLower(strings.TrimSpace(asString(item["role"])))
		if role == "system" || role == "developer" {
			if text := extractResponsesContentText(item["content"]); text != "" {
				instructionParts = append(instructionParts, text)
			}
			continue
		}
		filtered = append(filtered, item)
	}

	if len(filtered) > 0 {
		body["input"] = filtered
	}

	instructions := strings.TrimSpace(strings.Join(instructionParts, "\n\n"))
	if instructions == "" {
		instructions = defaultOpenAICodexInstructions
	}
	body["instructions"] = instructions
}

func normalizeResponsesInputItems(raw any) []map[string]interface{} {
	switch v := raw.(type) {
	case []map[string]interface{}:
		return v
	case []interface{}:
		items := make([]map[string]interface{}, 0, len(v))
		for _, entry := range v {
			if item, ok := entry.(map[string]interface{}); ok {
				items = append(items, item)
			}
		}
		return items
	default:
		return nil
	}
}

func extractResponsesContentText(raw any) string {
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case []map[string]interface{}:
		parts := make([]string, 0, len(v))
		for _, part := range v {
			ptype := strings.ToLower(strings.TrimSpace(asString(part["type"])))
			if ptype == "input_text" || ptype == "text" || ptype == "output_text" {
				if text := strings.TrimSpace(asString(part["text"])); text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n"))
	case []interface{}:
		parts := make([]string, 0, len(v))
		for _, entry := range v {
			if part, ok := entry.(map[string]interface{}); ok {
				ptype := strings.ToLower(strings.TrimSpace(asString(part["type"])))
				if ptype == "input_text" || ptype == "text" || ptype == "output_text" {
					if text := strings.TrimSpace(asString(part["text"])); text != "" {
						parts = append(parts, text)
					}
				}
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n"))
	default:
		return ""
	}
}

func asString(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	return ""
}
