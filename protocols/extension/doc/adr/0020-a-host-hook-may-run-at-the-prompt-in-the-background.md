# 0020: A host hook may run at the prompt, in the background, from contract 25

- **Status**: accepted
- **Date**: 2026-11-24
- **Scope**: `go.putnami.dev/protocol/extension` (`hostHooks`),
  `go.putnami.dev/protocol/cli` and the hook writer of `@putnami/cli`

## Context

The agent session hooks of `@putnami/contributor` publish what a session
does, and a reader of its mission sees the agent's answer without the
person's request. Claude Code hands each request to a `UserPromptSubmit`
hook before the turn starts. The closed event list of `hostHooks` does not
name that event, and every entry the CLI writes runs in the foreground: the
host waits for it, so a hook at the prompt would delay every request by the
launcher's start.

Claude Code runs a command handler that declares `"async": true` in the
background: the turn starts at once, and the handler's output and exit code
change nothing.

## Decision

1. **Claude Code runs a hook at `UserPromptSubmit`.** The event joins the
   closed list of the `claude-code` host. Codex keeps its list.
2. **An entry may declare `async`.** A Claude Code hook that declares it runs
   in the background, and the CLI writes `"async": true` on its handler. Only
   a Claude Code hook declares it (`invalid-host-hook-async`), and an entry
   that declares `wake` does not: a wake hook already runs in the background.
   `async` is part of what a hook does, so two entries that differ only by it
   are two hooks.
3. **Contract 25, `PromptHostHooksContract`.** A manifest with a hook at
   `UserPromptSubmit` or a hook that declares `async` requires it wherever it
   loads from. A reader below 25 would refuse the event or the member with a
   validation error, or write the hook in the foreground; it refuses the
   stamp instead, and says a newer putnami is needed.

## Rejected alternatives

- **Write every `UserPromptSubmit` hook in the background.** A later hook at
  the prompt may need to answer before the turn, so the choice stays with the
  entry.
- **Keep the event and bound its timeout.** A foreground hook still delays
  the request by the launcher's start, which a cold source workspace makes
  seconds long.
- **Raise only the `hostHooks` rung.** Contract 17 already binds every
  released manifest that declares hooks; a reader at 17 would load the new
  vocabulary and fail on it.

## Consequences

- `@putnami/contributor` stamps contract 25 when it declares its prompt hook.
- A workspace on an older putnami refuses that release of the extension with
  the contract message, until it upgrades.
