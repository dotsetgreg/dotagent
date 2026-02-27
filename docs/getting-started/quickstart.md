# Quickstart

## 1. Configure Provider and Discord

Run:

```bash
dotagent config set providers.openrouter.api_key '"YOUR_OPENROUTER_KEY"'
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
