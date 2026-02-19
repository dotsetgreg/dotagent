# dotagent cron add

## dotagent cron add

Add a scheduled job

### Synopsis

Add a recurring job with either --every (seconds) or --cron expression.

```
dotagent cron add [flags]
```

### Examples

```
  dotagent cron add --name backup --message "run backup" --every 3600
  dotagent cron add --name digest --message "send daily digest" --cron '0 9 * * *' --deliver --channel discord --to 1234
```

### Options

```
      --channel string   Channel name for delivery
  -c, --cron string      Cron expression (e.g. '0 9 * * *')
  -d, --deliver          Deliver result back to a channel target
  -e, --every int        Run every N seconds
  -h, --help             help for add
  -m, --message string   Message payload for the job
  -n, --name string      Job name
      --to string        Recipient/chat target
```

### SEE ALSO

* [dotagent cron](dotagent_cron.md)	 - Manage scheduled jobs

