package toolpacks

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dotsetgreg/dotagent/pkg/connectors"
)

func TestManager_LoadEnabledTools(t *testing.T) {
	workspace := t.TempDir()
	packDir := filepath.Join(workspace, "toolpacks", "demo-pack")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	manifest := Manifest{
		ID:      "demo-pack",
		Name:    "Demo",
		Version: "1.0.0",
		Enabled: true,
		Tools: []ManifestTool{
			{
				Name:            "demo_echo",
				Type:            "command",
				Description:     "demo",
				CommandTemplate: "echo {{msg}}",
			},
		},
	}
	raw, _ := json.MarshalIndent(manifest, "", "  ")
	if err := os.WriteFile(filepath.Join(packDir, "toolpack.json"), raw, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	mgr := NewManager(workspace, false)
	loaded, err := mgr.LoadEnabledTools()
	if err != nil {
		t.Fatalf("load enabled tools: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("expected 1 loaded tool, got %d", len(loaded))
	}
	res := loaded[0].Execute(context.Background(), map[string]interface{}{
		"msg": "from-pack",
	})
	if res.IsError {
		t.Fatalf("tool execution failed: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "from-pack") {
		t.Fatalf("expected command output to include from-pack, got %s", res.ForLLM)
	}
}

func TestManager_LoadEnabledTools_DuplicateToolNameAcrossPacks(t *testing.T) {
	workspace := t.TempDir()
	makePack := func(id string, enabled bool, toolName string, command string) {
		t.Helper()
		dir := filepath.Join(workspace, "toolpacks", id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		manifest := Manifest{
			ID:      id,
			Name:    id,
			Version: "1.0.0",
			Enabled: enabled,
			Tools: []ManifestTool{
				{
					Name:            toolName,
					Type:            "command",
					Description:     "dup test",
					CommandTemplate: command,
				},
			},
		}
		raw, _ := json.MarshalIndent(manifest, "", "  ")
		if err := os.WriteFile(filepath.Join(dir, "toolpack.json"), raw, 0o644); err != nil {
			t.Fatalf("write manifest: %v", err)
		}
	}

	makePack("pack-a", true, "demo_dup", "echo {{msg}}")
	makePack("pack-b", true, "demo_dup", "echo {{msg}}")

	mgr := NewManager(workspace, false)
	loaded, err := mgr.LoadEnabledTools()
	if err == nil {
		t.Fatalf("expected warning error for duplicate tool names")
	}
	if len(loaded) != 1 {
		t.Fatalf("expected one tool to be loaded, got %d", len(loaded))
	}
}

func TestManager_InstallFromPath_RejectsReservedToolName(t *testing.T) {
	workspace := t.TempDir()
	src := filepath.Join(workspace, "src-pack")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	manifest := Manifest{
		ID:      "bad-pack",
		Name:    "Bad Pack",
		Version: "1.0.0",
		Enabled: true,
		Tools: []ManifestTool{
			{
				Name:            "exec",
				Type:            "command",
				Description:     "invalid collision",
				CommandTemplate: "echo hi",
			},
		},
	}
	raw, _ := json.MarshalIndent(manifest, "", "  ")
	if err := os.WriteFile(filepath.Join(src, "toolpack.json"), raw, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	mgr := NewManager(workspace, false)
	_, err := mgr.InstallFromPath(src)
	if err == nil {
		t.Fatalf("expected manifest validation error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "collides") {
		t.Fatalf("expected collision error, got %v", err)
	}
}

func TestManager_Remove_PrunesLock(t *testing.T) {
	workspace := t.TempDir()
	packDir := filepath.Join(workspace, "source-pack")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	manifest := Manifest{
		ID:      "pack-lock",
		Name:    "Pack Lock",
		Version: "1.0.0",
		Enabled: true,
		Tools: []ManifestTool{
			{
				Name:            "pack_lock_echo",
				Type:            "command",
				Description:     "lock test",
				CommandTemplate: "echo {{msg}}",
			},
		},
	}
	raw, _ := json.MarshalIndent(manifest, "", "  ")
	if err := os.WriteFile(filepath.Join(packDir, "toolpack.json"), raw, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	mgr := NewManager(workspace, false)
	if _, err := mgr.InstallFromPath(packDir); err != nil {
		t.Fatalf("install: %v", err)
	}
	if _, ok, err := mgr.GetLock("pack-lock"); err != nil || !ok {
		t.Fatalf("expected lock after install, ok=%t err=%v", ok, err)
	}
	if err := mgr.Remove("pack-lock"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, ok, err := mgr.GetLock("pack-lock"); err != nil {
		t.Fatalf("get lock: %v", err)
	} else if ok {
		t.Fatalf("expected lock entry to be removed")
	}
}

func TestManager_LoadEnabledTools_OpenAPIConnector(t *testing.T) {
	workspace := t.TempDir()
	packDir := filepath.Join(workspace, "toolpacks", "api-pack")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/items/42" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"id":"42","name":"example"}`))
	}))
	defer server.Close()

	spec := map[string]interface{}{
		"openapi": "3.1.0",
		"paths": map[string]interface{}{
			"/items/{id}": map[string]interface{}{
				"get": map[string]interface{}{
					"operationId": "getItem",
					"parameters": []map[string]interface{}{
						{
							"name":     "id",
							"in":       "path",
							"required": true,
							"schema": map[string]interface{}{
								"type": "string",
							},
						},
					},
				},
			},
		},
	}
	specRaw, _ := json.MarshalIndent(spec, "", "  ")
	if err := os.WriteFile(filepath.Join(packDir, "spec.json"), specRaw, 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}

	manifest := Manifest{
		ID:      "api-pack",
		Name:    "API Pack",
		Version: "1.0.0",
		Enabled: true,
		Connectors: []ManifestConnector{
			{
				ID:   "api",
				Type: "openapi",
				OpenAPI: connectors.OpenAPIConfig{
					SpecPath: "spec.json",
					BaseURL:  server.URL + "/v1",
				},
			},
		},
		Tools: []ManifestTool{
			{
				Name:        "api_get_item",
				Type:        "openapi",
				ConnectorID: "api",
				OperationID: "getItem",
			},
		},
	}
	raw, _ := json.MarshalIndent(manifest, "", "  ")
	if err := os.WriteFile(filepath.Join(packDir, "toolpack.json"), raw, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	mgr := NewManager(workspace, false)
	loaded, err := mgr.LoadEnabledTools()
	if err != nil {
		t.Fatalf("load enabled tools: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("expected one tool, got %d", len(loaded))
	}
	res := loaded[0].Execute(context.Background(), map[string]interface{}{"id": "42"})
	if res.IsError {
		t.Fatalf("invoke failed: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, `"id":"42"`) {
		t.Fatalf("unexpected response: %s", res.ForLLM)
	}
}

func TestManager_LoadEnabledTools_MCPStreamableHTTPConnector(t *testing.T) {
	workspace := t.TempDir()
	packDir := filepath.Join(workspace, "toolpacks", "mcp-pack")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	initialized := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&req)
		method, _ := req["method"].(string)
		id := req["id"]
		switch method {
		case "initialize":
			initialized = true
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      id,
				"result":  map[string]interface{}{},
			})
		case "notifications/initialized":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      id,
				"result":  map[string]interface{}{},
			})
		case "tools/list":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      id,
				"result": map[string]interface{}{
					"tools": []map[string]interface{}{
						{
							"name":        "echo",
							"description": "Echo",
							"inputSchema": map[string]interface{}{
								"type": "object",
								"properties": map[string]interface{}{
									"msg": map[string]interface{}{
										"type": "string",
									},
								},
							},
						},
					},
				},
			})
		case "tools/call":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      id,
				"result": map[string]interface{}{
					"isError": false,
					"content": []map[string]interface{}{
						{
							"type": "text",
							"text": "ok-from-mcp",
						},
					},
				},
			})
		default:
			http.Error(w, "unexpected method", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	manifest := Manifest{
		ID:      "mcp-pack",
		Name:    "MCP Pack",
		Version: "1.0.0",
		Enabled: true,
		Connectors: []ManifestConnector{
			{
				ID:   "mcp",
				Type: "mcp",
				MCP: connectors.MCPConfig{
					Transport: "streamable_http",
					URL:       server.URL,
				},
			},
		},
		Tools: []ManifestTool{
			{
				Name:        "mcp_echo",
				Type:        "mcp",
				ConnectorID: "mcp",
				RemoteTool:  "echo",
			},
		},
	}
	raw, _ := json.MarshalIndent(manifest, "", "  ")
	if err := os.WriteFile(filepath.Join(packDir, "toolpack.json"), raw, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	mgr := NewManager(workspace, false)
	loaded, err := mgr.LoadEnabledTools()
	if err != nil {
		t.Fatalf("load enabled tools: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("expected one tool, got %d", len(loaded))
	}
	res := loaded[0].Execute(context.Background(), map[string]interface{}{"msg": "hello"})
	if res.IsError {
		t.Fatalf("invoke failed: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "ok-from-mcp") {
		t.Fatalf("unexpected response: %s", res.ForLLM)
	}
	if !initialized {
		t.Fatalf("expected initialize to be called")
	}
}

func TestManager_DoctorAndValidate(t *testing.T) {
	workspace := t.TempDir()
	packDir := filepath.Join(workspace, "toolpacks", "doctor-pack")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&req)
		method, _ := req["method"].(string)
		id := req["id"]
		switch method {
		case "initialize", "notifications/initialized":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": map[string]interface{}{}})
		case "tools/list":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      id,
				"result": map[string]interface{}{
					"tools": []map[string]interface{}{
						{"name": "echo", "description": "Echo", "inputSchema": map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}},
					},
				},
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": map[string]interface{}{}})
		}
	}))
	defer server.Close()

	manifest := Manifest{
		ID:      "doctor-pack",
		Name:    "Doctor Pack",
		Version: "1.0.0",
		Enabled: true,
		Connectors: []ManifestConnector{
			{
				ID:   "mcp",
				Type: "mcp",
				MCP: connectors.MCPConfig{
					Transport: "streamable_http",
					URL:       server.URL,
				},
			},
		},
		Tools: []ManifestTool{
			{Name: "mcp_echo", Type: "mcp", ConnectorID: "mcp", RemoteTool: "echo"},
		},
	}
	raw, _ := json.MarshalIndent(manifest, "", "  ")
	if err := os.WriteFile(filepath.Join(packDir, "toolpack.json"), raw, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	mgr := NewManager(workspace, false)
	warnings, err := mgr.Validate("")
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("expected no validation warnings, got %v", warnings)
	}

	results, err := mgr.Doctor(context.Background(), "")
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if len(results) == 0 {
		t.Fatalf("expected doctor results")
	}
	if results[0].Status != "ok" {
		t.Fatalf("expected ok doctor result, got %+v", results[0])
	}
}

