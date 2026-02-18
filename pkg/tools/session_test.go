package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/dotsetgreg/dotagent/pkg/memory"
)

type mockSessionService struct {
	sessions []memory.Session
	events   []memory.Event
}

func (m *mockSessionService) ListSessions(ctx context.Context, userID string, limit int) ([]memory.Session, error) {
	if len(m.sessions) > limit {
		return m.sessions[:limit], nil
	}
	return m.sessions, nil
}

func (m *mockSessionService) GetSession(ctx context.Context, sessionKey string) (memory.Session, error) {
	for _, s := range m.sessions {
		if s.SessionKey == sessionKey {
			return s, nil
		}
	}
	return memory.Session{}, nil
}

func (m *mockSessionService) ListSessionEvents(ctx context.Context, sessionKey string, limit int) ([]memory.Event, error) {
	if len(m.events) > limit {
		return m.events[:limit], nil
	}
	return m.events, nil
}

func TestSessionTool_ListAndStatus(t *testing.T) {
	svc := &mockSessionService{
		sessions: []memory.Session{
			{
				SessionKey:   "s1",
				Channel:      "discord",
				ChatID:       "chat1",
				UserID:       "u1",
				MessageCount: 3,
				UpdatedAtMS:  123,
			},
		},
	}
	tool := NewSessionTool(svc, nil, nil)

	list := tool.Execute(context.Background(), map[string]interface{}{"action": "list"})
	if list.IsError {
		t.Fatalf("list should succeed: %s", list.ForLLM)
	}
	if !strings.Contains(list.ForLLM, "s1") {
		t.Fatalf("expected list output to include session key")
	}

	status := tool.Execute(context.Background(), map[string]interface{}{
		"action":      "status",
		"session_key": "s1",
	})
	if status.IsError {
		t.Fatalf("status should succeed: %s", status.ForLLM)
	}
	if !strings.Contains(status.ForLLM, "Session s1") {
		t.Fatalf("expected status output to include session details")
	}
}

func TestSessionTool_SendUsesExecutor(t *testing.T) {
	svc := &mockSessionService{}
	called := false
	tool := NewSessionTool(
		svc,
		func(channel, chatID, userID string) (string, error) { return "resolved-session", nil },
		func(ctx context.Context, content, sessionKey, channel, chatID string) (string, error) {
			called = true
			return "ok", nil
		},
	)
	tool.SetContext("discord", "chat-1")

	res := tool.Execute(context.Background(), map[string]interface{}{
		"action":  "send",
		"message": "hello",
	})
	if res.IsError {
		t.Fatalf("send should succeed: %s", res.ForLLM)
	}
	if !called {
		t.Fatalf("expected executor to be called")
	}
}
