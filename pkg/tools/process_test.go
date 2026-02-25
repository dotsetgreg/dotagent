package tools

import (
	"context"
	"regexp"
	"runtime"
	"strconv"
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

func TestProcessTool_StartRestrictedBlocksShellOperators(t *testing.T) {
	tool := NewProcessTool(t.TempDir(), true)
	start := tool.Execute(context.Background(), map[string]interface{}{
		"action":  "start",
		"command": "echo test && ls",
	})
	if !start.IsError {
		t.Fatalf("expected restricted mode process start to be blocked")
	}
	if !strings.Contains(strings.ToLower(start.ForLLM), "restricted mode") {
		t.Fatalf("expected restricted mode guard message, got %q", start.ForLLM)
	}
}

func TestManagedProcess_AppendOutputCapsMemory(t *testing.T) {
	mp := &managedProcess{}
	mp.appendOutput([]byte(strings.Repeat("a", 1024)), 128)
	if got := len(mp.output); got != 128 {
		t.Fatalf("expected output ring size 128, got %d", got)
	}
	if mp.dropped != 896 {
		t.Fatalf("expected dropped bytes 896, got %d", mp.dropped)
	}
}

func TestProcessTool_PollStaleGuard(t *testing.T) {
	tool := NewProcessTool("", false)
	cmd := "sleep 3"
	if runtime.GOOS == "windows" {
		cmd = "Start-Sleep -Seconds 3"
	}
	start := tool.Execute(context.Background(), map[string]interface{}{
		"action":  "start",
		"command": cmd,
	})
	if start.IsError {
		t.Fatalf("start should succeed: %s", start.ForLLM)
	}
	id := extractProcID(t, start.ForLLM)

	var blocked *ToolResult
	for i := 0; i < stalePollLimit+1; i++ {
		res := tool.Execute(context.Background(), map[string]interface{}{
			"action":     "poll",
			"process_id": id,
		})
		if res.IsError {
			blocked = res
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if blocked == nil {
		t.Fatalf("expected stale polling guard to trigger")
	}
	if !strings.Contains(strings.ToLower(blocked.ForLLM), "stale polling") {
		t.Fatalf("expected stale polling error, got %q", blocked.ForLLM)
	}

	// force=true should clear guard once.
	forced := tool.Execute(context.Background(), map[string]interface{}{
		"action":     "poll",
		"process_id": id,
		"force":      true,
	})
	if forced.IsError {
		t.Fatalf("expected force poll to bypass stale guard, got %q", forced.ForLLM)
	}
}

func TestProcessTool_StartRespectsProcessLimit(t *testing.T) {
	tool := NewProcessTool("", false)
	tool.mu.Lock()
	for i := 0; i < maxManagedProcesses; i++ {
		id := "proc-limit-" + strconv.Itoa(i)
		tool.processes[id] = &managedProcess{running: true}
	}
	tool.mu.Unlock()

	start := tool.Execute(context.Background(), map[string]interface{}{
		"action":  "start",
		"command": "echo hello",
	})
	if !start.IsError {
		t.Fatalf("expected start to fail at process limit")
	}
	if !strings.Contains(strings.ToLower(start.ForLLM), "too many managed processes") {
		t.Fatalf("expected process limit error, got %q", start.ForLLM)
	}
}
