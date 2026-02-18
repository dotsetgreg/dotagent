package tools

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/dotsetgreg/dotagent/pkg/memory"
	"github.com/google/uuid"
)

type SessionService interface {
	ListSessions(ctx context.Context, userID string, limit int) ([]memory.Session, error)
	GetSession(ctx context.Context, sessionKey string) (memory.Session, error)
	ListSessionEvents(ctx context.Context, sessionKey string, limit int) ([]memory.Event, error)
}

type SessionKeyResolver func(channel, chatID, userID string) (string, error)
type SessionExecutor func(ctx context.Context, content, sessionKey, channel, chatID string) (string, error)

type SessionTool struct {
	service  SessionService
	resolver SessionKeyResolver
	exec     SessionExecutor

	mu      sync.RWMutex
	channel string
	chatID  string
}

func NewSessionTool(service SessionService, resolver SessionKeyResolver, exec SessionExecutor) *SessionTool {
	return &SessionTool{
		service:  service,
		resolver: resolver,
		exec:     exec,
		channel:  "cli",
		chatID:   "direct",
	}
}

func (t *SessionTool) Name() string {
	return "session"
}

func (t *SessionTool) Description() string {
	return "Inspect and operate on sessions. Actions: list, status, history, send, spawn."
}

func (t *SessionTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"action": map[string]interface{}{
				"type":        "string",
				"enum":        []string{"list", "status", "history", "send", "spawn"},
				"description": "Session action.",
			},
			"session_key": map[string]interface{}{
				"type":        "string",
				"description": "Target session key (optional for status/history, required for send unless spawn).",
			},
			"user_id": map[string]interface{}{
				"type":        "string",
				"description": "Optional user ID scope for listing sessions.",
			},
			"message": map[string]interface{}{
				"type":        "string",
				"description": "Message to send (required for send/spawn).",
			},
			"limit": map[string]interface{}{
				"type":        "integer",
				"description": "Maximum records for list/history. Default 20.",
				"minimum":     1.0,
				"maximum":     100.0,
			},
			"channel": map[string]interface{}{
				"type":        "string",
				"description": "Optional channel override for send/spawn.",
			},
			"chat_id": map[string]interface{}{
				"type":        "string",
				"description": "Optional chat ID override for send/spawn.",
			},
		},
		"required": []string{"action"},
	}
}

func (t *SessionTool) SetContext(channel, chatID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if strings.TrimSpace(channel) != "" {
		t.channel = channel
	}
	if strings.TrimSpace(chatID) != "" {
		t.chatID = chatID
	}
}

func (t *SessionTool) Execute(ctx context.Context, args map[string]interface{}) *ToolResult {
	if t.service == nil {
		return ErrorResult("session service is unavailable")
	}
	action, _ := args["action"].(string)
	action = strings.ToLower(strings.TrimSpace(action))
	switch action {
	case "list":
		return t.list(ctx, args)
	case "status":
		return t.status(ctx, args)
	case "history":
		return t.history(ctx, args)
	case "send":
		return t.send(ctx, args, false)
	case "spawn":
		return t.send(ctx, args, true)
	default:
		return ErrorResult("action must be one of: list, status, history, send, spawn")
	}
}

func (t *SessionTool) list(ctx context.Context, args map[string]interface{}) *ToolResult {
	userID, _ := args["user_id"].(string)
	limit := parseLimit(args["limit"], 20)
	sessions, err := t.service.ListSessions(ctx, strings.TrimSpace(userID), limit)
	if err != nil {
		return ErrorResult(fmt.Sprintf("list sessions failed: %v", err))
	}
	if len(sessions) == 0 {
		return SilentResult("No sessions found.")
	}
	lines := []string{"Sessions:"}
	for _, s := range sessions {
		lines = append(lines, fmt.Sprintf("- %s [%s/%s] messages=%d updated=%d", s.SessionKey, s.Channel, s.ChatID, s.MessageCount, s.UpdatedAtMS))
	}
	return SilentResult(strings.Join(lines, "\n"))
}

