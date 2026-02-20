# Config Reference

Generated from `pkg/config/config.go` and `config.DefaultConfig()`.

| Key | Type | Env Var | Default |
| --- | --- | --- | --- |
| `agents.defaults.max_tokens` | `int` | `DOTAGENT_AGENTS_DEFAULTS_MAX_TOKENS` | `16384` |
| `agents.defaults.max_tool_iterations` | `int` | `DOTAGENT_AGENTS_DEFAULTS_MAX_TOOL_ITERATIONS` | `50` |
| `agents.defaults.model` | `string` | `DOTAGENT_AGENTS_DEFAULTS_MODEL` | `"openai/gpt-5.2"` |
| `agents.defaults.provider` | `string` | `DOTAGENT_AGENTS_DEFAULTS_PROVIDER` | `"openrouter"` |
| `agents.defaults.restrict_to_workspace` | `bool` | `DOTAGENT_AGENTS_DEFAULTS_RESTRICT_TO_WORKSPACE` | `true` |
| `agents.defaults.temperature` | `float` | `DOTAGENT_AGENTS_DEFAULTS_TEMPERATURE` | `0.7` |
| `agents.defaults.workspace` | `string` | `DOTAGENT_AGENTS_DEFAULTS_WORKSPACE` | `"~/.dotagent/workspace"` |
| `channels.discord.allow_from` | `array<string>` | `DOTAGENT_CHANNELS_DISCORD_ALLOW_FROM` | `[]` |
| `channels.discord.token` | `string` | `DOTAGENT_CHANNELS_DISCORD_TOKEN` | `""` |
| `gateway.host` | `string` | `DOTAGENT_GATEWAY_HOST` | `"0.0.0.0"` |
| `gateway.port` | `int` | `DOTAGENT_GATEWAY_PORT` | `18790` |
| `heartbeat.enabled` | `bool` | `DOTAGENT_HEARTBEAT_ENABLED` | `true` |
| `heartbeat.interval` | `int` | `DOTAGENT_HEARTBEAT_INTERVAL` | `30` |
| `memory.audit_retention_days` | `int` | `DOTAGENT_MEMORY_AUDIT_RETENTION_DAYS` | `365` |
| `memory.candidate_limit` | `int` | `DOTAGENT_MEMORY_CANDIDATE_LIMIT` | `80` |
| `memory.embedding_model` | `string` | `DOTAGENT_MEMORY_EMBEDDING_MODEL` | `"dotagent-chargram-384-v1"` |
| `memory.event_retention_days` | `int` | `DOTAGENT_MEMORY_EVENT_RETENTION_DAYS` | `90` |
| `memory.max_recall_items` | `int` | `DOTAGENT_MEMORY_MAX_RECALL_ITEMS` | `8` |
| `memory.persona_file_sync_mode` | `string` | `DOTAGENT_MEMORY_PERSONA_FILE_SYNC_MODE` | `"export_only"` |
| `memory.persona_min_confidence` | `float` | `DOTAGENT_MEMORY_PERSONA_MIN_CONFIDENCE` | `0.52` |
| `memory.persona_policy_mode` | `string` | `DOTAGENT_MEMORY_PERSONA_POLICY_MODE` | `"balanced"` |
| `memory.persona_sync_apply` | `bool` | `DOTAGENT_MEMORY_PERSONA_SYNC_APPLY` | `true` |
| `memory.retrieval_cache_seconds` | `int` | `DOTAGENT_MEMORY_RETRIEVAL_CACHE_SECONDS` | `20` |
| `memory.worker_lease_seconds` | `int` | `DOTAGENT_MEMORY_WORKER_LEASE_SECONDS` | `60` |
| `memory.worker_poll_ms` | `int` | `DOTAGENT_MEMORY_WORKER_POLL_MS` | `700` |
| `providers.openai.api_base` | `string` | `DOTAGENT_PROVIDERS_OPENAI_API_BASE` | `""` |
| `providers.openai.api_key` | `string` | `DOTAGENT_PROVIDERS_OPENAI_API_KEY` | `""` |
| `providers.openai.oauth_access_token` | `string` | `DOTAGENT_PROVIDERS_OPENAI_OAUTH_ACCESS_TOKEN` | `-` |
| `providers.openai.oauth_token_file` | `string` | `DOTAGENT_PROVIDERS_OPENAI_OAUTH_TOKEN_FILE` | `-` |
| `providers.openai.organization` | `string` | `DOTAGENT_PROVIDERS_OPENAI_ORGANIZATION` | `-` |
| `providers.openai.project` | `string` | `DOTAGENT_PROVIDERS_OPENAI_PROJECT` | `-` |
| `providers.openai.proxy` | `string` | `DOTAGENT_PROVIDERS_OPENAI_PROXY` | `-` |
| `providers.openai_codex.api_base` | `string` | `DOTAGENT_PROVIDERS_OPENAI_CODEX_API_BASE` | `""` |
| `providers.openai_codex.oauth_access_token` | `string` | `DOTAGENT_PROVIDERS_OPENAI_CODEX_OAUTH_ACCESS_TOKEN` | `-` |
| `providers.openai_codex.oauth_token_file` | `string` | `DOTAGENT_PROVIDERS_OPENAI_CODEX_OAUTH_TOKEN_FILE` | `-` |
| `providers.openai_codex.proxy` | `string` | `DOTAGENT_PROVIDERS_OPENAI_CODEX_PROXY` | `-` |
| `providers.openrouter.api_base` | `string` | `DOTAGENT_PROVIDERS_OPENROUTER_API_BASE` | `""` |
| `providers.openrouter.api_key` | `string` | `DOTAGENT_PROVIDERS_OPENROUTER_API_KEY` | `""` |
| `providers.openrouter.proxy` | `string` | `DOTAGENT_PROVIDERS_OPENROUTER_PROXY` | `-` |
| `tools.web.brave.api_key` | `string` | `DOTAGENT_TOOLS_WEB_BRAVE_API_KEY` | `""` |
| `tools.web.brave.enabled` | `bool` | `DOTAGENT_TOOLS_WEB_BRAVE_ENABLED` | `false` |
| `tools.web.brave.max_results` | `int` | `DOTAGENT_TOOLS_WEB_BRAVE_MAX_RESULTS` | `5` |
| `tools.web.duckduckgo.enabled` | `bool` | `DOTAGENT_TOOLS_WEB_DUCKDUCKGO_ENABLED` | `true` |
| `tools.web.duckduckgo.max_results` | `int` | `DOTAGENT_TOOLS_WEB_DUCKDUCKGO_MAX_RESULTS` | `5` |
