# Session Recording

Every CLI execution is recorded as a **session** — an audit trail capturing the execution plan, job events, and final outcome. Sessions enable post-mortem debugging, performance analysis, and reproducibility.

## Session Lifecycle

```
1. Create  ──▶  Generate session ID, create directory
2. Plan    ──▶  Write execution plan snapshot
3. Execute ──▶  Append JSONL events as jobs run
4. Finalize ──▶ Write metadata with stats, git info, timing
5. Report  ──▶  Persist compact validation facts when collected
6. Prune   ──▶  Remove old sessions beyond retention limit
```

### Session ID

Format: `YYYYMMDD-HHMMSS-{6 hex chars}`

Example: `20260302-141530-a1b2c3`

The timestamp prefix enables chronological sorting. The random suffix prevents collisions for concurrent runs.

## Storage Layout

```
.putnami/sessions/
├── latest → 20260302-141530-a1b2c3/   (symlink to most recent)
├── 20260302-141530-a1b2c3/
│   ├── plan.json                       # Execution plan snapshot
│   ├── events.jsonl                    # JSONL event stream
│   ├── session.json                    # Finalized metadata
│   └── report.json                     # Optional compact validation facts
├── 20260302-140000-def456/
│   └── ...
└── ...
```

### Recorded contract

`plan.json` and `session.json` are the version-2 machine documents
`sessionPlanFile` and `sessionFile`
([`protocols/cli/doc/02-result-v2.md`](../../../protocols/cli/doc/02-result-v2.md)):
both carry `protocolVersion: 2`, and a recorded document WITHOUT the member is
version 1, written by a CLI build published before the v1 writers were removed.
`putnami sessions show` reads **both** versions, so a
workspace that upgraded across the flip keeps opening the sessions already on
disk. Only the writer is v2-only.

`events.jsonl` is the complete sanitized version-2 `sessionStreamRecord`
sequence: every `task:start`, `task:event`, `test:case`, and `task:end`,
followed by the bounded `session:end` record. The live `--output=jsonl` stream is a deterministic
budgeted selection of these exact bytes; normal live output also suppresses
debug-level task detail, while the artifact itself is not elided.
`sessions inspect` retains a read-only fallback for historical
version-1 `job:end` / `job:event` logs. New sessions write only version 2.

`report.json` is currently an optional, independently versioned compact slice.
A completed, non-aborted `--enforce-coverage` run writes every valid canonical
runtime `coverageSummary` fact it collected per project, including a measured
percentage from a test that failed its threshold or a session where another
task failed. An ordinary or aborted run writes no report. The shape is
language-neutral (Go statements and TypeScript lines retain their declared
granularity) and contains no raw result payloads or invocation-scoped data.

The interactive `test` summary scans completed reports newest-first for each
selected project. Coverage collected by the current run always wins. When no
current measurement exists, the summary replays the last validation fact and
labels it `stale` with its revision (or source session) so it cannot be read as
current-run coverage. Failed completed sessions remain eligible sources;
aborted or incomplete sessions never do. Report reads and writes are
display-only: they never change task parameters, cache keys, or pass/fail.

### `plan.json` — Execution Plan Snapshot

Captured at session start, before any job runs:

```json
{
  "protocolVersion": 2,
  "sessionId": "20260302-141530-a1b2c3",
  "commands": ["build"],
  "tasks": [
    {
      "identity": {
        "key": "/packages/my-lib:build~generate",
        "scope": "project",
        "project": { "id": "/packages/my-lib", "name": "my-lib" },
        "task": { "name": "build~generate", "command": "build", "step": "generate", "kind": "ts-generate" },
        "provider": { "extension": "@putnami/typescript" }
      },
      "cache": true
    },
    {
      "identity": {
        "key": "/packages/my-app:build~transpile",
        "scope": "project",
        "project": { "id": "/packages/my-app", "name": "my-app" },
        "task": { "name": "build~transpile", "command": "build", "step": "transpile", "kind": "ts-transpile" },
        "provider": { "extension": "@putnami/typescript" }
      },
      "dependsOn": ["/packages/my-app:build~generate", "/packages/my-lib:build~transpile"],
      "cache": true
    }
  ]
}
```

This snapshot is immutable — it records the planned state, not the actual execution outcome.

### `events.jsonl` — Event Stream

Append-only JSONL file. Each line is a structured event written as jobs execute.
It is the complete sanitized lifecycle trace behind bounded machine output. The
writer appends every valid task record before applying the live stream's normal
debug-detail policy or normal/verbose capacity, so successful test transcripts
and late detail omitted from stdout remain available for post-mortem inspection.

