# dotagent

## dotagent

Instance-based AI agent runtime with Docker-first operations

### Synopsis

dotagent is an operator-focused, instance-scoped AI agent runtime.

Use init/doctor/runtime/config/backup commands for production lifecycle operations.
Use agent/gateway in dev mode only.

```text
dotagent [flags]
```

### Options

```text
  -h, --help              help for dotagent
      --instance string   Instance ID under ~/.dotagent/instances (default "default")
  -v, --version           Show build/version metadata
```

### SEE ALSO

* [dotagent agent](dotagent_agent.md)   - Run direct local chat with the agent (dev mode)
* [dotagent backup](dotagent_backup.md)   - Create and restore instance backups
* [dotagent config](dotagent_config.md)   - Inspect and mutate instance configuration
* [dotagent cron](dotagent_cron.md)   - Manage scheduled jobs
* [dotagent doctor](dotagent_doctor.md)   - Run deterministic instance readiness checks
* [dotagent gateway](dotagent_gateway.md)   - Run native gateway (dev mode only)
* [dotagent init](dotagent_init.md)   - Initialize an instance-scoped DotAgent installation
* [dotagent migrate](dotagent_migrate.md)   - Migrate legacy ~/.dotagent config/workspace into instance layout
* [dotagent runtime](dotagent_runtime.md)   - Manage Docker runtime lifecycle for an instance
* [dotagent skills](dotagent_skills.md)   - Install, remove, search, and inspect skills
* [dotagent toolpacks](dotagent_toolpacks.md)   - Manage executable tool packs
* [dotagent version](dotagent_version.md)   - Show build/version metadata
