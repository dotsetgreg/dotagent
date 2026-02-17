# DotAgent

DotAgent is a Go-based personal agent runtime with:
- Messaging: Discord
- LLM provider: OpenRouter
- Memory: unified SQLite store (`workspace/state/memory.db`)

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

## Config Notes

- Provider is OpenRouter (`agents.defaults.provider=openrouter`)
- Discord is the only messaging channel (`channels.discord`)
- Default model is `openai/gpt-5.2`
- Canonical memory DB: `~/.dotagent/workspace/state/memory.db`

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

## Test

```bash
GOCACHE=/tmp/go-build go test ./...
```
