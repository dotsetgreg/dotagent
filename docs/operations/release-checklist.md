# Release Checklist

## Pre-Tag

1. `make fmt`
2. `make test`
3. `make docs-gen`
4. `make docs-check`
5. Verify `dotagent status` on a clean environment

## Tag Release

- Push `v*` tag
- Confirm docs release workflow publishes versioned docs (`mike`)

## Post-Release

- Validate `latest` docs alias
- Validate generated references render correctly