func TestManager_InstallFromGitHub_ExtractsFullToolpack(t *testing.T) {
	workspace := t.TempDir()
	commitSHA := "0123456789abcdef0123456789abcdef01234567"

	manifest := Manifest{
		ID:      "github-pack",
		Name:    "GitHub Pack",
		Version: "1.0.0",
		Enabled: false,
		Connectors: []ManifestConnector{
			{
				ID:   "api",
				Type: "openapi",
				OpenAPI: connectors.OpenAPIConfig{
					SpecPath: "spec/api.json",
					BaseURL:  "https://api.example.com",
				},
			},
		},
		Tools: []ManifestTool{
			{
				Name:        "api_get_thing",
				Type:        "openapi",
				ConnectorID: "api",
				OperationID: "getThing",
			},
		},
	}
	manifestRaw, _ := json.MarshalIndent(manifest, "", "  ")
	specRaw := []byte(`{"openapi":"3.1.0","paths":{"/thing":{"get":{"operationId":"getThing"}}}}`)

	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	writeZipFile := func(name string, data []byte) {
		t.Helper()
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create zip file %s: %v", name, err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatalf("write zip file %s: %v", name, err)
		}
	}
	writeZipFile("repo-main/toolpack.json", manifestRaw)
	writeZipFile("repo-main/spec/api.json", specRaw)
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/owner/repo/commits/main":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"sha": commitSHA,
			})
		case "/owner/repo/zip/" + commitSHA:
			w.Header().Set("Content-Type", "application/zip")
			_, _ = w.Write(zipBuf.Bytes())
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	prevArchiveURL := githubArchiveBaseURL
	prevAPIURL := githubAPIBaseURL
	githubArchiveBaseURL = server.URL
	githubAPIBaseURL = server.URL
	defer func() {
		githubArchiveBaseURL = prevArchiveURL
		githubAPIBaseURL = prevAPIURL
	}()

	mgr := NewManager(workspace, false)
	installed, err := mgr.InstallFromGitHub(context.Background(), "owner/repo")
	if err != nil {
		t.Fatalf("install from github: %v", err)
	}
	if installed.ID != "github-pack" {
		t.Fatalf("unexpected installed id: %s", installed.ID)
	}

	installedSpecPath := filepath.Join(workspace, "toolpacks", "github-pack", "spec", "api.json")
	content, err := os.ReadFile(installedSpecPath)
	if err != nil {
		t.Fatalf("expected spec file to be installed, read failed: %v", err)
	}
	if !strings.Contains(string(content), `"getThing"`) {
		t.Fatalf("unexpected installed spec content: %s", string(content))
	}
	lock, ok, err := mgr.GetLock("github-pack")
	if err != nil {
		t.Fatalf("get lock: %v", err)
	}
	if !ok {
		t.Fatalf("expected lock to exist")
	}
	expectedSource := "github:owner/repo@" + commitSHA
	if lock.Source != expectedSource {
		t.Fatalf("expected lock source %q, got %q", expectedSource, lock.Source)
	}
}

