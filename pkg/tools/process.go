package tools

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	defaultProcessOutputBytes = 64000
)

type managedProcess struct {
	id         string
	command    string
	workingDir string
	startedAt  time.Time
	finishedAt time.Time
	running    bool
	exitCode   int
	lastError  string
	output     []byte
	stdin      io.WriteCloser
	cancel     context.CancelFunc
	mu         sync.RWMutex
}

func (mp *managedProcess) appendOutput(chunk []byte, maxBytes int) {
	if len(chunk) == 0 {
		return
	}
	mp.mu.Lock()
	defer mp.mu.Unlock()
	mp.output = append(mp.output, chunk...)
	if maxBytes > 0 && len(mp.output) > maxBytes {
		mp.output = mp.output[len(mp.output)-maxBytes:]
	}
}

func (mp *managedProcess) snapshot(tail int) string {
	mp.mu.RLock()
	defer mp.mu.RUnlock()

	status := "running"
	if !mp.running {
		status = "completed"
	}

	output := mp.output
	if tail > 0 && len(output) > tail {
		output = output[len(output)-tail:]
	}
	return strings.TrimSpace(fmt.Sprintf(
		"Process %s\n- Status: %s\n- Command: %s\n- Working dir: %s\n- Started: %s\n- Finished: %s\n- Exit code: %d\n- Error: %s\n- Output:\n%s",
		mp.id,
		status,
		mp.command,
		valueOrUnset(mp.workingDir),
		mp.startedAt.Format(time.RFC3339),
		timeOrUnset(mp.finishedAt),
		mp.exitCode,
		valueOrUnset(mp.lastError),
		string(output),
	))
}

func valueOrUnset(v string) string {
	if strings.TrimSpace(v) == "" {
		return "(unset)"
	}
	return v
}

func timeOrUnset(ts time.Time) string {
	if ts.IsZero() {
		return "(unset)"
	}
	return ts.Format(time.RFC3339)
}

type ProcessTool struct {
	workspace string
	restrict  bool
	guard     *ExecTool
	maxOutput int

	mu        sync.RWMutex
	processes map[string]*managedProcess
}

func NewProcessTool(workspace string, restrict bool) *ProcessTool {
	guard := NewExecTool(workspace, restrict)
	guard.SetTimeout(0)
	return &ProcessTool{
		workspace: workspace,
		restrict:  restrict,
		guard:     guard,
		maxOutput: defaultProcessOutputBytes,
		processes: map[string]*managedProcess{},
	}
}

func (t *ProcessTool) Name() string {
	return "process"
}

func (t *ProcessTool) Description() string {
	return "Manage long-running shell processes with lifecycle control. Actions: start, list, poll, write, kill, clear."
}

func (t *ProcessTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"action": map[string]interface{}{
				"type":        "string",
				"enum":        []string{"start", "list", "poll", "write", "kill", "clear"},
				"description": "Process lifecycle action.",
			},
			"command": map[string]interface{}{
				"type":        "string",
				"description": "Shell command to start (required for action=start).",
			},
			"process_id": map[string]interface{}{
				"type":        "string",
				"description": "Managed process ID (required for poll/write/kill).",
			},
			"input": map[string]interface{}{
				"type":        "string",
				"description": "Input to write to stdin for action=write.",
			},
			"working_dir": map[string]interface{}{
				"type":        "string",
				"description": "Optional working directory for action=start.",
			},
			"tail_chars": map[string]interface{}{
				"type":        "integer",
				"description": "Output tail size in chars for action=poll. Default 4000.",
				"minimum":     128.0,
				"maximum":     32000.0,
			},
			"all_completed": map[string]interface{}{
				"type":        "boolean",
				"description": "For action=clear: remove all completed processes.",
			},
		},
		"required": []string{"action"},
	}
}

