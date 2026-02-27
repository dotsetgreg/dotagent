package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dotsetgreg/dotagent/pkg/config"
	"github.com/dotsetgreg/dotagent/pkg/providers"
	"github.com/dotsetgreg/dotagent/pkg/tools"
	"github.com/spf13/cobra"
)

type doctorCheck struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Detail  string `json:"detail,omitempty"`
	Warning bool   `json:"warning,omitempty"`
}

type doctorReport struct {
	Instance string        `json:"instance"`
	Ready    bool          `json:"ready"`
	Checks   []doctorCheck `json:"checks"`
}

func newInitCommand(instanceID *string) *cobra.Command {
	var (
		force          bool
		nonInteractive bool
		yes            bool
		migrateLegacy  bool
	)

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize an instance-scoped DotAgent installation",
		Long:  "Create instance directories, config v2, workspace templates, and runtime compose artifacts.",
		RunE: func(cmd *cobra.Command, args []string) error {
			id := resolveInstanceID(*instanceID)
			if err := validateInstanceID(id); err != nil {
				return err
			}
			cfgPath := instanceConfigPath(id)
			_, statErr := os.Stat(cfgPath)
			exists := statErr == nil

			if exists && !force && !yes {
				if nonInteractive {
					return fmt.Errorf("instance config already exists at %s (use --force or --yes)", cfgPath)
				}
				if !confirmPrompt(os.Stdin, os.Stdout, fmt.Sprintf("Config exists at %s. Overwrite?", cfgPath)) {
					return fmt.Errorf("aborted")
				}
			}

			if migrateLegacy && !exists {
				if _, err := os.Stat(legacyConfigPath()); err == nil {
					if err := migrateLegacyToInstance(id, force); err != nil {
						return err
					}
					fmt.Printf("Migrated legacy installation into instance %q.\n", id)
					if err := ensureRuntimeCompose(id); err != nil {
						return err
					}
					return nil
				}
			}

			if err := ensureInstanceLayout(id); err != nil {
				return err
			}
			cfg := config.DefaultConfigForInstance(id)
			if err := saveInstanceConfig(id, cfg, configMutationOptions{SkipHistory: true}); err != nil {
				return err
			}
			if err := createWorkspaceTemplates(cfg.WorkspacePath()); err != nil {
				return fmt.Errorf("create workspace templates: %w", err)
			}
			if err := ensureRuntimeCompose(id); err != nil {
				return err
			}

			fmt.Printf("dotagent instance %q is ready.\n", id)
			fmt.Printf("Config: %s\n", cfgPath)
			fmt.Println("Next steps:")
			fmt.Println("  1. dotagent config set providers.openrouter.api_key '\"<key>\"'")
			fmt.Println("  2. dotagent config set channels.discord.token '\"<discord-token>\"'")
			fmt.Println("  3. dotagent doctor --check")
			fmt.Println("  4. dotagent runtime up")
			fmt.Println("  5. dotagent runtime status --check")
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "Overwrite existing config and runtime artifacts")
	cmd.Flags().BoolVar(&nonInteractive, "non-interactive", false, "Fail instead of prompting when user input is required")
	cmd.Flags().BoolVar(&yes, "yes", false, "Assume yes for overwrite prompts")
	cmd.Flags().BoolVar(&migrateLegacy, "migrate-legacy", true, "Import legacy ~/.dotagent layout when present")
	return cmd
}

func newMigrateCommand(instanceID *string) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Migrate legacy ~/.dotagent config/workspace into instance layout",
		RunE: func(cmd *cobra.Command, args []string) error {
			id := resolveInstanceID(*instanceID)
			if err := validateInstanceID(id); err != nil {
				return err
			}
			if err := migrateLegacyToInstance(id, force); err != nil {
				return err
			}
			if err := ensureRuntimeCompose(id); err != nil {
				return err
			}
			fmt.Printf("Legacy installation migrated to instance %q.\n", id)
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "Overwrite existing files in target instance when needed")
	return cmd
}

