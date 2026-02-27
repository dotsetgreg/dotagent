package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dotsetgreg/dotagent/pkg/health"
	"github.com/dotsetgreg/dotagent/pkg/providers"
	"github.com/spf13/cobra"
)

func newServeCheckCommand() *cobra.Command {
	var (
		checkMode bool
		format    string
	)
	cmd := &cobra.Command{
		Use:    "serve-check",
		Short:  "Run gateway self-readiness checks",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			report := runServeCheck(resolveInstanceID(os.Getenv("DOTAGENT_INSTANCE")))
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
				return fmt.Errorf("gateway is not ready")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&checkMode, "check", false, "Exit non-zero when readiness checks fail")
	cmd.Flags().StringVar(&format, "format", "text", "Output format: text|json")
	return cmd
}

func runServeCheck(instanceID string) doctorReport {
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

	cfg, err := loadConfig()
	if err != nil {
		addCheck("config_load", false, err.Error())
		return report
	}
	addCheck("config_load", true, getConfigPath())

	if err := providers.ValidateProviderConfig(cfg); err != nil {
		addCheck("provider_credentials", false, err.Error())
	} else {
		addCheck("provider_credentials", true, providers.ActiveProviderName(cfg))
	}

	discordReady := strings.TrimSpace(cfg.Channels.Discord.Token) != ""
	addCheck("discord_token", discordReady, "channels.discord.token")

	memoryDBPath := filepath.Join(cfg.DataPath(), "state", "memory.db")
	if err := ensureFileAccessible(memoryDBPath); err != nil {
		addCheck("memory_db_accessible", false, err.Error())
	} else {
		addCheck("memory_db_accessible", true, memoryDBPath)
	}

	host := strings.TrimSpace(cfg.Gateway.Host)
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	readyURL := fmt.Sprintf("http://%s:%d/ready", host, cfg.Gateway.Port)

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(readyURL)
	if err != nil {
		addCheck("ready_endpoint", false, err.Error())
		sort.Slice(report.Checks, func(i, j int) bool { return report.Checks[i].Name < report.Checks[j].Name })
		return report
	}
	defer resp.Body.Close()

	addCheck("ready_http_status", resp.StatusCode == http.StatusOK, fmt.Sprintf("status=%d", resp.StatusCode))

	statusResp := health.StatusResponse{}
	if err := json.NewDecoder(resp.Body).Decode(&statusResp); err != nil {
		addCheck("ready_response_parse", false, err.Error())
		sort.Slice(report.Checks, func(i, j int) bool { return report.Checks[i].Name < report.Checks[j].Name })
		return report
	}
	addCheck("ready_status", strings.EqualFold(strings.TrimSpace(statusResp.Status), "ready"), strings.TrimSpace(statusResp.Status))

	if len(statusResp.Checks) > 0 {
		names := make([]string, 0, len(statusResp.Checks))
		for name := range statusResp.Checks {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			check := statusResp.Checks[name]
			ok := strings.EqualFold(strings.TrimSpace(check.Status), "ok")
			detail := strings.TrimSpace(check.Message)
			if detail == "" {
				detail = check.Status
			}
			addCheck("ready_"+name, ok, detail)
		}
	}

	sort.Slice(report.Checks, func(i, j int) bool { return report.Checks[i].Name < report.Checks[j].Name })
	return report
}
