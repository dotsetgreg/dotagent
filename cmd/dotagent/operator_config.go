package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dotsetgreg/dotagent/pkg/config"
)

type configMutationOptions struct {
	SkipHistory bool
}

func loadInstanceConfig(instanceID string) (*config.Config, string, error) {
	instanceID = resolveInstanceID(instanceID)
	if err := validateInstanceID(instanceID); err != nil {
		return nil, "", err
	}
	configPath := instanceConfigPath(instanceID)
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		return nil, configPath, err
	}
	return cfg, configPath, nil
}

func saveInstanceConfig(instanceID string, cfg *config.Config, opts configMutationOptions) error {
	instanceID = resolveInstanceID(instanceID)
	if err := validateInstanceID(instanceID); err != nil {
		return err
	}
	cfgPath := instanceConfigPath(instanceID)
	if !opts.SkipHistory {
		if err := backupConfigToHistory(instanceID, cfgPath); err != nil {
			return err
		}
	}
	return config.SaveConfig(cfgPath, cfg)
}

func backupConfigToHistory(instanceID string, cfgPath string) error {
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read existing config for history backup: %w", err)
	}
	historyDir := configHistoryDir(instanceID)
	if err := os.MkdirAll(historyDir, 0o755); err != nil {
		return fmt.Errorf("create history dir: %w", err)
	}
	name := time.Now().UTC().Format("20060102T150405.000000000Z") + ".json"
	historyPath := filepath.Join(historyDir, name)
	if err := os.WriteFile(historyPath, raw, 0o600); err != nil {
		return fmt.Errorf("write history snapshot: %w", err)
	}
	return nil
}

func configHistoryEntries(instanceID string) ([]string, error) {
	historyDir := configHistoryDir(instanceID)
	entries, err := os.ReadDir(historyDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(strings.ToLower(e.Name()), ".json") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

func restoreConfigFromHistory(instanceID string, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("history id is required")
	}
	cfgPath := instanceConfigPath(instanceID)
	snapshotPath := filepath.Join(configHistoryDir(instanceID), id)
	raw, err := os.ReadFile(snapshotPath)
	if err != nil {
		return fmt.Errorf("read history snapshot %s: %w", id, err)
	}
	var cfg config.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("invalid snapshot %s: %w", id, err)
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("snapshot %s failed validation: %w", id, err)
	}
	if err := backupConfigToHistory(instanceID, cfgPath); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(cfgPath, raw, 0o600); err != nil {
		return fmt.Errorf("restore snapshot %s: %w", id, err)
	}
	return nil
}

