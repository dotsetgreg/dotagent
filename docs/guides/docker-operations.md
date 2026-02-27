# Docker Operations

## Preserve Memory and Workspace

Persist the instance root volume:

`~/.dotagent/instances/<id>/`

Critical path:
- `data/state/memory.db`

Do not remove that volume during upgrades.

## Upgrade Flow

1. Pull latest code on host.
2. Rebuild image/container.
3. Restart runtime with same mounted instance volume.
4. Validate with `dotagent runtime status --check`.

## Health Checks

Gateway exposes:
- `/health`
- `/ready`

Default bind:
- `gateway.host`: `0.0.0.0`
- `gateway.port`: `18790`

## Ollama on Host

If DotAgent runs in Docker but Ollama runs on the host, set:

- `providers.ollama.api_base`: `http://host.docker.internal:11434/v1`
