# Incident Response

## Runtime Incident Triage

1. Capture `dotagent status` output.
2. Capture provider error message and HTTP status.
3. Confirm config path and loaded workspace.
4. Verify Discord token and gateway readiness.

## Memory Safety

Before invasive changes:
- backup workspace volume
- especially `workspace/state/memory.db`

## Rollback

- Roll back container image
- keep workspace volume unchanged
