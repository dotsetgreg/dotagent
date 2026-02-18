package agent

import (
	"testing"

	"github.com/dotsetgreg/dotagent/pkg/providers"
)

func TestToolPolicy_ConversationModeDisablesLocalTools(t *testing.T) {
	p := newToolPolicy(t.TempDir())
	turn := p.PolicyForTurn("Hey, do you remember me from our earlier conversation?")

	if turn.Mode != turnToolModeConversation {
		t.Fatalf("expected conversation mode, got %s", turn.Mode)
	}
	if turn.Allows("exec") {
		t.Fatalf("expected exec to be disabled in conversation mode")
	}
	if turn.Allows("read_file") {
		t.Fatalf("expected read_file to be disabled in conversation mode")
	}
	if !turn.Allows("web_fetch") {
		t.Fatalf("expected web_fetch to remain enabled in conversation mode")
	}
}

func TestToolPolicy_WorkspaceModeAllowsLocalTools(t *testing.T) {
	p := newToolPolicy(t.TempDir())
	turn := p.PolicyForTurn("Run go test and edit pkg/agent/loop.go")

	if turn.Mode != turnToolModeWorkspaceOps {
		t.Fatalf("expected workspace mode, got %s", turn.Mode)
	}
	if !turn.Allows("exec") {
		t.Fatalf("expected exec to be enabled in workspace mode")
	}
	if !turn.Allows("read_file") {
		t.Fatalf("expected read_file to be enabled in workspace mode")
	}
}

func TestToolPolicy_ConversationPhrasingDoesNotTriggerWorkspaceMode(t *testing.T) {
	p := newToolPolicy(t.TempDir())
	turn := p.PolicyForTurn("I'm reading the third book right now and need to get back to it.")

	if turn.Mode != turnToolModeConversation {
		t.Fatalf("expected conversation mode for non-operational wording, got %s", turn.Mode)
	}
}

func TestToolPolicy_BlocksInternalStateAccess(t *testing.T) {
	workspace := t.TempDir()
	p := newToolPolicy(workspace)
	turn := turnToolPolicy{
		Mode:     turnToolModeWorkspaceOps,
		allowAll: true,
	}

	if ok, _ := p.ValidateToolCall(turn, "read_file", map[string]interface{}{"path": "state/state.json"}); ok {
		t.Fatalf("expected read_file state/state.json to be blocked")
	}
	if ok, _ := p.ValidateToolCall(turn, "list_dir", map[string]interface{}{"path": "state"}); ok {
		t.Fatalf("expected list_dir state to be blocked")
	}
	if ok, _ := p.ValidateToolCall(turn, "exec", map[string]interface{}{"command": "sqlite3 state/memory.db 'select 1'"}); ok {
		t.Fatalf("expected sqlite command against state/memory.db to be blocked")
	}
	if ok, reason := p.ValidateToolCall(turn, "read_file", map[string]interface{}{"path": "README.md"}); !ok {
		t.Fatalf("expected non-state path to be allowed, got reason: %s", reason)
	}
}

func TestToolPolicy_FilterDefinitions(t *testing.T) {
	p := newToolPolicy(t.TempDir())
	turn := p.PolicyForTurn("Do you remember anything about me?")
	defs := []providers.ToolDefinition{
		def("exec"),
		def("read_file"),
		def("web_fetch"),
		def("web_search"),
	}

	filtered := p.FilterDefinitions(defs, turn)
	if len(filtered) != 2 {
		t.Fatalf("expected 2 tool defs after filtering, got %d", len(filtered))
	}
	if filtered[0].Function.Name != "web_fetch" && filtered[1].Function.Name != "web_fetch" {
		t.Fatalf("expected web_fetch to be present")
	}
	if filtered[0].Function.Name != "web_search" && filtered[1].Function.Name != "web_search" {
		t.Fatalf("expected web_search to be present")
	}
}

func def(name string) providers.ToolDefinition {
	return providers.ToolDefinition{
		Type: "function",
		Function: providers.ToolFunctionDefinition{
			Name:        name,
			Description: "test",
			Parameters: map[string]interface{}{
				"type": "object",
			},
		},
	}
}
