package providers

import (
	"fmt"
	"os"
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
	if mode == "oauth_token_file" {
		resolved := expandHome(source)
		if _, statErr := os.Stat(resolved); statErr != nil {
			return fmt.Errorf("OpenAI OAuth token file not accessible at %s: %w", resolved, statErr)
		}
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
	if apiKey := strings.TrimSpace(cfg.Providers.OpenAI.APIKey); apiKey != "" {
		return "api_key", apiKey, nil
	}
	if token := strings.TrimSpace(cfg.Providers.OpenAI.OAuthAccessToken); token != "" {
		return "oauth_access_token", token, nil
	}
	if tokenFile := strings.TrimSpace(cfg.Providers.OpenAI.OAuthTokenFile); tokenFile != "" {
		return "oauth_token_file", tokenFile, nil
	}
	return "", "", fmt.Errorf("OpenAI credentials are required (set providers.openai.api_key, providers.openai.oauth_access_token, or providers.openai.oauth_token_file)")
}
