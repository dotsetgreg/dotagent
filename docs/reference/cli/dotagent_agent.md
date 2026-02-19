# dotagent agent

## dotagent agent

Run direct local chat with the agent (CLI mode)

### Synopsis

Run an interactive local agent session or send one-shot messages without Discord.

```text
dotagent agent [flags]
```

### Examples

```text
  dotagent agent
  dotagent agent --session cli:workspace
  dotagent agent --message "summarize my TODOs"
```

### Options

```text
  -d, --debug            Enable debug logging
  -h, --help             help for agent
  -m, --message string   One-shot prompt to send to the agent
  -s, --session string   Session key for continuity (default "cli:default")
```

### SEE ALSO

* [dotagent](dotagent.md)   - Personal AI agent with Discord gateway, tools, memory, and provider routing
