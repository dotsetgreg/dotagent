package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/caarlos0/env/v11"
)

// FlexibleStringSlice is a []string that also accepts JSON numbers,
// so allow_from can contain both "123" and 123.
type FlexibleStringSlice []string

func (f *FlexibleStringSlice) UnmarshalJSON(data []byte) error {
	// Try []string first
	var ss []string
	if err := json.Unmarshal(data, &ss); err == nil {
		*f = ss
		return nil
	}

	// Try []interface{} to handle mixed types
	var raw []interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	result := make([]string, 0, len(raw))
	for _, v := range raw {
		switch val := v.(type) {
		case string:
			result = append(result, val)
		case float64:
			result = append(result, fmt.Sprintf("%.0f", val))
		default:
			result = append(result, fmt.Sprintf("%v", val))
		}
	}
	*f = result
	return nil
}

type Config struct {
	Agents    AgentsConfig    `json:"agents"`
	Channels  ChannelsConfig  `json:"channels"`
	Providers ProvidersConfig `json:"providers"`
	Gateway   GatewayConfig   `json:"gateway"`
	Tools     ToolsConfig     `json:"tools"`
	Memory    MemoryConfig    `json:"memory"`
	Heartbeat HeartbeatConfig `json:"heartbeat"`
	mu        sync.RWMutex
}

type AgentsConfig struct {
	Defaults AgentDefaults `json:"defaults"`
}

type AgentDefaults struct {
	Workspace           string  `json:"workspace" env:"DOTAGENT_AGENTS_DEFAULTS_WORKSPACE"`
	RestrictToWorkspace bool    `json:"restrict_to_workspace" env:"DOTAGENT_AGENTS_DEFAULTS_RESTRICT_TO_WORKSPACE"`
	Provider            string  `json:"provider" env:"DOTAGENT_AGENTS_DEFAULTS_PROVIDER"`
	Model               string  `json:"model" env:"DOTAGENT_AGENTS_DEFAULTS_MODEL"`
	MaxTokens           int     `json:"max_tokens" env:"DOTAGENT_AGENTS_DEFAULTS_MAX_TOKENS"`
	Temperature         float64 `json:"temperature" env:"DOTAGENT_AGENTS_DEFAULTS_TEMPERATURE"`
	MaxToolIterations   int     `json:"max_tool_iterations" env:"DOTAGENT_AGENTS_DEFAULTS_MAX_TOOL_ITERATIONS"`
}

type ChannelsConfig struct {
	Discord DiscordConfig `json:"discord"`
}

type DiscordConfig struct {
	Token     string              `json:"token" env:"DOTAGENT_CHANNELS_DISCORD_TOKEN"`
	AllowFrom FlexibleStringSlice `json:"allow_from" env:"DOTAGENT_CHANNELS_DISCORD_ALLOW_FROM"`
}

type HeartbeatConfig struct {
	Enabled  bool `json:"enabled" env:"DOTAGENT_HEARTBEAT_ENABLED"`
	Interval int  `json:"interval" env:"DOTAGENT_HEARTBEAT_INTERVAL"` // minutes, min 5
}

type ProvidersConfig struct {
	OpenRouter ProviderConfig `json:"openrouter"`
}

type ProviderConfig struct {
	APIKey  string `json:"api_key" env:"DOTAGENT_PROVIDERS_OPENROUTER_API_KEY"`
	APIBase string `json:"api_base" env:"DOTAGENT_PROVIDERS_OPENROUTER_API_BASE"`
	Proxy   string `json:"proxy,omitempty" env:"DOTAGENT_PROVIDERS_OPENROUTER_PROXY"`
}

type GatewayConfig struct {
	Host string `json:"host" env:"DOTAGENT_GATEWAY_HOST"`
	Port int    `json:"port" env:"DOTAGENT_GATEWAY_PORT"`
}

type BraveConfig struct {
	Enabled    bool   `json:"enabled" env:"DOTAGENT_TOOLS_WEB_BRAVE_ENABLED"`
	APIKey     string `json:"api_key" env:"DOTAGENT_TOOLS_WEB_BRAVE_API_KEY"`
	MaxResults int    `json:"max_results" env:"DOTAGENT_TOOLS_WEB_BRAVE_MAX_RESULTS"`
}

type DuckDuckGoConfig struct {
	Enabled    bool `json:"enabled" env:"DOTAGENT_TOOLS_WEB_DUCKDUCKGO_ENABLED"`
	MaxResults int  `json:"max_results" env:"DOTAGENT_TOOLS_WEB_DUCKDUCKGO_MAX_RESULTS"`
}

type WebToolsConfig struct {
	Brave      BraveConfig      `json:"brave"`
	DuckDuckGo DuckDuckGoConfig `json:"duckduckgo"`
}

type ToolPolicyConfig struct {
	DefaultMode   string            `json:"default_mode" env:"DOTAGENT_TOOLS_POLICY_DEFAULT_MODE"`
	Allow         []string          `json:"allow"`
	Deny          []string          `json:"deny"`
	ProviderModes map[string]string `json:"provider_modes"`
}