func newDoctorCommand(instanceID *string) *cobra.Command {
	var (
		checkMode bool
		format    string
	)
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Run deterministic instance readiness checks",
		RunE: func(cmd *cobra.Command, args []string) error {
			id := resolveInstanceID(*instanceID)
			report := runDoctor(id)
			format = strings.ToLower(strings.TrimSpace(format))
			switch format {
			case "", "text":
				printDoctorText(report)
			case "json":
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				if err := enc.Encode(report); err != nil {
					return err
				}
			default:
				return fmt.Errorf("unsupported format %q (expected text or json)", format)
			}
			if checkMode && !report.Ready {
				return fmt.Errorf("doctor checks failed")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&checkMode, "check", false, "Exit non-zero when checks fail")
	cmd.Flags().StringVar(&format, "format", "text", "Output format: text|json")
	return cmd
}

func newRuntimeCommand(instanceID *string) *cobra.Command {
	runtimeRoot := &cobra.Command{
		Use:   "runtime",
		Short: "Manage Docker runtime lifecycle for an instance",
	}

	runtimeRoot.AddCommand(&cobra.Command{
		Use:   "up",
		Short: "Start gateway runtime with Docker Compose",
		RunE: func(cmd *cobra.Command, args []string) error {
			id := resolveInstanceID(*instanceID)
			if err := ensureRuntimeCompose(id); err != nil {
				return err
			}
			return runCompose(id, "up", "-d", "dotagent-gateway")
		},
	})

	runtimeRoot.AddCommand(&cobra.Command{
		Use:   "down",
		Short: "Stop and remove runtime containers",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCompose(resolveInstanceID(*instanceID), "down")
		},
	})

	runtimeRoot.AddCommand(&cobra.Command{
		Use:   "restart",
		Short: "Restart the gateway runtime container",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCompose(resolveInstanceID(*instanceID), "restart", "dotagent-gateway")
		},
	})

	var (
		follow bool
		tail   int
	)
	logsCmd := &cobra.Command{
		Use:   "logs",
		Short: "Stream runtime logs",
		RunE: func(cmd *cobra.Command, args []string) error {
			id := resolveInstanceID(*instanceID)
			composeArgs := []string{"logs"}
			if follow {
				composeArgs = append(composeArgs, "-f")
			}
			if tail > 0 {
				composeArgs = append(composeArgs, "--tail", strconv.Itoa(tail))
			}
			composeArgs = append(composeArgs, "dotagent-gateway")
			return runCompose(id, composeArgs...)
		},
	}
	logsCmd.Flags().BoolVarP(&follow, "follow", "f", true, "Follow logs")
	logsCmd.Flags().IntVar(&tail, "tail", 200, "Line count from end of logs")
	runtimeRoot.AddCommand(logsCmd)

	runtimeRoot.AddCommand(&cobra.Command{
		Use:   "ps",
		Short: "Show runtime container status",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCompose(resolveInstanceID(*instanceID), "ps")
		},
	})

	var (
		checkMode bool
		format    string
	)
	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "Show runtime + readiness status",
		RunE: func(cmd *cobra.Command, args []string) error {
			id := resolveInstanceID(*instanceID)
			report := runDoctor(id)
			running := runtimeContainerRunning(id)
			report.Checks = append(report.Checks, doctorCheck{
				Name:   "runtime_container_running",
				OK:     running,
				Detail: "dotagent-gateway",
			})
			report.Ready = report.Ready && running
			if running {
				gatewayReady, gatewayDetail, gatewayChecks := runtimeGatewayProbe(id)
				report.Checks = append(report.Checks, doctorCheck{
					Name:   "runtime_gateway_ready",
					OK:     gatewayReady,
					Detail: gatewayDetail,
				})
				report.Checks = append(report.Checks, gatewayChecks...)
				report.Ready = report.Ready && gatewayReady
			}
			switch strings.ToLower(strings.TrimSpace(format)) {
			case "", "text":
				printDoctorText(report)
			case "json":
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				if err := enc.Encode(report); err != nil {
					return err
				}
			default:
				return fmt.Errorf("unsupported format %q", format)
			}
			if checkMode && !report.Ready {
				return fmt.Errorf("runtime is not ready")
			}
			return nil
		},
	}
	statusCmd.Flags().BoolVar(&checkMode, "check", false, "Exit non-zero when runtime is not ready")
	statusCmd.Flags().StringVar(&format, "format", "text", "Output format: text|json")
	runtimeRoot.AddCommand(statusCmd)

	return runtimeRoot
}

