package connectors

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenAPIRuntime_Invoke(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/users/123" {
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		if r.URL.Query().Get("verbose") != "true" {
			http.Error(w, "missing query", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	spec := map[string]interface{}{
		"openapi": "3.1.0",
		"paths": map[string]interface{}{
			"/users/{id}": map[string]interface{}{
				"get": map[string]interface{}{
					"operationId": "getUser",
					"parameters": []map[string]interface{}{
						{
							"name":     "id",
							"in":       "path",
							"required": true,
							"schema": map[string]interface{}{
								"type": "string",
							},
						},
						{
							"name": "verbose",
							"in":   "query",
							"schema": map[string]interface{}{
								"type": "boolean",
							},
						},
					},
				},
			},
		},
	}
	tmp := t.TempDir()
	specPath := filepath.Join(tmp, "spec.json")
	raw, _ := json.Marshal(spec)
	if err := os.WriteFile(specPath, raw, 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}

	rt, err := NewOpenAPIRuntime("users", OpenAPIConfig{
		SpecPath: specPath,
		BaseURL:  server.URL + "/v1",
	})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}

	desc, schema, err := rt.ToolSchema(context.Background(), "getUser")
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	if !strings.Contains(strings.ToLower(desc), "get") && desc == "" {
		t.Fatalf("unexpected description: %q", desc)
	}
	props, _ := schema["properties"].(map[string]interface{})
	if _, ok := props["id"]; !ok {
		t.Fatalf("schema missing id property: %v", schema)
	}

	out, err := rt.Invoke(context.Background(), "getUser", map[string]interface{}{
		"id":      "123",
		"verbose": true,
	})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if out.IsError {
		t.Fatalf("unexpected invocation error: %+v", out)
	}
	if !strings.Contains(out.Content, `"ok":true`) {
		t.Fatalf("unexpected content: %s", out.Content)
	}
}

func TestOpenAPIRuntime_Invoke_WithAuthHeaderOnly(t *testing.T) {
	receivedAuth := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/v1/users/123" {
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	spec := map[string]interface{}{
		"openapi": "3.1.0",
		"paths": map[string]interface{}{
			"/users/{id}": map[string]interface{}{
				"get": map[string]interface{}{
					"operationId": "getUser",
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
	tmp := t.TempDir()
	specPath := filepath.Join(tmp, "spec.json")
	raw, _ := json.Marshal(spec)
	if err := os.WriteFile(specPath, raw, 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}

	t.Setenv("DOTAGENT_TEST_OPENAPI_TOKEN", "Bearer test-token")
	rt, err := NewOpenAPIRuntime("users-auth", OpenAPIConfig{
		SpecPath:   specPath,
		BaseURL:    server.URL + "/v1",
		AuthHeader: "Authorization",
		AuthToken:  "env:DOTAGENT_TEST_OPENAPI_TOKEN",
	})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}

	out, err := rt.Invoke(context.Background(), "getUser", map[string]interface{}{
		"id": "123",
	})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if out.IsError {
		t.Fatalf("unexpected invocation error: %+v", out)
	}
	if receivedAuth != "Bearer test-token" {
		t.Fatalf("expected auth header to be propagated, got %q", receivedAuth)
	}
}

func TestOpenAPIRuntime_MissingBaseURLFails(t *testing.T) {
	spec := map[string]interface{}{
		"openapi": "3.1.0",
		"paths": map[string]interface{}{
			"/users/{id}": map[string]interface{}{
				"get": map[string]interface{}{
					"operationId": "getUser",
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
	tmp := t.TempDir()
	specPath := filepath.Join(tmp, "spec.json")
	raw, _ := json.Marshal(spec)
	if err := os.WriteFile(specPath, raw, 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}

	rt, err := NewOpenAPIRuntime("users-missing-base", OpenAPIConfig{
		SpecPath: specPath,
	})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}

	if err := rt.Health(context.Background()); err == nil {
		t.Fatalf("expected health to fail without base url")
	}
}