```jsonl
{"protocolVersion":2,"record":"task:start","time":"2025-03-02T14:15:30Z","identity":{"key":"/packages/my-lib:build~generate","scope":"project","project":{"id":"/packages/my-lib","name":"my-lib"},"task":{"name":"build~generate","command":"build","step":"generate","kind":"build-generate"},"provider":{"extension":"@putnami/typescript"}}}
{"protocolVersion":2,"record":"task:event","time":"2025-03-02T14:15:30Z","identity":{"key":"/packages/my-lib:build~generate","scope":"project","project":{"id":"/packages/my-lib","name":"my-lib"},"task":{"name":"build~generate","command":"build","step":"generate","kind":"build-generate"},"provider":{"extension":"@putnami/typescript"}},"event":{"type":"phase","name":"compile","action":"start"}}
{"protocolVersion":2,"record":"task:end","time":"2025-03-02T14:15:31Z","identity":{"key":"/packages/my-lib:build~generate","scope":"project","project":{"id":"/packages/my-lib","name":"my-lib"},"task":{"name":"build~generate","command":"build","step":"generate","kind":"build-generate"},"provider":{"extension":"@putnami/typescript"}},"task":{"identity":{"key":"/packages/my-lib:build~generate","scope":"project","project":{"id":"/packages/my-lib","name":"my-lib"},"task":{"name":"build~generate","command":"build","step":"generate","kind":"build-generate"},"provider":{"extension":"@putnami/typescript"}},"status":"success","reuse":"none","exitCode":0,"durationMs":800,"taskWallMs":825}}
{"protocolVersion":2,"record":"session:end","time":"2025-03-02T14:15:35Z","run":{"outcome":"success","exitCode":0,"counts":{"total":1,"succeeded":1,"failed":0,"canceled":0,"skipped":0},"reuse":{"localCache":0,"remoteCache":0,"coalesced":0},"durationMs":5000},"machineOutput":{"mode":"normal","sanitization":"terminal-safe-redacted-v1","budget":{"maxBytes":1048576,"maxRecords":1024,"failureReserveBytes":262144,"failureReserveRecords":256,"finalReserveBytes":16384,"finalReserveRecords":1},"elided":{"ordinary":{"records":0,"bytes":0},"failure":{"records":0,"bytes":0}},"artifact":{"sessionId":"20250302-141530-a1b2c3","path":"events.jsonl","retention":"session"}}}
```

Strings are made terminal-safe and credential-shaped values are redacted before
persistence. Records are appended immediately with mutex protection and no
batching, so a crash preserves all records written up to that point. The final
record describes the live copy's fixed budget and exact ordinary/failure
elisions, and names this file with `retention: "session"`.

### `session.json` — Finalized Metadata

Written at the end of execution:

