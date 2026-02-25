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
	Workspace                 string  `json:"workspace" env:"DOTAGENT_AGENTS_DEFAULTS_WORKSPACE"`
	RestrictToWorkspace       bool    `json:"restrict_to_workspace" env:"DOTAGENT_AGENTS_DEFAULTS_RESTRICT_TO_WORKSPACE"`
	Provider                  string  `json:"provider" env:"DOTAGENT_AGENTS_DEFAULTS_PROVIDER"`
	Model                     string  `json:"model" env:"DOTAGENT_AGENTS_DEFAULTS_MODEL"`
	MaxTokens                 int     `json:"max_tokens" env:"DOTAGENT_AGENTS_DEFAULTS_MAX_TOKENS"`
	Temperature               float64 `json:"temperature" env:"DOTAGENT_AGENTS_DEFAULTS_TEMPERATURE"`
	MaxToolIterations         int     `json:"max_tool_iterations" env:"DOTAGENT_AGENTS_DEFAULTS_MAX_TOOL_ITERATIONS"`
	MaxConcurrentRuns         int     `json:"max_concurrent_runs" env:"DOTAGENT_AGENTS_DEFAULTS_MAX_CONCURRENT_RUNS"`
	SessionFileLockEnabled    bool    `json:"session_file_lock_enabled" env:"DOTAGENT_AGENTS_DEFAULTS_SESSION_FILE_LOCK_ENABLED"`
	SessionLockTimeoutMS      int     `json:"session_lock_timeout_ms" env:"DOTAGENT_AGENTS_DEFAULTS_SESSION_LOCK_TIMEOUT_MS"`
	SessionLockStaleSeconds   int     `json:"session_lock_stale_seconds" env:"DOTAGENT_AGENTS_DEFAULTS_SESSION_LOCK_STALE_SECONDS"`
	SessionLockMaxHoldSeconds int     `json:"session_lock_max_hold_seconds" env:"DOTAGENT_AGENTS_DEFAULTS_SESSION_LOCK_MAX_HOLD_SECONDS"`
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
	OpenRouter  OpenRouterProviderConfig  `json:"openrouter"`
	OpenAI      OpenAIProviderConfig      `json:"openai"`
	OpenAICodex OpenAICodexProviderConfig `json:"openai_codex"`
}

type OpenRouterProviderConfig struct {
	APIKey  string `json:"api_key" env:"DOTAGENT_PROVIDERS_OPENROUTER_API_KEY"`
	APIBase string `json:"api_base" env:"DOTAGENT_PROVIDERS_OPENROUTER_API_BASE"`
	Proxy   string `json:"proxy,omitempty" env:"DOTAGENT_PROVIDERS_OPENROUTER_PROXY"`
}

type OpenAIProviderConfig struct {
	APIKey           string `json:"api_key" env:"DOTAGENT_PROVIDERS_OPENAI_API_KEY"`
	OAuthAccessToken string `json:"oauth_access_token,omitempty" env:"DOTAGENT_PROVIDERS_OPENAI_OAUTH_ACCESS_TOKEN"`
	OAuthTokenFile   string `json:"oauth_token_file,omitempty" env:"DOTAGENT_PROVIDERS_OPENAI_OAUTH_TOKEN_FILE"`
	APIBase          string `json:"api_base" env:"DOTAGENT_PROVIDERS_OPENAI_API_BASE"`
	Proxy            string `json:"proxy,omitempty" env:"DOTAGENT_PROVIDERS_OPENAI_PROXY"`
	Organization     string `json:"organization,omitempty" env:"DOTAGENT_PROVIDERS_OPENAI_ORGANIZATION"`
	Project          string `json:"project,omitempty" env:"DOTAGENT_PROVIDERS_OPENAI_PROJECT"`
}