func newConfigCommand(instanceID *string) *cobra.Command {
	root := &cobra.Command{
		Use:   "config",
		Short: "Inspect and mutate instance configuration",
	}

	root.AddCommand(&cobra.Command{
		Use:   "path",
		Short: "Print active config path",
		RunE: func(cmd *cobra.Command, args []string) error {
			id := resolveInstanceID(*instanceID)
			fmt.Println(instanceConfigPath(id))
			return nil
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "show",
		Short: "Print resolved config",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfgPath := instanceConfigPath(resolveInstanceID(*instanceID))
			raw, err := os.ReadFile(cfgPath)
			if err != nil {
				return err
			}
			fmt.Println(string(raw))
			return nil
		},
	})

	setCmd := &cobra.Command{
		Use:   "set <dot.path> <value>",
		Short: "Set a config key using dot-path syntax",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := resolveInstanceID(*instanceID)
			rawMap, err := readConfigMap(instanceConfigPath(id))
			if err != nil {
				return err
			}
			value := parseCLIValue(args[1])
			if err := setDotPath(rawMap, args[0], value); err != nil {
				return err
			}
			return writeConfigMap(id, rawMap)
		},
	}
	root.AddCommand(setCmd)

	unsetCmd := &cobra.Command{
		Use:   "unset <dot.path>",
		Short: "Unset/remove a config key",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := resolveInstanceID(*instanceID)
			rawMap, err := readConfigMap(instanceConfigPath(id))
			if err != nil {
				return err
			}
			if err := unsetDotPath(rawMap, args[0]); err != nil {
				return err
			}
			return writeConfigMap(id, rawMap)
		},
	}
	root.AddCommand(unsetCmd)

	var (
		approvedBy  string
		approveNote string
	)
	approveCmd := &cobra.Command{
		Use:   "approve <request-id>",
		Short: "Approve a pending guarded config request",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := resolveInstanceID(*instanceID)
			requestsPath, auditPath := configAdminPaths(id)
			store, err := tools.LoadConfigRequestStore(requestsPath)
			if err != nil {
				return err
			}
			reqID := strings.TrimSpace(args[0])
			if reqID == "" {
				return fmt.Errorf("request-id is required")
			}
			var req *tools.ConfigRequest
			for i := range store.Requests {
				if store.Requests[i].ID == reqID {
					req = &store.Requests[i]
					break
				}
			}
			if req == nil {
				return fmt.Errorf("request %s not found in %s", reqID, requestsPath)
			}
			if req.Status != "pending" {
				return fmt.Errorf("request %s is %s (expected pending)", reqID, req.Status)
			}
			approver := strings.TrimSpace(approvedBy)
			if approver == "" {
				approver = strings.TrimSpace(os.Getenv("USER"))
			}
			if approver == "" {
				approver = "operator"
			}
			now := time.Now().UnixMilli()
			req.Status = "approved"
			req.ApprovedBy = approver
			req.ApprovalNote = strings.TrimSpace(approveNote)
			req.ApprovedAt = now
			req.UpdatedAt = now
			req.Error = ""
			if err := tools.SaveConfigRequestStore(requestsPath, store); err != nil {
				return err
			}
			_ = tools.AppendConfigAuditLog(auditPath, map[string]any{
				"timestamp":     now,
				"action":        "approved",
				"request_id":    req.ID,
				"key":           req.Key,
				"value_preview": req.ValuePreview,
				"value_digest":  req.ValueDigest,
				"approved_by":   req.ApprovedBy,
				"approval_note": req.ApprovalNote,
			})
			fmt.Printf("Approved config request %s (%s).\n", req.ID, req.Key)
			return nil
		},
	}
	approveCmd.Flags().StringVar(&approvedBy, "by", "", "Approver identity for audit trail (default: $USER)")
	approveCmd.Flags().StringVar(&approveNote, "note", "", "Optional approval note")
	root.AddCommand(approveCmd)

	root.AddCommand(&cobra.Command{
		Use:   "validate",
		Short: "Validate active config",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, _, err := loadInstanceConfig(resolveInstanceID(*instanceID))
			return err
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "history",
		Short: "List config revision history entries",
		RunE: func(cmd *cobra.Command, args []string) error {
			id := resolveInstanceID(*instanceID)
			entries, err := configHistoryEntries(id)
			if err != nil {
				return err
			}
			if len(entries) == 0 {
				fmt.Println("No config history entries.")
				return nil
			}
			for _, e := range entries {
				fmt.Println(e)
			}
			return nil
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "rollback <history-id>",
		Short: "Restore config from history entry",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return restoreConfigFromHistory(resolveInstanceID(*instanceID), args[0])
		},
	})

	return root
}

