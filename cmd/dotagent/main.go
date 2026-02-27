// DotAgent - Ultra-lightweight personal AI agent
// Inspired by and based on nanobot: https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 DotAgent contributors

package main

import (
	"bufio"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/chzyer/readline"
	"github.com/dotsetgreg/dotagent/pkg/agent"
	"github.com/dotsetgreg/dotagent/pkg/bus"
	"github.com/dotsetgreg/dotagent/pkg/channels"
	"github.com/dotsetgreg/dotagent/pkg/config"
	"github.com/dotsetgreg/dotagent/pkg/cron"
	"github.com/dotsetgreg/dotagent/pkg/health"
	"github.com/dotsetgreg/dotagent/pkg/heartbeat"
	"github.com/dotsetgreg/dotagent/pkg/logger"
	"github.com/dotsetgreg/dotagent/pkg/providers"
	"github.com/dotsetgreg/dotagent/pkg/skills"
	"github.com/dotsetgreg/dotagent/pkg/toolpacks"
	"github.com/dotsetgreg/dotagent/pkg/tools"
)

//go:generate cp -r ../../workspace .
//go:embed workspace
var embeddedFiles embed.FS

var (
	version   = "dev"
	gitCommit string
	buildTime string
	goVersion string
)

const appName = "dotagent"

// formatVersion returns the version string with optional git commit
func formatVersion() string {
	v := version
	if gitCommit != "" {
		v += fmt.Sprintf(" (git: %s)", gitCommit)
	}
	return v
}

// formatBuildInfo returns build time and go version info
func formatBuildInfo() (build string, goVer string) {
	if buildTime != "" {
		build = buildTime
	}
	goVer = goVersion
	if goVer == "" {
		goVer = runtime.Version()
	}
	return
}

func printVersion() {
	fmt.Printf("%s %s\n", appName, formatVersion())
	build, goVer := formatBuildInfo()
	if build != "" {
		fmt.Printf("  Build: %s\n", build)
	}
	if goVer != "" {
		fmt.Printf("  Go: %s\n", goVer)
	}
}

func main() {
	if err := executeCLI(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func printHelp() {
	fmt.Printf("%s - Personal AI Assistant v%s\n\n", appName, version)
	fmt.Println("Usage: dotagent <command>")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  onboard     Initialize dotagent configuration and workspace")
	fmt.Println("  agent       Interact with the agent directly")
	fmt.Println("  gateway     Start dotagent gateway")
	fmt.Println("  status      Show dotagent status")
	fmt.Println("  cron        Manage scheduled tasks")
	fmt.Println("  skills      Manage skills (install, list, remove)")
	fmt.Println("  toolpacks   Manage executable tool packs")
	fmt.Println("  version     Show version information")
}

func onboard() {
	configPath := getConfigPath()

	if _, err := os.Stat(configPath); err == nil {
		fmt.Printf("Config already exists at %s\n", configPath)
		fmt.Print("Overwrite? (y/n): ")
		reader := bufio.NewReader(os.Stdin)
		response, readErr := reader.ReadString('\n')
		if readErr != nil {
			fmt.Printf("Error reading input: %v\n", readErr)
			fmt.Println("Aborted.")
			return
		}
		response = strings.ToLower(strings.TrimSpace(response))
		if response != "y" && response != "yes" {
			fmt.Println("Aborted.")
			return
		}
	}

	instanceID := resolveInstanceID(os.Getenv("DOTAGENT_INSTANCE"))
	cfg := config.DefaultConfigForInstance(instanceID)
	if err := config.SaveConfig(configPath, cfg); err != nil {
		fmt.Printf("Error saving config: %v\n", err)
		os.Exit(1)
	}

	workspace := cfg.WorkspacePath()
	if err := createWorkspaceTemplates(workspace); err != nil {
		fmt.Printf("Error creating workspace templates: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("%s is ready!\n", appName)
	fmt.Println("\nNext steps:")
	fmt.Println("  1. Configure your LLM provider credentials in", configPath)
	fmt.Println("     - OpenRouter: providers.openrouter.api_key (https://openrouter.ai/keys)")
	fmt.Println("     - OpenAI API: set agents.defaults.provider=openai and configure exactly one auth source:")
	fmt.Println("       providers.openai.api_key OR providers.openai.oauth_access_token OR providers.openai.oauth_token_file")
	fmt.Println("     - OpenAI Codex OAuth: set agents.defaults.provider=openai-codex and configure exactly one auth source:")
	fmt.Println("       providers.openai_codex.oauth_access_token OR providers.openai_codex.oauth_token_file")
	fmt.Println("     - Ollama (local): set agents.defaults.provider=ollama, agents.defaults.model=<local-model>, providers.ollama.api_base=http://127.0.0.1:11434/v1")
	fmt.Println("  2. (Gateway mode) Add your Discord bot token to channels.discord.token")
	fmt.Println("  3. Chat locally: dotagent agent -m \"Hello!\"")
	fmt.Println("  4. Run gateway: dotagent gateway")
	fmt.Println("  5. Check readiness: dotagent status")
}

func copyEmbeddedToTarget(targetDir string) error {
	// Ensure target directory exists
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return fmt.Errorf("Failed to create target directory: %w", err)
	}

	// Walk through all files in embed.FS
	err := fs.WalkDir(embeddedFiles, "workspace", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Skip directories
		if d.IsDir() {
			return nil
		}

		// Read embedded file
		data, err := embeddedFiles.ReadFile(path)
		if err != nil {
			return fmt.Errorf("Failed to read embedded file %s: %w", path, err)
		}

		new_path, err := filepath.Rel("workspace", path)
		if err != nil {
			return fmt.Errorf("Failed to get relative path for %s: %v\n", path, err)
		}

		// Build target file path
		targetPath := filepath.Join(targetDir, new_path)

		// Ensure target file's directory exists
		if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
			return fmt.Errorf("Failed to create directory %s: %w", filepath.Dir(targetPath), err)
		}

		// Write file
		if err := os.WriteFile(targetPath, data, 0644); err != nil {
			return fmt.Errorf("Failed to write file %s: %w", targetPath, err)
		}

		return nil
	})

	return err
}

func createWorkspaceTemplates(workspace string) error {
	return copyEmbeddedToTarget(workspace)
}

func validateRuntimeConfig(cfg *config.Config, requireDiscord bool) error {
	if err := providers.ValidateProviderConfig(cfg); err != nil {
		return fmt.Errorf("provider configuration error: %w", err)
	}
	if requireDiscord && strings.TrimSpace(cfg.Channels.Discord.Token) == "" {
		configPath := getConfigPath()
		return fmt.Errorf("channels.discord.token is required in %s or DOTAGENT_CHANNELS_DISCORD_TOKEN", configPath)
	}
	return nil
}

func agentCmd() {
	message := ""
	sessionKey := "cli:default"

	args := os.Args[2:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--debug", "-d":
			logger.SetLevel(logger.DEBUG)
			fmt.Println("🔍 Debug mode enabled")
		case "-m", "--message":
			if i+1 < len(args) {
				message = args[i+1]
				i++
			}
		case "-s", "--session":
			if i+1 < len(args) {
				sessionKey = args[i+1]
				i++
			}
		}
	}

	cfg, err := loadConfig()
	if err != nil {
		fmt.Printf("Error loading config: %v\n", err)
		os.Exit(1)
	}
	if err := validateRuntimeConfig(cfg, false); err != nil {
		fmt.Printf("Configuration error: %v\n", err)
		os.Exit(1)
	}

	provider, err := providers.CreateProvider(cfg)
	if err != nil {
		fmt.Printf("Error creating provider: %v\n", err)
		os.Exit(1)
	}

	msgBus := bus.NewMessageBus()
	agentLoop, err := agent.NewAgentLoop(cfg, msgBus, provider)
	if err != nil {
		fmt.Printf("Error initializing memory subsystem: %v\n", err)
		os.Exit(1)
	}

	// Print agent startup info (only for interactive mode)
	startupInfo := agentLoop.GetStartupInfo()
	logger.InfoCF("agent", "Agent initialized",
		map[string]interface{}{
			"tools_count":      startupInfo["tools"].(map[string]interface{})["count"],
			"skills_total":     startupInfo["skills"].(map[string]interface{})["total"],
			"skills_available": startupInfo["skills"].(map[string]interface{})["available"],
		})

	if message != "" {
		ctx := context.Background()
		response, err := agentLoop.ProcessDirect(ctx, message, sessionKey)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("\n%s %s\n", appName, response)
	} else {
		fmt.Printf("%s Interactive mode (Ctrl+C to exit)\n\n", appName)
		interactiveMode(agentLoop, sessionKey)
	}
}

