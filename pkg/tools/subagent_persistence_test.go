package tools

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dotsetgreg/dotagent/pkg/bus"
	"github.com/dotsetgreg/dotagent/pkg/providers"
)

type countingSubagentProvider struct {
	mu      sync.Mutex
	calls   int
	content string
}

func (p *countingSubagentProvider) Chat(_ context.Context, _ []providers.Message, _ []providers.ToolDefinition, _ string, _ map[string]interface{}) (*providers.LLMResponse, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	return &providers.LLMResponse{Content: p.content}, nil
}

func (p *countingSubagentProvider) GetDefaultModel() string {
	return "test-model"
}

func (p *countingSubagentProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func waitForSubagentTaskStatus(sm *SubagentManager, taskID string, timeout time.Duration, status ...string) (*SubagentTask, bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		task, ok := sm.GetTask(taskID)
		if ok {
			for _, s := range status {
				if task.Status == s {
					return task, true
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil, false
}

func TestSubagentManager_ResumesRunningTaskFromStateFile(t *testing.T) {
	tmpDir := t.TempDir()
	now := time.Now().Add(-30 * time.Second).UnixMilli()
	statePath := filepath.Join(tmpDir, "state", subagentStateFile)
	state := persistedSubagentState{
		Version: subagentStateVersion,
		NextID:  2,
		Tasks: []*SubagentTask{
			{
				ID:            "subagent-1",
				Task:          "resume this task",
				Label:         "resume",
				OriginChannel: "discord",
				OriginChatID:  "chat-resume",
				Status:        "running",
				Created:       now,
				Updated:       now,
			},
		},
	}
	if err := writeJSONFileAtomic(statePath, state); err != nil {
		t.Fatalf("write state file: %v", err)
	}

	msgBus := bus.NewMessageBus()
	provider := &countingSubagentProvider{content: "resumed-ok"}
	manager := NewSubagentManager(provider, "test-model", tmpDir, tmpDir, msgBus)
	manager.SetTools(NewToolRegistry())

	task, ok := waitForSubagentTaskStatus(manager, "subagent-1", 3*time.Second, "completed")
	if !ok {
		t.Fatalf("timed out waiting for resumed task completion")
	}
	if task.Result == "" || !strings.Contains(strings.ToLower(task.Result), "resumed-ok") {
		t.Fatalf("expected resumed task result to contain provider output, got %q", task.Result)
	}
	if provider.callCount() == 0 {
		t.Fatalf("expected resumed task to invoke provider")
	}
}

func TestSubagentManager_RetriesPendingCompletionNotificationOnStartup(t *testing.T) {
	tmpDir := t.TempDir()
	now := time.Now().Add(-10 * time.Second).UnixMilli()
	statePath := filepath.Join(tmpDir, "state", subagentStateFile)
	state := persistedSubagentState{
		Version: subagentStateVersion,
		NextID:  2,
		Tasks: []*SubagentTask{
			{
				ID:                 "subagent-1",
				Task:               "already done",
				Label:              "notify-me",
				OriginChannel:      "discord",
				OriginChatID:       "chat-notify",
				Status:             "completed",
				Result:             "final output",
				Created:            now,
				Updated:            now,
				CompletedAt:        now,
				CompletionNotified: false,
			},
		},
	}
	if err := writeJSONFileAtomic(statePath, state); err != nil {
		t.Fatalf("write state file: %v", err)
	}

	msgBus := bus.NewMessageBus()
	provider := &countingSubagentProvider{content: "unused"}
	manager := NewSubagentManager(provider, "test-model", tmpDir, tmpDir, msgBus)
	manager.SetTools(NewToolRegistry())

	consumeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	inbound, ok := msgBus.ConsumeInbound(consumeCtx)
	if !ok {
		t.Fatalf("expected pending completion notification to be published")
	}
	if inbound.Channel != "system" {
		t.Fatalf("expected system channel completion notice, got %q", inbound.Channel)
	}
	if !strings.Contains(inbound.SenderID, "subagent:subagent-1") {
		t.Fatalf("unexpected sender id: %q", inbound.SenderID)
	}
	if !strings.Contains(inbound.Content, "final output") {
		t.Fatalf("expected notification content to include task result, got %q", inbound.Content)
	}

	task, ok := waitForSubagentTaskStatus(manager, "subagent-1", 2*time.Second, "completed")
	if !ok {
		t.Fatalf("expected task to remain available after notify retry")
	}
	if !task.CompletionNotified {
		t.Fatalf("expected completion_notified=true after successful retry")
	}
}

func TestSubagentManager_PersistsTaskIDsAcrossRestarts(t *testing.T) {
	tmpDir := t.TempDir()

	providerA := &countingSubagentProvider{content: "first"}
	managerA := NewSubagentManager(providerA, "test-model", tmpDir, tmpDir, bus.NewMessageBus())
	managerA.SetTools(NewToolRegistry())
	if _, err := managerA.Spawn(context.Background(), "first task", "first", "discord", "chat-a", nil); err != nil {
		t.Fatalf("spawn first task: %v", err)
	}
	if _, ok := waitForSubagentTaskStatus(managerA, "subagent-1", 3*time.Second, "completed"); !ok {
		t.Fatalf("first task did not complete")
	}

	providerB := &countingSubagentProvider{content: "second"}
	managerB := NewSubagentManager(providerB, "test-model", tmpDir, tmpDir, bus.NewMessageBus())
	managerB.SetTools(NewToolRegistry())
	if _, err := managerB.Spawn(context.Background(), "second task", "second", "discord", "chat-b", nil); err != nil {
		t.Fatalf("spawn second task: %v", err)
	}
	if _, ok := waitForSubagentTaskStatus(managerB, "subagent-2", 3*time.Second, "completed"); !ok {
		t.Fatalf("second task did not complete or ID continuity broke")
	}
}
