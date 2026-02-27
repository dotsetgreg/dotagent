package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dotsetgreg/dotagent/pkg/config"
	"github.com/google/uuid"
)

type ConfigRequest struct {
	ID           string `json:"id"`
	Key          string `json:"key"`
	ValuePreview string `json:"value_preview,omitempty"`
	ValueDigest  string `json:"value_digest,omitempty"`
	// ValueRaw is retained for backward compatibility with pre-redaction request files.
	ValueRaw     string `json:"value_raw,omitempty"`
	Rationale    string `json:"rationale"`
	Status       string `json:"status"`
	ApprovedBy   string `json:"approved_by,omitempty"`
	ApprovalNote string `json:"approval_note,omitempty"`
	Error        string `json:"error,omitempty"`
	CreatedAt    int64  `json:"created_at"`
	UpdatedAt    int64  `json:"updated_at"`
	ApprovedAt   int64  `json:"approved_at,omitempty"`
	AppliedAt    int64  `json:"applied_at,omitempty"`
}

type ConfigRequestStore struct {
	Requests []ConfigRequest `json:"requests"`
}

type ConfigRestartPending struct {
	RequestID      string `json:"request_id"`
	Key            string `json:"key"`
	BackupPath     string `json:"backup_path"`
	CreatedAt      int64  `json:"created_at"`
	DeadlineAt     int64  `json:"deadline_at"`
	CheckTimeoutMS int64  `json:"check_timeout_ms"`
}

type ConfigApplyOptions struct {
	ConfigPath         string
	HistoryDir         string
	AuditLogPath       string
	RequestsPath       string
	PendingRestartPath string
	RequireApproval    bool
	MutableKeys        []string
	OnRestartRequest   func(ctx context.Context) error
	PostApplyCheck     func(ctx context.Context) error
	PostApplyTimeout   time.Duration
}

type ConfigRequestTool struct {
	opts ConfigApplyOptions
}

type ConfigApplyTool struct {
	opts ConfigApplyOptions
}

func NewConfigRequestTool(opts ConfigApplyOptions) *ConfigRequestTool {
	return &ConfigRequestTool{opts: opts}
}

func NewConfigApplyTool(opts ConfigApplyOptions) *ConfigApplyTool {
	return &ConfigApplyTool{opts: opts}
}

func (t *ConfigRequestTool) Name() string {
	return "config_request"
}

func (t *ConfigRequestTool) Description() string {
	return "Propose and inspect guarded runtime configuration changes. Actions: propose, list, show."
}

func (t *ConfigRequestTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"action": map[string]interface{}{
				"type": "string",
				"enum": []string{"propose", "list", "show"},
			},
			"key": map[string]interface{}{
				"type": "string",
			},
			"value": map[string]interface{}{
				"type": "string",
			},
			"rationale": map[string]interface{}{
				"type": "string",
			},
			"request_id": map[string]interface{}{
				"type": "string",
			},
		},
		"required": []string{"action"},
	}
}

func (t *ConfigRequestTool) Execute(ctx context.Context, args map[string]interface{}) *ToolResult {
	action, _ := args["action"].(string)
	action = strings.ToLower(strings.TrimSpace(action))
	switch action {
	case "propose":
		key, _ := args["key"].(string)
		key = strings.TrimSpace(key)
		if key == "" {
			return ErrorResult("key is required for action=propose")
		}
		valueRaw, _ := args["value"].(string)
		if strings.TrimSpace(valueRaw) == "" {
			return ErrorResult("value is required for action=propose")
		}
		rationale, _ := args["rationale"].(string)

		if !isMutableConfigKey(t.opts.MutableKeys, key) {
			return ErrorResult(fmt.Sprintf("config key %q is not mutable by policy", key))
		}

		store, err := loadConfigRequestStore(t.opts.RequestsPath)
		if err != nil {
			return ErrorResult(err.Error())
		}
		now := time.Now().UnixMilli()
		req := ConfigRequest{
			ID:           "cfgreq-" + uuid.NewString(),
			Key:          key,
			ValuePreview: previewConfigValue(key, valueRaw),
			ValueDigest:  digestConfigRawValue(valueRaw),
			Rationale:    strings.TrimSpace(rationale),
			Status:       "pending",
			CreatedAt:    now,
			UpdatedAt:    now,
		}
		store.Requests = append(store.Requests, req)
		if err := saveConfigRequestStore(t.opts.RequestsPath, store); err != nil {
			return ErrorResult(err.Error())
		}
		_ = appendConfigAuditLog(t.opts.AuditLogPath, map[string]any{
			"timestamp":       now,
			"action":          "proposed",
			"request_id":      req.ID,
			"key":             req.Key,
			"value_preview":   req.ValuePreview,
			"value_digest":    req.ValueDigest,
			"sensitive_value": isSensitiveConfigKey(req.Key),
			"rationale":       req.Rationale,
		})
		return UserResult(fmt.Sprintf("Proposed config request %s for %s. Ask user to approve with `dotagent config approve %s` before applying.", req.ID, req.Key, req.ID))
	case "list":
		store, err := loadConfigRequestStore(t.opts.RequestsPath)
		if err != nil {
			return ErrorResult(err.Error())
		}
		if len(store.Requests) == 0 {
			return UserResult("No config requests.")
		}
		lines := []string{"Config requests:"}
		for _, req := range store.Requests {
			preview := strings.TrimSpace(req.ValuePreview)
			if preview == "" {
				preview = "(no preview)"
			}
			lines = append(lines, fmt.Sprintf("- %s [%s] %s=%s", req.ID, req.Status, req.Key, preview))
		}
		return UserResult(strings.Join(lines, "\n"))
	case "show":
		reqID, _ := args["request_id"].(string)
		reqID = strings.TrimSpace(reqID)
		if reqID == "" {
			return ErrorResult("request_id is required for action=show")
		}
		store, err := loadConfigRequestStore(t.opts.RequestsPath)
		if err != nil {
			return ErrorResult(err.Error())
		}
		for _, req := range store.Requests {
			if req.ID == reqID {
				raw, _ := json.MarshalIndent(req, "", "  ")
				return UserResult(string(raw))
			}
		}
		return ErrorResult("request not found")
	default:
		return ErrorResult("action must be one of: propose, list, show")
	}
}