type ToolsConfig struct {
	Web    WebToolsConfig   `json:"web"`
	Policy ToolPolicyConfig `json:"policy"`
}

type MemoryConfig struct {
	MaxRecallItems        int     `json:"max_recall_items" env:"DOTAGENT_MEMORY_MAX_RECALL_ITEMS"`
	CandidateLimit        int     `json:"candidate_limit" env:"DOTAGENT_MEMORY_CANDIDATE_LIMIT"`
	RetrievalCacheSeconds int     `json:"retrieval_cache_seconds" env:"DOTAGENT_MEMORY_RETRIEVAL_CACHE_SECONDS"`
	WorkerPollMS          int     `json:"worker_poll_ms" env:"DOTAGENT_MEMORY_WORKER_POLL_MS"`
	WorkerLeaseSeconds    int     `json:"worker_lease_seconds" env:"DOTAGENT_MEMORY_WORKER_LEASE_SECONDS"`
	EmbeddingModel        string  `json:"embedding_model" env:"DOTAGENT_MEMORY_EMBEDDING_MODEL"`
	EventRetentionDays    int     `json:"event_retention_days" env:"DOTAGENT_MEMORY_EVENT_RETENTION_DAYS"`
	AuditRetentionDays    int     `json:"audit_retention_days" env:"DOTAGENT_MEMORY_AUDIT_RETENTION_DAYS"`
	PersonaSyncApply      bool    `json:"persona_sync_apply" env:"DOTAGENT_MEMORY_PERSONA_SYNC_APPLY"`
	PersonaFileSyncMode   string  `json:"persona_file_sync_mode" env:"DOTAGENT_MEMORY_PERSONA_FILE_SYNC_MODE"`
	PersonaPolicyMode     string  `json:"persona_policy_mode" env:"DOTAGENT_MEMORY_PERSONA_POLICY_MODE"`
	PersonaMinConfidence  float64 `json:"persona_min_confidence" env:"DOTAGENT_MEMORY_PERSONA_MIN_CONFIDENCE"`
}

func DefaultConfig() *Config {
	return &Config{
		Agents: AgentsConfig{
			Defaults: AgentDefaults{
				Workspace:           "~/.dotagent/workspace",
				RestrictToWorkspace: true,
				Provider:            "openrouter",
				Model:               "openai/gpt-5.2",
				MaxTokens:           8192,
				Temperature:         0.7,
				MaxToolIterations:   20,
			},
		},
		Channels: ChannelsConfig{
			Discord: DiscordConfig{
				Token:     "",
				AllowFrom: FlexibleStringSlice{},
			},
		},
		Providers: ProvidersConfig{
			OpenRouter: ProviderConfig{},
		},
		Gateway: GatewayConfig{
			Host: "0.0.0.0",
			Port: 18790,
		},
		Tools: ToolsConfig{
			Web: WebToolsConfig{
				Brave: BraveConfig{
					Enabled:    false,
					APIKey:     "",
					MaxResults: 5,
				},
				DuckDuckGo: DuckDuckGoConfig{
					Enabled:    true,
					MaxResults: 5,
				},
			},
			Policy: ToolPolicyConfig{
				DefaultMode: "auto",
				Allow:       []string{},
				Deny:        []string{},
				ProviderModes: map[string]string{
					"openrouter": "auto",
				},
			},
		},
		Memory: MemoryConfig{
			MaxRecallItems:        8,
			CandidateLimit:        80,
			RetrievalCacheSeconds: 20,
			WorkerPollMS:          700,
			WorkerLeaseSeconds:    60,
			EmbeddingModel:        "dotagent-chargram-384-v1",
			EventRetentionDays:    90,
			AuditRetentionDays:    365,
			PersonaSyncApply:      true,
			PersonaFileSyncMode:   "export_only",
			PersonaPolicyMode:     "balanced",
			PersonaMinConfidence:  0.52,
		},
		Heartbeat: HeartbeatConfig{
			Enabled:  true,
			Interval: 30, // default 30 minutes
		},
	}
}

func LoadConfig(path string) (*Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, err
	}

	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, err
	}

	if err := env.Parse(cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}

func SaveConfig(path string, cfg *Config) error {
	cfg.mu.RLock()
	defer cfg.mu.RUnlock()

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	return os.WriteFile(path, data, 0600)
}

func (c *Config) WorkspacePath() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return expandHome(c.Agents.Defaults.Workspace)
}

func (c *Config) GetAPIKey() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Providers.OpenRouter.APIKey
}

func (c *Config) GetAPIBase() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.Providers.OpenRouter.APIBase != "" {
		return c.Providers.OpenRouter.APIBase
	}
	return "https://openrouter.ai/api/v1"
}

func expandHome(path string) string {
	if path == "" {
		return path
	}
	if path[0] == '~' {
		home, _ := os.UserHomeDir()
		if len(path) > 1 && path[1] == '/' {
			return home + path[1:]
		}
		return home
	}
	return path
}
