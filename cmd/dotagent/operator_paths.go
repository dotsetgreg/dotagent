package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	defaultInstanceID = "default"
)

var instanceIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

func resolveInstanceID(raw string) string {
	id := strings.TrimSpace(raw)
	if id == "" {
		id = strings.TrimSpace(os.Getenv("DOTAGENT_INSTANCE"))
	}
	if id == "" {
		id = defaultInstanceID
	}
	return id
}

func validateInstanceID(id string) error {
	if !instanceIDPattern.MatchString(id) {
		return fmt.Errorf("invalid instance id %q", id)
	}
	return nil
}

func dotagentHomeDir() string {
	if v := strings.TrimSpace(os.Getenv("DOTAGENT_HOME")); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".dotagent"
	}
	return filepath.Join(home, ".dotagent")
}

func instanceRootDir(instanceID string) string {
	return filepath.Join(dotagentHomeDir(), "instances", instanceID)
}

func instanceConfigPath(instanceID string) string {
	return filepath.Join(instanceRootDir(instanceID), "config", "config.json")
}

func legacyConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".dotagent", "config.json")
	}
	return filepath.Join(home, ".dotagent", "config.json")
}

func legacyWorkspacePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".dotagent", "workspace")
	}
	return filepath.Join(home, ".dotagent", "workspace")
}

func configHistoryDir(instanceID string) string {
	return filepath.Join(instanceRootDir(instanceID), "config", "history")
}

func runtimeDir(instanceID string) string {
	return filepath.Join(instanceRootDir(instanceID), "runtime")
}

func logsDir(instanceID string) string {
	return filepath.Join(instanceRootDir(instanceID), "logs")
}

func dataDir(instanceID string) string {
	return filepath.Join(instanceRootDir(instanceID), "data")
}

func workspaceDir(instanceID string) string {
	return filepath.Join(instanceRootDir(instanceID), "workspace")
}