func TestParseGitHubRepoSpec(t *testing.T) {
	spec, err := parseGitHubRepoSpec("owner/repo")
	if err != nil {
		t.Fatalf("parse owner/repo: %v", err)
	}
	if spec.Repo != "owner/repo" || spec.Ref != "main" {
		t.Fatalf("unexpected parse result: %+v", spec)
	}

	spec, err = parseGitHubRepoSpec("owner/repo@v1.2.3")
	if err != nil {
		t.Fatalf("parse owner/repo@ref: %v", err)
	}
	if spec.Repo != "owner/repo" || spec.Ref != "v1.2.3" {
		t.Fatalf("unexpected parse result: %+v", spec)
	}

	if _, err := parseGitHubRepoSpec("not-a-repo"); err == nil {
		t.Fatalf("expected parse failure for invalid repository format")
	}
}

func TestResolveGitHubCommitSHA_PassthroughSHA(t *testing.T) {
	sha := "abcdefabcdefabcdefabcdefabcdefabcdefabcd"
	resolved, err := resolveGitHubCommitSHA(context.Background(), "owner/repo", sha)
	if err != nil {
		t.Fatalf("resolve sha: %v", err)
	}
	if resolved != sha {
		t.Fatalf("expected %s, got %s", sha, resolved)
	}
}
