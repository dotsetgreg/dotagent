# dotagent init

## dotagent init

Initialize an instance-scoped DotAgent installation

### Synopsis

Create instance directories, config v2, workspace templates, and runtime compose artifacts.

```text
dotagent init [flags]
```

### Options

```text
      --force             Overwrite existing config and runtime artifacts
  -h, --help              help for init
      --migrate-legacy    Import legacy ~/.dotagent layout when present (default true)
      --non-interactive   Fail instead of prompting when user input is required
      --yes               Assume yes for overwrite prompts
```

### Options inherited from parent commands

```text
      --instance string   Instance ID under ~/.dotagent/instances (default "default")
```

### SEE ALSO

* [dotagent](dotagent.md)   - Instance-based AI agent runtime with Docker-first operations
