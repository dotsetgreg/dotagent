# DotAgent

DotAgent is a lightweight AI agent runtime written in Go.

Supported runtime integrations:
- Messaging: Discord only
- LLM provider: OpenRouter only

## Requirements

- Go 1.25+
- OpenRouter API key
- Discord bot token (gateway mode only)

## Install

```bash
git clone https://github.com/dotsetgreg/dotagent.git
cd dotagent
make deps
make build
```

## Quick Start

### 1. Initialize config and workspace

```bash
dotagent onboard
```

### 2. Configure OpenRouter and Discord

Edit `~/.dotagent/config.json`:

```json
{
  "agents": {
    "defaults": {
      "workspace": "~/.dotagent/workspace",
      "restrict_to_workspace": true,
      "provider": "openrouter",
      "model": "openai/gpt-5.2",
      "max_tokens": 8192,
      "temperature": 0.7,
      "max_tool_iterations": 20
    }
  },
  "providers": {
    "openrouter": {
      "api_key": "sk-or-v1-...",
      "api_base": "https://openrouter.ai/api/v1"
    }
  },
  "channels": {
    "discord": {
      "enabled": true,
      "token": "YOUR_DISCORD_BOT_TOKEN",
      "allow_from": []
    }
  }
}
```

### 3. Run gateway mode

```bash
dotagent gateway
```

### 4. Or run local CLI mode

```bash
dotagent agent -m "Summarize this repo"
```

## Discord Setup

1. Create a Discord application and bot: <https://discord.com/developers/applications>
2. Copy bot token into `channels.discord.token`
3. Enable **MESSAGE CONTENT INTENT** for the bot
4. Invite bot with scope `bot` and permissions like `Send Messages` and `Read Message History`

## Environment Variables

```bash
DOTAGENT_PROVIDERS_OPENROUTER_API_KEY=sk-or-v1-...
DOTAGENT_PROVIDERS_OPENROUTER_API_BASE=https://openrouter.ai/api/v1
DOTAGENT_AGENTS_DEFAULTS_PROVIDER=openrouter
DOTAGENT_AGENTS_DEFAULTS_MODEL=openai/gpt-5.2

DOTAGENT_CHANNELS_DISCORD_ENABLED=true
DOTAGENT_CHANNELS_DISCORD_TOKEN=YOUR_DISCORD_BOT_TOKEN
```

## Commands

```bash
dotagent onboard
dotagent agent
dotagent agent -m "your prompt"
dotagent gateway
dotagent status
dotagent migrate
dotagent cron
dotagent skills
dotagent version
```

## Test

```bash
go test ./pkg/...
```

## License

MIT
