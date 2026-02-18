package connectors

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	mcpTransportStdio          = "stdio"
	mcpTransportStreamableHTTP = "streamable_http"
	defaultMCPProtocolVersion  = "2025-06-18"
)

type MCPConfig struct {
	Transport        string            `json:"transport,omitempty"`
	URL              string            `json:"url,omitempty"`
	Headers          map[string]string `json:"headers,omitempty"`
	Command          string            `json:"command,omitempty"`
	Args             []string          `json:"args,omitempty"`
	Env              map[string]string `json:"env,omitempty"`
	WorkingDir       string            `json:"working_dir,omitempty"`
	TimeoutSeconds   int               `json:"timeout_seconds,omitempty"`
	MaxConcurrency   int               `json:"max_concurrency,omitempty"`
	RetryMaxAttempts int               `json:"retry_max_attempts,omitempty"`
	RetryBackoffMS   int               `json:"retry_backoff_ms,omitempty"`
}

type mcpTool struct {
	Name        string
	Description string
	Parameters  map[string]interface{}
}

type mcpRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *mcpRPCError    `json:"error"`
	Method  string          `json:"method,omitempty"`
}

type mcpRPCRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int64       `json:"id,omitempty"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

type MCPRuntime struct {
	id        string
	cfg       MCPConfig
	timeout   time.Duration
	retry     RetryPolicy
	semaphore chan struct{}

	httpClient *http.Client
	headers    map[string]string

	mu          sync.RWMutex
	toolsCache  map[string]mcpTool
	sessionID   string
	initialized bool
	nextID      int64

	stdioMu sync.Mutex
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  *bufio.Reader
}

func NewMCPRuntime(id string, cfg MCPConfig) (*MCPRuntime, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("mcp runtime id is required")
	}
	cfg = normalizeMCPConfig(cfg)
	cfg.Transport = strings.ToLower(strings.TrimSpace(cfg.Transport))
	if cfg.Transport == "" {
		if strings.TrimSpace(cfg.URL) != "" {
			cfg.Transport = mcpTransportStreamableHTTP
		} else {
			cfg.Transport = mcpTransportStdio
		}
	}
	switch cfg.Transport {
	case mcpTransportStdio:
		if strings.TrimSpace(cfg.Command) == "" {
			return nil, fmt.Errorf("mcp stdio transport requires command")
		}
	case mcpTransportStreamableHTTP:
		if strings.TrimSpace(cfg.URL) == "" {
			return nil, fmt.Errorf("mcp streamable_http transport requires url")
		}
	default:
		return nil, fmt.Errorf("unsupported mcp transport: %s", cfg.Transport)
	}

	timeout := 30 * time.Second
	if cfg.TimeoutSeconds > 0 {
		timeout = time.Duration(cfg.TimeoutSeconds) * time.Second
	}
	maxConcurrency := cfg.MaxConcurrency
	if maxConcurrency <= 0 {
		if cfg.Transport == mcpTransportStdio {
			maxConcurrency = 1
		} else {
			maxConcurrency = 4
		}
	}
	retry := RetryPolicy{
		MaxAttempts: cfg.RetryMaxAttempts,
		Backoff:     time.Duration(cfg.RetryBackoffMS) * time.Millisecond,
	}
	if retry.Backoff <= 0 {
		retry.Backoff = 250 * time.Millisecond
	}

	rt := &MCPRuntime{
		id:         id,
		cfg:        cfg,
		timeout:    timeout,
		retry:      retry,
		semaphore:  make(chan struct{}, maxConcurrency),
		httpClient: &http.Client{Timeout: timeout},
		headers:    ResolveStringMap(cfg.Headers),
		toolsCache: map[string]mcpTool{},
	}
	return rt, nil
}

func (r *MCPRuntime) ID() string {
	return r.id
}

func (r *MCPRuntime) Type() string {
	return "mcp"
}

func (r *MCPRuntime) Health(ctx context.Context) error {
	_, err := r.listTools(ctx, true)
	return err
}

func (r *MCPRuntime) ToolSchema(ctx context.Context, target string) (string, map[string]interface{}, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", nil, fmt.Errorf("mcp target tool is required")
	}
	tools, err := r.listTools(ctx, false)
	if err != nil {
		return "", nil, err
	}
	tool, ok := tools[target]
	if !ok {
		return "", nil, fmt.Errorf("mcp tool %q not found", target)
	}
	return tool.Description, compactJSONSchema(tool.Parameters), nil
}

func (r *MCPRuntime) Invoke(ctx context.Context, target string, args map[string]interface{}) (InvocationResult, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return InvocationResult{}, fmt.Errorf("mcp target tool is required")
	}
	if err := r.acquire(ctx); err != nil {
		return InvocationResult{}, err
	}
	defer r.release()

	var out InvocationResult
	opErr := withRetry(ctx, r.retry, func(attempt int) error {
		callCtx, cancel := context.WithTimeout(ctx, r.timeout)
		defer cancel()
		raw, err := r.call(callCtx, "tools/call", map[string]interface{}{
			"name":      target,
			"arguments": args,
		})
		if err != nil {
			return err
		}
		var result struct {
			Content           []map[string]interface{} `json:"content"`
			StructuredContent interface{}              `json:"structuredContent"`
			IsError           bool                     `json:"isError"`
		}
		if unmarshalErr := json.Unmarshal(raw, &result); unmarshalErr != nil {
			out = InvocationResult{Content: string(raw)}
			return nil
		}
		textParts := make([]string, 0, len(result.Content))
		for _, item := range result.Content {
			typ, _ := item["type"].(string)
			if typ != "text" {
				continue
			}
			txt, _ := item["text"].(string)
			txt = strings.TrimSpace(txt)
			if txt != "" {
				textParts = append(textParts, txt)
			}
		}
		content := strings.Join(textParts, "\n")
		if content == "" && result.StructuredContent != nil {
			if rawStructured, marshalErr := json.Marshal(result.StructuredContent); marshalErr == nil {
				content = string(rawStructured)
			}
		}
		if content == "" {
			content = string(raw)
		}
		out = InvocationResult{
			Content: content,
			IsError: result.IsError,
		}
		return nil
	})
	if opErr != nil {
		return InvocationResult{}, opErr
	}
	return out, nil
}

func (r *MCPRuntime) Close() error {
	r.stdioMu.Lock()
	defer r.stdioMu.Unlock()
	return r.resetStdioLocked()
}

func (r *MCPRuntime) listTools(ctx context.Context, force bool) (map[string]mcpTool, error) {
	if !force {
		r.mu.RLock()
		if len(r.toolsCache) > 0 {
			cached := cloneMCPTools(r.toolsCache)
			r.mu.RUnlock()
			return cached, nil
		}
		r.mu.RUnlock()
	}

	if err := r.acquire(ctx); err != nil {
		return nil, err
	}
	defer r.release()

	var tools map[string]mcpTool
	err := withRetry(ctx, r.retry, func(attempt int) error {
		callCtx, cancel := context.WithTimeout(ctx, r.timeout)
		defer cancel()
		raw, err := r.call(callCtx, "tools/list", map[string]interface{}{})
		if err != nil {
			return err
		}
		parsed, parseErr := parseMCPToolsList(raw)
		if parseErr != nil {
			return parseErr
		}
		tools = parsed
		return nil
	})
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	r.toolsCache = tools
	r.mu.Unlock()
	return cloneMCPTools(tools), nil
}

func (r *MCPRuntime) call(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	switch r.cfg.Transport {
	case mcpTransportStdio:
		return r.callStdio(ctx, method, params)
	case mcpTransportStreamableHTTP:
		return r.callHTTP(ctx, method, params)
	default:
		return nil, fmt.Errorf("unsupported mcp transport: %s", r.cfg.Transport)
	}
}

func (r *MCPRuntime) callHTTP(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	if err := r.ensureHTTPInitialized(ctx); err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.nextID++
	id := r.nextID
	r.mu.Unlock()

	reqBody, _ := json.Marshal(mcpRPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSpace(r.cfg.URL), bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range r.headers {
		if strings.TrimSpace(v) == "" {
			continue
		}
		req.Header.Set(k, v)
	}
	r.mu.RLock()
	if strings.TrimSpace(r.sessionID) != "" {
		req.Header.Set("Mcp-Session-Id", r.sessionID)
	}
	r.mu.RUnlock()

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, readErr
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("mcp http call failed: status=%d body=%s", resp.StatusCode, string(body))
	}
	var rpcResp mcpRPCResponse
	if err := json.Unmarshal(body, &rpcResp); err != nil {
		return nil, fmt.Errorf("decode mcp http response: %w", err)
	}
	if rpcResp.Error != nil {
		return nil, fmt.Errorf("mcp rpc error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}
	if sessionID := strings.TrimSpace(resp.Header.Get("Mcp-Session-Id")); sessionID != "" {
		r.mu.Lock()
		r.sessionID = sessionID
		r.mu.Unlock()
	}
	return rpcResp.Result, nil
}

func (r *MCPRuntime) ensureHTTPInitialized(ctx context.Context) error {
	r.mu.RLock()
	if r.initialized {
		r.mu.RUnlock()
		return nil
	}
	r.mu.RUnlock()

	r.mu.Lock()
	if r.initialized {
		r.mu.Unlock()
		return nil
	}
	r.nextID++
	id := r.nextID
	r.mu.Unlock()

	initReq, _ := json.Marshal(mcpRPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  "initialize",
		Params: map[string]interface{}{
			"protocolVersion": defaultMCPProtocolVersion,
			"clientInfo": map[string]interface{}{
				"name":    "dotagent",
				"version": "dev",
			},
			"capabilities": map[string]interface{}{
				"tools": map[string]interface{}{},
			},
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSpace(r.cfg.URL), bytes.NewReader(initReq))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range r.headers {
		if strings.TrimSpace(v) == "" {
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := r.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return readErr
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("mcp initialize failed: status=%d body=%s", resp.StatusCode, string(body))
	}
	var initResp mcpRPCResponse
	if err := json.Unmarshal(body, &initResp); err != nil {
		return err
	}
	if initResp.Error != nil {
		return fmt.Errorf("mcp initialize rpc error %d: %s", initResp.Error.Code, initResp.Error.Message)
	}
	r.mu.Lock()
	r.initialized = true
	if sessionID := strings.TrimSpace(resp.Header.Get("Mcp-Session-Id")); sessionID != "" {
		r.sessionID = sessionID
	}
	r.mu.Unlock()

	_, _ = r.callHTTP(ctx, "notifications/initialized", map[string]interface{}{})
	return nil
}

func (r *MCPRuntime) callStdio(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	r.stdioMu.Lock()
	defer r.stdioMu.Unlock()

	if err := r.ensureStdioInitializedLocked(ctx); err != nil {
		return nil, err
	}

	r.mu.Lock()
	r.nextID++
	id := r.nextID
	r.mu.Unlock()

	req := mcpRPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}
	if err := writeMCPFrame(r.stdin, req); err != nil {
		_ = r.resetStdioLocked()
		return nil, err
	}
	for {
		frame, err := readMCPFrameWithContext(ctx, r.stdout, func() {
			_ = r.resetStdioLocked()
		})
		if err != nil {
			if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				_ = r.resetStdioLocked()
			}
			return nil, err
		}
		var resp mcpRPCResponse
		if err := json.Unmarshal(frame, &resp); err != nil {
			continue
		}
		if responseIDAsInt64(resp.ID) != id {
			continue
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("mcp rpc error %d: %s", resp.Error.Code, resp.Error.Message)
		}
		return resp.Result, nil
	}
}

func (r *MCPRuntime) ensureStdioInitializedLocked(ctx context.Context) error {
	if err := r.ensureStdioStartedLocked(); err != nil {
		return err
	}
	r.mu.RLock()
	ready := r.initialized
	r.mu.RUnlock()
	if ready {
		return nil
	}

	r.mu.Lock()
	r.nextID++
	id := r.nextID
	r.mu.Unlock()

	initReq := mcpRPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  "initialize",
		Params: map[string]interface{}{
			"protocolVersion": defaultMCPProtocolVersion,
			"clientInfo": map[string]interface{}{
				"name":    "dotagent",
				"version": "dev",
			},
			"capabilities": map[string]interface{}{
				"tools": map[string]interface{}{},
			},
		},
	}
	if err := writeMCPFrame(r.stdin, initReq); err != nil {
		return err
	}
	for {
		readCtx, cancel := context.WithTimeout(ctx, r.timeout)
		frame, err := readMCPFrameWithContext(readCtx, r.stdout, func() {
			_ = r.resetStdioLocked()
		})
		cancel()
		if err != nil {
			return err
		}
		var resp mcpRPCResponse
		if err := json.Unmarshal(frame, &resp); err != nil {
			continue
		}
		if responseIDAsInt64(resp.ID) != id {
			continue
		}
		if resp.Error != nil {
			return fmt.Errorf("mcp initialize rpc error %d: %s", resp.Error.Code, resp.Error.Message)
		}
		break
	}

	notify := mcpRPCRequest{
		JSONRPC: "2.0",
		Method:  "notifications/initialized",
		Params:  map[string]interface{}{},
	}
	if err := writeMCPFrame(r.stdin, notify); err != nil {
		return err
	}

	r.mu.Lock()
	r.initialized = true
	r.mu.Unlock()
	return nil
}

func (r *MCPRuntime) ensureStdioStartedLocked() error {
	if r.cmd != nil && r.cmd.Process != nil {
		return nil
	}
	cmd := exec.Command(strings.TrimSpace(r.cfg.Command), r.cfg.Args...) // #nosec G204 - command originates from trusted local manifest
	if strings.TrimSpace(r.cfg.WorkingDir) != "" {
		cmd.Dir = strings.TrimSpace(r.cfg.WorkingDir)
	}
	if env := ResolveStringMap(r.cfg.Env); len(env) > 0 {
		cmd.Env = append([]string{}, os.Environ()...)
		for k, v := range env {
			cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
		}
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() {
		_, _ = io.Copy(io.Discard, stderr)
	}()

	r.cmd = cmd
	r.stdin = stdin
	r.stdout = bufio.NewReader(stdout)
	r.mu.Lock()
	r.initialized = false
	r.mu.Unlock()
	return nil
}

func (r *MCPRuntime) resetStdioLocked() error {
	var errs []string
	if r.stdin != nil {
		if err := r.stdin.Close(); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if r.cmd != nil && r.cmd.Process != nil {
		_ = r.cmd.Process.Kill()
		if err := r.cmd.Wait(); err != nil {
			// process exit after kill is expected on reset; ignore
			_ = err
		}
	}
	r.cmd = nil
	r.stdin = nil
	r.stdout = nil
	r.mu.Lock()
	r.initialized = false
	r.toolsCache = map[string]mcpTool{}
	r.mu.Unlock()
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

func parseMCPToolsList(raw json.RawMessage) (map[string]mcpTool, error) {
	var parsed struct {
		Tools []struct {
			Name         string                 `json:"name"`
			Description  string                 `json:"description"`
			InputSchema  map[string]interface{} `json:"inputSchema"`
			InputSchema2 map[string]interface{} `json:"input_schema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	out := map[string]mcpTool{}
	for _, item := range parsed.Tools {
		name := strings.TrimSpace(item.Name)
		if name == "" {
			continue
		}
		out[name] = mcpTool{
			Name:        name,
			Description: strings.TrimSpace(item.Description),
			Parameters:  compactJSONSchema(firstSchema(item.InputSchema, item.InputSchema2)),
		}
	}
	return out, nil
}

