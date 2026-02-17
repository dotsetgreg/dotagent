# Memory Continuity Runbook

## Primary Symptoms

- Assistant asks user to repeat recently provided details.
- Recall cards unexpectedly empty.
- Large continuity regressions after compaction/restart.

## First Checks

1. Verify canonical session identity consistency:
   - same workspace/channel/chat/actor across turns.
2. Check continuity fail-closed metrics:
   - `memory.context.fail_closed`
3. Check degraded context metrics:
   - `memory.context.degraded`
4. Check job failures:
   - `memory.job.failed`
   - inspect `memory_jobs.error`
5. Check persona sync metrics:
   - `memory.persona.apply_sync.error`
   - `memory.persona.apply.accepted|rejected|deferred`
   - `memory.persona.conflict_detected`

## Useful SQL

```sql
-- Recent events in a session
SELECT id, turn_id, seq, role, archived, created_at_ms
FROM events
WHERE session_key = ?
ORDER BY created_at_ms DESC, seq DESC
LIMIT 100;

-- Latest structured snapshot
SELECT revision, created_at_ms, summary
FROM session_snapshots
WHERE session_key = ?
ORDER BY revision DESC
LIMIT 1;

-- Memory items by scope
SELECT id, scope_type, scope_id, kind, item_key, confidence, last_seen_at_ms
FROM memory_items
WHERE user_id = ? AND agent_id = ? AND deleted_at_ms = 0
ORDER BY last_seen_at_ms DESC
LIMIT 200;

-- Candidate persona updates and status
SELECT turn_id, field_path, status, rejected_reason, created_at_ms
FROM persona_candidates
WHERE user_id = ? AND agent_id = ?
ORDER BY created_at_ms DESC
LIMIT 100;

-- Persona revisions (newest first)
SELECT id, turn_id, field_path, operation, old_value, new_value, source, created_at_ms
FROM persona_revisions
WHERE user_id = ? AND agent_id = ?
ORDER BY created_at_ms DESC
LIMIT 100;

-- Failed worker jobs
SELECT id, job_type, status, error, payload_json, updated_at_ms
FROM memory_jobs
WHERE status = 'failed'
ORDER BY updated_at_ms DESC
LIMIT 100;

-- Auditable memory mutations
SELECT action, entity, entity_id, reason, payload_json, created_at_ms
FROM memory_audit_log
ORDER BY created_at_ms DESC
LIMIT 200;
```

## Regression Gates

- Unit/integration:
  - `make test-memory`
- Long-horizon synthetic eval:
  - `make memory-eval`
- Full memory canary gate:
  - `make memory-canary`

Release checklist additions:
- `go test ./pkg/agent ./pkg/memory -count=1`
- verify no hardcoded assistant identity in system prompt path
- verify persona sync apply metrics are non-error in canary environment

## Common Recovery Actions

1. If continuity is failing closed:
   - inspect session identity mismatch first.
   - inspect archived events + summary/snapshot availability.
2. If retrieval quality drops:
   - verify embedding model setting.
   - verify FTS trigger health.
3. If persona updates stall:
   - inspect deferred/rejected candidate reasons.
   - inspect evidence-hit counters (`persona_signals`).
4. If same-turn identity updates are missing:
   - verify `memory.persona.apply_sync.error` is zero.
   - verify accepted decisions exist for that turn in `persona_candidates`.
   - verify prompt contains the persona card and no hardcoded identity text.
