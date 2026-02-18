package tools

import (
	"context"
	"strings"
	"testing"
)

func TestRenderCommandTemplate(t *testing.T) {
	out, err := renderCommandTemplate("echo {{name}} {{count}}", map[string]interface{}{
		"name":  "alice",
		"count": float64(3),
	})
	if err != nil {
		t.Fatalf("render template: %v", err)
	}
	if !strings.Contains(out, "'alice'") || !strings.Contains(out, "'3'") {
		t.Fatalf("unexpected template render: %s", out)
	}
}

func TestTemplateCommandTool_Execute(t *testing.T) {
	tool := NewTemplateCommandTool(TemplateCommandConfig{
		Name:            "tmpl_echo",
		Description:     "echo template",
		CommandTemplate: "echo {{msg}}",
		Workspace:       "",
		Restrict:        false,
	})
	res := tool.Execute(context.Background(), map[string]interface{}{
		"msg": "hello-template",
	})
	if res.IsError {
		t.Fatalf("expected success, got %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "hello-template") {
		t.Fatalf("expected output to contain rendered value, got %s", res.ForLLM)
	}
}
