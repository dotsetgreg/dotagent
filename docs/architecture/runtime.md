# Runtime Architecture

DotAgent runtime has five main surfaces:

1. CLI command layer (`cmd/dotagent`)
2. Agent loop (`pkg/agent`)
3. Provider layer (`pkg/providers`)
4. Tool layer (`pkg/tools`, `pkg/toolpacks`)
5. Memory subsystem (`pkg/memory`)

## Command Source of Truth

The Cobra command tree defines:
- user-facing CLI UX
- generated CLI markdown reference
- generated man pages

This keeps command behavior and reference docs synchronized.
