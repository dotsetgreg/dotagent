package tools

import (
	"context"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

var procIDRegex = regexp.MustCompile(`proc-[a-f0-9-]+`)

func extractProcID(t *testing.T, text string) string {
	t.Helper()
	id := procIDRegex.FindString(text)
	if id == "" {
		t.Fatalf("failed to find process id in %q", text)
	}
	return id
}

func TestProcessTool_StartAndPoll(t *testing.T) {
	tool := NewProcessTool("", false)
	start := tool.Execute(context.Background(), map[string]interface{}{
		"action":  "start",
		"command": "echo hello-process",
	})
	if start.IsError {
		t.Fatalf("start should succeed: %s", start.ForLLM)
	}
	id := extractProcID(t, start.ForLLM)

	time.Sleep(120 * time.Millisecond)
	poll := tool.Execute(context.Background(), map[string]interface{}{
		"action":     "poll",
		"process_id": id,
		"tail_chars": float64(4000),
	})
	if poll.IsError {
		t.Fatalf("poll should succeed: %s", poll.ForLLM)
	}
	if !strings.Contains(poll.ForLLM, "hello-process") {
		t.Fatalf("expected output to contain command output, got %q", poll.ForLLM)
	}
}

func TestProcessTool_Kill(t *testing.T) {
	tool := NewProcessTool("", false)
	cmd := "sleep 5"
	if runtime.GOOS == "windows" {
		cmd = "Start-Sleep -Seconds 5"
	}
	start := tool.Execute(context.Background(), map[string]interface{}{
		"action":  "start",
		"command": cmd,
	})
	if start.IsError {
		t.Fatalf("start should succeed: %s", start.ForLLM)
	}
	id := extractProcID(t, start.ForLLM)
	kill := tool.Execute(context.Background(), map[string]interface{}{
		"action":     "kill",
		"process_id": id,
	})
	if kill.IsError {
		t.Fatalf("kill should succeed: %s", kill.ForLLM)
	}
}

func TestProcessTool_CloseStopsAndClears(t *testing.T) {
	tool := NewProcessTool("", false)
	cmd := "sleep 5"
	if runtime.GOOS == "windows" {
		cmd = "Start-Sleep -Seconds 5"
	}
	start := tool.Execute(context.Background(), map[string]interface{}{
		"action":  "start",
		"command": cmd,
	})
	if start.IsError {
		t.Fatalf("start should succeed: %s", start.ForLLM)
	}
	if err := tool.Close(); err != nil {
		t.Fatalf("close should succeed: %v", err)
	}
	list := tool.Execute(context.Background(), map[string]interface{}{
		"action": "list",
	})
	if list.IsError {
		t.Fatalf("list should succeed: %s", list.ForLLM)
	}
	if !strings.Contains(strings.ToLower(list.ForLLM), "no managed processes") {
		t.Fatalf("expected close to clear managed process records, got %q", list.ForLLM)
	}
}
