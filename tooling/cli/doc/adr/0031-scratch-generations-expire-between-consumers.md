# ADR 0031 — Scratch generations expire between consumers

- **Status**: accepted
- **Scope**: `tooling/cli/internal/store` (scratch lease), `internal/engine`

## Context

The workspace scratch root is mutable and has no entry schema. Extensions use
it for incremental compiler state, host binaries and temporary files. Project
renames leave orphaned entries. File modification time does not show access:
Go retains a matching host binary without rewriting it.

## Decision

Treat the whole root as one generation with a 30-day lifetime. At the next idle
consumer entry, delete an expired generation before granting its lease. An
existing unmarked root receives its first retention window when observed. All
scratch stays disposable: expiry may make a still-used project cold.

Keep the generation stamp and a permanent advisory lock outside the root. Runs
hold a shared lock through their final scratch access, including output capture
and gaps between tasks. A subprocess inherits a separate shared descriptor, so
the lock outlives a parent that exits; the parent closes its copy without
unlocking the child's open file description. A consumer only tries an
exclusive lock without waiting, then takes the shared one, so a nested consumer
never waits on a parent whose completion depends on it.

Reap only under exclusive ownership, and only on platforms whose lease passes
to child processes. Confine deletion to an opened workspace root and do not
follow links. A failed deletion leaves the generation expired and warns; a
failed consumer lease refuses execution. No cache key, protocol field or CAS
content depends on this.

## Consequences

- This is a retention policy, not a byte quota or an LRU. Scratch size is what
  extensions write in one window, plus the time until every active consumer
  finishes. Continuously active consumers defer expiry.
- Files in an unused worktree stay until it is used again or removed.
- A tool that writes scratch without a lease cannot join this coordination;
  concurrent consumers must use a CLI that takes the lease.
- A retention check costs one stamp lookup under an uncontended lock, with no
  inventory or size scan. Expiry is a confined recursive deletion whose work is
  proportional to the generation's entries.
- Extension-owned machine caches and the shared CAS keep their own policies.
