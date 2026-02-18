package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/dotsetgreg/dotagent/pkg/bus"
	"github.com/dotsetgreg/dotagent/pkg/config"
	"github.com/dotsetgreg/dotagent/pkg/providers"
)

type forcedStateIntrospectionProvider struct {
	calls          int
	firstCallTools []string
	sawBlockedTool bool
}

func (m *forcedStateIntrospectionProvider) Chat(ctx context.Context, messages []providers.Message, tools []providers.ToolDefinition, model string, opts map[string]interface{}) (*providers.LLMResponse, error) {
	m.calls++
	if m.calls == 1 {
		for _, td := range tools {
			m.firstCallTools = append(m.firstCallTools, td.Function.Name)
		}
		return &providers.LLMResponse{
			Content: "",
			ToolCalls: []providers.ToolCall{
				{
					ID:   "tc-1",
					Name: "exec",
					Arguments: map[string]interface{}{
						"command": "sqlite3 state/memory.db \"SELECT 1;\"",
					},
				},
			},
		}, nil
	}
	for _, msg := range messages {
		if msg.Role == "tool" && strings.Contains(msg.Content, "Tool call blocked by policy") {
			m.sawBlockedTool = true
			break
		}
	}
	return &providers.LLMResponse{
		Content:   "Fresh session: I don't have prior conversation details yet.",
		ToolCalls: []providers.ToolCall{},
	}, nil
}

func (m *forcedStateIntrospectionProvider) GetDefaultModel() string {
	return "mock-policy-integration"
}

func TestAgentLoop_ConversationTurnFiltersAndBlocksStateIntrospectionTools(t *testing.T) {
	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         t.TempDir(),
				Model:             "test-model",
				MaxTokens:         4096,
				MaxToolIterations: 10,
			},
		},
		Tools: config.ToolsConfig{
			Web: config.WebToolsConfig{
				DuckDuckGo: config.DuckDuckGoConfig{
					Enabled:    true,
					MaxResults: 5,
				},
			},
		},
	}
	msgBus := bus.NewMessageBus()
	provider := &forcedStateIntrospectionProvider{}
	al := mustNewAgentLoop(t, cfg, msgBus, provider)

	resp, err := al.ProcessDirectWithChannel(context.Background(), "Is this our first time talking?", "", "discord", "chat-policy")
	if err != nil {
		t.Fatalf("process failed: %v", err)
	}
	if !strings.Contains(resp, "Fresh session") {
		t.Fatalf("unexpected response: %s", resp)
	}
	if provider.calls < 2 {
		t.Fatalf("expected at least 2 provider calls, got %d", provider.calls)
	}
	if !provider.sawBlockedTool {
		t.Fatalf("expected blocked tool feedback to be injected into tool messages")
	}

	disallowed := map[string]struct{}{
		"exec":        {},
		"read_file":   {},
		"write_file":  {},
		"edit_file":   {},
		"append_file": {},
		"list_dir":    {},
	}
	for _, name := range provider.firstCallTools {
		if _, blocked := disallowed[name]; blocked {
			t.Fatalf("expected %s to be filtered from conversation-turn tool definitions", name)
		}
	}
}