func firstSchema(a, b map[string]interface{}) map[string]interface{} {
	if len(a) > 0 {
		return a
	}
	return b
}

func writeMCPFrame(w io.Writer, payload interface{}) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(raw))
	if _, err := io.WriteString(w, header); err != nil {
		return err
	}
	_, err = w.Write(raw)
	return err
}

func readMCPFrameBlocking(r *bufio.Reader) ([]byte, error) {
	headers := map[string]string{}
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		headers[strings.ToLower(strings.TrimSpace(parts[0]))] = strings.TrimSpace(parts[1])
	}
	clRaw := headers["content-length"]
	if clRaw == "" {
		return nil, fmt.Errorf("mcp frame missing content-length")
	}
	length, err := strconv.Atoi(clRaw)
	if err != nil || length <= 0 {
		return nil, fmt.Errorf("invalid mcp content-length: %s", clRaw)
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func readMCPFrameWithContext(ctx context.Context, r *bufio.Reader, onCancel func()) ([]byte, error) {
	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		payload, err := readMCPFrameBlocking(r)
		ch <- result{data: payload, err: err}
	}()

	select {
	case <-ctx.Done():
		if onCancel != nil {
			onCancel()
		}
		// Always drain the worker result to avoid leaked goroutines on cancellation.
		<-ch
		return nil, ctx.Err()
	case out := <-ch:
		return out.data, out.err
	}
}

