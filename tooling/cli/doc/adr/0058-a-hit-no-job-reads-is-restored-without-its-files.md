# ADR 0058 — A hit no job reads is restored without its files

- **Status**: accepted
- **Scope**: `@putnami/cli` (`internal/jobs`, `internal/store`,
  `internal/cacheprovider`), `go.putnami.dev/protocol/cache` (provider RPC)

## Context

A remote cache hit restored the usual way downloads every blob of its entry
and writes the task's outputs to the workspace. In a `minimal` CI gate most
hits are leaves: no job of the run reads their files, and the gate needs only
their status. Their bytes cost transfer time and disk for nothing.

Three facts limit what a run can skip:

- A dependent's cache key hashes its upstream's outputs as they are on disk.
  An input pattern such as `**/*.go` matches the `.gen/` files a generate task
  writes, so a dependent computes another key, or reads other bytes, when those
  files are missing. This holds even when the dependent is itself a hit.
- The spec gate reads the feature verification report and the executable
  criteria projection from the command output directory after the run. A task
  that reports a command-output file path at run time has a reader too.
- A task that polices the drift of an output compares the restored bytes with
  the bytes the checkout holds.

The provider RPC is parsed strictly on both sides: a provider refuses a
request field it does not know. A new field must never reach a provider that
did not opt in.

## Decision

A hit whose files no job of the run reads is restored **result-only**: the
provider returns the result and the manifest and places no blob, core records
the result in a local result-only entry, and the task reports its cached
status without writing the workspace.

1. **A capability-gated wire.** Core lists `restore-result-only` in the
   bootstrap `initialize` of every session. A provider that honors it echoes
   it. Only after the echo does core send `RestoreParams.resultOnly` or
   `PrefetchParams.resultOnlyKeys`; a provider without the echo receives
   byte-identical requests. The session records the echo and strips both
   fields without it. The session is the only record of the echo, so the rule
   below and the request core sends read the same fact.
2. **The files-needed rule.** `taskFilesNeeded` answers "needed" unless it
   can prove otherwise. Files are needed when the provider did not echo the
   capability or no provider runs; the mode is `full`; the mode is `toplevel`
   and the task belongs to a project the run selected; a planned job waits for
   the task, by dependency or by write serialization; the task declares a
   post-run report; or the task polices the drift of an output. The rule reads
   job keys and the plan, never cache keys, so a key that moves during the run
   cannot change the answer.
3. **A result-only entry with its own address domain.** The record lives at
   `sha256("putnami/store/result-only-task-entry\x00" + format + "\x00" + key)`, in a blob
   directory that holds one file, `result-only.json`, and no payload. The
   task-owned reader, the legacy reader and the remote path cannot name or read
   it. Publication is first-writer-wins. Garbage collection counts the record
   and evicts it like any other entry, and it keeps no CAS blob alive.
4. **A full entry always wins.** A run consults the result-only entry only
   when no full local entry exists for the key and the rule says the files are
   not needed. A full entry whose restore fails falls back to the provider's
   full restore or to an execution, which rewrites every output. A run that
   needs the files never reads a result-only entry.
5. **A status-only hit writes nothing.** It takes no output lock and runs none
   of the restore follow-ups (drift judgment, stamp refresh, reserved artifact
   recovery). It records the task's key for its dependents, replays the
   recorded events and marks the reuse, like every hit. It refuses a result
   that rewrote sources.
6. **Trust without descriptor verification.** A result-only hit must carry a
   channel the run trusts; a hint is a quiet miss, because it fetched no blob
   to warm. Core does not verify the hit's entry descriptor, because it reads
   no blob. The provider must answer with exactly the entry stored under the
   requested key.

### Why a task some job waits on keeps its files

Keeping files only for a dependent that executes is not enough. A dependent
that is a hit still computes its key from the workspace before it knows it is
a hit, and its restore can fail and fall back to an execution. The plan cannot
tell which dependents will execute, so any planned job that waits for a task
keeps the task's files.

### Why a separate entry kind

A task-owned entry promises its declared outputs: its descriptor lists them,
and every restore of it writes them. A record without files at the same
address would break that promise for every reader, including the spec gate's
report read and the drift judgment. A separate address domain, like the failure
record of [ADR 0030](0030-a-failed-task-is-cached-until-its-inputs-change.md),
keeps every existing reader unaware of it.

### Why negotiation decides after `initialize`

Negotiation counts a key that a local result-only entry serves as a local hit
and does not ask the provider for it, so the remote counts stay right. The
decision needs the provider's echo, which arrives at `initialize`. A run that
holds such a key therefore still starts the provider, and before the echo
every key stays with the provider.

## Consequences

- A `minimal` gate moves no blob for a hit no job reads.
- A status-only hit leaves existing output files as they are, so they can be
  absent or hold another key's bytes. A tool outside Putnami that reads a
  task's outputs needs `full`, or `toplevel` with the project selected.
- Test tasks of the Go, TypeScript and Python extensions declare the feature
  verification report, so their hits keep their files and still download every
  report.
- A rerun served only by result-only entries still starts the provider.
- The store grows by one small record per result-only key, evicted by the
  existing byte budget and idle reclaim.
