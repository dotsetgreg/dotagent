# Troubleshooting

## Provider 401 / 403 / 400 Errors

1. Confirm active provider + model in `dotagent status`.
2. Verify credential source matches provider auth requirements.
3. Check provider `api_base` for the selected provider.
4. Restart container after config changes.

## Tool Access Problems

1. Check `/tools status` in Discord.
2. Confirm `tools.policy.provider_modes` and `default_mode`.
3. Confirm `restrict_to_workspace` expectations.

## Docs Drift in CI

Run:

```bash
make docs-gen
make docs-check
```

Commit updated generated files under `docs/reference/`.