func newBackupCommand(instanceID *string) *cobra.Command {
	root := &cobra.Command{
		Use:   "backup",
		Short: "Create and restore instance backups",
	}

	var outPath string
	create := &cobra.Command{
		Use:   "create",
		Short: "Create a .tar.gz backup for the instance",
		RunE: func(cmd *cobra.Command, args []string) error {
			id := resolveInstanceID(*instanceID)
			if strings.TrimSpace(outPath) == "" {
				outPath = fmt.Sprintf("dotagent-%s-%s.tar.gz", id, time.Now().UTC().Format("20060102T150405Z"))
			}
			return createInstanceBackup(id, outPath)
		},
	}
	create.Flags().StringVar(&outPath, "output", "", "Backup output path (.tar.gz)")
	root.AddCommand(create)

	var (
		inPath string
		force  bool
	)
	restore := &cobra.Command{
		Use:   "restore",
		Short: "Restore instance from a .tar.gz backup",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(inPath) == "" {
				return fmt.Errorf("--input is required")
			}
			return restoreInstanceBackup(resolveInstanceID(*instanceID), inPath, force)
		},
	}
	restore.Flags().StringVar(&inPath, "input", "", "Backup archive path (.tar.gz)")
	restore.Flags().BoolVar(&force, "force", false, "Allow restoring into a non-empty instance root")
	root.AddCommand(restore)

	return root
}