func interactiveMode(agentLoop *agent.AgentLoop, sessionKey string) {
	prompt := fmt.Sprintf("%s You: ", appName)

	rl, err := readline.NewEx(&readline.Config{
		Prompt:          prompt,
		HistoryFile:     filepath.Join(os.TempDir(), ".dotagent_history"),
		HistoryLimit:    100,
		InterruptPrompt: "^C",
		EOFPrompt:       "exit",
	})

	if err != nil {
		fmt.Printf("Error initializing readline: %v\n", err)
		fmt.Println("Falling back to simple input mode...")
		simpleInteractiveMode(agentLoop, sessionKey)
		return
	}
	defer rl.Close()

	for {
		line, err := rl.Readline()
		if err != nil {
			if err == readline.ErrInterrupt || err == io.EOF {
				fmt.Println("\nGoodbye!")
				return
			}
			fmt.Printf("Error reading input: %v\n", err)
			continue
		}

		input := strings.TrimSpace(line)
		if input == "" {
			continue
		}

		if input == "exit" || input == "quit" {
			fmt.Println("Goodbye!")
			return
		}

		ctx := context.Background()
		response, err := agentLoop.ProcessDirect(ctx, input, sessionKey)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			continue
		}

		fmt.Printf("\n%s %s\n\n", appName, response)
	}
}

func simpleInteractiveMode(agentLoop *agent.AgentLoop, sessionKey string) {
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print(fmt.Sprintf("%s You: ", appName))
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				fmt.Println("\nGoodbye!")
				return
			}
			fmt.Printf("Error reading input: %v\n", err)
			continue
		}

		input := strings.TrimSpace(line)
		if input == "" {
			continue
		}

		if input == "exit" || input == "quit" {
			fmt.Println("Goodbye!")
			return
		}

		ctx := context.Background()
		response, err := agentLoop.ProcessDirect(ctx, input, sessionKey)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			continue
		}

		fmt.Printf("\n%s %s\n\n", appName, response)
	}
}