type OpenAICodexProviderConfig struct {
	OAuthAccessToken string `json:"oauth_access_token,omitempty" env:"DOTAGENT_PROVIDERS_OPENAI_CODEX_OAUTH_ACCESS_TOKEN"`
	OAuthTokenFile   string `json:"oauth_token_file,omitempty" env:"DOTAGENT_PROVIDERS_OPENAI_CODEX_OAUTH_TOKEN_FILE"`
	APIBase          string `json:"api_base" env:"DOTAGENT_PROVIDERS_OPENAI_CODEX_API_BASE"`
	Proxy            string `json:"proxy,omitempty" env:"DOTAGENT_PROVIDERS_OPENAI_CODEX_PROXY"`
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

type ToolsConfig struct {
	Web WebToolsConfig `json:"web"`
}

type MemoryConfig struct {
	MaxRecallItems                      int      `json:"max_recall_items" env:"DOTAGENT_MEMORY_MAX_RECALL_ITEMS"`
	CandidateLimit                      int      `json:"candidate_limit" env:"DOTAGENT_MEMORY_CANDIDATE_LIMIT"`
	RetrievalCacheSeconds               int      `json:"retrieval_cache_seconds" env:"DOTAGENT_MEMORY_RETRIEVAL_CACHE_SECONDS"`
	WorkerPollMS                        int      `json:"worker_poll_ms" env:"DOTAGENT_MEMORY_WORKER_POLL_MS"`
	WorkerLeaseSeconds                  int      `json:"worker_lease_seconds" env:"DOTAGENT_MEMORY_WORKER_LEASE_SECONDS"`
	EmbeddingModel                      string   `json:"embedding_model" env:"DOTAGENT_MEMORY_EMBEDDING_MODEL"`
	EmbeddingFallbackModels             []string `json:"embedding_fallback_models" env:"DOTAGENT_MEMORY_EMBEDDING_FALLBACK_MODELS"`
	EmbeddingOllamaAPIBase              string   `json:"embedding_ollama_api_base" env:"DOTAGENT_MEMORY_EMBEDDING_OLLAMA_API_BASE"`
	EmbeddingBatchSize                  int      `json:"embedding_batch_size" env:"DOTAGENT_MEMORY_EMBEDDING_BATCH_SIZE"`
	EmbeddingConcurrency                int      `json:"embedding_concurrency" env:"DOTAGENT_MEMORY_EMBEDDING_CONCURRENCY"`
	ToolLoopDetectionEnabled            bool     `json:"tool_loop_detection_enabled" env:"DOTAGENT_MEMORY_TOOL_LOOP_DETECTION_ENABLED"`
	ToolLoopWarningsEnabled             bool     `json:"tool_loop_warnings_enabled" env:"DOTAGENT_MEMORY_TOOL_LOOP_WARNINGS_ENABLED"`
	ToolLoopSignatureWarnThreshold      int      `json:"tool_loop_signature_warn_threshold" env:"DOTAGENT_MEMORY_TOOL_LOOP_SIGNATURE_WARN_THRESHOLD"`
	ToolLoopSignatureCriticalThreshold  int      `json:"tool_loop_signature_critical_threshold" env:"DOTAGENT_MEMORY_TOOL_LOOP_SIGNATURE_CRITICAL_THRESHOLD"`
	ToolLoopDriftWarnThreshold          int      `json:"tool_loop_drift_warn_threshold" env:"DOTAGENT_MEMORY_TOOL_LOOP_DRIFT_WARN_THRESHOLD"`
	ToolLoopDriftCriticalThreshold      int      `json:"tool_loop_drift_critical_threshold" env:"DOTAGENT_MEMORY_TOOL_LOOP_DRIFT_CRITICAL_THRESHOLD"`
	ToolLoopPollingWarnThreshold        int      `json:"tool_loop_polling_warn_threshold" env:"DOTAGENT_MEMORY_TOOL_LOOP_POLLING_WARN_THRESHOLD"`
	ToolLoopPollingCriticalThreshold    int      `json:"tool_loop_polling_critical_threshold" env:"DOTAGENT_MEMORY_TOOL_LOOP_POLLING_CRITICAL_THRESHOLD"`
	ToolLoopNoProgressWarnThreshold     int      `json:"tool_loop_no_progress_warn_threshold" env:"DOTAGENT_MEMORY_TOOL_LOOP_NO_PROGRESS_WARN_THRESHOLD"`
	ToolLoopNoProgressCriticalThreshold int      `json:"tool_loop_no_progress_critical_threshold" env:"DOTAGENT_MEMORY_TOOL_LOOP_NO_PROGRESS_CRITICAL_THRESHOLD"`
	ToolLoopPingPongWarnThreshold       int      `json:"tool_loop_ping_pong_warn_threshold" env:"DOTAGENT_MEMORY_TOOL_LOOP_PING_PONG_WARN_THRESHOLD"`
	ToolLoopPingPongCriticalThreshold   int      `json:"tool_loop_ping_pong_critical_threshold" env:"DOTAGENT_MEMORY_TOOL_LOOP_PING_PONG_CRITICAL_THRESHOLD"`
	ToolLoopGlobalCircuitThreshold      int      `json:"tool_loop_global_circuit_threshold" env:"DOTAGENT_MEMORY_TOOL_LOOP_GLOBAL_CIRCUIT_THRESHOLD"`
	ContextPruningMode                  string   `json:"context_pruning_mode" env:"DOTAGENT_MEMORY_CONTEXT_PRUNING_MODE"`
	ContextPruningKeepLastToolResults   int      `json:"context_pruning_keep_last_tool_results" env:"DOTAGENT_MEMORY_CONTEXT_PRUNING_KEEP_LAST_TOOL_RESULTS"`
	EventRetentionDays                  int      `json:"event_retention_days" env:"DOTAGENT_MEMORY_EVENT_RETENTION_DAYS"`
	AuditRetentionDays                  int      `json:"audit_retention_days" env:"DOTAGENT_MEMORY_AUDIT_RETENTION_DAYS"`
	PersonaSyncApply                    bool     `json:"persona_sync_apply" env:"DOTAGENT_MEMORY_PERSONA_SYNC_APPLY"`
	PersonaFileSyncMode                 string   `json:"persona_file_sync_mode" env:"DOTAGENT_MEMORY_PERSONA_FILE_SYNC_MODE"`
	PersonaPolicyMode                   string   `json:"persona_policy_mode" env:"DOTAGENT_MEMORY_PERSONA_POLICY_MODE"`
	PersonaMinConfidence                float64  `json:"persona_min_confidence" env:"DOTAGENT_MEMORY_PERSONA_MIN_CONFIDENCE"`
	CompactionSummaryTimeoutSeconds     int      `json:"compaction_summary_timeout_seconds" env:"DOTAGENT_MEMORY_COMPACTION_SUMMARY_TIMEOUT_SECONDS"`
	CompactionChunkChars                int      `json:"compaction_chunk_chars" env:"DOTAGENT_MEMORY_COMPACTION_CHUNK_CHARS"`
	CompactionMaxTranscriptChars        int      `json:"compaction_max_transcript_chars" env:"DOTAGENT_MEMORY_COMPACTION_MAX_TRANSCRIPT_CHARS"`
	CompactionPartialSkipChars          int      `json:"compaction_partial_skip_chars" env:"DOTAGENT_MEMORY_COMPACTION_PARTIAL_SKIP_CHARS"`
	FileMemoryEnabled                   bool     `json:"file_memory_enabled" env:"DOTAGENT_MEMORY_FILE_MEMORY_ENABLED"`
	FileMemoryDir                       string   `json:"file_memory_dir" env:"DOTAGENT_MEMORY_FILE_MEMORY_DIR"`
	FileMemoryPollSeconds               int      `json:"file_memory_poll_seconds" env:"DOTAGENT_MEMORY_FILE_MEMORY_POLL_SECONDS"`
	FileMemoryWatchEnabled              bool     `json:"file_memory_watch_enabled" env:"DOTAGENT_MEMORY_FILE_MEMORY_WATCH_ENABLED"`
	FileMemoryWatchDebounceMS           int      `json:"file_memory_watch_debounce_ms" env:"DOTAGENT_MEMORY_FILE_MEMORY_WATCH_DEBOUNCE_MS"`
	FileMemoryMaxFileBytes              int      `json:"file_memory_max_file_bytes" env:"DOTAGENT_MEMORY_FILE_MEMORY_MAX_FILE_BYTES"`
}

func DefaultConfig() *Config {
	return &Config{
		Agents: AgentsConfig{
			Defaults: AgentDefaults{
				Workspace:                 "~/.dotagent/workspace",
				RestrictToWorkspace:       true,
				Provider:                  "openrouter",
				Model:                     "openai/gpt-5.2",
				MaxTokens:                 16384,
				Temperature:               0.7,
				MaxToolIterations:         50,
				MaxConcurrentRuns:         4,
				SessionFileLockEnabled:    true,
				SessionLockTimeoutMS:      15000,
				SessionLockStaleSeconds:   1800,
				SessionLockMaxHoldSeconds: 420,
			},
		},
		Channels: ChannelsConfig{
			Discord: DiscordConfig{
				Token:     "",
				AllowFrom: FlexibleStringSlice{},
			},
		},
		Providers: ProvidersConfig{
			OpenRouter: OpenRouterProviderConfig{
				APIBase: "https://openrouter.ai/api/v1",
			},
			OpenAI: OpenAIProviderConfig{
				APIBase: "https://api.openai.com/v1",
			},
			OpenAICodex: OpenAICodexProviderConfig{
				APIBase: "https://chatgpt.com/backend-api",
			},
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
		},
		Memory: MemoryConfig{
			MaxRecallItems:                      8,
			CandidateLimit:                      80,
			RetrievalCacheSeconds:               20,
			WorkerPollMS:                        700,
			WorkerLeaseSeconds:                  60,
			EmbeddingModel:                      "dotagent-chargram-384-v1",
			EmbeddingFallbackModels:             []string{"dotagent-chargram-384-v1", "dotagent-hash-256-v1"},
			EmbeddingOllamaAPIBase:              "http://127.0.0.1:11434",
			EmbeddingBatchSize:                  96,
			EmbeddingConcurrency:                2,
			ToolLoopDetectionEnabled:            true,
			ToolLoopWarningsEnabled:             true,
			ToolLoopSignatureWarnThreshold:      2,
			ToolLoopSignatureCriticalThreshold:  3,
			ToolLoopDriftWarnThreshold:          6,
			ToolLoopDriftCriticalThreshold:      8,
			ToolLoopPollingWarnThreshold:        4,
			ToolLoopPollingCriticalThreshold:    5,
			ToolLoopNoProgressWarnThreshold:     4,
			ToolLoopNoProgressCriticalThreshold: 6,
			ToolLoopPingPongWarnThreshold:       4,
			ToolLoopPingPongCriticalThreshold:   6,
			ToolLoopGlobalCircuitThreshold:      12,
			ContextPruningMode:                  "off",
			ContextPruningKeepLastToolResults:   5,
			EventRetentionDays:                  90,
			AuditRetentionDays:                  365,
			PersonaSyncApply:                    true,
			PersonaFileSyncMode:                 "export_only",
			PersonaPolicyMode:                   "balanced",
			PersonaMinConfidence:                0.52,
			CompactionSummaryTimeoutSeconds:     60,
			CompactionChunkChars:                9000,
			CompactionMaxTranscriptChars:        48000,
			CompactionPartialSkipChars:          2600,
			FileMemoryEnabled:                   true,
			FileMemoryDir:                       "",
			FileMemoryPollSeconds:               15,
			FileMemoryWatchEnabled:              true,
			FileMemoryWatchDebounceMS:           1200,
			FileMemoryMaxFileBytes:              262144,
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
		if !os.IsNotExist(err) {
			return nil, err
		}
	} else {
		if err := json.Unmarshal(data, cfg); err != nil {
			return nil, err
		}
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