```json
{
  "protocolVersion": 2,
  "sessionId": "20260302-141530-a1b2c3",
  "startTime": "2025-03-02T14:15:30Z",
  "endTime": "2025-03-02T14:15:35Z",
  "commands": ["build"],
  "selection": {
    "mode": "impacted",
    "scoped": true,
    "projects": ["/packages/my-app", "/packages/my-lib"]
  },
  "git": {
    "branch": "feature/new-ui",
    "baseline": "main"
  },
  "tree": {
    "fingerprint": "4f3d0d6f0c1ba6c8c4b9a2e7d6c5b4a39281706f5e4d3c2b1a0f9e8d7c6b5a49",
    "dirty": true,
    "headSHA": "ad048a6b3c0d1e2f30415263748596a7b8c9d0e1"
  },
  "run": {
    "outcome": "success",
    "exitCode": 0,
    "counts": { "total": 4, "succeeded": 4, "failed": 0, "canceled": 0, "skipped": 0 },
    "reuse": { "localCache": 1, "remoteCache": 0, "coalesced": 1 },
    "durationMs": 5000,
    "cpu": {
      "allocatedMillicores": 8000,
      "allocatedSource": "cgroup-quota",
      "allocatedMs": 40000,
      "actualMs": 2140,
      "executions": 1
    }
  },
  "tasks": [
    {
      "identity": {
        "key": "/packages/my-lib:build~generate",
        "scope": "project",
        "project": { "id": "/packages/my-lib", "name": "my-lib" },
        "task": { "name": "build~generate", "command": "build", "step": "generate", "kind": "ts-generate" },
        "provider": { "extension": "@putnami/typescript" }
      },
      "executionId": "exec-1",
      "inputDigest": "sha256:9c1e4f0b7a2d3e5f60718293a4b5c6d7e8f90a1b2c3d4e5f6a7b8c9d0e1f2a3b",
      "status": "success",
      "reuse": "none",
      "exitCode": 0,
      "durationMs": 800,
      "taskWallMs": 825,
      "spawnToFirstEventMs": 145
    },
    {
      "identity": {
        "key": "/packages/my-app:build~transpile",
        "scope": "project",
        "project": { "id": "/packages/my-app", "name": "my-app" },
        "task": { "name": "build~transpile", "command": "build", "step": "transpile", "kind": "ts-transpile" },
        "provider": { "extension": "@putnami/typescript" }
      },
      "inputDigest": "sha256:0d5a7c3e9b1f2468ace013579bdf2468ace013579bdf2468ace013579bdf2468",
      "status": "success",
      "reuse": "coalesced",
      "exitCode": 0,
      "durationMs": 0,
      "taskWallMs": 1200
    }
  ],
  "executions": [
    {
      "id": "exec-1",
      "wallMs": 780,
      "userCpuMs": 1900,
      "systemCpuMs": 240,
      "maxRssBytes": 412094464,
      "ioOutBlocks": 96,
      "concurrency": 4,
      "tasks": 1
    }
  ],
  "environment": {
    "os": "linux",
    "arch": "amd64",
    "logicalCpus": 16,
    "cpuModel": "AMD EPYC 7B13 64-Core Processor",
    "windowMs": 5020,
    "cgroupCpu": {
      "periodUs": 100000,
      "quotaUs": 800000,
      "usageUs": 2480000,
      "throttle": { "periods": 50, "throttledPeriods": 6, "throttledUs": 184000 }
    },
    "cpuPressure": { "someStalledUs": 412000 },
    "hostCpu": { "stealTicks": 20, "ioWaitTicks": 10, "totalTicks": 4020 },
    "memoryCapacity": {
      "physicalBytes": 17179869184,
      "cgroupLimit": { "bytes": 8589934592, "source": "cgroup-v2" },
      "effectiveBytes": 8589934592,
      "effectiveSource": "cgroup-limit"
    },
    "cgroupMemory": {
      "source": "cgroup-v2",
      "closing": {
        "currentBytes": 1308622848,
        "composition": { "anonBytes": 696254464, "fileBytes": 503316480, "shmemBytes": 67108864 },
        "lifetimePeak": { "bytes": 1828716544 }
      },
      "events": { "low": 0, "high": 2, "max": 0, "oom": 0, "oomKill": 0 }
    },
    "memoryPressure": { "scope": "cgroup", "someStalledUs": 2800, "fullStalledUs": 0 }
  },
  "preparation": {
    "wallMs": 3155,
    "parallelism": 3,
    "phases": [
      { "phase": "resolution", "wallMs": 800, "steps": 6 },
      { "phase": "verification", "wallMs": 340, "steps": 12, "cpuMs": 122 },
      { "phase": "generation", "wallMs": 3281, "steps": 3, "cpuMs": 2526 },
      { "phase": "mutation", "wallMs": 1098, "steps": 12 }
    ]
  }
}
```

`run.durationMs` is the session wall (end minus start); v2 carries no top-level
duration member. `run.counts` spans every task including reused ones, and
`run.reuse` counts the provenance separately — a reused FAILURE therefore fails
the recorded run, where the v1 `stats` block could hide it.

`tasks[]` is **logical** and `executions[]` is **physical**. A batched dispatch
runs several projects in one subprocess, so several task records can share one
`executionId`; the execution is listed once and states its fan-out in `tasks`.
Sum `executions[]` to answer "what did this run cost the machine" — reuse
produces no execution at all, so a cache hit contributes nothing there — and
read `tasks[]` to attribute that cost per project.

`executions[]` is the run's complete spawn ledger, so it also lists executions
no task record references: a retried task names only its final attempt, and a
batch whose split failed is superseded by every member re-executing solo. Those
entries carry `"tasks": 0`, and their cost is real — leaving them out would
under-state what the run actually spent.

