package providers

import (
	"fmt"
	"strings"

	"github.com/dotsetgreg/dotagent/pkg/config"
)

const (
	defaultOpenAIAPIBase = "https://api.openai.com/v1"
	defaultOpenAIModel   = "gpt-5-mini"
)

func init() {
	RegisterFactory(ProviderOpenAI, newOpenAIProviderFromConfig, validateOpenAIConfig, openAICredentialStatus)
}

func validateOpenAIConfig(cfg *config.Config) error {
	if cfg == nil {
		return fmt.Errorf("config is required")
	}
	mode, source, err := resolveOpenAIAuthConfig(cfg)
	if err != nil {
		return err
	}
	if err := validateOAuthTokenFileSource(mode, source, "OpenAI"); err != nil {
		return err
	}
	return nil
}

func openAICredentialStatus(cfg *config.Config) (bool, string) {
	if cfg == nil {
		return false, ""
	}
	mode, _, err := resolveOpenAIAuthConfig(cfg)
	if err != nil {
		return false, ""
	}
	switch mode {
	case "api_key":
		return true, authModeAPIKey
	case "oauth_access_token", "oauth_token_file":
		return true, mode
	default:
		return false, ""
	}
}

func newOpenAIProviderFromConfig(cfg *config.Config) (LLMProvider, error) {
	if err := validateOpenAIConfig(cfg); err != nil {
		return nil, err
	}
	auth, err := resolveOpenAIAuthStrategy(cfg)
	if err != nil {
		return nil, err
	}

	apiBase := strings.TrimSpace(cfg.Providers.OpenAI.APIBase)
	if apiBase == "" {
		apiBase = defaultOpenAIAPIBase
	}
	extraHeaders := map[string]string{}
	if org := strings.TrimSpace(cfg.Providers.OpenAI.Organization); org != "" {
		extraHeaders["OpenAI-Organization"] = org
	}
	if project := strings.TrimSpace(cfg.Providers.OpenAI.Project); project != "" {
		extraHeaders["OpenAI-Project"] = project
	}

	return newChatCompletionsProvider(
		ProviderOpenAI,
		apiBase,
		defaultOpenAIModel,
		strings.TrimSpace(cfg.Providers.OpenAI.Proxy),
		auth,
		extraHeaders,
	)
}

func resolveOpenAIAuthStrategy(cfg *config.Config) (AuthStrategy, error) {
	mode, source, err := resolveOpenAIAuthConfig(cfg)
	if err != nil {
		return nil, err
	}
	switch mode {
	case "api_key":
		return NewAPIKeyAuth(NewStaticTokenSource(source, "providers.openai.api_key")), nil
	case "oauth_access_token":
		return NewBearerTokenAuth(NewStaticTokenSource(source, "providers.openai.oauth_access_token")), nil
	case "oauth_token_file":
		return NewBearerTokenAuth(NewFileTokenSource(source)), nil
	default:
		return nil, fmt.Errorf("unsupported OpenAI auth mode %q", mode)
	}
}

func resolveOpenAIAuthConfig(cfg *config.Config) (mode string, source string, err error) {
	if cfg == nil {
		return "", "", fmt.Errorf("config is required")
	}

	candidates := make([]credentialCandidate, 0, 3)
	if apiKey := strings.TrimSpace(cfg.Providers.OpenAI.APIKey); apiKey != "" {
		candidates = append(candidates, credentialCandidate{
			mode:   "api_key",
			source: apiKey,
			field:  "providers.openai.api_key",
		})
	}
	if token := strings.TrimSpace(cfg.Providers.OpenAI.OAuthAccessToken); token != "" {
		candidates = append(candidates, credentialCandidate{
			mode:   "oauth_access_token",
			source: token,
			field:  "providers.openai.oauth_access_token",
		})
	}
	if tokenFile := strings.TrimSpace(cfg.Providers.OpenAI.OAuthTokenFile); tokenFile != "" {
		candidates = append(candidates, credentialCandidate{
			mode:   "oauth_token_file",
			source: tokenFile,
			field:  "providers.openai.oauth_token_file",
		})
	}

	return selectSingleCredential(
		candidates,
		"OpenAI credentials are required (set providers.openai.api_key, providers.openai.oauth_access_token, or providers.openai.oauth_token_file)",
		"multiple OpenAI credential sources configured",
	)
}
