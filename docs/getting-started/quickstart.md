# Quickstart

## 1. Configure Provider and Discord

Run:

```bash
dotagent config set providers.openrouter.api_key '"YOUR_OPENROUTER_KEY"'
dotagent config set channels.discord.token '"YOUR_DISCORD_BOT_TOKEN"'
```

Or use local Ollama:

```bash
dotagent config set agents.defaults.provider '"ollama"'
dotagent config set agents.defaults.model '"llama3.2"'
dotagent config set providers.ollama.api_base '"http://127.0.0.1:11434/v1"'
dotagent config set channels.discord.token '"YOUR_DISCORD_BOT_TOKEN"'
```

Or edit:

`~/.dotagent/instances/default/config/config.json`

Minimum defaults:

```json
{
  "agents": {
    "defaults": {
      "provider": "openrouter",
      "model": "openai/gpt-5.2"
    }
  },
  "channels": {
    "discord": {
      "token": "YOUR_DISCORD_BOT_TOKEN"
    }
  }
}
```

## 2. Run Gateway

```bash
dotagent runtime up
```

## 3. Verify Commands

In Discord:
- `/persona show`

## 4. Local One-Shot Test

```bash
dotagent agent -m "hello"
```