func (t *ProcessTool) Execute(ctx context.Context, args map[string]interface{}) *ToolResult {
	action, _ := args["action"].(string)
	action = strings.TrimSpace(strings.ToLower(action))
	switch action {
	case "start":
		return t.start(args)
	case "list":
		return t.list()
	case "poll":
		return t.poll(args)
	case "write":
		return t.write(args)
	case "kill":
		return t.kill(args)
	case "clear":
		return t.clear(args)
	default:
		return ErrorResult("action must be one of: start, list, poll, write, kill, clear")
	}
}

func (t *ProcessTool) Close() error {
	t.mu.Lock()
	processes := make([]*managedProcess, 0, len(t.processes))
	for _, mp := range t.processes {
		processes = append(processes, mp)
	}
	t.processes = map[string]*managedProcess{}
	t.mu.Unlock()

	var errs []string
	for _, mp := range processes {
		mp.mu.Lock()
		cancel := mp.cancel
		stdin := mp.stdin
		mp.cancel = nil
		mp.stdin = nil
		mp.mu.Unlock()

		if cancel != nil {
			cancel()
		}
		if stdin != nil {
			if err := stdin.Close(); err != nil {
				errs = append(errs, err.Error())
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("process teardown encountered %d error(s): %s", len(errs), strings.Join(errs, "; "))
	}
	return nil
}

func (t *ProcessTool) start(args map[string]interface{}) *ToolResult {
	command, _ := args["command"].(string)
	command = strings.TrimSpace(command)
	if command == "" {
		return ErrorResult("command is required for action=start")
	}

	cwd := t.workspace
	if wd, ok := args["working_dir"].(string); ok && strings.TrimSpace(wd) != "" {
		resolved, err := validatePath(wd, t.workspace, t.restrict)
		if err != nil {
			return ErrorResult(err.Error())
		}
		cwd = resolved
	}
	if cwd == "" {
		cwd = "."
	}

	if guardErr := t.guard.guardCommand(command, cwd); guardErr != "" {
		return ErrorResult(guardErr)
	}

	procCtx, cancel := context.WithCancel(context.Background())
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(procCtx, "powershell", "-NoProfile", "-NonInteractive", "-Command", command)
	} else {
		cmd = exec.CommandContext(procCtx, "sh", "-c", command)
	}
	cmd.Dir = cwd

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return ErrorResult(fmt.Sprintf("failed to create stdout pipe: %v", err))
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return ErrorResult(fmt.Sprintf("failed to create stderr pipe: %v", err))
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return ErrorResult(fmt.Sprintf("failed to create stdin pipe: %v", err))
	}

	if err := cmd.Start(); err != nil {
		cancel()
		return ErrorResult(fmt.Sprintf("failed to start process: %v", err))
	}

	id := "proc-" + uuid.NewString()
	mp := &managedProcess{
		id:         id,
		command:    command,
		workingDir: cwd,
		startedAt:  time.Now(),
		running:    true,
		exitCode:   -1,
		stdin:      stdin,
		cancel:     cancel,
	}

	t.mu.Lock()
	t.processes[id] = mp
	t.mu.Unlock()

	go t.streamOutput(mp, stdout)
	go t.streamOutput(mp, stderr)
	go t.waitProcess(mp, cmd)

	return UserResult(fmt.Sprintf("Started process %s\n- Command: %s\n- Working dir: %s", id, command, cwd))
}

func (t *ProcessTool) streamOutput(mp *managedProcess, r io.Reader) {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			mp.appendOutput(buf[:n], t.maxOutput)
		}
		if err != nil {
			if err == io.EOF {
				return
			}
			mp.appendOutput([]byte("\n[stream error] "+err.Error()+"\n"), t.maxOutput)
			return
		}
	}
}

func (t *ProcessTool) waitProcess(mp *managedProcess, cmd *exec.Cmd) {
	err := cmd.Wait()
	mp.mu.Lock()
	defer mp.mu.Unlock()
	mp.running = false
	mp.finishedAt = time.Now()
	mp.exitCode = 0
	if err != nil {
		mp.lastError = err.Error()
		if exitErr, ok := err.(*exec.ExitError); ok {
			mp.exitCode = exitErr.ExitCode()
		} else {
			mp.exitCode = -1
		}
	}
}

