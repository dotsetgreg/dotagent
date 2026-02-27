# Provider Reference

Generated from provider factories and config structs.

## Supported Providers

- `ollama`
- `openai`
- `openai-codex`
- `openrouter`

## `openrouter`

OpenRouter chat completions provider.

- Config path: `providers.openrouter`
- Auth: Requires `api_key`.
- Default API base: `"https://openrouter.ai/api/v1"`

| Key | Type | Env Var | Default |
| --- | --- | --- | --- |
| `providers.openrouter.api_base` | `string` | `DOTAGENT_PROVIDERS_OPENROUTER_API_BASE` | `"https://openrouter.ai/api/v1"` |
| `providers.openrouter.api_key` | `string` | `DOTAGENT_PROVIDERS_OPENROUTER_API_KEY` | `""` |
| `providers.openrouter.proxy` | `string` | `DOTAGENT_PROVIDERS_OPENROUTER_PROXY` | `-` |

## `openai`

OpenAI Platform provider (Responses API).

- Config path: `providers.openai`
- Auth: Requires exactly one credential source: `api_key` OR `oauth_access_token` OR `oauth_token_file`.
- Default API base: `"https://api.openai.com/v1"`

| Key | Type | Env Var | Default |
| --- | --- | --- | --- |
| `providers.openai.api_base` | `string` | `DOTAGENT_PROVIDERS_OPENAI_API_BASE` | `"https://api.openai.com/v1"` |
| `providers.openai.api_key` | `string` | `DOTAGENT_PROVIDERS_OPENAI_API_KEY` | `""` |
| `providers.openai.oauth_access_token` | `string` | `DOTAGENT_PROVIDERS_OPENAI_OAUTH_ACCESS_TOKEN` | `-` |
| `providers.openai.oauth_token_file` | `string` | `DOTAGENT_PROVIDERS_OPENAI_OAUTH_TOKEN_FILE` | `-` |
| `providers.openai.organization` | `string` | `DOTAGENT_PROVIDERS_OPENAI_ORGANIZATION` | `-` |
| `providers.openai.project` | `string` | `DOTAGENT_PROVIDERS_OPENAI_PROJECT` | `-` |
| `providers.openai.proxy` | `string` | `DOTAGENT_PROVIDERS_OPENAI_PROXY` | `-` |

## `openai-codex`

ChatGPT/Codex OAuth provider for Codex backend routing.

- Config path: `providers.openai_codex`
- Auth: Requires exactly one credential source: `oauth_access_token` OR `oauth_token_file`.
- Default API base: `"https://chatgpt.com/backend-api"`

| Key | Type | Env Var | Default |
| --- | --- | --- | --- |
| `providers.openai_codex.api_base` | `string` | `DOTAGENT_PROVIDERS_OPENAI_CODEX_API_BASE` | `"https://chatgpt.com/backend-api"` |
| `providers.openai_codex.oauth_access_token` | `string` | `DOTAGENT_PROVIDERS_OPENAI_CODEX_OAUTH_ACCESS_TOKEN` | `-` |
| `providers.openai_codex.oauth_token_file` | `string` | `DOTAGENT_PROVIDERS_OPENAI_CODEX_OAUTH_TOKEN_FILE` | `-` |
| `providers.openai_codex.proxy` | `string` | `DOTAGENT_PROVIDERS_OPENAI_CODEX_PROXY` | `-` |

## `ollama`

Ollama local-model provider via OpenAI-compatible chat completions.

- Config path: `providers.ollama`
- Auth: No auth required by default; optional `api_key` supported.
- Default API base: `"http://127.0.0.1:11434/v1"`

| Key | Type | Env Var | Default |
| --- | --- | --- | --- |
| `providers.ollama.api_base` | `string` | `DOTAGENT_PROVIDERS_OLLAMA_API_BASE` | `"http://127.0.0.1:11434/v1"` |
| `providers.ollama.api_key` | `string` | `DOTAGENT_PROVIDERS_OLLAMA_API_KEY` | `-` |
| `providers.ollama.proxy` | `string` | `DOTAGENT_PROVIDERS_OLLAMA_PROXY` | `-` |