func ensureInstanceLayout(instanceID string) error {
	dirs := []string{
		filepath.Dir(instanceConfigPath(instanceID)),
		configHistoryDir(instanceID),
		workspaceDir(instanceID),
		dataDir(instanceID),
		logsDir(instanceID),
		runtimeDir(instanceID),
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return nil
}

func runDoctor(instanceID string) doctorReport {
	report := doctorReport{
		Instance: instanceID,
		Ready:    true,
		Checks:   []doctorCheck{},
	}
	addCheck := func(name string, ok bool, detail string) {
		report.Checks = append(report.Checks, doctorCheck{Name: name, OK: ok, Detail: detail})
		if !ok {
			report.Ready = false
		}
	}

	cfgPath := instanceConfigPath(instanceID)
	if _, err := os.Stat(cfgPath); err != nil {
		addCheck("config_exists", false, cfgPath)
		return report
	}
	addCheck("config_exists", true, cfgPath)

	cfg, _, err := loadInstanceConfig(instanceID)
	if err != nil {
		addCheck("config_valid", false, err.Error())
		return report
	}
	addCheck("config_valid", true, "ok")

	if err := providers.ValidateProviderConfig(cfg); err != nil {
		addCheck("provider_credentials", false, err.Error())
	} else {
		addCheck("provider_credentials", true, providers.ActiveProviderName(cfg))
	}
	discordReady := strings.TrimSpace(cfg.Channels.Discord.Token) != ""
	addCheck("discord_token", discordReady, "channels.discord.token")

	for _, p := range []struct {
		name string
		path string
	}{
		{name: "workspace_path", path: cfg.WorkspacePath()},
		{name: "data_path", path: cfg.DataPath()},
		{name: "logs_path", path: cfg.LogsPath()},
		{name: "runtime_path", path: cfg.RuntimePath()},
	} {
		_, statErr := os.Stat(p.path)
		addCheck(p.name, statErr == nil, p.path)
	}

	stateDir := filepath.Join(cfg.DataPath(), "state")
	stateWritable, stateWritableDetail := checkWritableDirectory(stateDir)
	addCheck("data_state_writable", stateWritable, stateWritableDetail)

	memoryDBPath := filepath.Join(stateDir, "memory.db")
	if err := ensureFileAccessible(memoryDBPath); err != nil {
		addCheck("memory_db_accessible", false, err.Error())
	} else {
		addCheck("memory_db_accessible", true, memoryDBPath)
	}

	if cfg.Heartbeat.Enabled {
		logsWritable, logsWritableDetail := checkWritableDirectory(cfg.LogsPath())
		addCheck("heartbeat_logs_writable", logsWritable, logsWritableDetail)
	}

	_, dockerErr := exec.LookPath("docker")
	addCheck("docker_available", dockerErr == nil, "docker")

	composePath := filepath.Join(cfg.RuntimePath(), "docker-compose.yml")
	_, composeErr := os.Stat(composePath)
	addCheck("runtime_compose_present", composeErr == nil, composePath)

	return report
}

func printDoctorText(report doctorReport) {
	fmt.Printf("Instance: %s\n", report.Instance)
	fmt.Printf("Ready: %t\n", report.Ready)
	for _, c := range report.Checks {
		status := "OK"
		if !c.OK {
			status = "FAIL"
		}
		if strings.TrimSpace(c.Detail) == "" {
			fmt.Printf("  - [%s] %s\n", status, c.Name)
			continue
		}
		fmt.Printf("  - [%s] %s: %s\n", status, c.Name, c.Detail)
	}
}

func checkWritableDirectory(dir string) (bool, string) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, fmt.Sprintf("%s (mkdir failed: %v)", dir, err)
	}
	probe := filepath.Join(dir, fmt.Sprintf(".dotagent-probe-%d", time.Now().UnixNano()))
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		return false, fmt.Sprintf("%s (write failed: %v)", dir, err)
	}
	_ = os.Remove(probe)
	return true, dir
}

