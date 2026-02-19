# Provider Reference

Generated from provider factories and config structs.

## Supported Providers

- `openai`
- `openai-codex`
- `openrouter`

## `openrouter`

OpenRouter chat completions provider.

- Config path: `providers.openrouter`
- Auth: Requires `api_key`.
- Default API base: `""`

| Key | Type | Env Var | Default |
| --- | --- | --- | --- |
| `providers.openrouter.api_base` | `string` | `DOTAGENT_PROVIDERS_OPENROUTER_API_BASE` | `""` |
| `providers.openrouter.api_key` | `string` | `DOTAGENT_PROVIDERS_OPENROUTER_API_KEY` | `""` |
| `providers.openrouter.proxy` | `string` | `DOTAGENT_PROVIDERS_OPENROUTER_PROXY` | `-` |

## `openai`

OpenAI Platform provider (Responses API).

- Config path: `providers.openai`
- Auth: Requires exactly one credential source: `api_key` OR `oauth_access_token` OR `oauth_token_file`.
- Default API base: `""`

| Key | Type | Env Var | Default |
| --- | --- | --- | --- |
| `providers.openai.api_base` | `string` | `DOTAGENT_PROVIDERS_OPENAI_API_BASE` | `""` |
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
- Default API base: `""`

| Key | Type | Env Var | Default |
| --- | --- | --- | --- |
| `providers.openai_codex.api_base` | `string` | `DOTAGENT_PROVIDERS_OPENAI_CODEX_API_BASE` | `""` |
| `providers.openai_codex.oauth_access_token` | `string` | `DOTAGENT_PROVIDERS_OPENAI_CODEX_OAUTH_ACCESS_TOKEN` | `-` |
| `providers.openai_codex.oauth_token_file` | `string` | `DOTAGENT_PROVIDERS_OPENAI_CODEX_OAUTH_TOKEN_FILE` | `-` |
| `providers.openai_codex.proxy` | `string` | `DOTAGENT_PROVIDERS_OPENAI_CODEX_PROXY` | `-` |
