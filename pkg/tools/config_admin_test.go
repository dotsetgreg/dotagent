package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dotsetgreg/dotagent/pkg/config"
)

func TestConfigRequestAndApplyFlow_ApprovalRequired(t *testing.T) {
	root := t.TempDir()
	cfgPath := filepath.Join(root, "config", "config.json")
	historyDir := filepath.Join(root, "config", "history")
	requestsPath := filepath.Join(root, "runtime", "config_requests.json")
	auditPath := filepath.Join(root, "runtime", "config_audit.log")
	pendingPath := filepath.Join(root, "runtime", "config_restart_pending.json")

	cfg := config.DefaultConfigForInstance("default")
	if err := config.SaveConfig(cfgPath, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}

	restartCalled := false
	opts := ConfigApplyOptions{
		ConfigPath:         cfgPath,
		HistoryDir:         historyDir,
		AuditLogPath:       auditPath,
		RequestsPath:       requestsPath,
		PendingRestartPath: pendingPath,
		RequireApproval:    true,
		MutableKeys:        []string{"channels.discord.token"},
		OnRestartRequest: func(ctx context.Context) error {
			restartCalled = true
			return nil
		},
	}

	reqTool := NewConfigRequestTool(opts)
	res := reqTool.Execute(context.Background(), map[string]interface{}{
		"action":    "propose",
		"key":       "channels.discord.token",
		"value":     `"abc123"`,
		"rationale": "set runtime token",
	})
	if res.IsError {
		t.Fatalf("propose should succeed: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "cfgreq-") {
		t.Fatalf("expected request id in result, got %q", res.ForLLM)
	}

	store, err := loadConfigRequestStore(requestsPath)
	if err != nil {
		t.Fatalf("load request store: %v", err)
	}
	if len(store.Requests) != 1 {
		t.Fatalf("expected one request, got %d", len(store.Requests))
	}
	reqID := store.Requests[0].ID
	if strings.TrimSpace(store.Requests[0].ValueRaw) != "" {
		t.Fatalf("expected value_raw to be empty for secret-safe storage")
	}
	if strings.TrimSpace(store.Requests[0].ValuePreview) != "<redacted>" {
		t.Fatalf("expected redacted preview, got %q", store.Requests[0].ValuePreview)
	}
	if strings.TrimSpace(store.Requests[0].ValueDigest) == "" {
		t.Fatalf("expected request digest to be set")
	}

	applyTool := NewConfigApplyTool(opts)
	fail := applyTool.Execute(context.Background(), map[string]interface{}{
		"action":     "apply",
		"request_id": reqID,
		"value":      `"abc123"`,
	})
	if !fail.IsError {
		t.Fatalf("expected apply without explicit approval state to fail")
	}
	if !strings.Contains(strings.ToLower(fail.ForLLM), "not approved") {
		t.Fatalf("expected not approved message, got %q", fail.ForLLM)
	}

	store.Requests[0].Status = "approved"
	store.Requests[0].ApprovedBy = "test-operator"
	store.Requests[0].ApprovedAt = time.Now().UnixMilli()
	store.Requests[0].UpdatedAt = store.Requests[0].ApprovedAt
	if err := saveConfigRequestStore(requestsPath, store); err != nil {
		t.Fatalf("save approved store: %v", err)
	}

	ok := applyTool.Execute(context.Background(), map[string]interface{}{
		"action":     "apply",
		"request_id": reqID,
		"value":      `"abc123"`,
		"restart":    true,
	})
	if ok.IsError {
		t.Fatalf("apply should succeed: %s", ok.ForLLM)
	}
	if !restartCalled {
		t.Fatalf("expected restart callback to be called")
	}

	loaded, err := config.LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("load updated config: %v", err)
	}
	if strings.TrimSpace(loaded.Channels.Discord.Token) != "abc123" {
		t.Fatalf("expected updated token, got %q", loaded.Channels.Discord.Token)
	}

	if _, err := os.Stat(pendingPath); err != nil {
		t.Fatalf("expected pending restart marker, got: %v", err)
	}

	auditRaw, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	auditText := string(auditRaw)
	if !strings.Contains(auditText, `"action":"proposed"`) {
		t.Fatalf("expected proposed audit entry, got %q", auditText)
	}
	if !strings.Contains(auditText, `"action":"apply_started"`) {
		t.Fatalf("expected apply_started audit entry, got %q", auditText)
	}
	if !strings.Contains(auditText, `"action":"applied"`) {
		t.Fatalf("expected applied audit entry, got %q", auditText)
	}
	if strings.Contains(auditText, "abc123") {
		t.Fatalf("audit log should not contain raw secret values: %q", auditText)
	}
}

