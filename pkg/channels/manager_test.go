package channels

import (
	"context"
	"fmt"
	"testing"

	"github.com/dotsetgreg/dotagent/pkg/bus"
)

type stubChannel struct {
	name      string
	failUntil int
	err       error
	attempts  int
	sent      []bus.OutboundMessage
}

func (s *stubChannel) Name() string                    { return s.name }
func (s *stubChannel) Start(ctx context.Context) error { return nil }
func (s *stubChannel) Stop(ctx context.Context) error  { return nil }
func (s *stubChannel) IsRunning() bool                 { return true }
func (s *stubChannel) IsAllowed(senderID string) bool  { return true }
func (s *stubChannel) Send(ctx context.Context, msg bus.OutboundMessage) error {
	s.attempts++
	if s.err != nil && s.attempts <= s.failUntil {
		return s.err
	}
	s.sent = append(s.sent, msg)
	return nil
}

func TestManager_SendWithRetryRetriesTransientErrors(t *testing.T) {
	m := &Manager{}
	ch := &stubChannel{
		name:      "stub",
		failUntil: 2,
		err:       fmt.Errorf("temporary timeout"),
	}
	msg := bus.OutboundMessage{Channel: "stub", ChatID: "c1", Content: "hello"}
	if err := m.sendWithRetry(context.Background(), ch, msg); err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if ch.attempts != 3 {
		t.Fatalf("expected 3 attempts, got %d", ch.attempts)
	}
	if len(ch.sent) != 1 || ch.sent[0].Content != "hello" {
		t.Fatalf("expected original message delivered once, got %+v", ch.sent)
	}
}

func TestManager_SendWithRetrySendsTerminalFailureNotice(t *testing.T) {
	m := &Manager{}
	ch := &stubChannel{
		name:      "stub",
		failUntil: 3,
		err:       fmt.Errorf("temporary timeout"),
	}
	msg := bus.OutboundMessage{Channel: "stub", ChatID: "c1", Content: "hello"}
	if err := m.sendWithRetry(context.Background(), ch, msg); err == nil {
		t.Fatalf("expected send failure")
	}
	if ch.attempts != 4 {
		t.Fatalf("expected 4 attempts (3 retries + fallback), got %d", ch.attempts)
	}
	if len(ch.sent) != 1 {
		t.Fatalf("expected one delivered fallback message, got %d", len(ch.sent))
	}
	if ch.sent[0].Content == "hello" {
		t.Fatalf("expected fallback content after failures")
	}
}
