# Documentation System

This repository uses a split documentation architecture:

1. Code-generated references (authoritative):
- CLI command reference (`docs/reference/cli/*`)
- Man pages (`docs/reference/man/*`)
- Config reference (`docs/reference/config.md`)
- Provider reference (`docs/reference/providers.md`)
- Tool reference (`docs/reference/tools.md`)

2. Curated narrative docs (hand-authored):
- `docs/getting-started/*`
- `docs/guides/*`
- `docs/architecture/*`
- `docs/operations/*`
- `docs/contributing/*`

## Source of Truth

- CLI behavior: Cobra command tree in `cmd/dotagent/cli_cobra.go`
- Config schema/defaults: `pkg/config/config.go`
- Provider registry: `pkg/providers/*`
- Runtime tool registration and descriptions: `pkg/agent/loop.go`, `pkg/tools/*`

## Local Workflow

1. Generate references:
```bash
make docs-gen
```
2. Validate references are up to date + site builds:
```bash
make docs-check
```
3. Serve docs locally:
```bash
make docs-serve
```

## CI Rules

- PRs fail if generated docs are stale.
- PRs fail if MkDocs strict build fails.
- Release tags publish versioned docs with `mike`.