func gatewayCmd() {
	// Check for --debug flag
	args := os.Args[2:]
	for _, arg := range args {
		if arg == "--debug" || arg == "-d" {
			logger.SetLevel(logger.DEBUG)
			fmt.Println("🔍 Debug mode enabled")
			break
		}
	}

	instanceID := resolveInstanceID(os.Getenv("DOTAGENT_INSTANCE"))
	configPath := getConfigPath()

	cfg, err := loadConfig()
	if err != nil {
		recovered, recoverErr := maybeRollbackPendingConfigOnLoadFailure(instanceID, configPath, err)
		if recoverErr != nil {
			fmt.Printf("Error loading config: %v\n", err)
			fmt.Printf("Error rolling back pending config apply: %v\n", recoverErr)
			os.Exit(1)
		}
		if recovered {
			cfg, err = loadConfig()
		}
	}
	if err != nil {
		fmt.Printf("Error loading config: %v\n", err)
		os.Exit(1)
	}
	if err := validateRuntimeConfig(cfg, true); err != nil {
		fmt.Printf("Configuration error: %v\n", err)
		os.Exit(1)
	}

	provider, err := providers.CreateProvider(cfg)
	if err != nil {
		fmt.Printf("Error creating provider: %v\n", err)
		os.Exit(1)
	}

	msgBus := bus.NewMessageBus()
	agentLoop, err := agent.NewAgentLoop(cfg, msgBus, provider)
	if err != nil {
		fmt.Printf("Error initializing memory subsystem: %v\n", err)
		os.Exit(1)
	}

	// Print agent startup info
	fmt.Println("\n📦 Agent Status:")
	startupInfo := agentLoop.GetStartupInfo()
	toolsInfo := startupInfo["tools"].(map[string]interface{})
	skillsInfo := startupInfo["skills"].(map[string]interface{})
	fmt.Printf("  • Tools: %d loaded\n", toolsInfo["count"])
	fmt.Printf("  • Skills: %d/%d available\n",
		skillsInfo["available"],
		skillsInfo["total"])

	// Log to file as well
	logger.InfoCF("agent", "Agent initialized",
		map[string]interface{}{
			"tools_count":      toolsInfo["count"],
			"skills_total":     skillsInfo["total"],
			"skills_available": skillsInfo["available"],
		})

	// Setup cron tool and service
	cronService, err := setupCronTool(agentLoop, msgBus, cfg.DataPath(), cfg.WorkspacePath(), cfg.Agents.Defaults.RestrictToWorkspace)
	if err != nil {
		fmt.Printf("Failed to setup cron tool: %v\n", err)
		os.Exit(1)
	}

	heartbeatService := heartbeat.NewHeartbeatService(
		cfg.WorkspacePath(),
		cfg.DataPath(),
		cfg.LogsPath(),
		cfg.Heartbeat.Interval,
		cfg.Heartbeat.Enabled,
	)
	heartbeatService.SetBus(msgBus)
	heartbeatService.SetHandler(func(prompt, channel, chatID string) *tools.ToolResult {
		// Use cli:direct as fallback if no valid channel
		if channel == "" || chatID == "" {
			channel, chatID = "cli", "direct"
		}
		// Use ProcessHeartbeat - no session history, each heartbeat is independent
		response, err := agentLoop.ProcessHeartbeat(context.Background(), prompt, channel, chatID)
		if err != nil {
			return tools.ErrorResult(fmt.Sprintf("Heartbeat error: %v", err))
		}
		if response == "HEARTBEAT_OK" {
			return tools.SilentResult("Heartbeat OK")
		}
		// For heartbeat, always return silent - the subagent result will be
		// sent to user via processSystemMessage when the async task completes
		return tools.SilentResult(response)
	})

	channelManager, err := channels.NewManager(cfg, msgBus)
	if err != nil {
		fmt.Printf("Error creating channel manager: %v\n", err)
		os.Exit(1)
	}

	// Inject channel manager into agent loop for command handling
	agentLoop.SetChannelManager(channelManager)

	enabledChannels := channelManager.GetEnabledChannels()
	fmt.Printf("✓ Channels enabled: %s\n", strings.Join(enabledChannels, ", "))

	fmt.Printf("✓ Gateway started on %s:%d\n", cfg.Gateway.Host, cfg.Gateway.Port)
	fmt.Println("Press Ctrl+C to stop")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := cronService.Start(); err != nil {
		fmt.Printf("Error starting cron service: %v\n", err)
	}
	fmt.Println("✓ Cron service started")

	if err := heartbeatService.Start(); err != nil {
		fmt.Printf("Error starting heartbeat service: %v\n", err)
	}
	fmt.Println("✓ Heartbeat service started")

	if err := channelManager.StartAll(ctx); err != nil {
		fmt.Printf("Error starting channels: %v\n", err)
		cancel()
		heartbeatService.Stop()
		cronService.Stop()
		agentLoop.Stop()
		os.Exit(1)
	}

	healthServer := health.NewServer(cfg.Gateway.Host, cfg.Gateway.Port)
	refreshHealthChecks := func() {
		registerGatewayHealthChecks(healthServer, cfg, cronService, heartbeatService, channelManager)
	}
	refreshHealthChecks()
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refreshHealthChecks()
			}
		}
	}()
	go func() {
		if err := healthServer.Start(); err != nil && err != http.ErrServerClosed {
			logger.ErrorCF("health", "Health server error", map[string]interface{}{"error": err.Error()})
		}
	}()
	fmt.Printf("✓ Health endpoints available at http://%s:%d/health and /ready\n", cfg.Gateway.Host, cfg.Gateway.Port)

	if err := finalizePendingConfigApply(instanceID, configPath); err != nil {
		fmt.Printf("Pending config apply validation failed: %v\n", err)
		cancel()
		healthServer.Stop(context.Background())
		heartbeatService.Stop()
		cronService.Stop()
		agentLoop.Stop()
		channelManager.StopAll(ctx)
		os.Exit(1)
	}

	go agentLoop.Run(ctx)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	<-sigChan

	fmt.Println("\nShutting down...")
	cancel()
	healthServer.Stop(context.Background())
	heartbeatService.Stop()
	cronService.Stop()
	agentLoop.Stop()
	channelManager.StopAll(ctx)
	fmt.Println("✓ Gateway stopped")
}