func (t *ProcessTool) list() *ToolResult {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if len(t.processes) == 0 {
		return UserResult("No managed processes.")
	}
	lines := []string{"Managed processes:"}
	for _, mp := range t.processes {
		mp.mu.RLock()
		status := "running"
		if !mp.running {
			status = "completed"
		}
		lines = append(lines, fmt.Sprintf("- %s [%s] %s (started %s)", mp.id, status, mp.command, mp.startedAt.Format(time.RFC3339)))
		mp.mu.RUnlock()
	}
	return UserResult(strings.Join(lines, "\n"))
}

func (t *ProcessTool) poll(args map[string]interface{}) *ToolResult {
	id, _ := args["process_id"].(string)
	id = strings.TrimSpace(id)
	if id == "" {
		return ErrorResult("process_id is required for action=poll")
	}
	tail := 4000
	if raw, ok := args["tail_chars"].(float64); ok {
		if raw >= 128 {
			tail = int(raw)
		}
	}
	mp := t.get(id)
	if mp == nil {
		return ErrorResult(fmt.Sprintf("process %s not found", id))
	}
	return UserResult(mp.snapshot(tail))
}

func (t *ProcessTool) write(args map[string]interface{}) *ToolResult {
	id, _ := args["process_id"].(string)
	id = strings.TrimSpace(id)
	if id == "" {
		return ErrorResult("process_id is required for action=write")
	}
	input, _ := args["input"].(string)
	if input == "" {
		return ErrorResult("input is required for action=write")
	}
	mp := t.get(id)
	if mp == nil {
		return ErrorResult(fmt.Sprintf("process %s not found", id))
	}

	mp.mu.RLock()
	defer mp.mu.RUnlock()
	if !mp.running || mp.stdin == nil {
		return ErrorResult("process is not accepting input")
	}
	if !strings.HasSuffix(input, "\n") {
		input += "\n"
	}
	if _, err := io.WriteString(mp.stdin, input); err != nil {
		return ErrorResult(fmt.Sprintf("write failed: %v", err))
	}
	return UserResult(fmt.Sprintf("Wrote %s bytes to %s", strconv.Itoa(len(input)), id))
}

func (t *ProcessTool) kill(args map[string]interface{}) *ToolResult {
	id, _ := args["process_id"].(string)
	id = strings.TrimSpace(id)
	if id == "" {
		return ErrorResult("process_id is required for action=kill")
	}
	mp := t.get(id)
	if mp == nil {
		return ErrorResult(fmt.Sprintf("process %s not found", id))
	}
	mp.mu.RLock()
	running := mp.running
	cancel := mp.cancel
	mp.mu.RUnlock()
	if !running {
		return UserResult(fmt.Sprintf("Process %s is already completed.", id))
	}
	if cancel != nil {
		cancel()
	}
	return UserResult(fmt.Sprintf("Kill signal sent to %s", id))
}

func (t *ProcessTool) clear(args map[string]interface{}) *ToolResult {
	id, _ := args["process_id"].(string)
	id = strings.TrimSpace(id)
	allCompleted, _ := args["all_completed"].(bool)

	t.mu.Lock()
	defer t.mu.Unlock()

	if id != "" {
		delete(t.processes, id)
		return UserResult(fmt.Sprintf("Removed process record %s", id))
	}
	if !allCompleted {
		return ErrorResult("set all_completed=true or provide process_id")
	}
	removed := 0
	for pid, mp := range t.processes {
		mp.mu.RLock()
		running := mp.running
		mp.mu.RUnlock()
		if running {
			continue
		}
		delete(t.processes, pid)
		removed++
	}
	return UserResult(fmt.Sprintf("Removed %d completed process records", removed))
}

func (t *ProcessTool) get(id string) *managedProcess {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.processes[id]
}