func (t *SessionTool) status(ctx context.Context, args map[string]interface{}) *ToolResult {
	sk, err := t.resolveSessionKey(args, "")
	if err != nil {
		return ErrorResult(err.Error())
	}
	s, err := t.service.GetSession(ctx, sk)
	if err != nil {
		return ErrorResult(fmt.Sprintf("get session failed: %v", err))
	}
	return SilentResult(fmt.Sprintf("Session %s\n- Channel: %s\n- Chat ID: %s\n- User: %s\n- Messages: %d\n- Updated: %d",
		s.SessionKey,
		valueOrUnsetStr(s.Channel),
		valueOrUnsetStr(s.ChatID),
		valueOrUnsetStr(s.UserID),
		s.MessageCount,
		s.UpdatedAtMS,
	))
}

func (t *SessionTool) history(ctx context.Context, args map[string]interface{}) *ToolResult {
	sk, err := t.resolveSessionKey(args, "")
	if err != nil {
		return ErrorResult(err.Error())
	}
	limit := parseLimit(args["limit"], 20)
	events, err := t.service.ListSessionEvents(ctx, sk, limit)
	if err != nil {
		return ErrorResult(fmt.Sprintf("list session history failed: %v", err))
	}
	if len(events) == 0 {
		return SilentResult(fmt.Sprintf("No events in session %s", sk))
	}
	lines := []string{fmt.Sprintf("Recent events for %s:", sk)}
	for _, ev := range events {
		lines = append(lines, fmt.Sprintf("- [%s] %s", ev.Role, strings.TrimSpace(ev.Content)))
	}
	return SilentResult(strings.Join(lines, "\n"))
}

func (t *SessionTool) send(ctx context.Context, args map[string]interface{}, spawn bool) *ToolResult {
	message, _ := args["message"].(string)
	message = strings.TrimSpace(message)
	if message == "" {
		return ErrorResult("message is required")
	}

	channel, chatID := t.currentContext()
	if raw, ok := args["channel"].(string); ok && strings.TrimSpace(raw) != "" {
		channel = strings.TrimSpace(raw)
	}
	if raw, ok := args["chat_id"].(string); ok && strings.TrimSpace(raw) != "" {
		chatID = strings.TrimSpace(raw)
	}

	sessionKey, _ := args["session_key"].(string)
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		if spawn {
			sessionKey = "spawn:" + uuid.NewString()
		} else {
			resolved, err := t.resolveSessionKey(args, "")
			if err != nil {
				return ErrorResult(err.Error())
			}
			sessionKey = resolved
		}
	}

	if t.exec == nil {
		return ErrorResult("session executor is unavailable")
	}
	resp, err := t.exec(ctx, message, sessionKey, channel, chatID)
	if err != nil {
		return ErrorResult(fmt.Sprintf("session send failed: %v", err))
	}
	label := "sent"
	if spawn {
		label = "spawned"
	}
	return UserResult(fmt.Sprintf("Session %s %s.\nResponse:\n%s", sessionKey, label, resp))
}

func (t *SessionTool) resolveSessionKey(args map[string]interface{}, fallbackUserID string) (string, error) {
	if sk, ok := args["session_key"].(string); ok && strings.TrimSpace(sk) != "" {
		return strings.TrimSpace(sk), nil
	}
	if t.resolver == nil {
		return "", fmt.Errorf("session_key is required")
	}
	userID := fallbackUserID
	if raw, ok := args["user_id"].(string); ok && strings.TrimSpace(raw) != "" {
		userID = strings.TrimSpace(raw)
	}
	channel, chatID := t.currentContext()
	if channel == "" || chatID == "" {
		return "", fmt.Errorf("session_key is required (no active context)")
	}
	return t.resolver(channel, chatID, userID)
}

func (t *SessionTool) currentContext() (string, string) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.channel, t.chatID
}

func parseLimit(raw interface{}, fallback int) int {
	if raw == nil {
		return fallback
	}
	switch v := raw.(type) {
	case float64:
		if int(v) > 0 {
			return int(v)
		}
	case int:
		if v > 0 {
			return v
		}
	}
	return fallback
}

func valueOrUnsetStr(v string) string {
	if strings.TrimSpace(v) == "" {
		return "(unset)"
	}
	return v
}
