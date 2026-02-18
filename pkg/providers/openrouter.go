package providers

import (
	"fmt"
	"strings"

	"github.com/dotsetgreg/dotagent/pkg/config"
)

const (
	defaultOpenRouterAPIBase = "https://openrouter.ai/api/v1"
	defaultOpenRouterModel   = "openai/gpt-5.2"
)

func init() {
	RegisterFactory(ProviderOpenRouter, newOpenRouterProviderFromConfig, validateOpenRouterConfig, openRouterCredentialStatus)
}

func validateOpenRouterConfig(cfg *config.Config) error {
	if cfg == nil {
		return fmt.Errorf("config is required")
	}
	if strings.TrimSpace(cfg.Providers.OpenRouter.APIKey) == "" {
		return fmt.Errorf("OpenRouter API key is required (set providers.openrouter.api_key or DOTAGENT_PROVIDERS_OPENROUTER_API_KEY)")
	}
	return nil
}

func openRouterCredentialStatus(cfg *config.Config) (bool, string) {
	if cfg == nil {
		return false, ""
	}
	if strings.TrimSpace(cfg.Providers.OpenRouter.APIKey) == "" {
		return false, ""
	}
	return true, authModeAPIKey
}

func newOpenRouterProviderFromConfig(cfg *config.Config) (LLMProvider, error) {
	if err := validateOpenRouterConfig(cfg); err != nil {
		return nil, err
	}

	apiBase := strings.TrimSpace(cfg.Providers.OpenRouter.APIBase)
	if apiBase == "" {
		apiBase = defaultOpenRouterAPIBase
	}
	auth := NewAPIKeyAuth(NewStaticTokenSource(cfg.Providers.OpenRouter.APIKey, "providers.openrouter.api_key"))
	return newChatCompletionsProvider(
		ProviderOpenRouter,
		apiBase,
		defaultOpenRouterModel,
		strings.TrimSpace(cfg.Providers.OpenRouter.Proxy),
		auth,
		nil,
	)
}
