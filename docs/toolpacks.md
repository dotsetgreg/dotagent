# Toolpacks

Toolpacks are installable capability bundles that register additional tools at runtime.

## Directory Layout

Installed toolpacks live under:

`<workspace>/toolpacks/<toolpack-id>/toolpack.json`

Example packs:

- `examples/toolpacks/github-cli/toolpack.json`
- `examples/toolpacks/mcp-streamable/toolpack.json`
- `examples/toolpacks/openapi-local/toolpack.json`

## Manifest Schema

Toolpacks support three tool types:

- `command`
- `mcp`
- `openapi`

`mcp` and `openapi` tools require a connector defined in `connectors[]`.

```json
{
  "id": "example-pack",
  "name": "Example Pack",
  "version": "1.0.0",
  "description": "Connector-backed toolpack",
  "enabled": false,
  "connectors": [
    {
      "id": "mcp_main",
      "type": "mcp",
      "description": "Primary MCP endpoint",
      "mcp": {
        "transport": "streamable_http",
        "url": "https://mcp.example.com",
        "headers": {
          "Authorization": "env:MCP_TOKEN"
        },
        "timeout_seconds": 30
      }
    },
    {
      "id": "petstore_api",
      "type": "openapi",
      "openapi": {
        "spec_path": "spec/petstore.json",
        "base_url": "https://api.example.com",
        "headers": {
          "X-API-Key": "env:PETSTORE_API_KEY"
        }
      }
    }
  ],
  "tools": [
    {
      "name": "mcp_issue_search",
      "type": "mcp",
      "connector_id": "mcp_main",
      "remote_tool": "issue_search"
    },
    {
      "name": "api_get_pet",
      "type": "openapi",
      "connector_id": "petstore_api",
      "operation_id": "getPetById"
    },
    {
      "name": "gh_issue_list",
      "type": "command",
      "command_template": "gh issue list --limit {{limit}}",
      "parameters": {
        "type": "object",
        "properties": {
          "limit": {
            "type": "integer"
          }
        },
        "required": ["limit"]
      }
    }
  ]
}
```

## Connector Notes

### MCP Connector

Supported transports:

- `stdio`
- `streamable_http`

Typical config:

- `stdio`: `command`, `args`, optional `env`, `working_dir`
- `streamable_http`: `url`, optional `headers`

Runtime behavior:

- Performs MCP initialize handshake.
- Discovers remote tools via `tools/list`.
- Executes via `tools/call`.
- Supports timeout, retries, and bounded concurrency.
- Supports `env:VAR_NAME` references for URL/command/args/env/header values.

### OpenAPI Connector

Spec source:

- `spec_path` (local JSON file)
- `spec_url` (remote JSON)

Runtime behavior:

- Parses operations from OpenAPI paths.
- Resolves local `$ref` schema pointers.
- Maps operation parameters/request body into tool schema.
- Executes HTTP calls with timeout/retries and connector headers.
- Supports `env:VAR_NAME` references for spec/base/auth/header values.
- Requires a concrete absolute base URL (`base_url` or `servers[0].url`).

## Validation Rules

- `id`, `name`, `version`, and `tools` are required.
- Tool names must match `[a-z][a-z0-9_]{1,63}`.
- Tool names cannot collide with built-in tools.
- Duplicate tool names in one manifest are rejected.
- `command` tools require `command_template`.
- `mcp`/`openapi` tools require `connector_id`.
- Connector `id` values must be unique.
- Connector type must be `mcp` or `openapi`.
- `openapi` connectors require `spec_path` or `spec_url`.
- Symlinks are rejected during local install.

## CLI

```bash
dotagent toolpacks list
dotagent toolpacks install ./examples/toolpacks/github-cli
dotagent toolpacks install owner/repo@v1.0.0
dotagent toolpacks enable github-cli
dotagent toolpacks show github-cli
dotagent toolpacks validate
dotagent toolpacks doctor
dotagent toolpacks disable github-cli
dotagent toolpacks remove github-cli
```

`toolpacks validate [id]` checks manifest/config correctness.

`toolpacks doctor [id]` instantiates connectors and runs health checks.

`toolpacks install owner/repo[@ref]` resolves `ref` (default `main`) to a commit SHA, downloads that immutable archive, and installs the full toolpack directory (manifest plus referenced assets such as OpenAPI specs). The lock entry stores the pinned commit source.