func maybeRollbackPendingConfigOnLoadFailure(instanceID, configPath string, loadErr error) (bool, error) {
	pendingPath := configRestartPendingPath(instanceID)
	pending, err := tools.LoadConfigRestartPending(pendingPath)
	if err != nil {
		return false, err
	}
	if pending == nil {
		return false, nil
	}
	if err := tools.RollbackConfigFromBackup(configPath, pending.BackupPath); err != nil {
		return false, err
	}
	_, auditPath := configAdminPaths(instanceID)
	now := time.Now().UnixMilli()
	_ = tools.AppendConfigAuditLog(auditPath, map[string]any{
		"timestamp":   now,
		"action":      "post_restart_rollback",
		"request_id":  pending.RequestID,
		"key":         pending.Key,
		"backup_path": pending.BackupPath,
		"reason":      fmt.Sprintf("config load failed: %v", loadErr),
	})
	_ = tools.RemoveConfigRestartPending(pendingPath)
	return true, nil
}

func finalizePendingConfigApply(instanceID, configPath string) error {
	pendingPath := configRestartPendingPath(instanceID)
	pending, err := tools.LoadConfigRestartPending(pendingPath)
	if err != nil {
		return err
	}
	if pending == nil {
		return nil
	}

	_, auditPath := configAdminPaths(instanceID)
	checkTimeout := 10 * time.Second
	if pending.CheckTimeoutMS > 0 {
		checkTimeout = time.Duration(pending.CheckTimeoutMS) * time.Millisecond
	}
	checkDeadline := time.Now().Add(checkTimeout)
	report := runServeCheck(instanceID)
	for !report.Ready && time.Now().Before(checkDeadline) {
		time.Sleep(1 * time.Second)
		report = runServeCheck(instanceID)
	}
	now := time.Now().UnixMilli()
	deadlineExpired := pending.DeadlineAt > 0 && now > pending.DeadlineAt
	if deadlineExpired || !report.Ready {
		reason := failedChecksSummary(report)
		if deadlineExpired {
			if strings.TrimSpace(reason) == "" {
				reason = "post-restart readiness deadline exceeded"
			} else {
				reason = reason + "; post-restart readiness deadline exceeded"
			}
		}
		rollbackErr := tools.RollbackConfigFromBackup(configPath, pending.BackupPath)
		if rollbackErr != nil {
			_ = tools.AppendConfigAuditLog(auditPath, map[string]any{
				"timestamp":      now,
				"action":         "post_restart_rollback_failed",
				"request_id":     pending.RequestID,
				"key":            pending.Key,
				"backup_path":    pending.BackupPath,
				"reason":         reason,
				"rollback_error": rollbackErr.Error(),
			})
			return fmt.Errorf("rollback from %s failed: %w", pending.BackupPath, rollbackErr)
		}
		_ = tools.AppendConfigAuditLog(auditPath, map[string]any{
			"timestamp":   now,
			"action":      "post_restart_rollback",
			"request_id":  pending.RequestID,
			"key":         pending.Key,
			"backup_path": pending.BackupPath,
			"reason":      reason,
		})
		_ = tools.RemoveConfigRestartPending(pendingPath)
		return fmt.Errorf("post-restart readiness failed: %s", reason)
	}

	_ = tools.AppendConfigAuditLog(auditPath, map[string]any{
		"timestamp":   now,
		"action":      "post_restart_verified",
		"request_id":  pending.RequestID,
		"key":         pending.Key,
		"backup_path": pending.BackupPath,
	})
	_ = tools.RemoveConfigRestartPending(pendingPath)
	return nil
}

func failedChecksSummary(report doctorReport) string {
	failures := make([]string, 0, len(report.Checks))
	for _, check := range report.Checks {
		if check.OK {
			continue
		}
		detail := strings.TrimSpace(check.Detail)
		if detail == "" {
			failures = append(failures, check.Name)
			continue
		}
		failures = append(failures, fmt.Sprintf("%s: %s", check.Name, detail))
	}
	if len(failures) == 0 {
		return ""
	}
	return strings.Join(failures, "; ")
}

func registerGatewayHealthChecks(healthServer *health.Server, cfg *config.Config, cronService *cron.CronService, heartbeatService *heartbeat.HeartbeatService, channelManager *channels.Manager) {
	healthServer.RegisterCheck("provider_config", func() (bool, string) {
		if err := providers.ValidateProviderConfig(cfg); err != nil {
			return false, err.Error()
		}
		return true, providers.ActiveProviderName(cfg)
	})
	healthServer.RegisterCheck("discord_token", func() (bool, string) {
		if strings.TrimSpace(cfg.Channels.Discord.Token) == "" {
			return false, "channels.discord.token is empty"
		}
		return true, "configured"
	})
	healthServer.RegisterCheck("memory_db", func() (bool, string) {
		path := filepath.Join(cfg.DataPath(), "state", "memory.db")
		if err := ensureFileAccessible(path); err != nil {
			return false, err.Error()
		}
		return true, path
	})
	healthServer.RegisterCheck("cron_service", func() (bool, string) {
		status := cronService.Status()
		running, _ := status["enabled"].(bool)
		if !running {
			return false, "scheduler not running"
		}
		return true, "running"
	})
	healthServer.RegisterCheck("heartbeat_service", func() (bool, string) {
		if !cfg.Heartbeat.Enabled {
			return true, "disabled"
		}
		if heartbeatService.IsRunning() {
			return true, "running"
		}
		return false, "not running"
	})
	healthServer.RegisterCheck("channels_running", func() (bool, string) {
		statuses := channelManager.GetStatus()
		if len(statuses) == 0 {
			return false, "no channels enabled"
		}
		failures := make([]string, 0)
		for name, raw := range statuses {
			info, ok := raw.(map[string]interface{})
			if !ok {
				failures = append(failures, name)
				continue
			}
			running, _ := info["running"].(bool)
			if !running {
				failures = append(failures, name)
			}
		}
		if len(failures) > 0 {
			return false, "not running: " + strings.Join(failures, ", ")
		}
		return true, fmt.Sprintf("%d channel(s) running", len(statuses))
	})
}

