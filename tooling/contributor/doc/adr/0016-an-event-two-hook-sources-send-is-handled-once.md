# ADR 0016 — An event two hook sources send is handled once

- **Status**: accepted
- **Date**: 2026-11-17
- **Scope**: `@putnami/contributor` (`internal/agentsession`, `agent hook`)
- **Amends**: [ADR 0007](0007-the-contributor-declares-agent-session-hooks.md),
  decision 3
- **Follows**: [CLI ADR 0098](../../../cli/doc/adr/0098-host-hooks-reach-every-session.md),
  which writes the hooks into the user's host files as well as the
  workspace's

## Context

Since CLI ADR 0098, an opted-in workspace's hooks live in two files of each
host: the workspace's and the user's. Claude Code runs a handler that both
files define only once, but a source workspace renders its handler with
another launcher, and Codex runs the matching hooks of every file. One event
can then reach `agent hook` twice, at the same time: two mission sheets at
session start, two syncs, two idle waits that may both wake the session.

## Decision

1. **An event is handled once.** Before it handles an event, `agent hook`
   claims it: the digest of the engine, the `--idle-wake` flag and the
   payload's exact bytes names a marker in the session directory, created
   only when it does not exist. A second hook that finds a marker younger
   than 60 seconds prints nothing and exits 0.
2. **The claim needs no lock.** The marker is created exclusively, so two
   hooks that run at once agree without the session lock, and a tool call
   still never waits.
3. **Markers do not pile up.** A claim removes the markers older than the
   window it finds in the directory.
4. **The claim fails open.** A marker that cannot be written leaves one line
   in `hook.log`, and the event is handled: a duplicate costs one more sync,
   a lost event costs a report.

## Consequences

- Two distinct events of one session rarely share a payload: a tool call names
  its tool use, and a turn end names its prompt or its last message. Two turn
  ends with the same last message and no prompt id within 60 seconds count as
  one; the next event publishes the state.
- An event the host sends again after 60 seconds is handled again.
- A skipped duplicate leaves no line in `hook.log`: the hook that claimed the
  event logs its own outcome.
