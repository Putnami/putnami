# Machine result contract, version 2

This document is the settled contract for the CLI's **machine** output, and it
is the **only** one: the version-1 emitters are deleted (see
[ADR 0002 of `@putnami/cli`](../../../tooling/cli/doc/adr/0002-cli-vnext-contracts.md)). Every document the CLI writes carries
`protocolVersion: 2`, on all four surfaces. A document **without** the member is
version 1 and can only have come from a build published before the removal.

If you are moving a consumer off version 1, read
[Migrating from version 1](#migrating-from-version-1) at the bottom — it maps
every v1 field onto its v2 spelling and states the rollback.

- Schema: [`schemas/result-v2.json`](../schemas/result-v2.json)
  (`$id` `https://putnami.dev/schemas/putnami-cli-result-v2.json`,
  published at <https://putnami.dev/schemas/putnami-cli-result-v2.json>)
- Go types + validator: [`result_v2.go`](../result_v2.go),
  [`result_v2_rules.go`](../result_v2_rules.go); the non-job envelope's
  constructor and writer are [`result_v2_envelope.go`](../result_v2_envelope.go)
- TypeScript mirror: `@putnami/cli-protocol` (`src/result-v2.ts`)
- Cross-language corpus: [`conformance/manifest.json`](../conformance/manifest.json)

## Why version 2

Version 1 has four machine surfaces and none of them says which contract it
speaks:

| Surface | Version 1 shape |
|---------|-----------------|
| `--output=json` | `Result` — `command` / `status` / `data` / `error` / `exitCode` ([`result.go`](../result.go)); the `$id` exists but is never emitted |
| `--output=jsonl` | `job:start` / `job:event` / `job:end` / `session:end` (declared by `protocols/runtime/stream.go`, deleted with its last producer at B7a — see below) |
| MCP `run_jobs` / `plan_jobs` | `runResult` / `planResult` (`tooling/cli/internal/mcp/adapter.go`) |
| session files | `SessionMetadata` / `PlanSnapshot` / `SessionEvent` (`tooling/cli/internal/workspace_state/session.go`) |

A consumer therefore infers the contract from the CLI version it happens to be
talking to. Version 2 stamps `protocolVersion: 2` on every document: **a
document without the field is version 1**, and there is no other way to tell
them apart.

Two version-1 dualities are also settled here. Both were left in place
deliberately by the CLI foundation work, because collapsing them changes a
shipped wire — see
[ADR 0001](../../../tooling/cli/doc/adr/0001-cli-foundation-boundaries.md) and
[ADR 0002](../../../tooling/cli/doc/adr/0002-cli-vnext-contracts.md).

## Decision 1 — unified success is STRICT

Version 1 ships two success predicates
(`tooling/cli/internal/jobs/result_reduce.go`):

- `Success` — strict: a task whose **failed** result was reused still fails the
  run. The process exit code, the recorded session verdict and `--output=json`
  read this one.
- `SuccessIgnoringReuse` — lenient: a reused failure is invisible. The
  `--output=jsonl` `session:end` summary read this one, because its tally
  `continue`d on any cache hit before reaching the failure branch.

They disagree on exactly one input: a reused failure.

> **Version 2 keeps the strict rule and deletes the lenient one.** A run
> succeeds only when every selected task succeeded, reuse included. A cache hit
> carries the verdict it stored: a reused success counts as a success, a reused
> failure fails the run. `SuccessIgnoringReuse` has no v2 spelling.

The rationale is the cache invariant, not a preference between two tallies: a
cache hit is a claim that re-running the task would produce the same result, so
a hit that restores a failure *is* a failure. A verdict that varied with cache
temperature would make "did my workspace pass?" unanswerable from the output —
the same run would be green on a warm machine and red on a cold one.

The shape enforces it. `run.counts` is the verdict histogram over **every**
selected task and has no `cached` bucket; reuse is counted separately in
`run.reuse`, a provenance histogram over the same task set. The two are
orthogonal, so the accounting that produced the lenient rule — dropping a task
out of the verdict tally because it was a cache hit — cannot be expressed:

```json
"counts": { "total": 3, "succeeded": 2, "failed": 1, "canceled": 0, "skipped": 0 },
"reuse":  { "localCache": 3, "remoteCache": 0, "coalesced": 0 }
```

That run is a **failure** with all three tasks restored from cache. Version 1's
`session:end` called it a success.

Machine-checked rules:

- `counts.total == succeeded + failed + canceled + skipped`
- `reuse.localCache + reuse.remoteCache + reuse.coalesced <= counts.total`
- `outcome == "success"` requires `counts.failed == 0`
- `outcome == "failure"` requires `counts.failed > 0`
- when `failures` is present, `len(failures) == counts.failed` — a counted
  failure cannot then be omitted from the list an agent reads

## Decision 2 — abort wins, and every surface agrees

Version 1 orders abort against failure **two different ways** on purpose:

- the process exit code puts abort first — an interrupted run returns `130`,
  even with failures (`tooling/cli/internal/engine/execute.go`, `sessionExitCode`);
- the `--output=json` envelope puts failure first, so an interrupted run with
  failures reported `status: "failure"` and `exitCode: 1`
  (`tooling/cli/internal/output/json.go`). The stated reason: "an envelope that
  said 'interrupted' while jobs were failing would hide the actionable half of
  the run".

> **Version 2 keeps the process ordering — abort wins — and makes every surface
> agree with it. The exact precedence is `aborted` > `failure` > `success`.**

Two reasons the process ordering is the one that survives:

1. The exit code is the load-bearing half. Supervisors, CI gates and shells map
   `130` to "interrupted, safe to retry" and `1` to "the code under test is
   broken". Reordering *that* would silently reclassify every interrupted run as
   a genuine failure. Reordering the envelope costs one consumer a field lookup.
2. The concern that motivated the envelope's ordering is answered structurally,
   not by precedence. Version 2 reports the abort **distinctly** from the
   failures instead of making them compete for one slot: the verdict says
   `aborted`, and `run.counts.failed` plus `run.failures[]` still name every
   failure the run collected. Nothing is hidden.

Derivation, identical on all four surfaces:

```
outcome  = "aborted"  if the run was aborted        (abortedBy is then required)
         = "failure"  else if counts.failed > 0
         = "success"  otherwise

exitCode = 130                  if outcome == "aborted"
         = <forwarded code>     if outcome == "failure" and the command forwards
                                a workload's own exit code (e.g. `putnami run`)
         = 1                    else if outcome == "failure"
         = 0                    if outcome == "success"
```

Machine-checked rules:

- `outcome == "aborted"` ⟺ `abortedBy` present (`user` for an interactive
  Ctrl-C, `signal` for a supervisor's SIGTERM)
- `exitCode == 0` iff `outcome == "success"`; `exitCode == 130` iff
  `outcome == "aborted"`; a `failure` may carry any other non-zero code
- the envelope's `status` and `exitCode` **are** the process verdict, and when
  `run` is present `run.outcome == status` and `run.exitCode == exitCode`
- `error.code == "signal"` iff the document is aborted

`Result.status` therefore has three values in v2 (`success | failure |
aborted`) where v1 had two. That widening **is** the distinct reporting: an
abort is no longer spelled as a failure with a special error class.

## Decision 2a — a preview completes as a plan, not a run

`--plan` and plan-only `--dry-run` stop before session creation and task
execution. Their machine output therefore reports the fact they actually
proved: a typed `PlanSummary`.

- `--output=json` emits one successful envelope whose `plan` member is mutually
  exclusive with `run`, `data`, and `error`.
- `--output=jsonl` emits one terminal `plan:end` record with `plan`. It is not a
  `session:end`, carries no `run` or `machineOutput`, and is never appended to a
  session `events.jsonl` artifact.
- An empty plan is explicit (`metrics.tasks == 0`, `tasks == []`). It is valid
  protocol output so a caller can reject the no-op by policy without parsing a
  human notice or mistaking silence for success.

The envelope requires `status: "success"` and `exitCode: 0` because a plan
member means planning completed. A failure before that point uses the ordinary
failure envelope and has no `plan`. Synthesizing a successful zero-task run was
rejected: it would claim that a scheduler session executed when none existed.
Parsing the human plan table was rejected because presentation text is not a
versioned protocol.

## Decision 3 — task identity is typed

Version 1 identifies a task with strings assembled at the point of use:
`ScheduledJob.Key() = Project.ID + ":" + PlanName()`, `[]string` dependency
edges, and per-surface pairs of display names — the JSONL failure summary even
writes the plan key into **both** `package` and `job`, so no consumer can take
them apart.

Version 2 carries one structured identity everywhere a task is named:

```json
{
  "key": "/tooling/cli:build~compile",
  "scope": "project",
  "project":  { "id": "/tooling/cli", "name": "@putnami/cli" },
  "task":     { "name": "build~compile", "command": "build", "step": "compile", "kind": "go-build" },
  "provider": { "extension": "@putnami/go", "version": "0.1.0" }
}
```

| Field | Meaning |
|-------|---------|
| `key` | **Derived**, exactly `project.id + ":" + task.name`. It exists so consumers keep one stable join string (and so B2a can keep it byte-identical to the v1 plan key) — never an independent value. |
| `scope` | `project` for a per-project task, `workspace` for a task that runs once for the whole workspace. Version 1 expressed this as a synthetic project a reader had to recognize. |
| `project.id` / `project.name` | The workspace project id and its declared name, apart. |
| `task.name` | The canonical **plan** name (the scheduler/DAG name), never a display name. |
| `task.command` | The root command the task belongs to. |
| `task.step` | The pipeline step id, when the task is an expanded step. |
| `task.kind` | The step's declared task when there is one, otherwise the task name — what groups same-shaped work across projects. |
| `provider.extension` / `provider.version` | The extension providing the task. |

These are the fields a v2 producer emits; the display-name and legacy-key
fallbacks are gone. The
derived-key rule is machine-checked (`cli.result.invalid_key`), so an identity
whose `key` disagrees with its structured fields is rejected rather than
silently preferred by one consumer and not another.

## The six documents

| Document | Surface |
|----------|---------|
| `resultEnvelope` | `--output=json` — one object per invocation |
| `sessionStreamRecord` | `--output=jsonl` — task/session records for a run, or one terminal `plan:end` for a preview. Recorded session event logs use only task records plus `session:end`; `plan:end` never claims persistence. |
| `mcpResult` | MCP `run_jobs` / `plan_jobs` |
| `sessionFile` | recorded `session.json` |
| `sessionPlanFile` | recorded `plan.json` |
| `reportFile` | recorded `report.json` — the run's bounded synthesis, specified in [`03-report.md`](./03-report.md) |

Every document is **closed** (`additionalProperties: false`) and carries
`protocolVersion: 2`. Stream records rename v1's `job:*` to `task:*` and pin an
exact member set per variant, so a reader can switch on `record` alone.

`ValidateSessionStream` remains deliberately narrower than the per-record
validator: it validates a persisted bounded **session** pair and therefore
requires `session:end` plus its artifact proof. Passing a standalone
`plan:end` to it fails; validate that live preview line as
`sessionStreamRecord` instead.

The `event` payload of a `task:event` record stays owned by
[`protocols/runtime`](../../runtime/README.md) and is versioned there; this
contract treats it as an opaque object.

### Test case records

A `test:case` record reports one test case of a test task, so a reader shows
per-test results without parsing tool output:

```json
{"protocolVersion":2,"record":"test:case","time":"2026-09-28T10:00:00Z",
 "identity":{"key":"/go/api:test~test","scope":"project",
  "project":{"id":"/go/api","name":"example.com/acme/api"},
  "task":{"name":"test~test","command":"test","step":"test","kind":"test-exec"},
  "provider":{"extension":"@putnami/go"}},
 "testCase":{"name":"TestParse/empty","suite":"example.com/acme/api","status":"failed",
  "durationMs":12,"output":"parse_test.go:41: want error",
  "file":"go/api/parse_test.go","line":41}}
```

The record carries exactly `identity` and `testCase`. `identity` names the task
the case belongs to, the same identity its `task:end` carries.

| Member | Rule |
|--------|------|
| `name` | Required. The runner's full test name: Go `TestX/sub`, TypeScript describe names and case joined by ` > `, Python's node ID after the file (`TestA::test_b[param]`). |
| `suite` | Required. The Go package import path, or the workspace-relative test file for TypeScript and Python. |
| `status` | Required. `passed`, `failed` or `skipped`. |
| `durationMs` | Required. The runner's duration, an integer of at least 0. |
| `output` | Optional, non-empty. The failure text or the skip reason. A `passed` case carries none. |
| `outputTruncated` | Optional. `true` when the producer cut `output`; only with `output`. |
| `file` / `line` | Optional. The workspace-relative file the runner ties to the case and its 1-based line, only when the runner knows them; `line` only with `file`. TypeScript and Python report where the case is declared. Go reports no declaration site, so a Go case carries the first test-file location its output names, and a Go case without output carries none. |

The bounds are UTF-8 byte counts, pinned by the schema and by `TestCaseMax*` in
[`test_case.go`](../test_case.go):

- `name`, `suite` and `file` hold at most 1,024 bytes (`TestCaseMaxTextBytes`).
- `output` holds at most 4,096 bytes (`TestCaseMaxOutputBytes`).
- A task reports at most 1,000 cases (`TestCaseMaxPerTask`). This bound spans
  records, so the schema states it as the value-only `$defs.testCaseMaxPerTask`
  and no per-record validator checks it. The task's `task:end` counts the cases
  it did not report in `task.testCasesDropped` (an integer of at least 1,
  omitted when nothing was dropped).
- A task's cases hold at most 1 MiB (`TestCaseMaxBytesPerTask`), measured as
  the sum of each case's compact JSON encoding. The members of one batched run
  share 8 MiB (`TestCaseMaxBytesPerBatch`) in equal parts, capped at the task
  budget (`TestCaseBatchMemberBytes`): a batch hands every member's cases to the
  CLI in one result line, which the CLI reads whole. Both budgets are value-only
  `$defs`, like the count bound, and the cases they leave out count in
  `testCasesDropped`.

`testCasesDropped` is a member of the task record, so it appears wherever a
task record does: in `task:end`, and in the `tasks` of the session file
(`session.json`).

The schema states the text bounds as `maxLength`, which JSON Schema counts in
code points; `ValidateDocument` and `validateDocument` enforce the stricter byte
bound (`cli.result.invalid_value`).

A producer builds the payload with `BoundTestCases`, or with
`BoundTestCasesWithin` and the member budget in a batch. It sanitizes each
string with sanitizer v1 before it cuts, so the CLI's own sanitization never
grows a bounded string again, and drops a case it cannot describe. When the
cases exceed the count or byte budget, it keeps the failed ones first, then the
skipped, then the passed, each while it fits, and reports the rest as
dropped. `TruncateTestCaseOutput` keeps the
head and the tail of a long output and sets `outputTruncated`.

The CLI writes a task's `test:case` records immediately before that task's
`task:end`, for solo, batched and cache-replayed tasks alike, and reports each
case once. An extension hands the cases to the CLI in its test result's `data`
(`testCases` and `testCasesDropped`, see
[`protocols/runtime`](../../runtime/doc/04-output-contract.md)); the CLI removes
`testCases` from the result it copies into `task:event`, so the stream never
carries a case twice. `task:end.task.testCasesDropped` adds the producer's
dropped count to the CLI's.

`test:case` is additive to the v2 stream: a reader that does not know a
`record` value skips the record, and the `sessions` commands already do. The
addition is not invisible everywhere:

- A strict validator built before this contract, a `ValidateDocument` or
  `validateDocument` from an older release, rejects a `test:case` record
  (unknown `record` value) and a task record that carries `testCasesDropped`
  (unknown member). Upgrade the validator before validating new streams.
- An older CLI's `sessions inspect` lists a `test:case` record by its record
  type without its case payload.

The general run summary is **one type** on all four legacy surfaces — the
envelope's `run` member, a legacy `session:end` record's `run`, the MCP
`run_jobs` result's `run`, and the session file's `run` are the same shape. The
opt-in bounded stream profile uses the strict projection `StreamRunSummary`,
which preserves verdict/count semantics but makes the unbounded `failures` and
`publications` arrays impossible. See [`04-machine-output.md`](./04-machine-output.md).

`run.cache.local`, when present, attributes local cache serving that task spans
do not: direct `hits`/`misses`, union `servedMs`, additive phase walls for
key/input digest computation, version/capability source bindings, and restore
verification/materialization, plus the bindings' physical Git
`spawnedProcesses`. `hits` equals `run.reuse.localCache`; `servedMs` cannot
exceed the run wall. The CLI partitions each segment in the served union once,
with stable priority `bindings`, then `keys`, then `restoreVerify`, so nested
work and parallel workers never multiply the breakdown. The three phase fields
sum to `servedMs`, except that independent integer-millisecond truncation may
leave the sum up to 2 ms smaller.

### Docker publication facts

`run.publications[]` is an additive optional list for Docker publish workloads.
Each fact carries the typed publish-task `identity`, the resolved release
`session`, concrete `registry` and `image`, optional `tags`, and a verified
`imageDigest`. The digest is constrained to `sha256:<64 lowercase hex>`; a tag
or another digest algorithm is invalid and producers omit an unverifiable
record rather than substituting a mutable reference.

`timings.buildMs` is joined from the matching Docker package task. The remaining
timing members measure the publisher's registry phases: cache lookup/transfer,
registry push, reference publication, and digest resolution. `cacheOutcome` is
`hit | miss`: a hit reuses a retagged digest (`contentStatus: "retagged"`,
`digestReused: true`), while a miss pushes fresh content
(`contentStatus: "pushed"`, `digestReused: false`). `concurrency` carries the
configured scheduler cap and the maximum live `docker-push` overlap observed by
the renderer. Its effective value may be zero when all work avoided a
registry-facing phase, but never exceeds the cap.

On `--output=jsonl`, the underlying `published` artifact appears first as a
typed `task:event`; the terminal `session:end.run.publications[]` adds the
cross-task build timing and run-level concurrency. The aggregated
`--output=json` envelope exposes the same terminal list. Dry/local-only
publishes and entries without concrete registry-backed immutable provenance do
not enter this optional list.

### Physical executions

`sessionFile.executions[]` is an additive optional list: one entry per
**physical execution** — one subprocess the CLI spawned — and each task record
that came out of it carries the same `executionId`.

It exists because the task list is **logical and fans out**. A batched dispatch
runs *n* projects in one subprocess, so *n* task records describe work that was
measured once; summing `tasks[].durationMs` therefore multiplies the batch's
real cost by *n*. A measured run showed that gap directly: 796 logical task
records against 57.1
de-duplicated physical task-minutes. A consumer that asks "what did this run
cost the machine" sums `executions[]`, where every subprocess appears exactly
once; a consumer that asks "which project paid for it" keeps reading `tasks[]`.

| Member | Meaning |
|--------|---------|
| `id` | handle within this document; `tasks[].executionId` references it |
| `wallMs` | the subprocess's own wall — immediately before fork/exec until wait returned |
| `userCpuMs` / `systemCpuMs` | CPU time of the process tree, waited children included |
| `maxRssBytes` | peak resident set size, **always bytes** |
| `ioInBlocks` / `ioOutBlocks` | rusage block-IO operation counts |
| `concurrency` | tool-native parallelism granted (the CPU budget exported to the process) |
| `tasks` | how many logical records this one execution produced — 1 for a solo spawn, *n* for a batch |

`maxRssBytes` is normalized by the producer: `ru_maxrss` is **bytes** on darwin
and **kilobytes** on linux, and a ledger mixing the two would compare a gigabyte
against a megabyte across a developer machine and CI. Every member is measured,
never derived, so a counter the platform does not expose is **omitted** rather
than reported as a measured zero — `ru_inblock`/`ru_oublock` are 0 for most
darwin processes, and a platform exposing no rusage at all still records its
wall and CPU.

Two rules hold, and the validator enforces the second:

- **Reuse spends nothing.** A reused or skipped task adds no execution of its
  own to the ledger. Whether it carries an `executionId` says where its result
  came from, and the two coalescing arrangements differ: a cache hit, a skipped
  task and a **lease-coalesced** waiter — one that adopted a result another
  process published through the cache — reference no execution, because no
  subprocess in *this* document produced their result. A **shared-node**
  follower reports `reuse: "coalesced"` too, but its work WAS done here, by the
  group's leader, so it carries the **leader's `executionId`**: one physical
  entry, referenced by every logical row that came out of it (the `tasks`
  member above counts them). Either way the subprocess is recorded once, which
  is what makes "count physical work exactly once" true rather than
  aspirational.
- **No dangling reference.** A record's `executionId` must name an execution the
  same document declares (`cli.result.invalid_key` otherwise). The converse is
  deliberately allowed: an execution nothing references is real work — a
  superseded retry attempt — and dropping it would understate the machine's cost.

Batch members no longer each claim the leader's whole wall: a member's
`durationMs` is its **share** of the shared attempt. Being precise about what
reconciles against what, because a consumer will try:

- The shares divide the shared attempt's **task-level wall** — the same quantity
  a solo task's `durationMs` reports. That wall starts before the context file
  and the output directory are prepared, so it is slightly *wider* than
  `executions[].wallMs`, which is the subprocess's own wall (the one the rusage
  accounts for). Sharing the task-level wall is deliberate: it keeps a batched
  member's `durationMs` the same measurement as the same task's solo
  `durationMs`, which is the solo↔batch comparability the batching work exists
  to preserve.
- The division is exact in nanoseconds, remainder included, but each record is
  then truncated independently by milliseconds. Summing *n* members therefore
  loses up to *n*−1 ms.
- So `sum(tasks[].durationMs)` over one execution's members does **not**
  reconcile against that execution's `wallMs`, and is not meant to: it is
  systematically a little wider (CLI-side setup) and a little narrower (ms
  truncation). For physical cost read `executions[]`, where each number is
  stated once and measured directly; the task durations are for attribution.

### Runner environment

`sessionFile.environment` is an additive optional object: the **machine** the
session ran on, and how much of it the session got.

It exists because a CPU total on its own answers no question. A measured run
recorded 120.9 allocated vCPU-minutes against 80.7 actual — an average of 5.34 of 8 cores — and that gap
has three different explanations with three different fixes:

- the plan had nothing else to run (a **scheduling** problem);
- the cgroup quota suspended the runner mid-period (a **quota** problem, which
  more parallelism makes *worse*);
- the host was oversubscribed and gave our ticks to a neighbor (a **placement**
  problem no change to the build can fix).

Timings cannot tell them apart. `cpu.stat`'s throttle counters, PSI and
`/proc/stat`'s steal column can.

| Member | Meaning |
|--------|---------|
| `os` / `arch` | GOOS/GOARCH of the recording CLI |
| `logicalCpus` | CPUs visible to the process — what the scheduler sized its pool from, **not** the quota |
| `cpuModel` | processor brand string, absent where the platform publishes none |
| `windowMs` | wall between the opening and closing samples: the window every delta below covers |
| `cgroupCpu` | the cgroup v2 (`cpu.max`, `cpu.stat`) or v1 (`cpu.cfs_*`, `cpuacct.usage`) CPU controller |
| `cpuPressure` | PSI for CPU (`/proc/pressure/cpu`) |
| `hostCpu` | the `/proc/stat` aggregate — steal and iowait against their own total |
| `memoryCapacity` | stable physical, finite cgroup and effective memory capacity, with explicit provenance |
| `cgroupMemory` | closing cgroup memory gauges plus cgroup v2 event deltas |
| `memoryPressure` | memory PSI deltas with an explicit `host` or `cgroup` scope |

Two rules govern the whole block, and both are the point of it:

- **Absent is not zero.** macOS has no cgroups and no PSI; a Linux kernel can be
  built without PSI. Those blocks are then **omitted**, because a zeroed
  `cgroupCpu` would read as *"measured: never throttled"* for a machine that
  cannot throttle at all. The consequence is deliberate: where a zero IS a
  measurement — `throttledPeriods: 0` is the finding that rules out quota
  starvation — the counters live inside a **nested object** whose presence says
  "this was read", instead of an `omitempty` scalar that could not tell the two
  apart. `cgroupCpu.quotaUs` is the one exception and it is unambiguous the
  other way: v2 `cpu.max` says either a positive quota or `max`, while v1
  `cpu.cfs_quota_us` says either a positive quota or `-1`. In both layouts an
  absent `quotaUs` means **unlimited**.
- **Counters are windowed.** Every kernel counter here is cumulative since boot
  or since the cgroup was created. Each is sampled twice — once before the first
  job is dispatched, once after the last subprocess is waited for — and only the
  **delta** is reported, against `windowMs`. A delta that comes out negative
  means the counter was reset or the two samples came from different cgroups;
  that block is **dropped**, not clamped, because a clamped counter is
  indistinguishable from a measured zero.

`hostCpu` is reported in **USER_HZ ticks**, exactly as the kernel states them:
converting to milliseconds needs a USER_HZ the producer cannot read, so
`totalTicks` is published beside the columns and the only reading this block is
for — the share of the window lost to steal or iowait — stays unit-free. The
total sums the columns up to and including `steal`; `guest` and `guest_nice`
are excluded because the kernel already counts them inside `user` and `nice`.
Neither `stealTicks` nor `ioWaitTicks` may exceed `totalTicks`
(`cli.result.count_mismatch` otherwise) — that combination means two mismatched
sample windows were subtracted.

Memory follows the same absence and window rules without collapsing facts that
have different ownership or lifetimes:

- `memoryCapacity.physicalBytes` is host physical RAM.
  `memoryCapacity.cgroupLimit` is present only for a finite controller limit and
  carries `source: cgroup-v1 | cgroup-v2`. `effectiveBytes` is the smaller
  available stable bound, and `effectiveSource: physical | cgroup-limit` names
  the input that supplied it. This records the scheduler's existing capacity
  input; it does not change admission policy.
- `cgroupMemory.closing.currentBytes`, `composition` and `lifetimePeak` are
  **closing gauges**, not deltas. `lifetimePeak.bytes` is explicitly the peak
  over the **cgroup lifetime** (`memory.peak` on v2 or
  `memory.max_usage_in_bytes` on v1), even though it was sampled at session
  close. It is never a session peak.
- `cgroupMemory.closing.composition` is the exact cgroup v2 `memory.stat`
  `anon`, `file` and `shmem` reading. The kernel's `file` value **includes
  `shmem`**; consumers must not sum `fileBytes + shmemBytes`. Cgroup v1's
  similarly named counters do not have that exact composition contract, so the
  block is absent on v1 rather than translated approximately.
- `cgroupMemory.events` is the complete standard v2 `memory.events` subset used
  here — `low`, `high`, `max`, `oom`, `oomKill` — reported as deltas over
  `windowMs`. Cgroup v1 `failcnt` is not an OOM, high or max event and is never
  relabelled as one.
- `memoryPressure` carries the `some` and `full` PSI total deltas. Its scope is
  explicit: `host` means `/proc/pressure/memory`; `cgroup` means the cgroup v2
  `memory.pressure` file. The units match, but the populations do not, so a
  consumer must not treat the two scopes as interchangeable.

Every optional zero above is preserved by a containing object's presence: a
present event block full of zeros means the counters were measured and did not
move; an absent block means the facts were unsupported, malformed, reset or
outside the safe JSON integer range. Producers omit an unusable block rather
than fabricating or clamping it. No member declares scratch backing, and no
filesystem type may be used to infer one.

These session-scoped cgroup facts do not change `executions[].maxRssBytes`:
MaxRSS remains the peak resident set size of one physical subprocess tree. It
must not be summed with, substituted for or attributed from shared-cgroup
`anon`, `file`, `shmem` or `current` gauges.

### Actual versus allocated CPU

`run.cpu` is an additive optional object on the run summary, pairing what the
runner **granted** with what the run **burned**:

| Member | Meaning |
|--------|---------|
| `allocatedMillicores` | thousandths of a core the runner may use |
| `allocatedSource` | `cgroup-quota` when a bandwidth limit is in force, `logical-cpus` otherwise |
| `allocatedMs` | `durationMs × allocatedMillicores / 1000`, truncating |
| `actualMs` | CPU consumed, summed over `executions[]` with each execution counted **once** |
| `executions` | how many physical executions `actualMs` was summed from |

`actualMs` reads the physical ledger, never the task list: summing
`tasks[].durationMs` multiplies a batch's cost by its fan-out, and this is the
number every later saving claim is measured against. `allocatedSource` is not
cosmetic — a quota **suspends** a run that exceeds it, while a core count is
only the point past which there is nothing left to run on, so a consumer that
conflated them would read throttling into a machine that was merely busy.

`allocatedMs` is tied to the summary's own `durationMs`
(`cli.result.count_mismatch` otherwise), so the two always divide. `actualMs` is
deliberately **not** bounded by `allocatedMs`: a cgroup quota is enforced per
period rather than per run, so a run can exceed its budget over short bursts,
and clamping the measurement would hide exactly the oversubscription this block
exists to expose.

The block is absent when the run spawned nothing, and absent when the producer
captured no environment — an allocation figure with a guessed denominator is
worse than none, because every ratio derived from it would inherit the guess
silently. The member lives on `runSummary`, so any surface carrying one may
report it; a surface reduced without the physical ledger (the `--output=jsonl`
`session:end` record, and the `--output=json` envelope) simply omits it, which
is the same "no measurement, no claim" rule the environment block follows.

### Dependency preparation

`sessionFile.preparation` is an additive optional object: the stage that
resolves, fetches, builds and publishes the artifacts a run needs **before it
can plan anything**, decomposed into ownership phases.

It is a separate block from `executions[]`, and the separation is a correctness
property rather than a layout choice. An `executionRecord` is one **subprocess**
— joined to by `taskRecord.executionId`, summed into `run.cpu.actualMs`. Most of
preparation is in-process work (hashing input trees, walking module closures,
copying files into an isolated staging view) that no subprocess performed and no
task record could reference; filing it as an execution would put phantom spawns
in the ledger and report CLI-process CPU as child CPU. **Nothing in
`preparation` is in `executions[]`, and nothing in `executions[]` is in
`preparation`** — so the two may be added together, and neither may be added to
itself twice.

| Member | Meaning |
|--------|---------|
| `wallMs` | the stage's own wall: the sum of its top-level invocations, which run sequentially within a session |
| `parallelism` | the widest independent-step fan-out the stage was permitted |
| `phases[]` | the ownership classes the stage **entered**, in canonical order |

Each phase carries `phase`, `wallMs` (the **sum of that class's spans**),
`steps` (how many spans were summed), and `cpuMs` where the class spawned
subprocesses.

The ownership vocabulary is **closed**, because an unclassified step is a cost
nobody can act on:

| Phase | What it owns |
|-------|--------------|
| `network` | talking to a remote: registry metadata, artifact downloads, integrity fetches |
| `resolution` | deciding what is needed and under which content identity |
| `verification` | proving what is on disk is what was promised: integrity, identity, ABI handshakes |
| `generation` | running a declared prepare/build command |
| `mutation` | staging, the content-addressed store's ownership-lock wait, publication, reclaim |

Three rules govern the block:

- **Absent is not zero**, the same rule the environment block follows. A warm
  content-addressed store rebuilds nothing, and the way it says so is that
  `generation` is **missing** from `phases[]` — not present with `wallMs: 0`,
  which would read as *"the compile ran and was free"*.
- **`cpuMs` is measured, never derived.** A class that ran entirely in the CLI
  process reports no child CPU rather than a figure inferred from its wall.
- **Phase walls are sums, not intervals.** Under parallelism the spans overlap,
  so `sum(phases[].wallMs)` can legitimately exceed `wallMs` — the gap *is* what
  the parallelism bought. At `parallelism: 1` no overlap is possible and the
  phases must fit inside the stage
  (`cli.result.count_mismatch` on `preparation.wallMs` otherwise). This is why
  `parallelism` is a required member: without it a reader cannot tell an
  overlapping sum from a broken one. Each class also appears **at most once**
  — the list is a histogram over classes, not a per-step log — and a repeat is
  the same `cli.result.count_mismatch`.

### Gated tree fingerprint

`sessionFile.tree` is an additive optional object: **which worktree the session
ran against**, identified by content rather than by name.

It exists because *"the same files are dirty"* is not *"the same bytes are on
disk"*. `git status --porcelain` reports paths and status codes, so an agent
that gates a tree and then edits one of the files it had already dirtied leaves
that output byte-for-byte identical — a path list accepts a gate that proved
nothing about the content now on disk. `sessionFile.git` records a branch and a
baseline and `reportGit` records a commit; neither says anything about the
uncommitted state. Without this block, every consumer that wants to trust a
record later — a finalizer, a reviewer, a failure replay, a weekly ledger — has
to recompute a digest itself, against a worktree that may have moved since or
may not exist any more.

| Member | Meaning |
|--------|---------|
| `fingerprint` | lowercase hex sha256 of the canonical byte stream below — always 64 characters, whatever hash the repository uses for its own objects |
| `dirty` | whether the worktree differed from `headSHA` |
| `headSHA` | the FULL object id of the commit the worktree sat on |

**The canonical byte stream.** One definition, so that two parties comparing
fingerprints are comparing the same measurement. The digest is
`sha256` of:

```
putnami-tree-fingerprint/2\n
head <headSHA>\n
tracked <count>\n
<entry>   (× count, ordered by path)
untracked <count>\n
<entry>   (× count, ordered by path)
```

where each `<entry>` is:

```
<kind> <sha256 of the entry's content> <byte length of path>\n<path>\n
```

- **The tracked section is every path whose worktree state differs from HEAD,
  hashed from the bytes ON DISK.** Nothing in the stream is rendered by git. The
  set comes from `git status --porcelain=v2 -z --no-renames
  --ignore-submodules=none -uno`, a documented machine format that carries modes
  and object ids as fields. Those three flags pin the settings a repository could
  otherwise change: rename detection is score-based and configurable, untracked
  entries belong to their own section, and `diff.ignoreSubmodules` or
  `submodule.<name>.ignore` would otherwise hide a dirty submodule entirely.
  `-z` also removes `core.quotePath`, because NUL-terminated output is never
  quoted.
- `<kind>` is `file`, `exec`, `link`, `dir`, `gone`, `sub` or `sub-absent`. The
  executable bit is in the stream because a `chmod +x` changes what a tree DOES
  while leaving every byte it contains identical. A `link` hashes its TARGET, not
  what the target points at: following it would make a dangling link unreadable
  and would fold a file already hashed under its own path into a second entry. A
  `gone` is a tracked path no longer in the worktree, distinct from an emptied
  one. A `dir` is git's own spelling for an untracked nested repository
  (`ls-files --others` collapses one to `<path>/`), whose contents are outside
  this repository's tree state — git reports nothing about them either — so its
  path enters the stream and its content does not.
- **A submodule is hashed recursively.** A `sub` entry's content digest is the
  submodule worktree's own fingerprint, computed by this same definition; nesting
  is bounded at ten levels. A `sub-absent` entry is a gitlink with no checkout,
  whose content is genuinely not on disk, so its digest covers the gitlink object
  id instead. Version 1 hashed `git diff`, which renders a dirty submodule as
  `Subproject commit <sha>-dirty` — one word, whatever is inside it — so once a
  submodule was dirty, further edits within it left the parent's digest
  unchanged.
- **Version 1 is not merely superseded, it was wrong**, which is why the version
  tag opens the stream. Beyond the submodule blindness above, it hashed rendered
  diff output, so display settings changed the digest of identical bytes:
  `--binary` does not imply `--full-index` for a text diff, and `core.abbrev`,
  `diff.mnemonicPrefix`, `diff.noprefix` and `diff.srcPrefix` each made two
  machines disagree about the same tree.
- Outside the digest, because git itself does not report it: a path marked
  `assume-unchanged` or `skip-worktree`, and the executable bit when
  `core.fileMode` is false. Those are repository-wide settings every reader of
  that repository shares, not per-machine rendering.
- Every variable-length part either is already a fixed-width digest or carries
  its byte length, so no path can be confused with another one plus a
  separator.
- HEAD is folded in as well as the delta, because a delta is only meaningful
  against the commit it was taken from: a moved HEAD must read as a different
  tree instead of canceling out against a coincidentally identical diff.
- Ignored files are outside the tree a gate reasons about, and
  `--exclude-standard` is what leaves them out.

`putnami tree fingerprint` prints this digest and is the only implementation;
`--output=json` adds `dirty` and `headSHA` under these same member names.

**When it is captured, and what that means.** The producer takes the measurement
when the session OPENS, before its first task runs. The recorded fingerprint is
therefore a statement about the tree the session's work CONSUMED. A capture taken
at finalize would describe what the run PRODUCED — the tree after a
`lint --fix` rewrote a file — which is a different claim, and not the one a
reader asking *"was this run made against the code I am looking at?"* needs. A
reader who compares the recorded fingerprint with the tree in front of them
learns both things at once: they match, or the run changed the tree.

**Absence is the only "unknown".** A producer outside a git worktree, in a
repository with no commit, or one whose git call failed, records no `tree` block
at all rather than a zeroed or clean-looking one. That is why `dirty` is a plain
required boolean instead of the optional one `reportGit.dirty` uses: a report
exists whether or not its producer could inspect the tree, while this object is
written only when the fingerprint was computed — and computing it IS the
measurement that decides dirtiness. A present block always knows all three
facts.

### Task input digest

`taskRecord.inputDigest` is an additive optional member: the cache key the
scheduler computed for the task, spelled `sha256:` followed by 64 lowercase hex
characters. It appears on every surface that carries a task record — the
`--output=json` envelope, the `task:end` stream record, the MCP result and the
recorded session file.

It exists so a reader can tell **why** a task executed or reused a result
without re-running the engine. `reuse` says what the run did; `inputDigest`
says which inputs it did it for. Three readers need that pair:

- **"Already red on `main`"**: a task that failed at the same digest on `main`
  failed on inputs your change did not touch.
- **Cache hit rate by digest**: how often one key was served instead of
  executed.
- **The failure-replay audit**: a replayed failure is recorded at a key, and
  the digest is that key.

**Stability.** The digest is the same value the cache addresses its entry by,
not a second hash of it. It therefore moves exactly when the key does: a change
to a declared input, the task contract, the extension, or an upstream task's
key. Two records with one digest were keyed on the same inputs. What goes into
the key is specified by the CLI's caching documentation, not by this contract;
a producer that changes the key's composition changes every digest, and a reader
must not compare digests across such a change.

**Presence.**

| Record | `inputDigest` |
|--------|---------------|
| executed (`reuse: "none"`), including a failure replayed from the failure cache | present |
| `local-cache`, `remote-cache`, `coalesced` | present |
| `canceled` after its key was computed | present |
| `skipped` | **absent** — the scheduler never looked its key up |
| a task with no cache identity (a contract that cannot use the cache) | absent |

A `skipped` record that carries the member is invalid (`cli.result.invalid_value`
at `tasks[i].inputDigest`): a reader grouping records by digest would otherwise
count a task that never ran as one more observation of that key. A digest in
any other spelling is invalid for the same code. `putnami sessions summary
--by-digest` is the CLI's own reader of the member.

## Validation and the cross-language corpus

`ValidateDocument(kind, bytes)` (Go) and `validateDocument(kind, text)`
(TypeScript) return the same `{code, path}` violations, sorted by path then
code, from the closed vocabulary below. Both walk the decoded JSON value rather
than round-tripping through their own types, precisely so the two report the
same paths. Two more rules keep the "same" exact:

- **Ordering is byte/code-unit order**, never locale order — Go compares bytes
  and TypeScript compares UTF-16 code units, which agree on ASCII and depend on
  no host locale data (`"Zfield"` sorts before `"afield"` in both).
- **An integer member is any JSON number carrying an integral value in
  IEEE-754's safe range** (±(2⁵³ − 1)) — the rule JSON Schema's `integer`
  implies, and `Number.isSafeInteger`'s exact set. `1.0` and `1e2` conform;
  `1.5` does not, and neither does a value a JavaScript consumer would silently
  round.

| Code | Meaning |
|------|---------|
| `cli.result.invalid_json` | not parseable, not one value, or not an object |
| `cli.result.unknown_field` | a member the document does not declare |
| `cli.result.missing_field` | a required member is absent |
| `cli.result.unexpected_field` | a declared member this variant forbids |
| `cli.result.invalid_type` | wrong JSON type |
| `cli.result.invalid_enum` | value outside a closed vocabulary |
| `cli.result.invalid_value` | right type, disallowed value (empty string, negative count) |
| `cli.result.invalid_protocol_version` | `protocolVersion` is not 2 |
| `cli.result.invalid_key` | `identity.key` disagrees with the structured identity |
| `cli.result.count_mismatch` | the run histograms do not add up |
| `cli.result.outcome_mismatch` | the verdict breaks unified success or the abort precedence |
| `cli.result.exit_code_mismatch` | an exit code disagrees with its verdict, or two members of one document disagree |
| `cli.result.error_mismatch` | the error member's presence or class disagrees with the verdict |
| `cli.result.budget_exceeded` | a bounded live stream or its final record exceeds the fixed byte or record allowance |
| `cli.result.elision_mismatch` | the live selection or its omission counters disagree with the retained complete stream |
| `cli.result.unsanitized` | terminal controls, invalid Unicode, or an unredacted sensitive value reached machine output |

[`conformance/manifest.json`](../conformance/manifest.json) is a single corpus
both runtimes execute, pinning the **exact** violation set of every fixture. A
Go/TypeScript divergence — a different code, a different path, one violation too
many — fails that runtime's tests against a corpus the other still passes. See
[`conformance/README.md`](../conformance/README.md) for the update procedure.

## Migrating from version 1

Version 1 was emitted by CLI builds published before the v2 cutover. It is gone
from every emitter; the Go type [`Result`](../result.go) and
[`schemas/result.json`](../schemas/result.json) are retained so a consumer can
still READ documents it recorded earlier. Reading is all they support: the v1
`NewResult` constructor and `WriteResult` writer are deleted, so no surface —
first-party or not — can build a versionless document with this module.

The retained shape is held still on purpose: `../testdata/prior-releases/`
carries the v1 schema in its last released state, and
`../prior_release_test.go` fails if `schemas/result.json` drifts from it. This
contract's row of the repository-wide compatibility budget — emit 2, read 1, no
downgrade lever — is
[Compatibility and Migration](../../../tooling/cli/doc/21-compatibility-and-migration.md).

**Detecting the version is the whole compatibility story.** `protocolVersion` is
present on every v2 document and absent from every v1 one, so a consumer that
must handle both branches on `typeof doc.protocolVersion === 'number'` and needs
nothing else — not the CLI version, not a flag.

### `--output=json` — the envelope

| Version 1 | Version 2 |
|-----------|-----------|
| _(no version member)_ | `protocolVersion: 2` |
| `command` | `command` (unchanged) |
| `status`: `success \| failure` | `status`: `success \| failure \| aborted` |
| `exitCode` | `exitCode` (unchanged taxonomy) |
| `error.{code,message,next}` | `error.{code,message,next}` (unchanged) |
| `data.succeeded` / `.failed` / `.canceled` / `.skipped` | `run.counts.{succeeded,failed,canceled,skipped}` |
| `data.cached` | `run.reuse.localCache + run.reuse.remoteCache` |
| `data.coalesced` | `run.reuse.coalesced` |
| `data.total` | `run.counts.total` (= the sum of the four verdict buckets) |
| `data.durationMs` | `run.durationMs` |
| `data.aborted` / `data.abortedBy` | `run.outcome == "aborted"` / `run.abortedBy` |
| `data.jobs[]` (`package`, `job`, `status`, `durationMs`, `cache`, `coalesced`, `error`) | not on the envelope — the failures are `run.failures[]` with a typed identity; the full per-task list is the recorded session file's `tasks[]` |
| _(no field: v1 had none)_ | `run.failures[].diagnostics[]` |
| human plan table from `--plan` / plan-only `--dry-run` | `plan` with `status: "success"`, `exitCode: 0`, and no `run` / `data` / `error` |

Two behavior changes travel with the shape, both settled above:

- an interrupted run that also had failures reported `status: "failure"` /
  `exitCode: 1` in v1, and reports `status: "aborted"` / `exitCode: 130` in v2
  (decision 2). The failures stay visible in `run.counts.failed` and
  `run.failures[]`;
- a run whose only failure was restored from cache reported success on the v1
  `--output=jsonl` summary, and fails in v2 (decision 1).

A **non-job** command (`doctor`, `contracts check`, `context pack`,
`migrate vnext`, …) has no run to report: its envelope carries
`protocolVersion`, `command`, `status`, `exitCode`, the command's own payload in
`data`, and `error` when it failed. Those payloads are unchanged from v1 — only
the envelope around them gained the version stamp and the third status.

### `--output=jsonl` — the session stream

| Version 1 record | Version 2 record |
|------------------|------------------|
| `{"event":"job:start", "package", "job", "time"}` | `{"protocolVersion":2, "record":"task:start", "time", "identity"}` |
| `{"event":"job:event", "package", "job", "type", "data", "time"}` | `{"protocolVersion":2, "record":"task:event", "time", "identity", "event"}` |
| `{"event":"job:end", "package", "job", "status", "duration", "cache", "coalesced", "error"}` | `{"protocolVersion":2, "record":"task:end", "time", "identity", "task"}` |
| `{"event":"session:end", "success", "succeeded", …, "failures":[{"package","job","error"}]}` | `{"protocolVersion":2, "record":"session:end", "time", "run"}` |
| none | `{"protocolVersion":2, "record":"test:case", "time", "identity", "testCase"}` |
| human plan table from `--plan` / plan-only `--dry-run` | `{"protocolVersion":2, "record":"plan:end", "time", "plan"}` |

`package`/`job` were both the plan key on the v1 failure list, which no consumer
could take apart; `identity` carries them separately plus the derived `key`.

The `event` member of a `task:event` record is the subprocess runtime event,
whose vocabulary `protocols/runtime` owns; this contract treats it as an opaque
object and the schema constrains it no further. What the CLI writes there is the
producer's line **normalized** — a nested `data` payload flattened onto the
object, and the producer's `v` stamp dropped. Dropping it is deliberate: the
record already carries `protocolVersion`, and a CLI that requires extension
contract 3 accepts exactly one runtime-event version, so a per-line restatement
would only create a second place for the two to disagree. A consumer that needs
the inner version reads it from the CLI's contract, not from the event.

The version-1 column is documentation, not code: `protocols/runtime/stream.go`
and `schemas/stream.json` declared those envelopes until they were deleted. The
v2 cutover had already removed their last producer, leaving a protocol with no
writer and no reader — which this repository deletes rather than freezes. A
consumer still parsing v1 lines works from this table and from a pinned
pre-removal build.

A legacy `session:end` carries the same `runSummary` as the envelope, MCP result,
and session file. A bounded producer opts in with `machineOutput` and the
distinct `BoundedSessionEndRecord`: its `streamRunSummary` retains the exact
verdict, counts, reuse, duration and optional CPU facts, but cannot carry the
unbounded failure/publication arrays. Fixed budgets, sanitizer v1, exact split
elision and the retained full-detail artifact are specified in
[`04-machine-output.md`](./04-machine-output.md).

`plan:end` is a standalone terminal preview record, not the first or last line
of a session stream. Consumers branch on the discriminator: `plan:end.plan`
proves planning completed; `session:end.run` proves a session executed. A
consumer that requires work also checks `plan.metrics.tasks > 0`.

### MCP `run_jobs` / `plan_jobs`

The tool answers `{protocolVersion, tool, commands, run}` (for `run_jobs`) or
`{protocolVersion, tool, commands, plan}` (for `plan_jobs`). The v1 answer's
flat `success`/`total`/`succeeded`/`failed`/`cached`/`coalesced`/`skipped`/
`durationMs` map onto `run` exactly as the envelope table above; its
`failures[].{project,job}` become `run.failures[].identity`, and each failure
still carries its `diagnostics[]`. `plan_jobs`'s `jobs[]` become `plan.tasks[]`,
with `key` moving to `identity.key`. `get_diagnostics` is **unchanged**: it is
not a machine document, and still answers `{count, diagnostics}`.

### Recorded session files

`.putnami/sessions/<id>/session.json` and `plan.json` are the `sessionFile` and
`sessionPlanFile` documents. The bounded profile now contracts `events.jsonl`
as the complete sanitized counterpart of the capped live stream, but only when
the producer opts in with `machineOutput` on its terminal record. The current
producer remains on the compatible legacy `SessionStreamRecord` path until the
dedicated producer-adoption task lands; this slice defines and validates the
bounded wire without claiming the CLI already applies its caps.

A preview creates no session directory, so its live `plan:end` is never written
to `events.jsonl`. Whole-session validation enforces that distinction by
requiring a terminal `session:end`; accepting `plan:end` as an individual JSONL
document does not make it a persisted session event.

The CLI's own session readers (`putnami sessions show`, `putnami sessions inspect`)
read both versions: sessions recorded before the flip stay openable. Only the
writer is v2-only.

#### Nesting and selection

`sessionFile.parentSessionId` names the session of the run that **spawned** this
one, and is absent for a top-level run. A task may invoke the CLI again — a
validation guard that builds what it checks is the canonical case — and the
nested run records its own session beside its parent's. Without the member the
only discriminator is time containment, and time containment is not one:
concurrent worktrees share a store root through symlinks, and two ordinary runs
in one worktree overlap. An accounting consumer that counts a nested run as a
top-level one **double-counts its CPU**, because the parent already charges the
spawning task's whole subprocess tree.

`sessionFile.selection` records how the run chose the projects it planned over:

| Member | Meaning |
|--------|---------|
| `mode` | `all`, `impacted` or `projects` — the same closed vocabulary the job context's `selection.mode` uses |
| `scoped` | false only for the unscoped whole-workspace projection; a session may claim workspace coverage only then |
| `projects` | the selected projects' canonical ids, sorted; absent when the run selected nothing |
| `releaseSetProjects` | the subset of `projects` whose publish and package steps the session's release-set plan owned, sorted; absent when the session coordinated no release set and when its plan selected no member |

It exists because `git.baseline` cannot answer the question: it is present only
when the user named a baseline, and the selected ids alone cannot separate an
explicit selection from an impact projection that resolved to the same
projects — two runs that cost and mean different things.

A session that names `publish` beside other commands plans two things at once:
the publication the channel head decided, and the verification the caller's own
selection asked for. `projects` is the union — what the run planned and executed
— and `mode`, `baseline` and `baselineSource` describe how the VERIFICATION half
was chosen, because that is the half a selection flag decides. Without
`releaseSetProjects` a consumer could not tell the two apart, and would read a
library that was only verified as one the run published.

A session may not name itself as its own parent. That shape is a producer that
stamped the ambient parent id beside its own id, and a consumer trusting it
would drop the top-level run out of its ledger; validation rejects it with
`cli.result.invalid_key` at `parentSessionId`.

#### Execution placement

`sessionFile.placement` records the caller's `requested` location and the
`actual` execution location. Both are required when the object is present,
and each is either `local` or `remote`. For example,
`{"requested":"remote","actual":"local"}` records local fallback when no
execution provider is installed. A reader must use `actual` to determine
where execution took place; the request alone is not evidence of remote work.

The whole object is absent when placement is unknown, including sessions
written before placement was recorded. Explicit `null`, omitted members,
unknown members and values outside the two locations are invalid. Placement
is observational metadata: it does not change task identities, cache keys or
the meaning and bytes of `tree.fingerprint`.

#### Execution provenance

`placement.provenance` is the EXECUTING engine's statement of the bound
execution request it ran, taken from the request it was handed: `sourceDigest`
(the runner contract's canonical source-manifest digest, `sha256:` and 64
lowercase hex characters), `inputDigest` (the execution-input digest in the
same spelling) and `submission` (the request's idempotency key, 32 lowercase
hex characters). All three are required when the block is present; the block
is absent for a local run, for a local fallback and for every older producer,
so absence is the only unknown and no member is ever a placeholder. It is only
valid beside `actual: "remote"`: a bound request executes only as the remote
placement. It names no provider, attempt or machine — those are the
transport's own observations and live beside the session, not inside it — and
it is never a task cache input. A submitting engine compares it with the
request it submitted before it adopts an imported session as its verdict.

`executions[]`, `tasks[].executionId`, `tasks[].inputDigest`, `environment`, `run.cpu`,
`preparation`, `tree`, `placement`, `placement.provenance`, `parentSessionId`
and `selection` are **additive**: every member an older reader knows keeps its name, its type and
its meaning, so a session file written by a current CLI still parses as one
written before. A reader that validates against the closed schema needs this
version of `result-v2.json`, which is the same requirement any additive member
carries.

### Rolling back

There is no environment variable and no flag. `PUTNAMI_MACHINE_OUTPUT=v1`, the
canary's rollback lever, is **inert**: it selects nothing, it is never an error,
and setting it to `v1` prints one notice on stderr saying so.

To read version-1 documents again, pin a CLI build published before the removal:

```sh
putnami pin 0.1.0-<sha>
```

Version pins are exact and the launcher honors them, so a consumer that needs
more time gets it by pinning rather than by asking a current build to speak an
older wire.

## What this document does NOT cover

- The extension contract version ([`contract.go`](../contract.go)) is a separate
  axis: `cliContract` and `protocolVersion` move independently.
- The runtime event payload inside a `task:event` record belongs to
  `protocols/runtime` and is versioned there.
- Production adoption and writing of the bounded `events.jsonl` profile. This
  contract slice defines its wire and conformance checks only.