func (t *ConfigApplyTool) Name() string {
	return "config_apply"
}

func (t *ConfigApplyTool) Description() string {
	return "Apply an approved config request with validation, history backup, and restart trigger. Actions: apply."
}

func (t *ConfigApplyTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"action": map[string]interface{}{
				"type": "string",
				"enum": []string{"apply"},
			},
			"request_id": map[string]interface{}{
				"type": "string",
			},
			"value": map[string]interface{}{
				"type": "string",
			},
			"restart": map[string]interface{}{
				"type": "boolean",
			},
		},
		"required": []string{"action", "request_id", "value"},
	}
}

func (t *ConfigApplyTool) Execute(ctx context.Context, args map[string]interface{}) *ToolResult {
	action, _ := args["action"].(string)
	action = strings.ToLower(strings.TrimSpace(action))
	if action != "apply" {
		return ErrorResult("action must be apply")
	}

	reqID, _ := args["request_id"].(string)
	reqID = strings.TrimSpace(reqID)
	if reqID == "" {
		return ErrorResult("request_id is required")
	}
	valueRaw, ok := args["value"].(string)
	if !ok {
		return ErrorResult("value is required")
	}
	if strings.TrimSpace(valueRaw) == "" {
		return ErrorResult("value is required")
	}

	restart := true
	if v, ok := args["restart"].(bool); ok {
		restart = v
	}
	store, err := loadConfigRequestStore(t.opts.RequestsPath)
	if err != nil {
		return ErrorResult(err.Error())
	}
	var req *ConfigRequest
	for i := range store.Requests {
		if store.Requests[i].ID == reqID {
			req = &store.Requests[i]
			break
		}
	}
	if req == nil {
		return ErrorResult("request not found")
	}
	if t.opts.RequireApproval && req.Status != "approved" {
		return ErrorResult("request is not approved; explicit operator approval is required")
	}
	if !t.opts.RequireApproval && req.Status != "pending" && req.Status != "approved" {
		return ErrorResult(fmt.Sprintf("request status is %s (expected pending or approved)", req.Status))
	}
	if !isMutableConfigKey(t.opts.MutableKeys, req.Key) {
		return ErrorResult(fmt.Sprintf("config key %q is not mutable by policy", req.Key))
	}

	expectedDigest := strings.TrimSpace(req.ValueDigest)
	if expectedDigest == "" && strings.TrimSpace(req.ValueRaw) != "" {
		expectedDigest = digestConfigRawValue(req.ValueRaw)
	}
	providedDigest := digestConfigRawValue(valueRaw)
	if expectedDigest != "" && expectedDigest != providedDigest {
		return ErrorResult("provided value does not match approved proposal digest")
	}

	now := time.Now().UnixMilli()
	preview := req.ValuePreview
	if strings.TrimSpace(preview) == "" {
		preview = previewConfigValue(req.Key, valueRaw)
	}
	_ = appendConfigAuditLog(t.opts.AuditLogPath, map[string]any{
		"timestamp":         now,
		"action":            "apply_started",
		"request_id":        req.ID,
		"key":               req.Key,
		"value_preview":     preview,
		"value_digest":      providedDigest,
		"restart_requested": restart,
	})

	backupPath, rolledBack, applyErr := applyConfigRequest(ctx, t.opts, *req, valueRaw, restart)
	now = time.Now().UnixMilli()
	if applyErr != nil {
		req.Status = "failed"
		req.Error = applyErr.Error()
		req.UpdatedAt = now
		_ = saveConfigRequestStore(t.opts.RequestsPath, store)
		_ = appendConfigAuditLog(t.opts.AuditLogPath, map[string]any{
			"timestamp":     now,
			"action":        "apply_failed",
			"request_id":    req.ID,
			"key":           req.Key,
			"value_preview": preview,
			"value_digest":  providedDigest,
			"backup_path":   backupPath,
			"rolled_back":   rolledBack,
			"error":         applyErr.Error(),
		})
		return ErrorResult(fmt.Sprintf("config apply failed: %v", applyErr))
	}
	req.Status = "applied"
	req.Error = ""
	req.UpdatedAt = now
	req.AppliedAt = now
	if err := saveConfigRequestStore(t.opts.RequestsPath, store); err != nil {
		return ErrorResult(err.Error())
	}
	_ = appendConfigAuditLog(t.opts.AuditLogPath, map[string]any{
		"timestamp":     now,
		"action":        "applied",
		"request_id":    req.ID,
		"key":           req.Key,
		"value_preview": preview,
		"value_digest":  providedDigest,
		"backup_path":   backupPath,
		"restart":       restart,
	})
	msg := fmt.Sprintf("Applied config request %s (%s).", req.ID, req.Key)
	if restart {
		msg += " Restart requested."
	}
	return UserResult(msg)
}

