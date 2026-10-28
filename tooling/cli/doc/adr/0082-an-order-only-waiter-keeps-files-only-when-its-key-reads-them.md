# ADR 0082 — An order-only waiter keeps a task's files only when its key reads them

- **Status**: accepted
- **Date**: 2026-10-28
- **Amends**: [ADR 0058](0058-a-hit-no-job-reads-is-restored-without-its-files.md),
  rule 2
- **Scope**: `@putnami/cli` (`internal/jobs/remotecache`), the Go extension's
  `build` command, the Cloud cache provider (`cloud/extension`), features
  `cli/job-planning-execution` and `cloud/cache-provider-configuration`

## Context

ADR 0058 keeps the files of every task a planned job waits for, by dependency
or by write serialization. Write serialization orders two jobs that touch one
declared resource: a later writer of a `.gen` tree waits for every job that
read the tree before it. That edge never reaches a cache key
(`SchedulingPredecessors`), and the waiter's key reads the task's bytes only
when one of its inputs selects a file the task writes, which key movement
already computes (`KeyMovement.Writers`).

A Go workload's `build-infra` step merges committed requirements and writes
`.gen`. It depended on `compile`, though it reads no binary; without that
edge, serialization still orders it after `compile`, which reads `.gen`.
Either edge kept the executable files-needed. A cache server may keep an
entry without its largest files; a run that needs them reads the entry as a
miss and builds every executable again under the same key.

A hosted run negotiates every key in `full` mode, because nothing configures
a mode on a runner and the provider falls back to `full`. A `full` run needs
every file of every hit, whatever the plan says. A `minimal` session asked
the server for no downloads at all, so a hit whose files a job reads failed
to restore and executed.

## Decision

1. **An order-only edge counts only when the waiter reads the bytes.** A task
   keeps its files for a waiter that depends on it functionally, as before.
   A waiter that only serializes after it keeps them only when key movement
   names the task as a writer of a file the waiter's key reads, or when the
   task declares an output `readByDependents`. When key movement cannot be
   computed, every serialization edge counts.
2. **Infra does not wait for compile.** The Go `build` command's `infra` step
   depends on `generate` and `describe` only. Its key stops folding the
   compile key, and it runs when the compile fails.
3. **A hosted session is `minimal`.** The Cloud provider resolves the mode
   from core's intent, then the configured mode, then `minimal` for a hosted
   launch and `full` otherwise. An explicit `PUTNAMI_CACHE_MODE` or
   `.putnami/cache.json` still wins.
4. **A restore of the files always asks for downloads.** In a `minimal`
   session, the provider negotiates the keys whose files core reads in `full`
   and the result-only keys in `minimal`. The session mode decides which
   tasks core serves result-only; it no longer decides whether a files
   restore receives downloads.

## Consequences

- A run on a tree that an earlier run built serves each `build-compile`
  entry from its result, even when the server kept the entry without the
  executable.
- Every hosted leaf (lint, validate, infra) moves no bytes on a hit.
- A task that reads a predecessor's bytes without hashing them was already
  unsound, so the narrower rule loses no read a key relied on. A tool outside
  Putnami that reads outputs after a hosted run needs `PUTNAMI_CACHE_MODE`.
- A developer machine in `full` still builds an executable whose entry lacks
  it, as before.