func statusCmd() {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Printf("Error loading config: %v\n", err)
		return
	}

	configPath := getConfigPath()

	fmt.Printf("%s Status\n", appName)
	fmt.Printf("Version: %s\n", formatVersion())
	build, _ := formatBuildInfo()
	if build != "" {
		fmt.Printf("Build: %s\n", build)
	}
	fmt.Println()

	if _, err := os.Stat(configPath); err == nil {
		fmt.Println("Config:", configPath, "✓")
	} else {
		fmt.Println("Config:", configPath, "✗")
	}

	workspace := cfg.WorkspacePath()
	if _, err := os.Stat(workspace); err == nil {
		fmt.Println("Workspace:", workspace, "✓")
	} else {
		fmt.Println("Workspace:", workspace, "✗")
	}
	memoryDB := filepath.Join(cfg.DataPath(), "state", "memory.db")
	if _, err := os.Stat(memoryDB); err == nil {
		fmt.Println("Memory DB:", memoryDB, "✓")
	} else {
		fmt.Println("Memory DB:", memoryDB, "not initialized")
	}

	if _, err := os.Stat(configPath); err == nil {
		selectedProvider := providers.ActiveProviderName(cfg)
		fmt.Printf("Model: %s\n", cfg.Agents.Defaults.Model)
		fmt.Printf("Provider: %s\n", selectedProvider)

		status := func(enabled bool) string {
			if enabled {
				return "✓"
			}
			return "not set"
		}
		_, apiReady, authMode, providerErr := providers.ProviderCredentialStatus(cfg)
		if providerErr == nil {
			if err := providers.ValidateProviderConfig(cfg); err != nil {
				providerErr = err
				apiReady = false
			}
		} else {
			apiReady = false
		}
		discordReady := strings.TrimSpace(cfg.Channels.Discord.Token) != ""

		providerLine := "Provider credentials: " + status(apiReady)
		if authMode != "" {
			providerLine += fmt.Sprintf(" (%s)", authMode)
		}
		if providerErr != nil {
			providerLine += fmt.Sprintf(" (%v)", providerErr)
		}
		fmt.Println(providerLine)
		fmt.Println("Discord token:", status(discordReady))
		fmt.Println("Agent ready:", status(apiReady))
		fmt.Println("Gateway ready:", status(apiReady && discordReady))
	}
}

func getConfigPath() string {
	if explicit := strings.TrimSpace(os.Getenv("DOTAGENT_CONFIG")); explicit != "" {
		return explicit
	}
	instanceID := resolveInstanceID(strings.TrimSpace(os.Getenv("DOTAGENT_INSTANCE")))
	return instanceConfigPath(instanceID)
}

func setupCronTool(agentLoop *agent.AgentLoop, msgBus *bus.MessageBus, storeRoot string, workspace string, restrict bool) (*cron.CronService, error) {
	cronStorePath := filepath.Join(storeRoot, "cron", "jobs.json")

	// Create cron service
	cronService, err := cron.NewCronService(cronStorePath, nil)
	if err != nil {
		return nil, err
	}

	// Create and register CronTool
	cronTool := tools.NewCronTool(cronService, agentLoop, msgBus, workspace, restrict)
	agentLoop.RegisterTool(cronTool)

	// Set the onJob handler
	cronService.SetOnJob(func(job *cron.CronJob) (string, error) {
		result := cronTool.ExecuteJob(context.Background(), job)
		return result, nil
	})

	return cronService, nil
}

func loadConfig() (*config.Config, error) {
	return config.LoadConfig(getConfigPath())
}

func cronCmd() {
	if len(os.Args) < 3 {
		cronHelp()
		return
	}

	subcommand := os.Args[2]

	// Load config to get workspace path
	cfg, err := loadConfig()
	if err != nil {
		fmt.Printf("Error loading config: %v\n", err)
		return
	}

	cronStorePath := filepath.Join(cfg.DataPath(), "cron", "jobs.json")

	switch subcommand {
	case "list":
		cronListCmd(cronStorePath)
	case "add":
		cronAddCmd(cronStorePath)
	case "remove":
		if len(os.Args) < 4 {
			fmt.Println("Usage: dotagent cron remove <job_id>")
			return
		}
		cronRemoveCmd(cronStorePath, os.Args[3])
	case "enable":
		cronEnableCmd(cronStorePath, false)
	case "disable":
		cronEnableCmd(cronStorePath, true)
	default:
		fmt.Printf("Unknown cron command: %s\n", subcommand)
		cronHelp()
	}
}

