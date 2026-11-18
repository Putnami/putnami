# ADR 0018 — A plan change syncs within the turn

- **Status**: accepted
- **Date**: 2026-11-18
- **Scope**: `@putnami/contributor` (`internal/agentsession`, `agent hook`)
- **Amends**: [ADR 0007](0007-the-contributor-declares-agent-session-hooks.md),
  decision 3: `PostToolUse` may call the provider

## Context

The session hooks sync at `SessionStart`, `Stop`, `StopFailure`,
`SessionEnd` and the idle wake. `PostToolUse` records a plan change in the
session cache and sends nothing, so the change reaches the mission at the end
of the turn. A build turn often runs for an hour or more: code, the gate, a
review, the publication. During that time the mission shows an old plan,
while `agent stage` already syncs at once and the stage moves live.

## Decision

1. **A plan change syncs when the last sync is old.** When `PostToolUse`
   records a plan the last sync did not send, and both the last sync and the
   last sync a tool call started are at least 60 seconds old, it sends one
   `missions.sync` before it returns. When the last sync answer set a
   `minIntervalMs` longer than 60 seconds, that pace is the interval: the
   contract exempts only the syncs at session start, at stop and during a
   wait from it, and this sync is a routine one.
2. **Every sync carries the usage.** The sync of a tool call reads the usage
   the transcripts added, within the per-hook byte bound, and carries it, as
   every other sync does.
3. **The claim happens under the lock the hook already holds.** The hook
   takes the session lock without waiting, applies the plan change, builds
   the sync request and records the start of the sync in the cache, then
   releases the lock for the call. Two plan changes within that interval send
   at most one sync, even when their hooks run at once. Once the provider
   answered, the hook merges the answer under the lock, waiting at most 500
   ms for it, as every sync does.
4. **The other rules hold.** When another hook holds the lock, the hook
   spools the call and returns at once, without a sync. The sync takes the
   event's budget, 25 seconds, and at most 10 seconds per call; a sync that
   does not fit goes with the next one. `PostToolUse` delivers no message:
   the messages the sync answers stay in the inbox for the end of the turn.

The rejected alternative released the lock and ran the ordinary sync, which
takes the lock again with a 500 ms wait and records its time only once the
provider answered: two tool calls in that window would both sync, and a tool
call could wait for the lock before it records its plan change.

## Consequences

- During a turn with plan changes, the mission sees the plan and the usage at
  most about one interval late, at the cost of one provider call per interval
  at most.
- The tool call that triggers the sync waits for it, within the event's
  budget.
- A plan change within the interval after a sync still waits for the next
  sync: a later plan change, a stage, or the end of the turn.