A batch member's `durationMs` is its share of the shared attempt rather than the
whole batch's wall. The share divides the **task-level** wall (the same quantity
a solo task's `durationMs` reports, which starts before the context file is
written), not `executions[].wallMs` (the subprocess's own wall). The division is
exact in nanoseconds but each record is truncated to milliseconds independently,
so summing *n* members loses up to *n*−1 ms. Do not reconcile
`sum(tasks[].durationMs)` against `wallMs`; use `executions[]` for cost.

`maxRssBytes` is always **bytes**, normalized by the CLI because the underlying
`ru_maxrss` is bytes on macOS and kilobytes on Linux. A counter the platform
does not report is omitted rather than written as zero.

### The reported selection

`selection` records how the run chose the projects it planned over: `mode`
(`all`, `impacted` or `projects`), `scoped`, and the sorted `projects` ids. A
record written before the member existed carries none, and absence is unknown —
never `all`.

A session that names `publish` beside other commands plans two things at once,
so it records both. `projects` stays what the run planned and executed, and
`releaseSetProjects` is the subset whose publish and package steps the
release-set plan owned — the members the channel head decided. `mode`, and the
baseline in the `git` block, describe how the OTHER half was chosen, because
that is the half a selection flag decides (ADR 0017 §2):

```json
{
  "commands": ["lint", "test", "build", "validate", "publish"],
  "selection": {
    "mode": "impacted",
    "scoped": true,
    "projects": ["/apps/console", "/libs/widget"],
    "releaseSetProjects": ["/apps/console"]
  }
}
```

`releaseSetProjects` is absent when the session coordinated no release set, and
when its plan selected no member. Subtract it from `projects` to read what the
session verified without publishing.

### The gated tree

`tree` names **which worktree the session ran against**, by content. `git` names
a branch and a baseline; neither says anything about the uncommitted state, and
`git status` reports paths and status codes — so an agent that gates a tree and
then edits a file it had already dirtied leaves that output byte-for-byte
identical. Comparing `tree.fingerprint` tells the two states apart.

| Member | Meaning |
|--------|---------|
| `fingerprint` | lowercase hex sha256 covering HEAD, the bytes at every tracked path that differs from it (recursing through submodules), and the content of every untracked non-ignored file |
| `dirty` | whether the worktree differed from `headSHA` |
| `headSHA` | the full object id of the commit the worktree sat on |

The digest is captured **when the session opens**, before the first task runs, so
it states the tree the run's work CONSUMED. A run whose `lint --fix` rewrote a
file therefore records the tree as it went IN — which is what makes the record
useful: compare it with the tree in front of you and you learn either that they
match or that the run changed it.

`putnami tree fingerprint` prints the same digest for the worktree it runs in and
is the only implementation of the calculation; two parties comparing
fingerprints produced two ways compare nothing. It needs no workspace and writes
nothing:

```bash
putnami tree fingerprint
# 4f3d0d6f0c1ba6c8c4b9a2e7d6c5b4a39281706f5e4d3c2b1a0f9e8d7c6b5a49

putnami tree fingerprint --output=json
# {"protocolVersion":2,"command":"tree fingerprint","status":"success","exitCode":0,
#  "data":{"dirty":true,"fingerprint":"4f3d...","headSHA":"ad048a6b..."}}
```

The block is **optional by absence**: a session outside a git worktree, in a
repository with no commit, or one whose git call failed records no `tree` at all
rather than a clean-looking one. Sessions recorded before the block existed keep
opening unchanged. The exact byte stream the digest covers is specified once, in
[the result contract](../../../protocols/cli/doc/02-result-v2.md).

### The runner environment

`environment` records the **machine** the session ran on, so a duration can be
interpreted rather than merely compared. A task that took twice as long may have
hit its cgroup quota, been starved by a neighbor on a shared host, or simply had
more work; `cgroupCpu.throttle`, `cpuPressure` and `hostCpu.stealTicks` are how
those are told apart.

Every kernel counter here is **cumulative**, so the CLI samples twice — before
the first job is dispatched and after the last subprocess is waited for — and
records only the **delta**. `windowMs` is the wall between the two samples, and
is what every delta is read against. A counter that went backwards (a reset, or
two samples from different cgroups) drops its block rather than reporting a
clamped zero.

A file the platform does not have yields an **absent** block. On macOS
`cgroupCpu`, `cpuPressure` and `hostCpu` are simply not there, and the session
records `os`, `arch`, `logicalCpus`, `cpuModel` and `windowMs` alone. That is
deliberate: a zeroed `cgroupCpu` would claim this machine *measured* no
throttling, which is a different statement from being unable to measure it.
Where a zero IS a measurement — `"throttledPeriods": 0` rules out quota
starvation — the counters sit in a nested object, so the object's presence
carries "this was read" and the zeros inside carry the result. The one member
whose absence means something else is `cgroupCpu.quotaUs`: `cpu.max` says either
a positive quota or `max`, so an absent `quotaUs` means **unlimited**.

#### Reading memory facts

Memory facts deliberately keep capacity, subprocess cost, shared-cgroup state,
and pressure separate:

| Fact | Scope and lifetime | Correct reading |
|---|---|---|
| `memoryCapacity` | Stable capacity captured at session opening | `physicalBytes` and a finite `cgroupLimit` retain their provenance. `effectiveBytes` is their smaller available value and `effectiveSource` identifies the selected input. It is the same conservative bound used to size scheduler admission; recording it does not introduce a new policy. |
| `executions[].maxRssBytes` | Peak RSS of one physical subprocess tree | Use it for execution/task history. It is neither a session peak nor an attributable share of the shared cgroup. |
| `cgroupMemory.closing.currentBytes` | Shared-cgroup gauge at session close | Memory charged to the cgroup at one instant, including activity the session cannot attribute to one execution. |
| `cgroupMemory.closing.composition` | Shared-cgroup v2 gauges at session close | `anonBytes`, `fileBytes`, and `shmemBytes` describe the closing `memory.stat` sample. Kernel `file` **includes** `shmem`, so never sum `fileBytes + shmemBytes`. |
| `cgroupMemory.closing.lifetimePeak` | Peak over the cgroup's lifetime, sampled at close | It may include work before this session. It is never the peak over `windowMs`. |
| `cgroupMemory.events` | Cgroup v2 counter deltas over `windowMs` | A present all-zero object means the events were measured and did not occur. `high`, `max`, `oom`, and `oomKill` are pressure evidence, not per-task attribution. |
| `memoryPressure` | Host- or cgroup-scoped PSI deltas over `windowMs` | Read `scope` before comparing runs: host and cgroup PSI use the same units but describe different populations. |

An absent memory object means unknown, unsupported, malformed, reset, or unsafe
to encode — never measured zero. Cgroup v1 can provide a finite limit, closing
usage, and a cgroup-lifetime peak, but it does not publish the v2 composition or
event objects: v1 `failcnt` is not relabelled as a v2 pressure event. No field
declares scratch backing, and filesystem or memory readings must not be used to
infer whether scratch is block-backed or memory-backed.

These facts are persisted only in the local session artifact. They do not enter
the CLI's anonymous OTLP usage events; the separate telemetry contract is
documented in [Profiling and Telemetry](12-profiling-and-telemetry.md).

`hostCpu` is in **USER_HZ ticks** as the kernel states them — converting to
milliseconds needs a USER_HZ the CLI cannot read — with `totalTicks` beside them
so `stealTicks / totalTicks` is a unit-free share of the window. The total sums
the columns up to `steal`; `guest` and `guest_nice` are excluded because the
kernel already counts them inside `user` and `nice`.

`run.cpu` pairs that allocation with what the run actually burned:
`allocatedMillicores` is the cgroup quota where one is in force and the visible
core count otherwise (`allocatedSource` says which), `allocatedMs` is that rate
over `run.durationMs`, and `actualMs` is the CPU summed over `executions[]` with
each execution counted **once**. `actualMs` can exceed `allocatedMs` — a quota
is enforced per period, not per run — and `cgroupCpu.usageUs` is wider than
both, because it includes the CLI process itself and anything else in the
cgroup. The block is omitted when the run spawned nothing or captured no
environment; an allocation figure with a guessed denominator would poison every
ratio derived from it.

### Dependency preparation

`preparation` records the stage that runs **before** planning: resolving each
extension runtime's content identity, building the ones that are not already in
the machine-global content-addressed store, verifying them, and publishing them.
It is on the critical path of every command, so it is decomposed by **ownership**
rather than reported as one number:

| Phase | What it owns |
|-------|--------------|
| `network` | talking to a remote — registry metadata, artifact downloads |
| `resolution` | enumerating declared inputs and hashing them into a content digest |
| `verification` | executable checks, the "inputs did not move" re-hash, and the runtime ABI handshake |
| `generation` | running the extension's declared `prepare` command |
| `mutation` | staging, the store's per-digest ownership-lock wait, the atomic publish, and the reclaim |

Runtime synchronization performs **no network I/O of its own** — installed
extension binaries are materialized by the lock-driven ensure pass at CLI
startup, and a prepare command's own module downloads happen inside its
subprocess and are therefore counted under `generation`, which is where the
measurement can actually see them. A phase the stage never entered is **absent**,
so a warm run has no `generation` row at all rather than one reading zero.

`wallMs` at the top is the stage's own wall — what the run actually waited for.
The phase `wallMs` values are **sums of spans**: independent extension runtimes
synchronize concurrently (up to `parallelism`), so `generation` routinely exceeds
the stage wall, and that gap is the parallelism working. Do not add phases up as
elapsed time unless `parallelism` is 1.

`cpuMs` is measured child CPU and appears only on the phases that spawn
subprocesses. It never overlaps `executions[]`: preparation happens before the
scheduler exists, so no execution record and no task record accounts for any of
it.

When a remote cache provider participated in the run, `session.json` also
contains a `cache` object. Its live counters (`hits`, `misses`, `bytesFetched`,
`bytesUploaded`, and timing fields) describe scheduler activity; the
`providerSummary*` fields are the provider's terminal `summary` totals. Use the
latter for transfer accounting in benchmarks, and keep byte totals separate from
both the session wall (`run.durationMs`) and OS CPU time.

```json
{
  "cache": {
    "providerSummaryAvailable": true,
    "providerSummaryRestoredBytes": 8400000,
    "providerSummaryUploadedBytes": 2100000
  }
}
```

## Session Events

New `events.jsonl` files use the same version-2 variants as machine JSONL:

| Record | When | Required payload |
|--------|------|------------------|
| `task:start` | Task starts | typed `identity` |
| `task:event` | Runtime or session audit event is accepted | typed `identity`, normalized `event` |
| `test:case` | A test task reached its terminal result, once per kept case, immediately before its `task:end` | typed `identity`, `testCase` |
| `task:end` | Task reaches any terminal result | typed `identity`, canonical `task` result |
| `session:end` | Renderer closes the run | bounded `run`, `machineOutput` budget/elision/artifact summary |

The renderer and artifact share one canonical sanitizer and serializer. This
makes the live stream a byte-for-byte deterministic subsequence of the artifact
and keeps the final line identical in both. Failure-priority events and failed
or canceled task ends consume a dedicated live reserve, so early success noise
cannot crowd out later failure evidence.

Session-wide `scheduler:parallel`, `selection:impacted`,
`selection:change-shape` and `invocation:reaped` audit signals use a stable
workspace-scoped `@putnami/cli` system identity because the v2 stream has no
free-standing event variant. Their original type
and data remain in the normalized event object; the legacy `job:end` projection
is not duplicated.

An `--impacted` run records `selection:impacted` once, right after the opening
`scheduler:parallel` event and before any task starts: the baseline and the
commit the diff measured against, every changed file and which of them no
commit records, every seed, every edge and every task-scoped project with the
`tasks` of it the change reaches. It is the uncapped form of the
`--verbose` explanation block; [Explaining a selection](06-workspace-and-projects.md#explaining-a-selection)
lists its members.

It then records `selection:change-shape` once: the size of the change by
category and the areas its authored code spans.
[Change size and mixed intent](06-workspace-and-projects.md#change-size-and-mixed-intent)
lists its members.

For recorded executions, the opening `scheduler:parallel` event also carries
optional `event.sessionId`. The engine writes that session's v2 `plan.json`
before the scheduler emits the event, and the event precedes every scheduler
task start. The same path serves verification and publish-containing runs;
reading this event does not repeat discovery or preparation hooks. Sessionless
schedulers omit the member, and human output is unchanged.

A runner can bind the top-level event from its invocation's fresh JSONL stream
to `.putnami/sessions/<sessionId>/plan.json` while work continues. Do not choose
the newest session directory: a task can start a nested CLI invocation, whose
output stays inside the parent task's envelope. The ID and plan are advisory,
untrusted input; readers own ID validation and filesystem privilege boundaries.
The event uses the existing live budget and retention rules. A missing or
elided event, or an unreadable plan, provides no early evidence and does not
change the run's verdict.

Exactly one `task:end` is written for every task completed by the renderer,
including reuse, dependency skips, failure-triggered drains, stuck graphs, and
never-started cancellations. Its canonical task projection is the same shape
recorded in `session.json.tasks[]`.

A test task's `test:case` records precede its `task:end` in runner order,
written in one step so no other task's record falls between them. The
`sessions` readers skip `test:case` when they measure or list a run;
`sessions inspect --output=jsonl` prints each one whole.

Historical version-1 logs remain readable by `sessions inspect` and `sessions
gate`; they are never rewritten in place. Rollback of the v2 writer is an exact
older CLI version pin. Such a binary can still use its own newly recorded
sessions, while sessions created by the newer writer should be inspected before
the pin or retained for a later upgrade rather than converted lossy.

## Watch Mode Sessions

In watch mode, each iteration creates a **child session**. The parent session records the overall watch lifecycle, while child sessions capture individual re-execution cycles. This enables analyzing each change-triggered run independently.

## Session Management

### List Sessions

```bash
putnami sessions list
```

Shows recent sessions with ID, date, commands, project count, success/failure status, and duration.

`--revision <sha>` keeps the sessions whose recorded `tree.headSHA` starts
with the given lowercase hex prefix (7 to 64 characters) and adds the head
commit and the actual placement to each row; `--output=jsonl` rows carry
`revision` and `placement` when the record states them.

### Inspect a Session

```bash
putnami sessions inspect 20260302-141530-a1b2c3
putnami sessions inspect 20260302-141530-a1b2c3 --output=jsonl
putnami sessions inspect --run <attempt-or-submission>
```

Shows the full session details: plan, events, stats, and git state. `--run`
names a remote attempt (see [the portable runner](18-portable-runner.md)):
an imported attempt is shown from the store, one that is not is resumed and
imported first.

### Export Sessions

```bash
putnami sessions export > sessions.jsonl
putnami sessions export --since 2026-09-01T00:00:00Z
putnami sessions export --output=json
```

`sessions export` streams every recorded `session.json` still present in the
store, **one per line, oldest first**, deduplicated by session id. Each line is
the recorded `sessionFile` document forwarded **verbatim** — only whitespace is
folded, so the writer's indentation becomes one line and nothing else changes.
Export never re-serializes a record through a Go struct: the binary reading a
record is not always the binary that wrote it, and a round trip would silently
drop a member a newer producer added.

Because the store is pruned (see [Retention and Pruning](#retention-and-pruning))
and an agent worktree is deleted with its records, export is how per-run CPU,
task counts and cache reuse leave a worktree before it disappears. Wall-clock
duration is not a substitute: under concurrent worktrees the same command varies
several-fold.

Stream rules:

- The order is the **recorded** `startTime`, not the session id. An id resolves
  only to the second and breaks a collision with a *random* suffix, so two runs
  started in the same second would otherwise be emitted in a coin-flip order.
  A record whose start time is unreadable falls back to the second its directory
  name encodes, and the directory name is the final tiebreak, so the order stays
  total.
- A session directory with no `session.json` — an interrupted run recorded none
  — is skipped, and so is a document that does not parse. Neither fails the
  command.
- `--since <timestamp>` takes an RFC 3339 timestamp and keeps records whose
  **recorded** start time is at or after it; a record recorded exactly at the
  timestamp is kept. The filter reads the recorded `startTime`, not the
  directory name, and a record whose start time is unreadable is **not** emitted
  under `--since` (it cannot be shown to satisfy the filter). The same record is
  still emitted without the flag.
- Every line declares its own version. A record written by a CLI published
  before the version-2 flip carries no `protocolVersion` and the version-1
  shape; export states it as recorded rather than upgrading it, exactly as
  `sessions inspect` reads both shapes.
- With no `--output`, the raw JSONL stream goes to stdout, so
  `putnami sessions export > records.jsonl` is directly usable. Under
  `--output=json` or `--output=jsonl` the same rows travel inside the shared
  result envelope, like every other `sessions` subcommand.

Pulling the CPU accounting out of the stream:

```bash
putnami sessions export \
  | jq -c '{
      session: (.sessionId // .id),
      commands,
      tasks: .run.counts.total,
      cpuActualMs: .run.cpu.actualMs,
      cpuAllocatedMs: .run.cpu.allocatedMs,
      localHits: .run.reuse.localCache,
      remoteHits: .run.reuse.remoteCache,
      exitCode: .run.exitCode,
      wallMs: .run.durationMs
    }'
```

Version-1 lines carry `id`, `durationMs` and `stats` instead of `sessionId`,
`run.durationMs` and `run.counts`; `(.sessionId // .id)` above is the shape of
the guard a consumer needs for a store that spans the flip.

### Summarize Sessions

```bash
putnami sessions summary
putnami sessions summary --since 2026-09-13T00:00:00Z
putnami sessions summary --command lint,test,build,validate
putnami sessions summary --by-digest
putnami sessions summary --output=json
```

`sessions summary` reduces every recorded session to **one row**:

`sessionId · startTime · commands · selection · tasks total/executed/reused ·
run.cpu.actualMs · wall · outcome`

It answers what a weekly ledger and every benchmark ask, so nobody has to write
that reduction again. It is the same read as `sessions export` — the same
ordering (recorded `startTime`) and the same deduplication (recorded session id)
— so the two surfaces can never disagree about which runs a worktree recorded.
The deduplication is load-bearing rather than tidy: a worktree whose
`.putnami/sessions` reaches a shared store through a symlink would otherwise
count the same run twice.

Row rules:

- **Nested runs are marked, not hidden.** A task may invoke the CLI again — a
  validation guard that builds what it checks is the canonical case — and the
  nested run records its own session. It carries `parentSessionId`, so its row
  says `nested` and names the session that spawned it. Its CPU is real work and
  stays listed; what it must never be is mistaken for a run of its own. Time
  containment is *not* a substitute: concurrent worktrees share a store root and
  two ordinary runs in one worktree overlap.
- **`--command <list>` selects top-level runs whose recorded command set is
  exactly the list**, in any order. Two rules, and both are load-bearing. Exact
  set equality means `--command build` lists the runs whose command set *is*
  `build` and never a gate that contains one; subset matching would return both.
  Top-level-only means a nested run never matches, *whatever it ran* — the filter
  asks which runs of this shape happened, and a run a task spawned is not a run
  of its own. Leaving that to the command set alone would make the answer depend
  on an assumption nothing enforces: that no task ever invokes the CLI with the
  set being asked for. A validation guard running the full gate would be returned
  as a gate and counted twice, since the parent already charges the spawning
  task's subprocess tree. To see nested runs, list without `--command` (they are
  listed and marked) or filter `nested` in `--output=json`.
- **`executed` is `total - reused`**, and `reused` is `run.reuse` summed (local
  cache, remote cache, in-run coalescing), so the two always add up to `total`.
- **`selection`** renders the way the run was spelled: `--all`,
  `--all filtered (n)`, `--impacted (n)` or `--projects a,b`. A record that
  stated no selection renders `-`; it is unknown, never `--all`.
- **CPU is `run.cpu.actualMs`** and renders `-` when the record carries no CPU
  block — a run that spawned nothing, or a producer that captured no runner
  environment, measured none, and printing `0` would put a fabricated number in
  a ledger.
- **`--since <timestamp>`** is export's cutoff: RFC 3339, inclusive at the
  boundary, read from the recorded `startTime`.
- A record written by a CLI published before the version-2 flip still lists. It
  simply carries none of the members version 2 added, so selection, parent and
  CPU read as **absent**.
- With `--output=json` (or `jsonl`) the rows arrive under the result envelope's
  `data` as **an array, at every row count** — `[]` when nothing matches, and a
  one-element array for a single session. `.data[]` is therefore always valid,
  including on the single-gate case a fresh worktree produces.

```bash
# The gate sessions of the last /fix, and what each one cost.
putnami sessions summary --since 2026-09-13T00:00:00Z --command lint,test,build,validate --output=json \
  | jq -c '.data[] | {session: .sessionId, tasks, cpuMs: .cpuActualMs, wallMs, outcome}'
```

#### Group task records by input digest

`--by-digest` regroups the task records of the sessions the other flags
selected by their `inputDigest` — the cache key each task was keyed on (see
[the contract](../../../protocols/cli/doc/02-result-v2.md#task-input-digest)).
Use it to answer why a task ran: a digest reused on every run is a stable key,
and a task that executed at a new digest had an input move.

```bash
putnami sessions summary --since 2026-09-17T00:00:00Z --by-digest
```

```text
  DIGEST                TASK                                         RECORDS  EXECUTED  REUSED  FAILED
  ------                ----                                         -------  --------  ------  ------
  sha256:aaaaaaaaaaa... /tooling/cli:test~test                       2        2         0       2
  sha256:bbbbbbbbbbb... /tooling/cli:test~test                       2        1         1       0

  2 digests over 4 records · 1 record without a digest (skipped, no cache identity, or recorded by an older CLI)
```

Read it this way: `/tooling/cli:test~test` failed twice at the first digest,
then executed once at a new digest and was reused on the next run.

Grouping rules:

- Digests list in the order they were first recorded: oldest session first, then
  record order.
- `executed` is `records - reused`, the same derivation as the session rows. A
  failure replayed from the failure cache counts as executed: it spawned nothing,
  but it reused no result either.
- `failed` counts failed records, executed or replayed.
- A record without a digest is never grouped. The footer counts those records: a
  skipped task, a task with no cache identity, and every record an older CLI
  wrote.
- With `--output=json` each row adds `tasks` (the distinct task keys at that
  digest) and `occurrences` (one entry per record: `sessionId`, `task`, `status`,
  `reuse`). The rows are an array at every count, as for session rows.

```bash
# Every digest a task failed at, and the sessions that recorded it.
putnami sessions summary --by-digest --output=json \
  | jq -c '.data[] | select(.failed > 0) | {inputDigest, tasks, sessions: [.occurrences[].sessionId]}'
```

### Retention and Pruning

The session store retains 20 sessions by default. Set `sessions.keep` in the
workspace config to retain more (or fewer):

```json
{
  "sessions": {
    "keep": 200
  }
}
```

Resolution is **workspace config > global config > default 20**, so
`~/.putnami/config.json` sets a machine-wide floor a repo can override. There is
deliberately no environment variable: session records are per-worktree, and a committed
`putnami.workspace.json` already reaches every worktree that writes them — unlike
the machine-global build store, whose GC settings do take `PUTNAMI_STORE_*`
overrides. The value must be at least 1; `0` is rejected by the schema because it
could be read as either "unlimited" or "delete everything".

Retention bounds a worktree's disk, not a record's usefulness. Records that must
outlive the worktree are collected with
[`sessions export`](#export-sessions) while it still exists.

Ordinary terminal runs move the
`latest` symlink when they finalize. Ephemeral adapter runs (currently MCP
`run_jobs`) remain listable but do not move it, so `latest` is the most recently
finalized non-ephemeral user run and may be older than other session directories.
Pruning always preserves that target and removes the oldest other sessions,
preventing the symlink from becoming dangling.

## Technical Decisions

### Why Append-Only JSONL?

- **Crash safety** — Each event is written immediately. No data loss on unexpected termination.
- **Streaming** — Events can be tailed in real-time for monitoring.
- **Parsability** — Each line is independent; no need to parse the entire file.
- **Compactness** — No JSON array overhead or pretty-printing.

### Why a Separate Plan Snapshot?

The plan is captured before execution starts. This records the *intended* state:

- Which jobs were planned (some may later be skipped due to failures).
- What the dependency graph looked like at plan time.
- What cache status was expected.

Comparing the plan to actual events reveals scheduling anomalies and helps debug dependency issues.

### Why Per-Session Directories?

Each session gets its own directory to:

- Enable atomic cleanup (delete one directory).
- Avoid file contention between concurrent CLI processes.
- Support different data formats per file (JSON for plan/metadata, JSONL for events).