func cronHelp() {
	fmt.Println("\nCron commands:")
	fmt.Println("  list              List all scheduled jobs")
	fmt.Println("  add              Add a new scheduled job")
	fmt.Println("  remove <id>       Remove a job by ID")
	fmt.Println("  enable <id>      Enable a job")
	fmt.Println("  disable <id>     Disable a job")
	fmt.Println()
	fmt.Println("Add options:")
	fmt.Println("  -n, --name       Job name")
	fmt.Println("  -m, --message    Message for agent")
	fmt.Println("  -e, --every      Run every N seconds")
	fmt.Println("  -c, --cron       Cron expression (e.g. '0 9 * * *')")
	fmt.Println("  -d, --deliver     Deliver response to channel")
	fmt.Println("  --to             Recipient for delivery")
	fmt.Println("  --channel        Channel for delivery")
}

func cronListCmd(storePath string) {
	cs, err := cron.NewCronService(storePath, nil)
	if err != nil {
		fmt.Printf("Error loading cron store: %v\n", err)
		return
	}
	jobs := cs.ListJobs(true) // Show all jobs, including disabled

	if len(jobs) == 0 {
		fmt.Println("No scheduled jobs.")
		return
	}

	fmt.Println("\nScheduled Jobs:")
	fmt.Println("----------------")
	for _, job := range jobs {
		var schedule string
		if job.Schedule.Kind == "every" && job.Schedule.EveryMS != nil {
			schedule = fmt.Sprintf("every %ds", *job.Schedule.EveryMS/1000)
		} else if job.Schedule.Kind == "cron" {
			schedule = job.Schedule.Expr
		} else {
			schedule = "one-time"
		}

		nextRun := "scheduled"
		if job.State.NextRunAtMS != nil {
			nextTime := time.UnixMilli(*job.State.NextRunAtMS)
			nextRun = nextTime.Format("2006-01-02 15:04")
		}

		status := "enabled"
		if !job.Enabled {
			status = "disabled"
		}

		fmt.Printf("  %s (%s)\n", job.Name, job.ID)
		fmt.Printf("    Schedule: %s\n", schedule)
		fmt.Printf("    Status: %s\n", status)
		fmt.Printf("    Next run: %s\n", nextRun)
	}
}

func cronAddCmd(storePath string) {
	name := ""
	message := ""
	var everySec *int64
	cronExpr := ""
	deliver := false
	channel := ""
	to := ""

	args := os.Args[3:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-n", "--name":
			if i+1 < len(args) {
				name = args[i+1]
				i++
			}
		case "-m", "--message":
			if i+1 < len(args) {
				message = args[i+1]
				i++
			}
		case "-e", "--every":
			if i+1 < len(args) {
				var sec int64
				fmt.Sscanf(args[i+1], "%d", &sec)
				everySec = &sec
				i++
			}
		case "-c", "--cron":
			if i+1 < len(args) {
				cronExpr = args[i+1]
				i++
			}
		case "-d", "--deliver":
			deliver = true
		case "--to":
			if i+1 < len(args) {
				to = args[i+1]
				i++
			}
		case "--channel":
			if i+1 < len(args) {
				channel = args[i+1]
				i++
			}
		}
	}

	if name == "" {
		fmt.Println("Error: --name is required")
		return
	}

	if message == "" {
		fmt.Println("Error: --message is required")
		return
	}

	if everySec == nil && cronExpr == "" {
		fmt.Println("Error: Either --every or --cron must be specified")
		return
	}

	var schedule cron.CronSchedule
	if everySec != nil {
		everyMS := *everySec * 1000
		schedule = cron.CronSchedule{
			Kind:    "every",
			EveryMS: &everyMS,
		}
	} else {
		schedule = cron.CronSchedule{
			Kind: "cron",
			Expr: cronExpr,
		}
	}

	cs, err := cron.NewCronService(storePath, nil)
	if err != nil {
		fmt.Printf("Error loading cron store: %v\n", err)
		return
	}
	job, err := cs.AddJob(name, schedule, message, deliver, channel, to)
	if err != nil {
		fmt.Printf("Error adding job: %v\n", err)
		return
	}

	fmt.Printf("✓ Added job '%s' (%s)\n", job.Name, job.ID)
}

func cronRemoveCmd(storePath, jobID string) {
	cs, err := cron.NewCronService(storePath, nil)
	if err != nil {
		fmt.Printf("Error loading cron store: %v\n", err)
		return
	}
	if cs.RemoveJob(jobID) {
		fmt.Printf("✓ Removed job %s\n", jobID)
	} else {
		fmt.Printf("✗ Job %s not found\n", jobID)
	}
}

func cronEnableCmd(storePath string, disable bool) {
	if len(os.Args) < 4 {
		fmt.Println("Usage: dotagent cron enable/disable <job_id>")
		return
	}

	jobID := os.Args[3]
	cs, err := cron.NewCronService(storePath, nil)
	if err != nil {
		fmt.Printf("Error loading cron store: %v\n", err)
		return
	}
	enabled := !disable

	job := cs.EnableJob(jobID, enabled)
	if job != nil {
		if enabled && !job.Enabled {
			if strings.TrimSpace(job.State.LastError) != "" {
				fmt.Printf("✗ Job '%s' cannot be enabled: %s\n", job.Name, job.State.LastError)
				return
			}
			fmt.Printf("✗ Job '%s' cannot be enabled because no future run can be scheduled\n", job.Name)
			return
		}
		status := "enabled"
		if disable {
			status = "disabled"
		}
		fmt.Printf("✓ Job '%s' %s\n", job.Name, status)
	} else {
		fmt.Printf("✗ Job %s not found\n", jobID)
	}
}

