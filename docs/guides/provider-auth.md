# Provider and Auth Guide

DotAgent supports:
- `openrouter`
- `openai`
- `openai-codex`
- `ollama`

## Selection

Set:
- `agents.defaults.provider`
- `agents.defaults.model`

## Auth Models

- `openrouter`: `providers.openrouter.api_key`
- `openai`: exactly one of:
  - `providers.openai.api_key`
  - `providers.openai.oauth_access_token`
  - `providers.openai.oauth_token_file`
- `openai-codex`: exactly one of:
  - `providers.openai_codex.oauth_access_token`
  - `providers.openai_codex.oauth_token_file`
- `ollama`: no auth required by default; optional:
  - `providers.ollama.api_key`

## Base URLs

- OpenRouter: `https://openrouter.ai/api/v1`
- OpenAI Platform: `https://api.openai.com/v1`
- OpenAI Codex backend: `https://chatgpt.com/backend-api`
- Ollama (OpenAI-compatible): `http://127.0.0.1:11434/v1`

See full generated details in [Provider Reference](../reference/providers.md).
