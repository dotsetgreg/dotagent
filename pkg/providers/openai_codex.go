package providers

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/dotsetgreg/dotagent/pkg/config"
)

const (
	defaultOpenAICodexAPIBase = "https://api.openai.com/v1"
	defaultOpenAICodexModel   = "gpt-5"
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
	if mode == "oauth_token_file" {
		resolved := expandHome(source)
		if _, statErr := os.Stat(resolved); statErr != nil {
			return fmt.Errorf("OpenAI Codex OAuth token file not accessible at %s: %w", resolved, statErr)
		}
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

	return newResponsesProvider(
		ProviderOpenAICodex,
		apiBase,
		defaultOpenAICodexModel,
		strings.TrimSpace(cfg.Providers.OpenAICodex.Proxy),
		auth,
		nil,
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

	type candidate struct {
		mode   string
		source string
		field  string
	}
	candidates := make([]candidate, 0, 2)
	if token := strings.TrimSpace(cfg.Providers.OpenAICodex.OAuthAccessToken); token != "" {
		candidates = append(candidates, candidate{
			mode:   "oauth_access_token",
			source: token,
			field:  "providers.openai_codex.oauth_access_token",
		})
	}
	if tokenFile := strings.TrimSpace(cfg.Providers.OpenAICodex.OAuthTokenFile); tokenFile != "" {
		candidates = append(candidates, candidate{
			mode:   "oauth_token_file",
			source: tokenFile,
			field:  "providers.openai_codex.oauth_token_file",
		})
	}

	switch len(candidates) {
	case 0:
		return "", "", fmt.Errorf("OpenAI Codex credentials are required (set providers.openai_codex.oauth_access_token or providers.openai_codex.oauth_token_file)")
	case 1:
		chosen := candidates[0]
		return chosen.mode, chosen.source, nil
	default:
		fields := make([]string, 0, len(candidates))
		for _, item := range candidates {
			fields = append(fields, item.field)
		}
		slices.Sort(fields)
		return "", "", fmt.Errorf(
			"multiple OpenAI Codex credential sources configured (%s); set exactly one",
			strings.Join(fields, ", "),
		)
	}
}