func skillsHelp() {
	fmt.Println("\nSkills commands:")
	fmt.Println("  list            List installed skills")
	fmt.Println("  install <repo>  Install skill from GitHub")
	fmt.Println("  remove <name>   Remove installed skill")
	fmt.Println("  search          Search available skills")
	fmt.Println("  show <name>     Show skill details")
	fmt.Println()
	fmt.Println("Examples:")
	fmt.Println("  dotagent skills list")
	fmt.Println("  dotagent skills install dotsetgreg/dotagent-skills/weather")
	fmt.Println("  dotagent skills remove weather")
}

func skillsListCmd(loader *skills.SkillsLoader) {
	allSkills := loader.ListSkills()

	if len(allSkills) == 0 {
		fmt.Println("No skills installed.")
		return
	}

	fmt.Println("\nInstalled Skills:")
	fmt.Println("------------------")
	for _, skill := range allSkills {
		fmt.Printf("  ✓ %s (%s)\n", skill.Name, skill.Source)
		if skill.Description != "" {
			fmt.Printf("    %s\n", skill.Description)
		}
	}
}

func skillsInstallCmd(installer *skills.SkillInstaller) {
	if len(os.Args) < 4 {
		fmt.Println("Usage: dotagent skills install <github-repo>")
		fmt.Println("Example: dotagent skills install dotsetgreg/dotagent-skills/weather")
		return
	}

	repo := os.Args[3]
	fmt.Printf("Installing skill from %s...\n", repo)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := installer.InstallFromGitHub(ctx, repo); err != nil {
		fmt.Printf("✗ Failed to install skill: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("✓ Skill '%s' installed successfully!\n", filepath.Base(repo))
}

func skillsRemoveCmd(installer *skills.SkillInstaller, skillName string) {
	fmt.Printf("Removing skill '%s'...\n", skillName)

	if err := installer.Uninstall(skillName); err != nil {
		fmt.Printf("✗ Failed to remove skill: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("✓ Skill '%s' removed successfully!\n", skillName)
}

func skillsSearchCmd(installer *skills.SkillInstaller) {
	fmt.Println("Searching for available skills...")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	availableSkills, err := installer.ListAvailableSkills(ctx)
	if err != nil {
		fmt.Printf("✗ Failed to fetch skills list: %v\n", err)
		return
	}

	if len(availableSkills) == 0 {
		fmt.Println("No skills available.")
		return
	}

	fmt.Printf("\nAvailable Skills (%d):\n", len(availableSkills))
	fmt.Println("--------------------")
	for _, skill := range availableSkills {
		fmt.Printf("  📦 %s\n", skill.Name)
		fmt.Printf("     %s\n", skill.Description)
		fmt.Printf("     Repo: %s\n", skill.Repository)
		if skill.Author != "" {
			fmt.Printf("     Author: %s\n", skill.Author)
		}
		if len(skill.Tags) > 0 {
			fmt.Printf("     Tags: %v\n", skill.Tags)
		}
		fmt.Println()
	}
}

func skillsShowCmd(loader *skills.SkillsLoader, skillName string) {
	content, ok := loader.LoadSkill(skillName)
	if !ok {
		fmt.Printf("✗ Skill '%s' not found\n", skillName)
		return
	}

	fmt.Printf("\n📦 Skill: %s\n", skillName)
	fmt.Println("----------------------")
	fmt.Println(content)
}

func toolpacksCmd() {
	if len(os.Args) < 3 {
		toolpacksHelp()
		return
	}

	cfg, err := loadConfig()
	if err != nil {
		fmt.Printf("Error loading config: %v\n", err)
		return
	}
	manager := toolpacks.NewManager(cfg.WorkspacePath(), cfg.Agents.Defaults.RestrictToWorkspace)
	action := strings.ToLower(strings.TrimSpace(os.Args[2]))

	switch action {
	case "list":
		toolpacksListCmd(manager)
	case "install":
		if len(os.Args) < 4 {
			fmt.Println("Usage: dotagent toolpacks install <path|owner/repo[@ref]>")
			return
		}
		toolpacksInstallCmd(manager, os.Args[3])
	case "enable":
		if len(os.Args) < 4 {
			fmt.Println("Usage: dotagent toolpacks enable <id>")
			return
		}
		toolpacksEnableCmd(manager, os.Args[3], true)
	case "disable":
		if len(os.Args) < 4 {
			fmt.Println("Usage: dotagent toolpacks disable <id>")
			return
		}
		toolpacksEnableCmd(manager, os.Args[3], false)
	case "remove", "uninstall":
		if len(os.Args) < 4 {
			fmt.Println("Usage: dotagent toolpacks remove <id>")
			return
		}
		toolpacksRemoveCmd(manager, os.Args[3])
	case "show":
		if len(os.Args) < 4 {
			fmt.Println("Usage: dotagent toolpacks show <id>")
			return
		}
		toolpacksShowCmd(manager, os.Args[3])
	case "validate":
		id := ""
		if len(os.Args) >= 4 {
			id = os.Args[3]
		}
		toolpacksValidateCmd(manager, id)
	case "doctor":
		id := ""
		if len(os.Args) >= 4 {
			id = os.Args[3]
		}
		toolpacksDoctorCmd(manager, id)
	default:
		fmt.Printf("Unknown toolpacks command: %s\n", action)
		toolpacksHelp()
	}
}

func toolpacksHelp() {
	fmt.Println("\nToolpacks commands:")
	fmt.Println("  list                  List installed toolpacks")
	fmt.Println("  install <src>         Install from local path or GitHub repo")
	fmt.Println("  enable <id>           Enable a toolpack")
	fmt.Println("  disable <id>          Disable a toolpack")
	fmt.Println("  remove <id>           Remove a toolpack")
	fmt.Println("  show <id>             Show toolpack manifest details")
	fmt.Println("  validate [id]         Validate manifests and connector configs")
	fmt.Println("  doctor [id]           Run connector health checks")
	fmt.Println()
	fmt.Println("Examples:")
	fmt.Println("  dotagent toolpacks list")
	fmt.Println("  dotagent toolpacks install ./examples/toolpacks/github-cli")
	fmt.Println("  dotagent toolpacks install owner/repo@v1.0.0")
}

func toolpacksListCmd(manager *toolpacks.Manager) {
	packs, err := manager.List()
	if err != nil {
		fmt.Printf("✗ Failed to list toolpacks: %v\n", err)
		return
	}
	if len(packs) == 0 {
		fmt.Println("No toolpacks installed.")
		return
	}
	fmt.Printf("Installed toolpacks (%d):\n", len(packs))
	for _, pack := range packs {
		status := "disabled"
		if pack.Enabled {
			status = "enabled"
		}
		fmt.Printf("  - %s (%s) %s\n", pack.ID, pack.Version, status)
		if strings.TrimSpace(pack.Description) != "" {
			fmt.Printf("      %s\n", pack.Description)
		}
		fmt.Printf("      tools: %d\n", len(pack.Tools))
		if lock, ok, lockErr := manager.GetLock(pack.ID); lockErr == nil && ok {
			fmt.Printf("      source: %s\n", lock.Source)
		}
	}
}

func toolpacksInstallCmd(manager *toolpacks.Manager, source string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var (
		pack toolpacks.Manifest
		err  error
	)

	if fi, statErr := os.Stat(source); statErr == nil && fi.IsDir() {
		pack, err = manager.InstallFromPath(source)
	} else {
		pack, err = manager.InstallFromGitHub(ctx, source)
	}
	if err != nil {
		fmt.Printf("✗ Failed to install toolpack: %v\n", err)
		return
	}
	fmt.Printf("✓ Installed toolpack %s (%s)\n", pack.ID, pack.Version)
}

func toolpacksEnableCmd(manager *toolpacks.Manager, id string, enabled bool) {
	if err := manager.Enable(id, enabled); err != nil {
		fmt.Printf("✗ Failed to update toolpack %s: %v\n", id, err)
		return
	}
	state := "disabled"
	if enabled {
		state = "enabled"
	}
	fmt.Printf("✓ Toolpack %s %s\n", id, state)
}

func toolpacksRemoveCmd(manager *toolpacks.Manager, id string) {
	if err := manager.Remove(id); err != nil {
		fmt.Printf("✗ Failed to remove toolpack %s: %v\n", id, err)
		return
	}
	fmt.Printf("✓ Toolpack %s removed\n", id)
}

func toolpacksShowCmd(manager *toolpacks.Manager, id string) {
	packs, err := manager.List()
	if err != nil {
		fmt.Printf("✗ Failed to list toolpacks: %v\n", err)
		return
	}
	for _, pack := range packs {
		if pack.ID != id {
			continue
		}
		if lock, ok, lockErr := manager.GetLock(id); lockErr == nil && ok {
			pack.Metadata = mergeToolpackMetadata(pack.Metadata, map[string]interface{}{
				"lock": lock,
			})
		}
		raw, _ := json.MarshalIndent(pack, "", "  ")
		fmt.Println(string(raw))
		return
	}
	fmt.Printf("✗ Toolpack %s not found\n", id)
}

func toolpacksValidateCmd(manager *toolpacks.Manager, id string) {
	warnings, err := manager.Validate(id)
	if err != nil {
		fmt.Printf("✗ Validation failed: %v\n", err)
		return
	}
	if len(warnings) == 0 {
		fmt.Println("✓ Validation passed with no warnings.")
		return
	}
	fmt.Printf("Validation warnings (%d):\n", len(warnings))
	for _, w := range warnings {
		fmt.Printf("  - %s\n", w)
	}
}

func toolpacksDoctorCmd(manager *toolpacks.Manager, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	results, err := manager.Doctor(ctx, id)
	if err != nil {
		fmt.Printf("✗ Doctor failed: %v\n", err)
		return
	}
	if len(results) == 0 {
		fmt.Println("No connectors found.")
		return
	}
	errors := 0
	fmt.Println("Connector health:")
	for _, res := range results {
		if res.Status != "ok" {
			errors++
		}
		if res.ConnectorID == "" {
			fmt.Printf("  - [%s] %s\n", strings.ToUpper(res.Status), res.Error)
			continue
		}
		if res.Error == "" {
			fmt.Printf("  - [%s] %s/%s (%s)\n", strings.ToUpper(res.Status), res.PackID, res.ConnectorID, res.Type)
			continue
		}
		fmt.Printf("  - [%s] %s/%s (%s): %s\n", strings.ToUpper(res.Status), res.PackID, res.ConnectorID, res.Type, res.Error)
	}
	if errors > 0 {
		fmt.Printf("✗ Doctor completed with %d error(s)\n", errors)
		return
	}
	fmt.Println("✓ Doctor completed successfully.")
}

func mergeToolpackMetadata(base map[string]interface{}, extra map[string]interface{}) map[string]interface{} {
	out := map[string]interface{}{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}
