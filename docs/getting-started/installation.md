# Installation

## Prerequisites

- Go 1.25+
- Docker / Docker Compose (recommended for gateway mode)
- A Discord bot token
- Provider credentials (`openrouter`, `openai`, or `openai-codex`)

## Build

```bash
make build
```

Binary output:
- `build/dotagent` (symlink)
- platform-specific binary in `build/`

## Initial Onboarding

```bash
dotagent onboard
```

This creates:
- `~/.dotagent/config.json`
- `~/.dotagent/workspace/*` template files

## Validate Setup

```bash
dotagent status
```

Check that provider credentials and Discord token are marked ready.
