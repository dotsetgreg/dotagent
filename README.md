# DotAgent

DotAgent is a Go-based personal agent runtime with:
- Messaging: Discord
- LLM provider: OpenRouter
- Memory: unified SQLite store (`workspace/state/memory.db`)
- Persona: unified identity/soul/user profile pipeline with automatic persistence and recall

## Requirements

- Go 1.25+
- OpenRouter API key
- Discord bot token (for `gateway` mode)

## Install

```bash
git clone https://github.com/dotsetgreg/dotagent.git
cd dotagent
go build -o dotagent ./cmd/dotagent
```

## Onboard

```bash
./dotagent onboard
```

This creates `~/.dotagent/config.json` and a default workspace.

Update at least:
- `providers.openrouter.api_key`
- `channels.discord.token` (for gateway mode)

Validate setup:

```bash
./dotagent status
```

## Run

Local CLI interaction:

```bash
./dotagent agent
./dotagent agent -m "Summarize this repo"
```

Discord gateway:

```bash
./dotagent gateway
```

Headless gateway management with Docker Compose:

```bash
# Start in background
docker compose --profile gateway up -d dotagent-gateway

# Check service status
docker compose ps dotagent-gateway

# Stream logs
docker compose logs -f dotagent-gateway

# Restart
docker compose restart dotagent-gateway

# Stop / start
docker compose stop dotagent-gateway
docker compose start dotagent-gateway
```

## Config Notes

- Provider is OpenRouter (`agents.defaults.provider=openrouter`)
- Discord is the only messaging channel (`channels.discord`)
- Default model is `openai/gpt-5.2`
- Canonical memory DB: `~/.dotagent/workspace/state/memory.db`
- Canonical persona profile and revision history are stored in the same SQLite DB

## Persona System

DotAgent now runs a first-class persona layer that is fully integrated with memory:

- Canonical persona profile in SQLite (single source of truth)
- Automatic candidate extraction from conversation turns (heuristic + model-assisted)
- Stability checks, contradiction checks, and sensitive-data guardrails before applying updates
- Revision log with rollback support
- Deterministic rendering of `IDENTITY.md`, `SOUL.md`, and `USER.md`
- Reverse import: manual edits to those files are merged back into canonical profile
- Persona prompt card is injected into context with token budgeting and cache

## Environment Variables

```bash
DOTAGENT_PROVIDERS_OPENROUTER_API_KEY=sk-or-v1-...
DOTAGENT_PROVIDERS_OPENROUTER_API_BASE=https://openrouter.ai/api/v1
DOTAGENT_AGENTS_DEFAULTS_PROVIDER=openrouter
DOTAGENT_AGENTS_DEFAULTS_MODEL=openai/gpt-5.2

DOTAGENT_CHANNELS_DISCORD_TOKEN=YOUR_DISCORD_BOT_TOKEN
DOTAGENT_CHANNELS_DISCORD_ALLOW_FROM=123456789012345678

DOTAGENT_MEMORY_MAX_RECALL_ITEMS=8
DOTAGENT_MEMORY_CANDIDATE_LIMIT=80
DOTAGENT_MEMORY_RETRIEVAL_CACHE_SECONDS=20
DOTAGENT_MEMORY_WORKER_POLL_MS=700
DOTAGENT_MEMORY_WORKER_LEASE_SECONDS=60
```

## Commands

```bash
dotagent onboard
dotagent agent
dotagent gateway
dotagent status
dotagent cron
dotagent skills
dotagent version
```

Skill notes:
- DotAgent no longer ships pre-bundled workspace skills.
- Install skills when needed with `dotagent skills install <owner/repo-or-path>`.

## Test

```bash
GOCACHE=/tmp/go-build go test ./...
```
