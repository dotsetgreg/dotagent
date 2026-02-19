# Contributing Docs

## Rules

- Do not manually edit generated files under:
  - `docs/reference/cli/*`
  - `docs/reference/man/*`
  - `docs/reference/config.md`
  - `docs/reference/providers.md`
  - `docs/reference/tools.md`

- Update code source-of-truth, then regenerate docs.

## Commands

```bash
make docs-gen
make docs-check
```

## Adding a New CLI Command

1. Add command metadata in Cobra tree.
2. Run `make docs-gen`.
3. Verify generated CLI + man docs update.
4. Add/adjust CLI snapshot tests.
