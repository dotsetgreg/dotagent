# DotAgent

DotAgent is a Go-based personal agent runtime with:
- Messaging: Discord
- LLM provider: OpenRouter
- Memory: unified SQLite store (`workspace/state/memory.db`)
- Persona: unified identity/soul/user profile pipeline with automatic persistence and recall
- Context continuity: strict fail-closed checks when durable context is unavailable
- Session identity: canonical v2 session keys derived from workspace/channel/conversation/actor
- Extensibility: installable toolpacks for command, MCP, and OpenAPI capability expansion

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
- Synchronous same-turn persona apply path for explicit directives (optional via config)
- Automatic candidate extraction from conversation turns (heuristic + model-assisted)
- Policy-driven acceptance/rejection with stable-field conflict handling and reason codes
- Revision log with rollback support
- Deterministic rendering of `IDENTITY.md`, `SOUL.md`, and `USER.md`
- Configurable file sync mode: `export_only` (default), `import_export`, `disabled`
- Persona prompt card is injected into context with token budgeting and cache

## Context + Memory Architecture

DotAgent uses a tiered, durable context system:

- Tier 0: recent verbatim turn history
- Tier 1: session summary + structured session snapshot
- Tier 2: recalled long-term memory cards (hybrid lexical + embedding + rerank)
- Tier 3: persona profile prompt block

Key guarantees:

- Atomic user-turn persistence: user event and immediate memory capture are written transactionally
- Persona precedence is deterministic: provider safety constraints > accepted current-turn directives > persisted profile > defaults
- Scope-aware long-term memory:
  - `session` scope for episodic/task state
  - `user` scope for durable preferences/facts across sessions
  - `global` scope for shared procedural/system memory
- Continuity fail-closed: if prior-session continuity artifacts are missing, agent processing stops safely rather than hallucinating continuity
- Provider-state hooks: optional support for provider-managed conversation state IDs, while still keeping local canonical event logs

Operational safeguards:

- Sensitive-content filtering before durable memory writes
- Durable audit log (`memory_audit_log`) for memory upserts/deletes
- Retention sweeps for archived events, expired/deleted memory, cache, and audit records
- Turn-level tool governance: conversational turns do not expose local filesystem/shell tools, and runtime state paths (`workspace/state/*`) are blocked from tool-based continuity introspection
- Runtime process/session tools:
  - `process` for long-running command lifecycle control (`start/list/poll/write/kill/clear`)
  - `session` for cross-session inspection and targeted send/spawn flows

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
DOTAGENT_MEMORY_EMBEDDING_MODEL=dotagent-chargram-384-v1
DOTAGENT_MEMORY_EVENT_RETENTION_DAYS=90
DOTAGENT_MEMORY_AUDIT_RETENTION_DAYS=365
DOTAGENT_MEMORY_PERSONA_SYNC_APPLY=true
DOTAGENT_MEMORY_PERSONA_FILE_SYNC_MODE=export_only
DOTAGENT_MEMORY_PERSONA_POLICY_MODE=balanced
DOTAGENT_MEMORY_PERSONA_MIN_CONFIDENCE=0.52
```

## Commands

```bash
dotagent onboard
dotagent agent
dotagent gateway
dotagent status
dotagent cron
dotagent skills
dotagent toolpacks
dotagent version
# Tool policy controls in chat:
/tools
/tools mode conversation
/tools mode workspace_ops
# In-chat persona diagnostics:
/persona show
/persona revisions
/persona candidates [status]
/persona rollback
```

Skill notes:
- DotAgent no longer ships pre-bundled workspace skills.
- Install skills when needed with `dotagent skills install <owner/repo-or-path>`.

## Test

```bash
GOCACHE=/tmp/go-build go test ./...
```

Memory-focused regression gates:

```bash
make test-memory
make memory-eval
make memory-canary
```

Reference docs:

- `docs/memory-architecture.md`
- `docs/memory-runbook.md`
- `docs/toolpacks.md`

Toolpack diagnostics:

```bash
dotagent toolpacks validate [id]
dotagent toolpacks doctor [id]
```
