package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestDefaultConfig_HeartbeatEnabled verifies heartbeat is enabled by default
func TestDefaultConfig_HeartbeatEnabled(t *testing.T) {
	cfg := DefaultConfig()

	if !cfg.Heartbeat.Enabled {
		t.Error("Heartbeat should be enabled by default")
	}
}

// TestDefaultConfig_WorkspacePath verifies workspace path is correctly set
func TestDefaultConfig_WorkspacePath(t *testing.T) {
	cfg := DefaultConfig()

	// Just verify the workspace is set, don't compare exact paths
	// since expandHome behavior may differ based on environment
	if cfg.Agents.Defaults.Workspace == "" {
		t.Error("Workspace should not be empty")
	}
}

// TestDefaultConfig_Model verifies model is set
func TestDefaultConfig_Model(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.Agents.Defaults.Model == "" {
		t.Error("Model should not be empty")
	}
	if cfg.Agents.Defaults.Model != "openai/gpt-5.2" {
		t.Errorf("Model = %q, want %q", cfg.Agents.Defaults.Model, "openai/gpt-5.2")
	}
}

// TestDefaultConfig_MaxTokens verifies max tokens has default value
func TestDefaultConfig_MaxTokens(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.Agents.Defaults.MaxTokens == 0 {
		t.Error("MaxTokens should not be zero")
	}
}

// TestDefaultConfig_MaxToolIterations verifies max tool iterations has default value
func TestDefaultConfig_MaxToolIterations(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.Agents.Defaults.MaxToolIterations == 0 {
		t.Error("MaxToolIterations should not be zero")
	}
}

// TestDefaultConfig_Temperature verifies temperature has default value
func TestDefaultConfig_Temperature(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.Agents.Defaults.Temperature == 0 {
		t.Error("Temperature should not be zero")
	}
}

// TestDefaultConfig_Gateway verifies gateway defaults
func TestDefaultConfig_Gateway(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.Gateway.Host != "0.0.0.0" {
		t.Error("Gateway host should have default value")
	}
	if cfg.Gateway.Port == 0 {
		t.Error("Gateway port should have default value")
	}
}

// TestDefaultConfig_Providers verifies provider structure
func TestDefaultConfig_Providers(t *testing.T) {
	cfg := DefaultConfig()

	// Verify provider credentials are empty by default.
	if cfg.Providers.OpenRouter.APIKey != "" {
		t.Error("OpenRouter API key should be empty by default")
	}
	if cfg.Providers.OpenAI.APIKey != "" {
		t.Error("OpenAI API key should be empty by default")
	}
	if cfg.Providers.OpenAI.OAuthAccessToken != "" {
		t.Error("OpenAI OAuth access token should be empty by default")
	}
	if cfg.Providers.OpenAICodex.OAuthAccessToken != "" {
		t.Error("OpenAI Codex OAuth access token should be empty by default")
	}
	if cfg.Providers.OpenAICodex.OAuthTokenFile != "" {
		t.Error("OpenAI Codex OAuth token file should be empty by default")
	}
}

// TestDefaultConfig_Channels verifies Discord config defaults
func TestDefaultConfig_Channels(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.Channels.Discord.Token != "" {
		t.Error("Discord token should be empty by default")
	}
}

// TestDefaultConfig_WebTools verifies web tools config
func TestDefaultConfig_WebTools(t *testing.T) {
	cfg := DefaultConfig()

	// Verify web tools defaults
	if cfg.Tools.Web.Brave.MaxResults != 5 {
		t.Error("Expected Brave MaxResults 5, got ", cfg.Tools.Web.Brave.MaxResults)
	}
	if cfg.Tools.Web.Brave.APIKey != "" {
		t.Error("Brave API key should be empty by default")
	}
	if cfg.Tools.Web.DuckDuckGo.MaxResults != 5 {
		t.Error("Expected DuckDuckGo MaxResults 5, got ", cfg.Tools.Web.DuckDuckGo.MaxResults)
	}
}

func TestSaveConfig_FilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file permission bits are not enforced on Windows")
	}

	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "config.json")

	cfg := DefaultConfig()
	if err := SaveConfig(path, cfg); err != nil {
		t.Fatalf("SaveConfig failed: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}

	perm := info.Mode().Perm()
	if perm != 0600 {
		t.Errorf("config file has permission %04o, want 0600", perm)
	}
}

// TestConfig_Complete verifies all config fields are set
func TestConfig_Complete(t *testing.T) {
	cfg := DefaultConfig()

	// Verify complete config structure
	if cfg.Agents.Defaults.Workspace == "" {
		t.Error("Workspace should not be empty")
	}
	if cfg.Agents.Defaults.Model == "" {
		t.Error("Model should not be empty")
	}
	if cfg.Agents.Defaults.Temperature == 0 {
		t.Error("Temperature should have default value")
	}
	if cfg.Agents.Defaults.MaxTokens == 0 {
		t.Error("MaxTokens should not be zero")
	}
	if cfg.Agents.Defaults.MaxToolIterations == 0 {
		t.Error("MaxToolIterations should not be zero")
	}
	if cfg.Gateway.Host != "0.0.0.0" {
		t.Error("Gateway host should have default value")
	}
	if cfg.Gateway.Port == 0 {
		t.Error("Gateway port should have default value")
	}
	if !cfg.Heartbeat.Enabled {
		t.Error("Heartbeat should be enabled by default")
	}
}

func TestLoadConfig_EnvOverridesWithoutFile(t *testing.T) {
	t.Setenv("DOTAGENT_AGENTS_DEFAULTS_MODEL", "env/model")
	path := filepath.Join(t.TempDir(), "missing-config.json")

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if got := cfg.Agents.Defaults.Model; got != "env/model" {
		t.Fatalf("expected env override model, got %q", got)
	}
}

func TestLoadConfig_OpenAIEnvOverrides(t *testing.T) {
	t.Setenv("DOTAGENT_AGENTS_DEFAULTS_PROVIDER", "openai")
	t.Setenv("DOTAGENT_PROVIDERS_OPENAI_API_KEY", "sk-openai")
	t.Setenv("DOTAGENT_PROVIDERS_OPENAI_PROJECT", "proj_test")
	path := filepath.Join(t.TempDir(), "missing-config.json")

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if got := cfg.Agents.Defaults.Provider; got != "openai" {
		t.Fatalf("expected provider openai, got %q", got)
	}
	if got := cfg.Providers.OpenAI.APIKey; got != "sk-openai" {
		t.Fatalf("expected openai api key from env, got %q", got)
	}
	if got := cfg.Providers.OpenAI.Project; got != "proj_test" {
		t.Fatalf("expected openai project from env, got %q", got)
	}
}

func TestLoadConfig_OpenAICodexEnvOverrides(t *testing.T) {
	t.Setenv("DOTAGENT_AGENTS_DEFAULTS_PROVIDER", "openai-codex")
	t.Setenv("DOTAGENT_PROVIDERS_OPENAI_CODEX_OAUTH_TOKEN_FILE", "/tmp/codex-auth.json")
	path := filepath.Join(t.TempDir(), "missing-config.json")

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if got := cfg.Agents.Defaults.Provider; got != "openai-codex" {
		t.Fatalf("expected provider openai-codex, got %q", got)
	}
	if got := cfg.Providers.OpenAICodex.OAuthTokenFile; got != "/tmp/codex-auth.json" {
		t.Fatalf("expected openai-codex oauth token file from env, got %q", got)
	}
}