func ensureFileAccessible(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("ensure parent dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

func ensureRuntimeCompose(instanceID string) error {
	cfg, _, err := loadInstanceConfig(instanceID)
	if err != nil {
		return err
	}
	composePath := filepath.Join(cfg.RuntimePath(), "docker-compose.yml")
	if err := os.MkdirAll(filepath.Dir(composePath), 0o755); err != nil {
		return err
	}
	content := buildComposeYAML(instanceID, cfg)
	return os.WriteFile(composePath, []byte(content), 0o644)
}

func buildComposeYAML(instanceID string, cfg *config.Config) string {
	image := strings.TrimSpace(cfg.Runtime.Image)
	if image == "" {
		image = "ghcr.io/dotsetgreg/dotagent:latest"
	}
	root := instanceRootDir(instanceID)
	containerName := "dotagent-" + sanitizeContainerSegment(instanceID) + "-gateway"
	return fmt.Sprintf(`services:
  dotagent-gateway:
    image: %s
    container_name: %s
    restart: unless-stopped
    environment:
      - DOTAGENT_CONFIG=/var/lib/dotagent/config/config.json
      - DOTAGENT_INSTANCE=%s
      - DOTAGENT_DATA_DIR=/var/lib/dotagent/data
      - DOTAGENT_LOGS_DIR=/var/lib/dotagent/logs
      - DOTAGENT_RUNTIME_DIR=/var/lib/dotagent/runtime
      - DOTAGENT_ALLOW_PROD_GATEWAY=1
    volumes:
      - %s:/var/lib/dotagent
    command: ["serve"]
`, image, containerName, instanceID, root)
}

func sanitizeContainerSegment(in string) string {
	in = strings.ToLower(strings.TrimSpace(in))
	out := strings.Builder{}
	for _, r := range in {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			out.WriteRune(r)
		} else {
			out.WriteRune('-')
		}
	}
	if out.Len() == 0 {
		return "default"
	}
	return out.String()
}

func composeFilePath(instanceID string) string {
	return filepath.Join(runtimeDir(instanceID), "docker-compose.yml")
}

func configAdminPaths(instanceID string) (requestsPath string, auditPath string) {
	if runtimeEnv := strings.TrimSpace(os.Getenv("DOTAGENT_RUNTIME_DIR")); runtimeEnv != "" {
		return filepath.Join(runtimeEnv, "config_requests.json"), filepath.Join(runtimeEnv, "config_audit.log")
	}
	cfg, _, err := loadInstanceConfig(instanceID)
	if err == nil {
		runtimeRoot := cfg.RuntimePath()
		return filepath.Join(runtimeRoot, "config_requests.json"), filepath.Join(runtimeRoot, "config_audit.log")
	}
	root := runtimeDir(instanceID)
	return filepath.Join(root, "config_requests.json"), filepath.Join(root, "config_audit.log")
}

func configRestartPendingPath(instanceID string) string {
	if runtimeEnv := strings.TrimSpace(os.Getenv("DOTAGENT_RUNTIME_DIR")); runtimeEnv != "" {
		return filepath.Join(runtimeEnv, "config_restart_pending.json")
	}
	cfg, _, err := loadInstanceConfig(instanceID)
	if err == nil {
		return filepath.Join(cfg.RuntimePath(), "config_restart_pending.json")
	}
	return filepath.Join(runtimeDir(instanceID), "config_restart_pending.json")
}

func runCompose(instanceID string, args ...string) error {
	if err := ensureRuntimeCompose(instanceID); err != nil {
		return err
	}
	composeFile := composeFilePath(instanceID)
	cmdArgs := append([]string{"compose", "-f", composeFile}, args...)
	cmd := exec.Command("docker", cmdArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	return cmd.Run()
}

func runtimeContainerRunning(instanceID string) bool {
	composeFile := composeFilePath(instanceID)
	cmd := exec.Command("docker", "compose", "-f", composeFile, "ps", "--status", "running", "--services")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) == "dotagent-gateway" {
			return true
		}
	}
	return false
}

func runtimeGatewayProbe(instanceID string) (bool, string, []doctorCheck) {
	composeFile := composeFilePath(instanceID)
	cmd := exec.Command("docker", "compose", "-f", composeFile, "exec", "-T", "dotagent-gateway", "dotagent", "serve-check", "--format", "json")
	out, err := cmd.CombinedOutput()
	trimmed := strings.TrimSpace(string(out))
	if err != nil {
		if trimmed == "" {
			trimmed = err.Error()
		}
		return false, trimmed, nil
	}

	payload := bytes.TrimSpace(out)
	if idx := bytes.IndexByte(payload, '{'); idx > 0 {
		payload = payload[idx:]
	}
	var probe doctorReport
	if err := json.Unmarshal(payload, &probe); err != nil {
		return false, fmt.Sprintf("serve-check parse failed: %v", err), nil
	}
	checks := make([]doctorCheck, 0, len(probe.Checks))
	for _, c := range probe.Checks {
		c.Name = "gateway_" + c.Name
		checks = append(checks, c)
	}
	return probe.Ready, fmt.Sprintf("gateway serve-check ready=%t", probe.Ready), checks
}

