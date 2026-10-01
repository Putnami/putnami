# The session report (`report.json`)

The **report** is a run's contracted *bilan*: one small, closed document
carrying the verdict, the per-command synthesis, a bounded per-job synthesis,
and the cache and scheduler facts a consumer would otherwise have to scrape out
of a recorded session. It is the sixth version-2 machine document
([`02-result-v2.md`](./02-result-v2.md) specifies the other five and the rules —
`protocolVersion`, violation codes, integer semantics — that all six share).

- Document kind: `reportFile` (`DocumentReportFile` / `DOCUMENT_KIND.reportFile`)
- Schema: [`schemas/result-v2.json`](../schemas/result-v2.json), `$defs/reportFile`
- Go types + validator: [`result_v2.go`](../result_v2.go),
  [`result_v2_rules.go`](../result_v2_rules.go)
- TypeScript mirror: `@putnami/cli-protocol` (`src/result-v2.ts`)
- Cross-language corpus: [`conformance/manifest.json`](../conformance/manifest.json),
  the `report.*` and `invalid.report.*` cases

## Why a report

Every run already records a versioned session (`session.json`, `plan.json`,
`events.jsonl`). What it does not record is a **synthesis**, so a consumer that
wants *"what did this run cost, what failed, what did it cover"* has to
reconstruct one:

- Cloud's CI runner binds the gate's session **by set-difference** over the
  session directory — a guess about which directory is new — and then carries a
  handful of CPU facts on an untyped field bag. Every additional fact means more
  scraping, against a document whose shape it does not own.
- Coverage, warnings and errors exist only in console output and `events.jsonl`.
  They are not collectable, so nothing can be analyzed over time.
- A recorded gate session in this workspace is **539 KB**; the same run's report
  is **26 KB**. The session is the debug payload. The report is the durable
  time-series record.

The report is the document that handoff should have had: small enough to POST,
closed enough to bind to, versioned by the same `protocolVersion` as every other
v2 surface, and pointing at the `sessionId` it was synthesized from so the full
detail stays one directory away.

## Three rules

Everything below follows from three properties, and every clause the validators
enforce exists to keep one of them true.

### 1. Absent is not zero

A fact the run **could not measure** is OMITTED. A zero means measured-zero,
everywhere in this document. It is the same rule the session's runner
environment already follows, and it matters most exactly where a consumer is
tempted to fill in a default:

| Omitted | Reads as | A zero would have claimed |
|---------|----------|---------------------------|
| `cache` | no remote cache participated | the cache was asked and answered nothing |
| `run.cpu` | nothing was spawned, or no runner environment was captured | the run burned no CPU |
| `coverage` | the command collected none | 0% covered |
| `cpuMs` | the platform measured no CPU for this task | it ran and cost nothing |
| `git.dirty` | the producer did not determine it | the worktree matched `sha` |

The corollary is that the report never *forces* instrumentation. It displays
what the run collected; a command that measures no coverage produces a report
without coverage, not a report claiming a regression to zero.

### 2. A projection, never an input

The report is derived from the run's own records **after** the run settled.
Nothing in it influences pass/fail, and nothing in it enters a cache key. A
report that failed to be written is a missing file, never a failed run — and two
runs that differ only in their reports are, by construction, the same run.

`endTime` is required for the same reason: a report is written from a *settled*
run, so an unfinished one has no report rather than a report a consumer would
file with an open interval.

### 3. Bounded by contract

The bounds are **contract clauses**, not producer preferences. A consumer that
accepts this document accepts a stated worst case, so the ceilings are in the
schema (`maxItems`, `maxLength`), enforced by both validators, and exported as
constants both runtimes and the schema are pinned against.

| Bound | Value | Constant |
|-------|-------|----------|
| jobs per report | 64 | `ReportMaxJobs` / `REPORT_MAX_JOBS` |
| diagnostics per job | 16 | `ReportMaxJobDiagnostics` / `REPORT_MAX_JOB_DIAGNOSTICS` |
| diagnostic message | 1024 UTF-8 **bytes** | `ReportMaxMessageBytes` / `REPORT_MAX_MESSAGE_BYTES` |

The numbers are sized on measurement, not taste — 21 recorded `lint,test,build`
sessions of this workspace: 745 selected tasks per run; 121 diagnostic-bearing
task records with a median of 26 diagnostics and a worst case of 139; diagnostic
messages of 31 bytes at p50, 108 at p99 and 8215 at the worst (1 message in
3813 over the cap). Projected over a real gate session, the bounds yield a 26 KB
report carrying 64 jobs and 62 diagnostics.

