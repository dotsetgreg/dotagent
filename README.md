# DotAgent

DotAgent is a Go-based personal agent runtime with:
- Messaging: Discord
- LLM providers: OpenRouter, OpenAI API, OpenAI Codex OAuth, Ollama local models
- Memory: unified SQLite store (`data/state/memory.db`)
- Persona: unified identity/soul/user profile pipeline with automatic persistence and recall
- Context continuity: strict fail-closed checks when durable context is unavailable
- Session identity: canonical v2 session keys derived from workspace/channel/conversation/actor
- Extensibility: installable toolpacks for command, MCP, and OpenAPI capability expansion

## Requirements

- Go 1.25+
- Provider credentials:
  - OpenRouter API key, or
  - OpenAI API key / bearer token, or
  - OpenAI Codex OAuth token (for `openai-codex`), or
  - Ollama local runtime (no auth required by default)
- Discord bot token (for `gateway` mode)

## Install

```bash
git clone https://github.com/dotsetgreg/dotagent.git
cd dotagent
go build -o dotagent ./cmd/dotagent
```

## Initialize Instance

```bash
./dotagent init --non-interactive
```

This creates:

- `~/.dotagent/instances/default/config/config.json`
- `~/.dotagent/instances/default/config/history/`
- `~/.dotagent/instances/default/workspace/`
- `~/.dotagent/instances/default/data/`
- `~/.dotagent/instances/default/logs/`
- `~/.dotagent/instances/default/runtime/`

Set minimum credentials (OpenRouter example):

```bash
dotagent config set providers.openrouter.api_key '"<OPENROUTER_KEY>"'
dotagent config set channels.discord.token '"<DISCORD_BOT_TOKEN>"'
```

Validate setup:

```bash
dotagent doctor --check
```

## Run (Production, Docker-First)

Managed runtime lifecycle:

```bash
dotagent runtime up
dotagent runtime status --check
dotagent runtime logs -f
dotagent runtime restart
dotagent runtime down
```

## Run (Development)

Local one-shot/interactive:

```bash
dotagent agent
dotagent agent -m "Summarize this repo"
dotagent gateway --dev
```

## Config Notes

- Supported providers: `openrouter`, `openai`, `openai-codex`, and `ollama` (`agents.defaults.provider`)
- OpenAI (`openai`) auth modes: API key, direct bearer token, or bearer token file (for externally refreshed OAuth tokens)
  - Set exactly one auth source: `providers.openai.api_key`, `providers.openai.oauth_access_token`, or `providers.openai.oauth_token_file`.
  - `providers.openai.oauth_token_file` accepts either a plain token file or Codex/OpenAI auth JSON (extracts `tokens.access_token`).
- OpenAI Codex (`openai-codex`) auth modes: direct bearer token or bearer token file
  - Set exactly one auth source: `providers.openai_codex.oauth_access_token` or `providers.openai_codex.oauth_token_file`.
  - `providers.openai_codex.oauth_token_file` accepts either a plain token file or Codex/OpenAI auth JSON (extracts `tokens.access_token`).
  - Default `providers.openai_codex.api_base` is `https://chatgpt.com/backend-api` (DotAgent resolves the request endpoint to `/codex/responses`).
  - DotAgent does not perform browser OAuth login; provide token material from your own auth workflow.
- Ollama (`ollama`) auth mode: none by default (optional API key)
  - Set `agents.defaults.provider=ollama` and choose a local model (for example `llama3.2`).
  - Default `providers.ollama.api_base` is `http://127.0.0.1:11434/v1`.
  - Optional: `providers.ollama.api_key` when your Ollama deployment requires auth.
- Discord is the only messaging channel (`channels.discord`)
- Default model is `openai/gpt-5.2` (OpenRouter default)
- Canonical memory DB: `~/.dotagent/instances/default/data/state/memory.db`
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
- Runtime process/session tools:
  - `process` for long-running command lifecycle control (`start/list/poll/write/kill/clear`)
  - `session` for cross-session inspection and targeted send/spawn flows

## Environment Variables

```bash
DOTAGENT_PROVIDERS_OPENROUTER_API_KEY=sk-or-v1-...
DOTAGENT_PROVIDERS_OPENROUTER_API_BASE=https://openrouter.ai/api/v1

DOTAGENT_PROVIDERS_OPENAI_API_KEY=sk-proj-...
DOTAGENT_PROVIDERS_OPENAI_OAUTH_ACCESS_TOKEN=
DOTAGENT_PROVIDERS_OPENAI_OAUTH_TOKEN_FILE=~/.codex/auth.json
DOTAGENT_PROVIDERS_OPENAI_API_BASE=https://api.openai.com/v1
DOTAGENT_PROVIDERS_OPENAI_ORGANIZATION=org_xxx
DOTAGENT_PROVIDERS_OPENAI_PROJECT=proj_xxx

DOTAGENT_PROVIDERS_OPENAI_CODEX_OAUTH_ACCESS_TOKEN=
DOTAGENT_PROVIDERS_OPENAI_CODEX_OAUTH_TOKEN_FILE=~/.codex/auth.json
DOTAGENT_PROVIDERS_OPENAI_CODEX_API_BASE=https://chatgpt.com/backend-api
DOTAGENT_PROVIDERS_OPENAI_CODEX_PROXY=

DOTAGENT_PROVIDERS_OLLAMA_API_BASE=http://127.0.0.1:11434/v1
DOTAGENT_PROVIDERS_OLLAMA_API_KEY=
DOTAGENT_PROVIDERS_OLLAMA_PROXY=

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
DOTAGENT_MEMORY_PERSONA_SYNC_TIMEOUT_MS=2200
```

OpenAI Codex OAuth in Docker:

```yaml
# docker-compose.yml (dotagent-gateway.volumes)
- ${HOME}/.codex/auth.json:/root/.codex/auth.json:ro
```

```json
{
  "agents": { "defaults": { "provider": "openai-codex", "model": "gpt-5" } },
  "providers": {
    "openai_codex": {
      "oauth_token_file": "/root/.codex/auth.json",
      "api_base": "https://chatgpt.com/backend-api"
    }
  }
}
```

Ollama on host + DotAgent in Docker:

```json
{
  "agents": { "defaults": { "provider": "ollama", "model": "llama3.2" } },
  "providers": {
    "ollama": {
      "api_base": "http://host.docker.internal:11434/v1"
    }
  }
}
```

Switching providers does not clear memory. Memory remains in `data/state/memory.db`.

## Commands

```bash
dotagent init
dotagent migrate
dotagent doctor
dotagent runtime
dotagent config
dotagent backup
dotagent agent
dotagent gateway --dev
dotagent cron
dotagent skills
dotagent toolpacks
dotagent version
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

Documentation:

- Site source: `docs/`
- Configured by: `mkdocs.yml`
- Generated references:
  - `docs/reference/cli/*`
  - `docs/reference/man/*`
  - `docs/reference/config.md`
  - `docs/reference/providers.md`
  - `docs/reference/tools.md`

Docs commands:

```bash
make docs-gen
make docs-check
make docs-build
make docs-serve
```

Toolpack diagnostics:

```bash
dotagent toolpacks validate [id]
dotagent toolpacks doctor [id]
```
