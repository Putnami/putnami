# ADR 0019 — A subcommand may declare that it reads no workspace state

- **Status**: accepted
- **Date**: 2026-11-14
- **Scope**: `go.putnami.dev/protocol/extension` (`protocols/extension`): the
  `workspaceState` member of a command-group subcommand, and the CLI's
  first-use bootstrap

## Context

Before an extension command runs, the CLI restores the workspace on first use:
when the recorded install state is missing or stale it runs the implicit
install, then restores the generated clients. A command such as
`putnami cloud whoami` reads a file and calls one endpoint. On a fresh
checkout it waited 16 seconds for an install and 46 `client-generate` jobs it
does not read.

## Decision

1. **A subcommand may declare `workspaceState: "none"`.** The vocabulary is
   closed: `none` is its one value, and the strict parser and the schema refuse
   any other (`invalid-enum`). The CLI reads it on the top-level subcommand it
   dispatches, as it reads `workspace` (ADR 0008).
2. **The CLI skips the bootstrap for it.** It runs neither the implicit install
   nor the generated-client restore before such a subcommand. Every other
   command keeps the bootstrap.
3. **No contract increment.** A CLI that drops the member runs the bootstrap,
   which is the behavior before the member: the command is correct and only
   slower. This is ADR 0008's test, and it holds.

## Rejected alternatives

- **Reuse `interactive`.** Every Cloud subcommand is interactive, and most of
  them do read installed state (registry recipes, the cache configuration).
- **A list of command names in the CLI.** The extension knows what its command
  reads; the CLI does not.

## Consequences

- An author who declares `none` on a subcommand that does read installed state
  sees it missing on a fresh checkout. The declaration is the author's claim.