var configApplyMu sync.Mutex

func applyConfigRequest(ctx context.Context, opts ConfigApplyOptions, req ConfigRequest, valueRaw string, restart bool) (string, bool, error) {
	configApplyMu.Lock()
	defer configApplyMu.Unlock()

	currentRaw, err := os.ReadFile(opts.ConfigPath)
	if err != nil {
		return "", false, fmt.Errorf("read config: %w", err)
	}
	if err := os.MkdirAll(opts.HistoryDir, 0o755); err != nil {
		return "", false, fmt.Errorf("create history dir: %w", err)
	}
	backupPath := filepath.Join(opts.HistoryDir, time.Now().UTC().Format("20060102T150405.000000000Z")+"-"+req.ID+".json")
	if err := os.WriteFile(backupPath, currentRaw, 0o600); err != nil {
		return "", false, fmt.Errorf("backup config: %w", err)
	}

	cfgMap := map[string]any{}
	if err := json.Unmarshal(currentRaw, &cfgMap); err != nil {
		return backupPath, false, fmt.Errorf("decode config: %w", err)
	}
	value := parseConfigRawValue(valueRaw)
	if err := setConfigDotPath(cfgMap, req.Key, value); err != nil {
		return backupPath, false, err
	}
	nextRaw, err := json.Marshal(cfgMap)
	if err != nil {
		return backupPath, false, err
	}
	candidate := &config.Config{}
	if err := json.Unmarshal(nextRaw, candidate); err != nil {
		return backupPath, false, err
	}
	if err := candidate.Validate(); err != nil {
		return backupPath, false, err
	}
	if err := writeAtomicFile(opts.ConfigPath, mustMarshalIndent(candidate), 0o600); err != nil {
		return backupPath, false, fmt.Errorf("write config: %w", err)
	}

	if err := runConfigApplyCheck(ctx, opts); err != nil {
		rollbackErr := writeAtomicFile(opts.ConfigPath, currentRaw, 0o600)
		if rollbackErr != nil {
			return backupPath, false, fmt.Errorf("post-apply readiness failed and rollback failed: %w (rollback error: %v)", err, rollbackErr)
		}
		return backupPath, true, fmt.Errorf("post-apply readiness failed, rolled back config: %w", err)
	}

	if restart {
		if strings.TrimSpace(opts.PendingRestartPath) != "" {
			deadline := time.Now().Add(opts.PostApplyTimeout)
			pending := ConfigRestartPending{
				RequestID:      req.ID,
				Key:            req.Key,
				BackupPath:     backupPath,
				CreatedAt:      time.Now().UnixMilli(),
				DeadlineAt:     deadline.UnixMilli(),
				CheckTimeoutMS: opts.PostApplyTimeout.Milliseconds(),
			}
			if err := SaveConfigRestartPending(opts.PendingRestartPath, pending); err != nil {
				rollbackErr := writeAtomicFile(opts.ConfigPath, currentRaw, 0o600)
				if rollbackErr != nil {
					return backupPath, false, fmt.Errorf("persist restart pending failed and rollback failed: %w (rollback error: %v)", err, rollbackErr)
				}
				return backupPath, true, fmt.Errorf("persist restart pending failed, rolled back config: %w", err)
			}
		}
		if opts.OnRestartRequest != nil {
			if err := opts.OnRestartRequest(ctx); err != nil {
				rollbackErr := writeAtomicFile(opts.ConfigPath, currentRaw, 0o600)
				if rollbackErr != nil {
					return backupPath, false, fmt.Errorf("restart request failed and rollback failed: %w (rollback error: %v)", err, rollbackErr)
				}
				_ = RemoveConfigRestartPending(opts.PendingRestartPath)
				return backupPath, true, fmt.Errorf("restart request failed, rolled back config: %w", err)
			}
		}
	}

	return backupPath, false, nil
}