func TestConfigApplyRollsBackWhenPostApplyCheckFails(t *testing.T) {
	root := t.TempDir()
	cfgPath := filepath.Join(root, "config", "config.json")
	historyDir := filepath.Join(root, "config", "history")
	requestsPath := filepath.Join(root, "runtime", "config_requests.json")
	auditPath := filepath.Join(root, "runtime", "config_audit.log")

	cfg := config.DefaultConfigForInstance("default")
	cfg.Channels.Discord.Token = "initial-token"
	if err := config.SaveConfig(cfgPath, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}

	opts := ConfigApplyOptions{
		ConfigPath:      cfgPath,
		HistoryDir:      historyDir,
		AuditLogPath:    auditPath,
		RequestsPath:    requestsPath,
		RequireApproval: true,
		MutableKeys:     []string{"channels.discord.token"},
		PostApplyCheck: func(ctx context.Context) error {
			return context.DeadlineExceeded
		},
		PostApplyTimeout: 10 * time.Millisecond,
	}

	reqTool := NewConfigRequestTool(opts)
	res := reqTool.Execute(context.Background(), map[string]interface{}{
		"action":    "propose",
		"key":       "channels.discord.token",
		"value":     `"new-token"`,
		"rationale": "test rollback",
	})
	if res.IsError {
		t.Fatalf("propose should succeed: %s", res.ForLLM)
	}

	store, err := loadConfigRequestStore(requestsPath)
	if err != nil {
		t.Fatalf("load request store: %v", err)
	}
	if len(store.Requests) != 1 {
		t.Fatalf("expected one request, got %d", len(store.Requests))
	}
	store.Requests[0].Status = "approved"
	store.Requests[0].ApprovedBy = "test-operator"
	store.Requests[0].ApprovedAt = time.Now().UnixMilli()
	store.Requests[0].UpdatedAt = store.Requests[0].ApprovedAt
	if err := saveConfigRequestStore(requestsPath, store); err != nil {
		t.Fatalf("save approved store: %v", err)
	}

	applyTool := NewConfigApplyTool(opts)
	result := applyTool.Execute(context.Background(), map[string]interface{}{
		"action":     "apply",
		"request_id": store.Requests[0].ID,
		"value":      `"new-token"`,
		"restart":    false,
	})
	if !result.IsError {
		t.Fatalf("expected apply to fail when post-apply check fails")
	}
	if !strings.Contains(result.ForLLM, "rolled back config") {
		t.Fatalf("expected rollback error message, got %q", result.ForLLM)
	}

	loaded, err := config.LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("load config after failed apply: %v", err)
	}
	if strings.TrimSpace(loaded.Channels.Discord.Token) != "initial-token" {
		t.Fatalf("expected token rollback to initial value, got %q", loaded.Channels.Discord.Token)
	}

	store, err = loadConfigRequestStore(requestsPath)
	if err != nil {
		t.Fatalf("load request store after apply: %v", err)
	}
	if got := store.Requests[0].Status; got != "failed" {
		t.Fatalf("expected failed request status, got %q", got)
	}

	auditRaw, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	if !strings.Contains(string(auditRaw), `"action":"apply_failed"`) {
		t.Fatalf("expected apply_failed audit entry, got %q", string(auditRaw))
	}
	if !strings.Contains(string(auditRaw), `"rolled_back":true`) {
		t.Fatalf("expected rolled_back=true audit flag, got %q", string(auditRaw))
	}
}

func TestConfigApplyRejectsValueDigestMismatch(t *testing.T) {
	root := t.TempDir()
	cfgPath := filepath.Join(root, "config", "config.json")
	historyDir := filepath.Join(root, "config", "history")
	requestsPath := filepath.Join(root, "runtime", "config_requests.json")
	auditPath := filepath.Join(root, "runtime", "config_audit.log")

	cfg := config.DefaultConfigForInstance("default")
	if err := config.SaveConfig(cfgPath, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}

	opts := ConfigApplyOptions{
		ConfigPath:      cfgPath,
		HistoryDir:      historyDir,
		AuditLogPath:    auditPath,
		RequestsPath:    requestsPath,
		RequireApproval: true,
		MutableKeys:     []string{"channels.discord.token"},
	}

	reqTool := NewConfigRequestTool(opts)
	res := reqTool.Execute(context.Background(), map[string]interface{}{
		"action":    "propose",
		"key":       "channels.discord.token",
		"value":     `"expected-token"`,
		"rationale": "digest check",
	})
	if res.IsError {
		t.Fatalf("propose should succeed: %s", res.ForLLM)
	}

	store, err := loadConfigRequestStore(requestsPath)
	if err != nil {
		t.Fatalf("load request store: %v", err)
	}
	store.Requests[0].Status = "approved"
	store.Requests[0].ApprovedBy = "test-operator"
	store.Requests[0].ApprovedAt = time.Now().UnixMilli()
	store.Requests[0].UpdatedAt = store.Requests[0].ApprovedAt
	if err := saveConfigRequestStore(requestsPath, store); err != nil {
		t.Fatalf("save approved store: %v", err)
	}

	applyTool := NewConfigApplyTool(opts)
	result := applyTool.Execute(context.Background(), map[string]interface{}{
		"action":     "apply",
		"request_id": store.Requests[0].ID,
		"value":      `"different-token"`,
	})
	if !result.IsError {
		t.Fatalf("expected digest mismatch to fail")
	}
	if !strings.Contains(strings.ToLower(result.ForLLM), "digest") {
		t.Fatalf("expected digest mismatch error, got %q", result.ForLLM)
	}
}
