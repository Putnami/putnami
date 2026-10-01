# ADR 0055 — Repository rules live in `.agents/constraints.md`, and Putnami writes no `.AI/` directory

- **Status**: accepted
- **Scope**: `@putnami/cli` (`internal/commands/agentctx`, `internal/mapgen`,
  `internal/engine`, the `mcp` command), public onboarding docs

## Context

A tool that installs into someone else's repository should add as little as it
can justify. Two mechanisms already carry what a generated context directory
would duplicate: the `putnami:guidance v2` block in `AGENTS.md` and
`CLAUDE.md` states the commands, selection rules, discovery routing, and gate;
the MCP server serves each extension's exact locked `AI.md` as a versioned
`putnami://extension-guidance/AI.md` resource, so framework recipes never need
copying into Git to stay current. A generated file in the tree goes stale
between regenerations.

Durable repository rules still need one hand-written home, and `.agents/`
already holds the workspace's agent content.

## Decision

1. `putnami init` and `putnami context generate` write the guidance block in
   `AGENTS.md` and `CLAUDE.md`, plus the opted-in agent content. They create no
   `.AI/` directory and no other generated context file. The only other root
   file Putnami writes is the `.mcp.json` registration
   ([ADR 0040](0040-register-the-mcp-server-and-reconcile-agent-workflows-at-session-start.md)).
   A guidance block Putnami cannot prove it wrote (edited, or written by a
   release whose bytes are no longer reconstructible) is preserved and
   reported; `putnami context generate --force` rewrites it.
2. The guidance block says ``Read `.agents/constraints.md` when it exists.``
   The file is human-maintained: Putnami reads it and never creates, rewrites,
   or deletes it.
3. The CLI does not know `.AI/`. It neither migrates nor deletes files in it,
   does not recognize whole-file entrypoints or the
   `putnami:generated-guidance v1` marker written by older releases, and does
   not refuse a workspace that commits `.AI/repo-map.json` or `.AI/repo-map.md`.
   Guessing that a file Putnami once generated is unwanted is the one mistake
   that loses a user's work.
4. The MCP contract baseline lives in
   `tooling/cli/internal/mcp/testdata/contract.json`, next to the test that
   reads it. The CLI-versus-MCP trial that decided to keep the MCP server is
   recorded in `tooling/cli/doc/15-mcp-server-spike.md`; no command analyzes
   its ledger.

## Consequences

- A workspace that keeps rules in `.AI/constraints.md` moves the file with
  `git mv .AI/constraints.md .agents/constraints.md`; until then, assistants
  are not pointed at it.
- An entrypoint an older release wrote whole is treated like any user file: it
  keeps its bytes and gains the guidance block.
- Leftover `.AI/` files are inert and hand-maintained; nothing reads or
  refreshes them.
- An assistant with no MCP server loses the extension recipes; the block's
  fallback rule (the pinned CLI plus focused file reads) and the published
  documentation cover that case.
- `context generate` still materializes the lock-pinned extensions and fails
  closed on a lock/store mismatch, although it copies no `AI.md` into the tree.
