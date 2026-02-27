# Installation

## Prerequisites

- Go 1.25+
- Docker / Docker Compose (recommended for gateway mode)
- A Discord bot token
- Provider setup (`openrouter`, `openai`, `openai-codex`, or local `ollama`)

## Build

```bash
make build
```

Binary output:
- `build/dotagent` (symlink)
- platform-specific binary in `build/`

## Initial Onboarding

```bash
dotagent init --non-interactive
```

This creates:
- `~/.dotagent/instances/default/config/config.json`
- `~/.dotagent/instances/default/workspace/*` template files
- `~/.dotagent/instances/default/data/*` runtime state directories

## Validate Setup

```bash
dotagent doctor --check
```

Check that provider credentials and Discord token are marked ready.
