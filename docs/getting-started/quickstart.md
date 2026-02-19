# Quickstart

## 1. Configure Provider and Discord

Edit `~/.dotagent/config.json`.

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
dotagent gateway
```

## 3. Verify Commands

In Discord:
- `/persona show`

## 4. Local One-Shot Test

```bash
dotagent agent -m "hello"
```