func readConfigMap(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func writeConfigMap(instanceID string, rawMap map[string]any) error {
	normalizedRaw, err := json.Marshal(rawMap)
	if err != nil {
		return err
	}
	var cfg config.Config
	if err := json.Unmarshal(normalizedRaw, &cfg); err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	return saveInstanceConfig(instanceID, &cfg, configMutationOptions{})
}

func parseCLIValue(raw string) any {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err == nil {
		return v
	}
	return raw
}

func setDotPath(root map[string]any, dotPath string, value any) error {
	parts := splitDotPath(dotPath)
	if len(parts) == 0 {
		return fmt.Errorf("path is required")
	}
	m := root
	for i := 0; i < len(parts)-1; i++ {
		part := parts[i]
		next, ok := m[part]
		if !ok {
			child := map[string]any{}
			m[part] = child
			m = child
			continue
		}
		child, ok := next.(map[string]any)
		if !ok {
			return fmt.Errorf("path %q is not an object", strings.Join(parts[:i+1], "."))
		}
		m = child
	}
	m[parts[len(parts)-1]] = value
	return nil
}

func unsetDotPath(root map[string]any, dotPath string) error {
	parts := splitDotPath(dotPath)
	if len(parts) == 0 {
		return fmt.Errorf("path is required")
	}
	m := root
	for i := 0; i < len(parts)-1; i++ {
		next, ok := m[parts[i]]
		if !ok {
			return nil
		}
		child, ok := next.(map[string]any)
		if !ok {
			return fmt.Errorf("path %q is not an object", strings.Join(parts[:i+1], "."))
		}
		m = child
	}
	delete(m, parts[len(parts)-1])
	return nil
}

func splitDotPath(dotPath string) []string {
	parts := strings.Split(strings.TrimSpace(dotPath), ".")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func confirmPrompt(in io.Reader, out io.Writer, prompt string) bool {
	_, _ = fmt.Fprintf(out, "%s (y/n): ", prompt)
	var input string
	if _, err := fmt.Fscanln(in, &input); err != nil {
		return false
	}
	normalized := strings.ToLower(strings.TrimSpace(input))
	return normalized == "y" || normalized == "yes"
}

func createInstanceBackup(instanceID, outPath string) error {
	root := instanceRootDir(instanceID)
	if _, err := os.Stat(root); err != nil {
		return fmt.Errorf("instance root not found: %w", err)
	}
	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	return filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}

		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = rel
		if info.IsDir() {
			header.Name += "/"
		}
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		src, err := os.Open(path)
		if err != nil {
			return err
		}
		defer src.Close()
		_, err = io.Copy(tw, src)
		return err
	})
}

