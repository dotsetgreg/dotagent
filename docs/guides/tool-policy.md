# Tool Policy Modes

Tool policy determines which tools are exposed to the model per turn.

## Modes

- `auto`: selects `workspace_ops` for workspace/system intent, otherwise `conversation`
- `conversation`: conversational-safe mode (web tools by default)
- `workspace_ops`: broad tool access for local operations

## Config

```json
{
  "tools": {
    "policy": {
      "default_mode": "auto",
      "allow": [],
      "deny": [],
      "provider_modes": {
        "openrouter": "auto",
        "openai": "auto",
        "openai-codex": "auto"
      }
    }
  }
}
```

## In-Chat Session Overrides

- `/tools status`
- `/tools mode auto`
- `/tools mode conversation`
- `/tools mode workspace_ops`
- `/tools mode clear`

## Important Distinction

- `tools.policy.*`: controls tool exposure policy
- `agents.defaults.restrict_to_workspace`: controls shell/path safety restrictions in command tools
