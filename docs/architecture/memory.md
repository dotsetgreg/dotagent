# Memory Architecture

Memory is persisted in workspace state and survives restarts when volumes persist.

Core storage:
- `workspace/state/memory.db`

Key behaviors:
- event persistence
- recall and ranking
- compaction/summarization
- persona profile extraction + revision history

Operationally, memory continuity depends on preserving the same workspace volume.
