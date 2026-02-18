package providers

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/dotsetgreg/dotagent/pkg/config"
)

const (
	ProviderOpenRouter = "openrouter"
	ProviderOpenAI     = "openai"
)

type providerFactory struct {
	build              func(cfg *config.Config) (LLMProvider, error)
	validate           func(cfg *config.Config) error
	credentialStatusFn func(cfg *config.Config) (configured bool, mode string)
}

var (
	factoryMu       sync.RWMutex
	factories       = map[string]providerFactory{}
	registrationErr error
)

func RegisterFactory(name string, build func(cfg *config.Config) (LLMProvider, error), validate func(cfg *config.Config) error, credentialStatusFn func(cfg *config.Config) (bool, string)) {
	name = NormalizeProviderName(name)
	factoryMu.Lock()
	defer factoryMu.Unlock()
	if name == "" {
		registrationErr = errors.Join(registrationErr, fmt.Errorf("providers: factory name is required"))
		return
	}
	if build == nil {
		registrationErr = errors.Join(registrationErr, fmt.Errorf("providers: factory build func is required"))
		return
	}
	factories[name] = providerFactory{
		build:              build,
		validate:           validate,
		credentialStatusFn: credentialStatusFn,
	}
}

func SupportedProviders() []string {
	factoryMu.RLock()
	defer factoryMu.RUnlock()
	providers := make([]string, 0, len(factories))
	for name := range factories {
		providers = append(providers, name)
	}
	sort.Strings(providers)
	return providers
}

func NormalizeProviderName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return ProviderOpenRouter
	}
	return name
}

func ActiveProviderName(cfg *config.Config) string {
	if cfg == nil {
		return ProviderOpenRouter
	}
	return NormalizeProviderName(cfg.Agents.Defaults.Provider)
}

func ValidateProviderConfig(cfg *config.Config) error {
	factory, _, err := getFactory(cfg)
	if err != nil {
		return err
	}
	if factory.validate == nil {
		return nil
	}
	if err := factory.validate(cfg); err != nil {
		return err
	}
	return nil
}

func ProviderCredentialStatus(cfg *config.Config) (provider string, configured bool, mode string, err error) {
	factory, name, err := getFactory(cfg)
	if err != nil {
		return "", false, "", err
	}
	provider = name
	if factory.credentialStatusFn != nil {
		configured, mode = factory.credentialStatusFn(cfg)
		return provider, configured, mode, nil
	}
	configured = factory.validate == nil || factory.validate(cfg) == nil
	return provider, configured, "", nil
}

func CreateProvider(cfg *config.Config) (LLMProvider, error) {
	factory, _, err := getFactory(cfg)
	if err != nil {
		return nil, err
	}
	provider, err := factory.build(cfg)
	if err != nil {
		return nil, err
	}
	return provider, nil
}

func getFactory(cfg *config.Config) (providerFactory, string, error) {
	name := ActiveProviderName(cfg)

	factoryMu.RLock()
	if registrationErr != nil {
		err := registrationErr
		factoryMu.RUnlock()
		return providerFactory{}, name, fmt.Errorf("provider registration failed: %w", err)
	}
	factory, ok := factories[name]
	factoryMu.RUnlock()
	if !ok {
		return providerFactory{}, name, fmt.Errorf("unsupported provider %q: supported providers are %s", name, strings.Join(SupportedProviders(), ", "))
	}
	return factory, name, nil
}
