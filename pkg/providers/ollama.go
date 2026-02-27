package providers

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/dotsetgreg/dotagent/pkg/config"
)

const (
	defaultOllamaAPIBase = "http://127.0.0.1:11434/v1"
	defaultOllamaModel   = "llama3.2"
)

func init() {
	RegisterFactory(ProviderOllama, newOllamaProviderFromConfig, validateOllamaConfig, ollamaCredentialStatus)
}

func validateOllamaConfig(cfg *config.Config) error {
	if cfg == nil {
		return fmt.Errorf("config is required")
	}
	apiBase := strings.TrimSpace(cfg.Providers.Ollama.APIBase)
	if apiBase == "" {
		apiBase = defaultOllamaAPIBase
	}
	if _, err := normalizeOllamaAPIBase(apiBase); err != nil {
		return err
	}
	return nil
}

func ollamaCredentialStatus(cfg *config.Config) (bool, string) {
	if cfg == nil {
		return false, ""
	}
	apiBase := strings.TrimSpace(cfg.Providers.Ollama.APIBase)
	if apiBase == "" {
		apiBase = defaultOllamaAPIBase
	}
	if _, err := normalizeOllamaAPIBase(apiBase); err != nil {
		return false, ""
	}
	if strings.TrimSpace(cfg.Providers.Ollama.APIKey) != "" {
		return true, authModeAPIKey
	}
	return true, authModeNone
}

func newOllamaProviderFromConfig(cfg *config.Config) (LLMProvider, error) {
	if err := validateOllamaConfig(cfg); err != nil {
		return nil, err
	}

	apiBase := strings.TrimSpace(cfg.Providers.Ollama.APIBase)
	if apiBase == "" {
		apiBase = defaultOllamaAPIBase
	}
	apiBase, err := normalizeOllamaAPIBase(apiBase)
	if err != nil {
		return nil, err
	}

	var auth AuthStrategy = NewNoAuth()
	if apiKey := strings.TrimSpace(cfg.Providers.Ollama.APIKey); apiKey != "" {
		auth = NewAPIKeyAuth(NewStaticTokenSource(apiKey, "providers.ollama.api_key"))
	}

	return newChatCompletionsProvider(
		ProviderOllama,
		apiBase,
		defaultOllamaModel,
		strings.TrimSpace(cfg.Providers.Ollama.Proxy),
		auth,
		nil,
	)
}

func normalizeOllamaAPIBase(raw string) (string, error) {
	base := strings.TrimSpace(raw)
	if base == "" {
		return "", fmt.Errorf("Ollama API base is required (set providers.ollama.api_base or DOTAGENT_PROVIDERS_OLLAMA_API_BASE)")
	}

	parsed, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("parse ollama api_base: %w", err)
	}
	if strings.TrimSpace(parsed.Scheme) == "" || strings.TrimSpace(parsed.Host) == "" {
		return "", fmt.Errorf("ollama api_base must include scheme and host (got %q)", raw)
	}

	path := strings.TrimRight(parsed.Path, "/")
	if strings.HasSuffix(path, "/chat/completions") {
		path = strings.TrimSuffix(path, "/chat/completions")
	}
	if path == "" || path == "/" {
		path = "/v1"
	} else if !strings.HasSuffix(path, "/v1") {
		path += "/v1"
	}
	parsed.Path = path
	parsed.RawQuery = ""
	parsed.Fragment = ""

	return strings.TrimRight(parsed.String(), "/"), nil
}