**The message bound is in bytes** because that is what a size budget is spent
in. The schema's `maxLength` counts code points and is therefore the *weaker* of
the two checks: a document the validators accept always satisfies the schema.
A producer truncates on a rune boundary, so the message stays valid UTF-8, and
the untruncated text remains in the session's own task record.

**What the ceilings drop is counted**, never silently lost:

Within a job, the shared `TestFailureDetailsTruncatedCode` /
`TEST_FAILURE_DETAILS_TRUNCATED_CODE` accounting marker is selected first,
followed by errors, warnings, and informational diagnostics; document order is
stable within each class. The marker therefore survives even when a preceding
coverage error and the bounded failure list together exceed the cap.

- `elidedJobs` counts the selected tasks the job list left out, so
  `len(jobs) + elidedJobs` equals `run.counts.total` — a reader can always tell
  a small run from a truncated one (`cli.result.count_mismatch` on `elidedJobs`
  otherwise).
- Nothing is elided until the list is **full**. Otherwise a producer could ship
  one job, call the other 744 elided, satisfy the accounting rule, and carry
  none of the information the bound was sized to allow
  (`cli.result.count_mismatch` on `jobs`).
- `jobs[].failureDetailsTruncated` states the producer-side omissions for that
  job. `truncatedCount` adds any diagnostics the report reducer dropped. A short
  list is valid only when the two counts are equal; if `truncatedCount` is
  larger, the diagnostics list must be full. A producer count cannot exceed or
  appear without the total. This preserves the honest-truncation clause while
  allowing a producer to bound details before report reduction.
- `commands[].errors` counts every real reported error plus every producer-side
  omitted failure detail. The synthetic accounting marker is informational and
  is not another error. `.warnings` counts every reported warning.
  Reducer-side drops never lower either total.
- `commands[].tests.failureDetailsTruncated`, when present, sums the producer
  omission count across the command. It is optional, so older reports remain
  readable without a protocol-version change.

When a run has more tasks than the budget allows, jobs are selected in this
order: **failed**, then **diagnostic-bearing**, then **coverage-bearing**, then
**longest wall**. A report therefore elides successes before failures, which is
also why `run` carries no `failures[]` — see below.

## The document

| Member | Presence | What it is |
|--------|----------|------------|
| `protocolVersion` | required | `2`, as on every v2 document |
| `sessionId` | required | the session this report synthesizes |
| `startTime` / `endTime` | required | RFC 3339; the run is settled |
| `origin` | required | `cli` or `mcp` |
| `enforceCoverage` | required | whether this run ran the enforcing cadence |
| `git` | optional | `reportGit` — absent outside a worktree |
| `run` | required | `reportRun` — the verdict |
| `commands[]` | required | `reportCommand` — the per-command synthesis |
| `jobs[]` | required | `reportJob` — bounded, at most 64 |
| `elidedJobs` | required | how many selected tasks the bound left out |
| `cache` | optional | `reportCache` — remote-cache economics |
| `scheduler` | optional | `reportScheduler` — parallelism and critical path |

The document is **closed** (`additionalProperties: false`), like every other v2
document.

### `run` — the verdict, without the tail

`reportRun` is deliberately **not** `runSummary`. That shape carries
`failures[]`, whose length equals `counts.failed` and whose members each embed a
full typed identity, an error and that task's diagnostics — an **unbounded**
member, and the report's whole premise is that its worst case is stated up
front. The failed tasks are not lost: they sort first into `jobs[]`.

Everything else is the run summary's rules verbatim, on deliberately identical
member names, so the two documents cannot start counting differently: the
histogram arithmetic (`counts` adds up, `reuse` never exceeds `counts.total`),
the exit-code agreement (0 / 130 / anything else), and the CPU budget tie
(`allocatedMs` is `allocatedMillicores` over the *same* `durationMs`). There is
no `abortedBy`: an abort **source** is a run-ledger fact, and the report keeps
the verdict.

### `commands[]` — commands, not phases

The vocabulary is **commands**. A task names the root command it belongs to
(`taskRef.command`), so this aggregation is a projection of facts the run already
carries — and "phase" is taken in this contract by `sessionPreparation`'s
ownership classes, which decompose something else entirely.

Two rules make the list a partition rather than a pile of rows:

- Each command appears **at most once** — it is a histogram over root commands.
- The per-command totals **sum to `run.counts.total`**, because every selected
  task belongs to exactly one root command. A sum that disagrees means the
  synthesis is describing a different task set than its own verdict does.

`freshWallMs` sums only the tasks that actually **executed**. A cache hit spends
no wall, and counting the duration it replayed would make a fully cached command
look as expensive as the run that populated the cache.