func restoreInstanceBackup(instanceID, inPath string, force bool) error {
	root := instanceRootDir(instanceID)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	if !force {
		entries, err := os.ReadDir(root)
		if err == nil && len(entries) > 0 {
			return fmt.Errorf("instance root %s is not empty (use --force)", root)
		}
	}
	f, err := os.Open(inPath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		targetPath := filepath.Join(root, filepath.FromSlash(hdr.Name))
		cleanTarget := filepath.Clean(targetPath)
		if !strings.HasPrefix(cleanTarget, filepath.Clean(root)+string(os.PathSeparator)) && cleanTarget != filepath.Clean(root) {
			return fmt.Errorf("backup entry escapes instance root: %s", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(cleanTarget, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(cleanTarget), 0o755); err != nil {
				return err
			}
			dst, err := os.OpenFile(cleanTarget, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(dst, tr); err != nil {
				_ = dst.Close()
				return err
			}
			if err := dst.Close(); err != nil {
				return err
			}
		}
	}
	return nil
}

func migrateLegacyToInstance(instanceID string, force bool) error {
	legacyCfgPath := legacyConfigPath()
	legacyWsPath := legacyWorkspacePath()
	if _, err := os.Stat(legacyCfgPath); err != nil {
		return fmt.Errorf("legacy config not found at %s", legacyCfgPath)
	}
	if err := ensureInstanceLayout(instanceID); err != nil {
		return err
	}
	backupPath, err := createLegacyMigrationBackup(instanceID, legacyCfgPath, legacyWsPath)
	if err != nil {
		return fmt.Errorf("create legacy migration backup: %w", err)
	}
	if strings.TrimSpace(backupPath) != "" {
		fmt.Printf("Legacy backup created at %s\n", backupPath)
	}
	targetCfgPath := instanceConfigPath(instanceID)
	if _, err := os.Stat(targetCfgPath); err == nil && !force {
		return fmt.Errorf("target config already exists at %s (use --force)", targetCfgPath)
	}

	oldCfg, err := config.LoadConfig(legacyCfgPath)
	if err != nil {
		return fmt.Errorf("load legacy config: %w", err)
	}
	oldCfg.SchemaVersion = 2
	oldCfg.Instance.ID = instanceID
	oldCfg.Paths.Workspace = workspaceDir(instanceID)
	oldCfg.Paths.Data = dataDir(instanceID)
	oldCfg.Paths.Logs = logsDir(instanceID)
	oldCfg.Paths.Runtime = runtimeDir(instanceID)
	oldCfg.Agents.Defaults.Workspace = oldCfg.Paths.Workspace
	oldCfg.Runtime.Mode = "docker"
	oldCfg.Runtime.Image = "ghcr.io/dotsetgreg/dotagent:latest"

	if err := saveInstanceConfig(instanceID, oldCfg, configMutationOptions{SkipHistory: true}); err != nil {
		return err
	}

	// Copy workspace files (persona/user content).
	if _, err := os.Stat(legacyWsPath); err == nil {
		if err := copyDir(legacyWsPath, workspaceDir(instanceID), force); err != nil {
			return fmt.Errorf("copy legacy workspace: %w", err)
		}
		legacyState := filepath.Join(legacyWsPath, "state")
		if _, err := os.Stat(legacyState); err == nil {
			if err := copyDir(legacyState, filepath.Join(dataDir(instanceID), "state"), true); err != nil {
				return fmt.Errorf("copy legacy runtime state: %w", err)
			}
		}
	}
	return nil
}

func createLegacyMigrationBackup(instanceID, legacyCfgPath, legacyWsPath string) (string, error) {
	backupDir := filepath.Join(runtimeDir(instanceID), "migration")
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		return "", err
	}
	backupPath := filepath.Join(backupDir, fmt.Sprintf("legacy-backup-%s.tar.gz", time.Now().UTC().Format("20060102T150405Z")))
	f, err := os.Create(backupPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	addPath := func(srcPath, archiveBase string) error {
		info, err := os.Stat(srcPath)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !info.IsDir() {
			return addFileToTar(tw, srcPath, archiveBase, info)
		}
		return filepath.Walk(srcPath, func(path string, walkInfo os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			rel, err := filepath.Rel(srcPath, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			name := archiveBase
			if rel != "." {
				name = filepath.ToSlash(filepath.Join(archiveBase, rel))
			}
			return addFileToTar(tw, path, name, walkInfo)
		})
	}

	if err := addPath(legacyCfgPath, "legacy/config/config.json"); err != nil {
		return "", err
	}
	if err := addPath(legacyWsPath, "legacy/workspace"); err != nil {
		return "", err
	}
	return backupPath, nil
}

func addFileToTar(tw *tar.Writer, srcPath, archiveName string, info os.FileInfo) error {
	header, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	header.Name = filepath.ToSlash(archiveName)
	if info.IsDir() {
		if !strings.HasSuffix(header.Name, "/") {
			header.Name += "/"
		}
	}
	if err := tw.WriteHeader(header); err != nil {
		return err
	}
	if info.IsDir() {
		return nil
	}
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()
	_, err = io.Copy(tw, src)
	return err
}

func copyDir(src, dst string, overwrite bool) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !overwrite {
			if _, err := os.Stat(target); err == nil {
				return nil
			}
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode())
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			_ = out.Close()
			return err
		}
		return out.Close()
	})
}

func topLevelCommandNames(cmd *cobra.Command) []string {
	children := cmd.Commands()
	out := make([]string, 0, len(children))
	for _, c := range children {
		if c.Hidden {
			continue
		}
		out = append(out, c.Name())
	}
	sort.Strings(out)
	return out
}
