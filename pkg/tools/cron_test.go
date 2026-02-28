package tools

import (
	"context"
	"testing"
	"time"

	"github.com/dotsetgreg/dotagent/pkg/bus"
	"github.com/dotsetgreg/dotagent/pkg/cron"
)

type stubCronExecutor struct {
	calls int
}

func (s *stubCronExecutor) ProcessDirectWithChannel(ctx context.Context, content, sessionKey, channel, chatID string) (string, error) {
	s.calls++
	return "ok", nil
}

func TestCronTool_AddJobCapturesActorFromContext(t *testing.T) {
	storePath := t.TempDir() + "/state/jobs.json"
	cs, err := cron.NewCronService(storePath, nil)
	if err != nil {
		t.Fatalf("new cron service: %v", err)
	}
	msgBus := bus.NewMessageBus()
	tool := NewCronTool(cs, &stubCronExecutor{}, msgBus, t.TempDir(), true)
	ctx := WithToolExecutionActor(withToolExecutionContext(context.Background(), "discord", "chat-1", nil), "user-42")
	res := tool.Execute(ctx, map[string]interface{}{
		"action":        "add",
		"message":       "remind me",
		"every_seconds": float64(60),
	})
	if res == nil || res.IsError {
		t.Fatalf("expected add success, got %+v", res)
	}
	jobs := cs.ListJobs(true)
	if len(jobs) != 1 {
		t.Fatalf("expected one job, got %d", len(jobs))
	}
	if jobs[0].Payload.Actor != "user-42" {
		t.Fatalf("expected actor user-42, got %q", jobs[0].Payload.Actor)
	}
}

func TestCronTool_ExecuteJob_DeliverFalsePublishesInbound(t *testing.T) {
	msgBus := bus.NewMessageBus()
	executor := &stubCronExecutor{}
	tool := NewCronTool(nil, executor, msgBus, t.TempDir(), true)
	job := &cron.CronJob{
		ID: "job-1",
		Payload: cron.CronPayload{
			Message: "run nightly check",
			Deliver: false,
			Channel: "discord",
			To:      "chat-1",
			Actor:   "user-42",
		},
	}
	out := tool.ExecuteJob(context.Background(), job)
	if out != "ok" {
		t.Fatalf("expected ok result, got %q", out)
	}
	if executor.calls != 0 {
		t.Fatalf("expected no direct executor calls when bus is available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	inbound, ok := msgBus.ConsumeInbound(ctx)
	if !ok {
		t.Fatalf("expected inbound cron message to be queued")
	}
	if inbound.Channel != "discord" || inbound.ChatID != "chat-1" {
		t.Fatalf("unexpected routing: %+v", inbound)
	}
	if inbound.SenderID != "user-42" {
		t.Fatalf("expected sender user-42, got %q", inbound.SenderID)
	}
	if inbound.Content != "run nightly check" {
		t.Fatalf("unexpected content: %q", inbound.Content)
	}
}