`cpuMs` is the sum of the **fresh** tasks' CPU shares: each execution's measured
CPU divided among the task records that reference it, so a batched dispatch is
not multiplied by its fan-out. Summed over commands it is **≤
`run.cpu.actualMs`** and may legitimately be less — a superseded retry attempt is
real cost belonging to the run's ledger and to no command. It is absent when no
execution of that command was measured.

**Per-command cache byte traffic is intentionally absent.** Bytes are stated
once, at run level: a per-command split would have to attribute shared blobs, and
any split of a deduplicated transfer is a fiction.

### `jobs[]` — the flattened identity

A job carries the three strings a consumer joins on — `key`, `project`, `task` —
plus the `command` that ties it to its `commands[]` entry. The full
`taskIdentity` (display name, provider, kind, step) stays in the session:
repeating it 64 times would spend the report's whole budget on identity a reader
can look up.

`key` remains a **derived** view, exactly `project + ":" + task` — the same rule
`taskIdentity` carries — so the report joins to the session's task records on a
value neither side can spell differently (`cli.result.invalid_key` otherwise).

### `cache` and `scheduler` — typed subsets

Both are deliberately narrow, and that is the point. The session file's `cache`
and `scheduler` members are free-shape (`any`): a consumer binding to them binds
to whatever the scheduler happened to serialize that release, which is exactly
what a contracted document must not repeat. The report states a **typed subset**
instead — the counters that answer "did the cache pay off" and the two scheduler
facts (`parallelism`, `criticalPathMs`) a budget consumer reads.

`restored` never exceeds `hits`: a key the cache did not hold cannot have been
materialized, so the reverse means two different accountings got mixed.

### `git` — report-owned

`reportGit` does **not** extend the session's git block. The session records what
**selection** was computed from (a branch and a baseline, both optional); a
report is a durable fact a consumer files against a commit, so `sha` is
**required** here — and required to be a full object id (40 hex for a sha1
repository, 64 for a sha256 one). A symbolic reference can move, and two reports
filed under `HEAD` describe different commits.

## Validation

`ValidateDocument(DocumentReportFile, bytes)` and
`validateDocument('reportFile', text)` return the same `{code, path}` violations
from the vocabulary in [`02-result-v2.md`](./02-result-v2.md#validation-and-the-cross-language-corpus).
The report adds no code to that closed set; its clauses map onto the existing
ones:

| Clause | Code | Path |
|--------|------|------|
| jobs past the cap | `cli.result.invalid_value` | `jobs` |
| diagnostics past the cap | `cli.result.invalid_value` | `jobs[i].diagnostics` |
| message past the cap | `cli.result.invalid_value` | `jobs[i].diagnostics[j].message` |
| `elidedJobs` does not account for the run | `cli.result.count_mismatch` | `elidedJobs` |
| eliding before the list is full | `cli.result.count_mismatch` | `jobs` |
| report-side diagnostic omissions on a short list | `cli.result.count_mismatch` | `jobs[i].truncatedCount` |
| producer omissions above or without the total | `cli.result.count_mismatch` | `jobs[i].truncatedCount` |
| a repeated command | `cli.result.count_mismatch` | `commands[i].command` |
| per-command totals do not sum to the run | `cli.result.count_mismatch` | `commands` |
| per-command CPU above the run's actual | `cli.result.invalid_value` | `commands` |
| test counters do not add up | `cli.result.count_mismatch` | `commands[i].tests.total` |
| covered above total | `cli.result.count_mismatch` | `…coverage.covered` |
| a percentage outside [0,100] | `cli.result.invalid_value` | `…coverage.percentage` |
| restores above hits | `cli.result.count_mismatch` | `cache.restored` |
| a job key that is not derived | `cli.result.invalid_key` | `jobs[i].key` |
| `git.sha` that is not a full object id | `cli.result.invalid_value` | `git.sha` |

Most of those have an accepted and a rejected fixture in the corpus. The **job
cap** (65 near-identical rows a reviewer learns nothing from), the rule that
jobs are not elided before that list is full, and the per-job honest-truncation
arithmetic are exercised by generated documents in both runtimes instead
(`result_v2_report_test.go`, `test/result-v2.test.ts`); the diagnostics-count
and message-length caps have corpus fixtures. The caps themselves are pinned
against the schema's `maxItems`/`maxLength` on both sides, which is what ties the
two runtimes' constants together.

## What this document does NOT cover

- **Who writes the report, and when.** This document defines the contract only;
  the producer and the `putnami report` reader are separate work.
- **Retention.** How long reports and sessions are kept is policy, not contract.
- **Ingestion.** The cloud endpoint that collects reports is cloud-side work.
- **The optional members' population.** A producer may leave `cache`,
  `scheduler`, `coverage`, `tests` and `cpuMs` absent from day one; later slices
  fill them in without the contract changing, which is the point of landing the
  whole shape at once.
