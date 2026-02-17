# Memory And Context Architecture

## Goals

- Preserve continuity across long conversations and process restarts.
- Keep memory retrieval general-purpose (topic-agnostic).
- Prevent "forgotten context" responses when durable context should exist.
- Maintain clear data ownership and auditability.

## Session Identity

- Canonical session identity is derived from:
  - workspace namespace
  - channel
  - conversation/chat id
  - actor/user id
- `v2` session keys are deterministic hashes of that identity.
- Legacy session keys are only used as fallback.

## Tiered Prompt Context

- Tier 0: recent message history (verbatim turn stream)
- Tier 1: session summary + structured session snapshot
- Tier 2: recalled memory cards
- Tier 3: persona card

Prompt assembly is split into:

- stable system prefix
- dynamic context block

This improves cacheability and makes debugging context composition easier.

Persona precedence contract (highest to lowest):

1. model/provider safety constraints
2. accepted explicit current-turn persona directives
3. persisted persona profile (`persona_profiles`)
4. default bootstrap persona

## Durable Data Model

- `events`: canonical append-only conversational history
- `session_compactions`: compaction journal/checkpoints
- `session_snapshots`: structured compaction artifacts
- `memory_items`: normalized long-term memory objects
  - scope-aware: `session`, `user`, `global`
- `memory_observations`: immutable provenance trail for memory updates
- `memory_audit_log`: auditable upsert/delete actions
- `memory_embeddings`: embedding vectors + model id
- `retrieval_cache`: short-lived recall result cache
- `persona_*`: profile, candidates, revisions, and evidence signals

## Retrieval Pipeline

- Hybrid candidate generation:
  - FTS lexical
  - embedding similarity
  - recency decay
- Scope filtering:
  - session scope
  - user scope
  - global scope
- Intent-aware weighting + reranking

Embedding is pluggable:

- default: `dotagent-chargram-384-v1`
- fallback: `dotagent-hash-256-v1`

## Continuity Safety

- On existing sessions, if no continuity artifacts are available
  (history + summary + recall all missing), context building fails closed.
- Continuation-cue queries also fail closed when continuity artifacts are missing.
- Agent loop returns a controlled recovery response instead of fabricating continuity.

## Atomic User Turn Write

User turn ingestion is transactional:

1. append user event
2. extract immediate memory signals
3. upsert memory items (scope-aware)
4. persist embeddings
5. invalidate retrieval cache
6. write audit/provenance entries

All in one DB transaction.

## Persona Mutation Pipeline

- Current-turn persona directives are applied synchronously before final response generation
  when `persona_sync_apply` is enabled.
- Worker jobs still run for durable maintenance (`consolidate`, `persona_apply`, `compact`)
  and are idempotent per turn.
- Candidate decisions are explicit: `accepted`, `rejected`, `deferred` with reason codes.
- Persona file sync modes:
  - `export_only` (default): profile writes files, no reverse-import
  - `import_export`: bidirectional sync
  - `disabled`: file sync disabled

This prevents contradictory identity messages and enables immediate same-turn updates.

## Retention And Governance

- Sensitive content is filtered from durable memory capture.
- Periodic retention sweep removes:
  - archived events past retention window
  - deleted/expired memory items
  - expired retrieval cache rows
  - aged audit logs
