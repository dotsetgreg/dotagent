# Troubleshooting

## Provider 401 / 403 / 400 Errors

1. Confirm active provider + model in `dotagent status`.
2. Verify credential source matches provider auth requirements.
3. Check provider `api_base` for the selected provider.
4. Restart container after config changes.

## Tool Access Problems

1. Confirm the message was routed to the expected agent session.
2. Confirm `restrict_to_workspace` expectations.
3. In restricted mode, avoid shell control operators in `exec` commands (`&&`, `|`, redirects).
4. For clone/install flows in restricted mode, use `working_dir` at workspace root plus relative output paths.
5. Check container/tool runtime logs for sandbox or execution guard errors (for example: `path outside working dir`).

## Docs Drift in CI

Run:

```bash
make docs-gen
make docs-check
```

Commit updated generated files under `docs/reference/`.