func responseIDAsInt64(raw interface{}) int64 {
	switch id := raw.(type) {
	case float64:
		return int64(id)
	case int64:
		return id
	case int:
		return int64(id)
	case string:
		if parsed, err := strconv.ParseInt(strings.TrimSpace(id), 10, 64); err == nil {
			return parsed
		}
	}
	return 0
}

func cloneMCPTools(in map[string]mcpTool) map[string]mcpTool {
	out := make(map[string]mcpTool, len(in))
	for k, v := range in {
		copiedSchema := map[string]interface{}{}
		raw, _ := json.Marshal(v.Parameters)
		_ = json.Unmarshal(raw, &copiedSchema)
		v.Parameters = copiedSchema
		out[k] = v
	}
	return out
}

func (r *MCPRuntime) acquire(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case r.semaphore <- struct{}{}:
		return nil
	}
}

func (r *MCPRuntime) release() {
	select {
	case <-r.semaphore:
	default:
	}
}

func normalizeMCPConfig(cfg MCPConfig) MCPConfig {
	cfg.Transport = strings.TrimSpace(cfg.Transport)
	cfg.URL = strings.TrimSpace(ResolveSecretRef(cfg.URL))
	cfg.Command = strings.TrimSpace(ResolveSecretRef(cfg.Command))
	cfg.WorkingDir = strings.TrimSpace(ResolveSecretRef(cfg.WorkingDir))
	if len(cfg.Args) > 0 {
		nextArgs := make([]string, 0, len(cfg.Args))
		for _, arg := range cfg.Args {
			nextArgs = append(nextArgs, strings.TrimSpace(ResolveSecretRef(arg)))
		}
		cfg.Args = nextArgs
	}
	return cfg
}
