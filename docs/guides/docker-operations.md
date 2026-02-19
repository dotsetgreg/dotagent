# Docker Operations

## Preserve Memory and Workspace

Persist `~/.dotagent/workspace` as a volume.

Critical path:
- `workspace/state/memory.db`

Do not remove that volume during upgrades.

## Upgrade Flow

1. Pull latest code on host.
2. Rebuild image/container.
3. Restart container with same mounted workspace volume.
4. Validate with `dotagent status`.

## Health Checks

Gateway exposes:
- `/health`
- `/ready`

Default bind:
- `gateway.host`: `0.0.0.0`
- `gateway.port`: `18790`