func runConfigApplyCheck(ctx context.Context, opts ConfigApplyOptions) error {
	if opts.PostApplyCheck == nil {
		return nil
	}
	checkCtx := ctx
	cancel := func() {}
	if opts.PostApplyTimeout > 0 {
		checkCtx, cancel = context.WithTimeout(ctx, opts.PostApplyTimeout)
	}
	defer cancel()
	return opts.PostApplyCheck(checkCtx)
}

func isMutableConfigKey(mutable []string, key string) bool {
	key = strings.TrimSpace(key)
	for _, item := range mutable {
		if strings.TrimSpace(item) == key {
			return true
		}
	}
	return false
}

func parseConfigRawValue(raw string) any {
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

func setConfigDotPath(root map[string]any, key string, value any) error {
	parts := strings.Split(strings.TrimSpace(key), ".")
	if len(parts) == 0 {
		return fmt.Errorf("key is required")
	}
	node := root
	for i := 0; i < len(parts)-1; i++ {
		part := strings.TrimSpace(parts[i])
		if part == "" {
			return fmt.Errorf("invalid key %q", key)
		}
		next, ok := node[part]
		if !ok {
			child := map[string]any{}
			node[part] = child
			node = child
			continue
		}
		child, ok := next.(map[string]any)
		if !ok {
			return fmt.Errorf("key prefix %q is not an object", strings.Join(parts[:i+1], "."))
		}
		node = child
	}
	node[strings.TrimSpace(parts[len(parts)-1])] = value
	return nil
}

func LoadConfigRequestStore(path string) (ConfigRequestStore, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ConfigRequestStore{Requests: []ConfigRequest{}}, nil
		}
		return ConfigRequestStore{}, err
	}
	var store ConfigRequestStore
	if err := json.Unmarshal(raw, &store); err != nil {
		return ConfigRequestStore{}, err
	}
	if store.Requests == nil {
		store.Requests = []ConfigRequest{}
	}
	return store, nil
}

func SaveConfigRequestStore(path string, store ConfigRequestStore) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

func AppendConfigAuditLog(path string, entry map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	raw, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(raw, '\n')); err != nil {
		return err
	}
	return nil
}

func SaveConfigRestartPending(path string, pending ConfigRestartPending) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	raw, err := json.MarshalIndent(pending, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomicFile(path, raw, 0o600)
}

func LoadConfigRestartPending(path string) (*ConfigRestartPending, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	pending := &ConfigRestartPending{}
	if err := json.Unmarshal(raw, pending); err != nil {
		return nil, err
	}
	return pending, nil
}

func RemoveConfigRestartPending(path string) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func RollbackConfigFromBackup(configPath, backupPath string) error {
	backupRaw, err := os.ReadFile(backupPath)
	if err != nil {
		return fmt.Errorf("read backup: %w", err)
	}
	return writeAtomicFile(configPath, backupRaw, 0o600)
}

func loadConfigRequestStore(path string) (ConfigRequestStore, error) {
	return LoadConfigRequestStore(path)
}

func saveConfigRequestStore(path string, store ConfigRequestStore) error {
	return SaveConfigRequestStore(path, store)
}

func appendConfigAuditLog(path string, entry map[string]any) error {
	return AppendConfigAuditLog(path, entry)
}

func mustMarshalIndent(cfg *config.Config) []byte {
	raw, _ := json.MarshalIndent(cfg, "", "  ")
	return raw
}

func writeAtomicFile(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	tmpFile, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()
	if err := tmpFile.Chmod(perm); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

func digestConfigRawValue(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func previewConfigValue(key, raw string) string {
	if isSensitiveConfigKey(key) {
		return "<redacted>"
	}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	var parsed any
	if err := json.Unmarshal([]byte(trimmed), &parsed); err == nil {
		reencoded, marshalErr := json.Marshal(parsed)
		if marshalErr == nil {
			trimmed = string(reencoded)
		}
	}
	if len(trimmed) > 80 {
		return trimmed[:80] + "..."
	}
	return trimmed
}

func isSensitiveConfigKey(key string) bool {
	k := strings.ToLower(strings.TrimSpace(key))
	if k == "" {
		return false
	}
	for _, token := range []string{"token", "api_key", "apikey", "secret", "password", "oauth", "private_key"} {
		if strings.Contains(k, token) {
			return true
		}
	}
	return false
}
