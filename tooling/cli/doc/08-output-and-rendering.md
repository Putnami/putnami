# Output and Rendering

The CLI provides multiple output formats for different environments: interactive development, CI pipelines, and cloud infrastructure. Output rendering is separated from job execution, allowing the same scheduling engine to produce different outputs.

## Renderer Architecture

All renderers implement the same interface:

```go
type Renderer interface {
    Start(planned []*ScheduledJob)
    JobStart(job *ScheduledJob)
    JobEvent(job *ScheduledJob, event RawJobEvent)
    JobComplete(job *ScheduledJob, result *JobResult)
    Finish(results map[string]*JobResult)
}
```

The scheduler calls these methods at each lifecycle point. Renderers are stateless from the scheduler's perspective — they consume events and produce output.

## Renderer Selection

### Auto-Detection

When no `--output` flag is provided, the renderer is selected based on environment:

| Condition | Renderer |
|-----------|----------|
| `K_SERVICE` env var set | Cloud Logging (Google Cloud Run) |
| `--quiet` flag | Text (quiet mode) |
| `--debug` flag | Text (debug mode, all events) |
| `--verbose` flag | Text (verbose mode) |
| TTY with live support | Live (project table, progress bar, colors) |
| Otherwise | Text or JSONL |

### Explicit Selection

```bash
putnami build --output=jsonl
putnami build --output=cloud-logging
```

### Environment Variables

```bash
PUTNAMI_OUTPUT=jsonl putnami build --all
```

## Installation progress

`putnami install` and the automatic first-use installation use compact progress
by default. On a supported terminal the current phase updates in place. When
the run ends, temporary progress is replaced by a concise list of completed
actions. Checks of artifacts already present do not add permanent lines, and
successful workspace-installer transcripts stay hidden.

The summary distinguishes restored artifacts from checks that changed nothing.
A workspace installer that reports only successful execution is summarized as
an executed reconciliation action; success alone does not prove that it added
dependencies. Warnings and failure diagnostics remain visible. `--verbose`
retains detailed phase and job output; `--quiet` suppresses normal progress.

Outside a supported terminal, installation prints its final action report
without cursor movement or animation. An implicit installation before a JSON,
JSONL or Cloud Logging command sends human progress to stderr so stdout remains
the requested machine stream. MCP preparation never writes human text to its
JSON-RPC stdout.

## Live Renderer

The live renderer provides a calm, **project-centric** terminal UI: one stable
row per project, redrawn in place. It activates automatically in TTY
environments that support ANSI escape codes, and is disabled in CI environments,
dumb terminals, and when `NO_COLOR` is set.

The live zone (the counts header with an inline progress bar, the project table,
an overflow footer, and a failure pane) is repainted by **differential redraw**: each tick
builds the whole frame, diffs it against the previous one, and rewrites only the
lines that changed — in a single write that overwrites in place rather than
clearing first. Because completed rows are never repainted, the view does not
flicker, which lets the redraw run at ~80ms and animate a smooth braille spinner
on running rows. (Serve mode also tails log lines to permanent scrollback above the
table; see [09-watch-mode.md](09-watch-mode.md).) Per-task / per-step detail is
reserved for `--verbose` and `--debug` (the Text renderer).

When the live project table is taller than the terminal, overflow is selected by status instead of by raw project order. Failed, running, blocked, and queued rows stay visible ahead of old completed rows; a visible row that just reached `done` is retained for 3 seconds before it expires from overflow selection. The selected rows are then rendered back in project order and the footer reports only hidden unfinished or failed projects, plus recently completed rows still inside that retention window. This keeps the remaining work visible near the end of large cached runs without making old completed projects inflate the "more" count.

The project-name column uses the detected terminal width instead of a fixed cap. Full project names are kept when the row fits; names are truncated only after preserving the status and summary columns that fit in the available space.

The header includes session elapsed time from the start of the run. The elapsed column on each project row tracks active project work only. Time spent queued or blocked by upstream project dependencies is excluded, and overlapping jobs for the same project are counted once.

### Layout Overview

Top to bottom: a one-line **header** (counts + an inline progress bar), the
**project table** (one row each), an **overflow footer** when rows exceed the
terminal height, and a **failure pane** once anything fails. The zone is kept a
couple of lines shorter than the terminal so the header stays on screen.

```
impacted: 8 projects · elapsed: 01:20 · running: 2 · done: 4 · failed: 2  ██████░░░ ~25s
  ✓ @putnami/utils   done    00:03  lint ✓ ok · test ✓ 162/162 · cov 71.4%
  ✗ core-api         test    00:11  lint ✓ ok · test ✗ 2 failed
  ⠹ billing          build   00:06  lint ✓ ok · test ✓ 88/88 · build 4/9
  ⠹ go.putnami.dev   build   00:05  build 1/6
  … 2 more
  ── failures (2 total) ─────────────────────────────
  ✗ core-api  test   2 assertions failed
  ✗ web-app   lint   no-unused-vars app.ts:88
```

### Header

A single dim line of session counts, with a compact global progress bar appended
on the right:

```
impacted: N projects · elapsed: MM:SS · running: R · done: D · blocked: B · failed: F  ██████░░░ ~ETA
```

`queued` and `skipped` counts are appended only when non-zero. `elapsed` is the
wall-clock time since the run started. The inline bar shows a short weighted bar
and the ETA (no percentage); it shrinks and then drops entirely on a terminal
too narrow to fit it, rather than wrapping the header.

### Global Progress Bar

One bar for the whole run, not per project — rendered inline on the header (above).

Each task is weighted by its **expected wall-clock duration** — the EMA the
scheduler learns in `.putnami/stats` and resolves onto every planned job (the
same history that drives CPU-budget weighting). A long build therefore advances
the bar more than a quick lint. An in-flight task with history earns **partial
credit** as its expected duration elapses (capped just under its full weight),
so the bar creeps through a long pole instead of stalling, and cache hits earn
their full weight immediately so cached runs race to 100%.

The **ETA** is the larger of two terms — remaining work at the realized
completion rate (counting only *finished* work, so in-flight progress can't make
it look faster than tasks actually complete) and the single longest remaining
task (the run can't end before its longest job does). It is then clamped to a
finish deadline that **only ever moves earlier**, so the countdown ticks down and
never bounces back up; the total task set is fixed at `Start`, so over the run we
only ever learn we are going faster. It is rounded to coarse buckets so it reads
steadily. When the stats store is cold (no history), every task weighs the same
and the bar degrades gracefully to a plain task-count fraction.

### Project Rows

One row per project, in stable plan order:

```
  {icon} {name} {status|command} {elapsed}  {summary cells}
```

- **icon** — status glyph (see Icons and Colors). Running rows show a spinning braille glyph.
- **name** — project name, colored; truncated only when the row is tight.
- **status/command** — while running, the active command name (e.g. `test`);
  otherwise the status word (`done`, `failed`, `blocked`, …). A `blocked` row
  shows `blocked by {project}` in place of summary cells.
- **elapsed** — `MM:SS` of **active project work only**. Time spent queued or
  blocked on upstream dependencies is excluded, and overlapping jobs for the
  same project are counted once. `--:--` until work starts.

**Summary cells** — one fixed-width cell per command, aligned across rows:

```
  lint ✓ ok · test ✓ 42/45 · cov 84.0% · build ✓ 3 files
```

Each cell shows the command label, a status glyph (`✓` fresh success, dim `↻`
cached, `✗` failed) or the dim `coalesced` outcome, and a compact value: lint
warnings/errors, `passed/total` tests,
coverage percent (its own `cov` column, inserted after `test`), files built. A
cached cell keeps its restored value (for example `test ↻ 42/45`); the distinct
text glyph preserves cache provenance when terminal output is copied without
ANSI styling. When every cacheable step is restored but a sub-second always-run
tail still executes, the cell stays dimmed and identifies that tail (for example
`build ↻ 38 files (+infra 10ms)`). A running cell shows live progress (`4/9`),
the current phase, or `…`. When the terminal is too narrow, lower-priority
cells are hidden while the remaining cells stay in canonical order. `package`
is hidden first, then
secondary detail such as `cov`, then `lint`, `test`, `build`, `publish`, and
finally `deploy`, with a dim `+N` marking how many cells are hidden.

### Overflow & Done-Row Retention

When the table is taller than the terminal, rows are selected by **status**
rather than raw order: failed, running, blocked, and queued rows stay visible
ahead of old completed rows. A row that just reached `done` is retained for 3
seconds before it can be dropped, so a freshly finished project doesn't vanish
instantly. Selected rows are rendered back in project order, and the footer

```
  … N more
```

counts only the hidden rows that still matter — unfinished, failed, or within
their done-retention window — so a long tail of old completed projects never
inflates it.

### Failure Pane

Once any command fails, a pinned pane is drawn at the bottom of the live zone:

```
  ── failures (6 total) ─────────────────────
  ✗ web-app   lint   no-unused-vars app.ts:88
  ✗ core-api  test   2 assertions failed
  ✗ @putnami/sdk build TS2345 src/index.ts:5
```

It rolls: the most-recent failures are shown (up to 5), older ones scroll out,
and the header reports the running total. Each line prefers the first error
diagnostic (`file:line code message`), falling back to a summarized cause. This
surfaces what just broke during a `--continue-on-error` run; the full
diagnostics still print in the end-of-run `Failures:` block.

### Error Details

Printed after the run completes, only for real failures (not cancellations).
In the final recap, the `Failures:` block is printed before any publish recap so
errors remain easy to find in long `--continue-on-error` runs. Failure details
prefer structured diagnostics, then the job error, and fall back to `error` or
`warn` log events when a task failed without emitting diagnostics.
Diagnostic and artifact file paths are normalized to workspace-relative paths
before text, JSONL, or cloud-logging renderers print them.

Each failed command prints a `{project} · {command} · {cause}` line followed by
its capped diagnostic/error detail, then a hint:

```
Failures:
@putnami/sdk · build · transpile failed
    src/index.ts:5:3 error[TS2345] Argument of type 'string' is not assignable...
core-api · test · 2 failing tests
    assertion failed: expected 200, got 500
Hint: re-run with --output=jsonl for structured diagnostics and a complete session artifact.
```

Canceled work (a run aborted mid-flight) is reported as skipped, with reasons in
a separate `Skipped:` block rather than as failures.

### Publish Recap

Publish runs end with a compact recap rather than one line per artifact:

```
Published:
Version: 0.1.0-68c82733
Go module version: v0.1.0-68c82733
Artifacts: 56 artifacts · archives 12 · docker 1 · go 32 · npm 11
Tags: latest (56) · canary (23) · 68c82733 (23)
```

When every artifact uses the same exact version, only `Version:` is shown. Mixed
Go and non-Go publishes normalize the shared version for copy/paste and include
the Go module version on the next line.

### Final Summary

When the run ends, the live zone is frozen to the final project table and a
block summary is printed:

```
Session completed in 4m12s

Projects:
8 impacted · 4 succeeded · 2 failed · 2 skipped

Tasks:
lint   12 passed · 0 failed · 0 errors · 14 warnings
test   8 passed · 1 failed · 2,418 tests · coverage 82.0%
build  6 passed · 0 failed · 300 files built · 1 artifacts

Failures:
...
```

`Projects:` counts projects; `Tasks:` rolls up per-command metrics across all
projects. `Failures:` (above) and, for publish runs, the `Published:` recap
follow.

#### Coverage on the test line

The `coverage` part shows what the run **measured**. Only the validation cadence
(`--enforce-coverage`) instruments, so an ordinary `putnami test` measures none —
and instead of dropping the figure entirely, the summary replays the last
recorded validation run's, marked as such:

```
test   8 passed · 0 failed · 2,418 tests · coverage 93.3% (validation @ abcdef1)
```

The marker is the honesty: the number describes an earlier run, at the commit
named after `@`, over *that* run's project set — not this one's. A run that
measured its own coverage always shows that instead, with no marker. The figure
is read from the recorded validation sessions (see
[session recording](11-session-recording.md)); a worktree that has never run
the validation cadence simply shows no coverage part. It never gates, never
changes a verdict, and never turns instrumentation on.

### Icons and Colors

| Icon | Color | Meaning |
|------|-------|---------|
| `⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏` | project color | Running (braille spinner) |
| `✓` | Green | Success |
| `✗` | Red | Failed |
| `✓` | Dim | Cached (fully or partially) |
| `coalesced` | Dim | Cold miss computed by a sibling process |
| `·` | Dim | Skipped |
| `○` | Dim | Queued / blocked |

Projects get a rotating color from: Cyan, Magenta, Yellow, Blue, Green, Red. Each
running row spins a braille glyph in its project color (~80ms per frame); queued
and blocked rows stay static. The inline progress bar is deliberately neutral —
the filled segment uses the default foreground and the remainder is dim, so it
informs without competing with the spinners or status colors. The header counts,
summary cell labels, elapsed times, the overflow footer, and ETA are dim.

### Color Control

```bash
putnami build --color         # Force colors
putnami build --no-color      # Disable colors
NO_COLOR=1 putnami build      # Standard convention
PUTNAMI_NO_COLOR=true putnami build
```

Color resolution order: CLI flag → `PUTNAMI_COLOR` → `NO_COLOR` → auto-detect.

## Text Renderer

The text renderer produces plain, human-readable output suitable for terminals and log files.

### Default Mode

Shows job names and results with a final recap:

```
  my-lib build        done   0.8s
  my-app build        done   1.2s

  2 succeeded  0 cached  1.2s
```

### Verbose Mode (`--verbose`)

Uses the same row grammar as the live renderer: icon, project, command, duration/placeholder, then the step or event detail. Unlike the live renderer, verbose output is permanent scrollback and prints every phase start, info/warn/error log message, diagnostic, and summary. This is useful for seeing what a job actually produced (e.g., raw test runner output):

```
  ● @putnami/utils test    --:--   generate starting
  ● @putnami/utils test    --:--   [generate] start
  ✓ @putnami/utils test    240ms   generate done
  ● @putnami/utils test    --:--   test starting
  ● @putnami/utils test    --:--   [discover] start
  ● @putnami/utils test    --:--   INF Found 9 test files
  ● @putnami/utils test    --:--   [run] start
  ● @putnami/utils test    --:--   INF bun test v1.3.13
  ● @putnami/utils test    --:--   INF 162 pass
  ● @putnami/utils test    --:--   INF 0 fail
  ● @putnami/utils test    --:--   [parse] start
  ✓ @putnami/utils test    226ms   test done
  ● @putnami/utils test            162 tests, 39% coverage

  2 succeeded  467ms
```

### Debug Mode (`--debug`)

Streams all events in real-time — phases, logs, progress, metrics:

```
  [debug] my-lib:build~generate starting
  [debug] my-lib:build~generate phase:compile start
  [debug] my-lib:build~generate log:info compiling 12 files
  [debug] my-lib:build~generate phase:compile end success
  [debug] my-lib:build~generate done 0.3s
```

### Quiet Mode (`--quiet`)

Suppresses all non-essential output. Only errors and the final summary are shown.

## Machine Output (`--output=json` and `--output=jsonl`)

Both machine formats speak the **version-2 machine contract**
([`protocols/cli/doc/02-result-v2.md`](../../../protocols/cli/doc/02-result-v2.md)).
Every document carries `protocolVersion: 2`; a document without the member is
version 1 and can only come from a CLI build published before the v1 emitters
were removed. There is no way to ask a current build for version 1 —
`PUTNAMI_MACHINE_OUTPUT` is inert and warns once on stderr when it is set to
`v1`; the rollback is to pin an older published build.

`--output=json` writes exactly one aggregated envelope when the run ends;
`--output=jsonl` streams a bounded selection of records as tasks execute. Both
are reduced from the same run, so outcome, counts, reuse, duration, CPU, and
exit code cannot disagree. JSONL's bounded terminal summary deliberately omits
the potentially unbounded `failures[]` and `publications[]` arrays; their
complete sanitized task records remain in the recorded session artifact.

### `--output=json`

```json
{
  "protocolVersion": 2,
  "command": "build",
  "status": "success",
  "run": {
    "outcome": "success",
    "exitCode": 0,
    "counts": { "total": 8, "succeeded": 8, "failed": 0, "canceled": 0, "skipped": 0 },
    "reuse": { "localCache": 2, "remoteCache": 0, "coalesced": 0 },
    "durationMs": 5000
  },
  "exitCode": 0
}
```

`status` is `success | failure | aborted`. An interrupted run reports `aborted`
with `exitCode: 130`, and the failures it did collect stay visible in
`run.counts.failed` and `run.failures[]`. `run.counts` spans every selected task,
reuse included — there is no `cached` bucket, because reuse is counted
separately in `run.reuse` — so a task whose FAILED result came from the cache
fails the run.

### Docker publish facts

`putnami publish --docker` adds `run.publications[]` when a workload completed
with a verified registry digest. Each item names the publish task and resolved
release session, then reports `registry`, `image`, optional `tags`, and an
immutable `imageDigest` (`sha256:<64 lowercase hex>`). A mutable tag is never
used as a fallback digest.

`timings` contains the package-task `buildMs` plus registry-facing
`cacheLookupMs`, `cacheTransferMs`, `registryPushMs`, `referencePublishMs`, and
`digestResolveMs`. `cacheOutcome` distinguishes `hit` from `miss`; a warm
registry reuse has `contentStatus: "retagged"` and `digestReused: true`, while a
cold content transfer has `contentStatus: "pushed"` and `digestReused: false`.
`concurrency.configuredCap` is the explicit `--max-parallel` value (or the
resolved scheduler limit for a mode), and `concurrency.effective` is the actual
maximum overlap of live `docker-push` phases. It can be zero when no
registry-facing phase ran.

The JSONL stream exposes admitted producer facts immediately as typed
`task:event` fields, and retains every sanitized fact in the session artifact.
The aggregated JSON envelope carries the joined package timing and run-level
concurrency in `run.publications[]`. Dry runs, local-only publishes, and records
without a verified immutable digest are omitted rather than emitting an
unverifiable reference.

### `--output=jsonl`

```jsonl
{"protocolVersion":2,"record":"task:start","time":"2025-03-02T14:15:30Z","identity":{"key":"/packages/my-lib:build~generate","scope":"project","project":{"id":"/packages/my-lib","name":"my-lib"},"task":{"name":"build~generate","command":"build","step":"generate","kind":"ts-generate"},"provider":{"extension":"@putnami/typescript"}}}
{"protocolVersion":2,"record":"task:event","time":"2025-03-02T14:15:30Z","identity":{"…":"as above"},"event":{"type":"phase","name":"compile","action":"start","time":"2025-03-02T14:15:30Z"}}
{"protocolVersion":2,"record":"task:event","time":"2025-03-02T14:15:31Z","identity":{"…":"as above"},"event":{"type":"diagnostic","severity":"warning","message":"unused import","location":{"file":"packages/my-lib/lib.ts","line":5,"column":1},"time":"2025-03-02T14:15:31Z"}}
{"protocolVersion":2,"record":"task:end","time":"2025-03-02T14:15:31Z","identity":{"…":"as above"},"task":{"identity":{"…":"as above"},"status":"success","reuse":"none","exitCode":0,"durationMs":800,"taskWallMs":825}}
{"protocolVersion":2,"record":"session:end","time":"2025-03-02T14:15:35Z","run":{"outcome":"success","exitCode":0,"counts":{"total":8,"succeeded":8,"failed":0,"canceled":0,"skipped":0},"reuse":{"localCache":2,"remoteCache":0,"coalesced":0},"durationMs":5000},"machineOutput":{"mode":"normal","sanitization":"terminal-safe-redacted-v1","budget":{"maxBytes":1048576,"maxRecords":1024,"failureReserveBytes":262144,"failureReserveRecords":256,"finalReserveBytes":16384,"finalReserveRecords":1},"elided":{"ordinary":{"records":0,"bytes":0},"failure":{"records":0,"bytes":0}},"artifact":{"sessionId":"20250302-141530-a1b2c3","path":"events.jsonl","retention":"session"}}}
```

The live stream has fixed hard partitions: normal mode allows 1 MiB / 1024
records with 256 KiB / 256 records reserved for failure evidence; `--verbose`
allows 8 MiB / 8192 records with a 2 MiB / 2048-record failure reserve. Both
reserve one 16 KiB terminal record. Normal mode suppresses debug-level task
detail, including per-test success transcripts, while `--verbose` admits it
under the larger budget. Records are sanitized, compacted, and then selected
whole in arrival order; ordinary traffic cannot consume the failure reserve.
`machineOutput.elided` reports exact omitted record and byte counts by class,
including normal-mode debug omissions. The mode never changes execution,
verdict, or exit status. The complete sanitized sequence is incrementally
retained at the named session-relative `events.jsonl` path and pruned with that
session.
Independently, each subprocess result retains at most the normal profile's
partitioned event detail in memory. Every valid event reaches the renderer and
artifact before that retention decision, while result extraction and the
runtime `meta` handshake are tracked separately; bounding detail therefore
cannot change output materialization, caching eligibility, or the verdict.

### Record fields

**`task:start`**: `identity`

**`task:event`**: `identity`, `event` — the subprocess runtime event, whose
vocabulary `protocols/runtime` owns. Its timestamp is the PRODUCER's, not the
moment the CLI forwarded it.

The `event` object is the producer's line **normalized**, not copied byte for
byte: a nested `data` payload is flattened onto the object (so `severity`,
`name`, `current`, `value`, … are always top-level), and the producer's `v`
stamp is dropped because the record's own `protocolVersion` and the contract-3
pin already fix the version — a CLI that requires extension contract 3 accepts
exactly one runtime-event version, so restating it per line would add a second
place for the two to disagree. Unknown event types travel through untouched, so
a reader must not assume a closed `type` set.

A test result's `data.testCases` member is removed from its `task:event`, and
from each member of a batch result's `data.batchResults`: the cases travel in
`test:case` records instead, so the stream carries each case once.

**`test:case`**: `identity`, `testCase` — one test case the task ran: `name`,
`suite`, `status` (`passed`, `failed`, or `skipped`), `durationMs`, and, when
known, `output` with `outputTruncated`, `file`, and `line`. A task's cases come
in runner order, immediately before its `task:end`, for executed, batched, and
cache-replayed tasks alike. A task reports at most 1,000 cases and at most
1 MiB of encoded cases, and a batch's members share 8 MiB: failed first, then
skipped, then passed. Normal mode keeps every case in `events.jsonl` only,
since the failed task's `task:end` and error diagnostics already carry the
failure live; `--verbose` admits all of them. The contract is in `protocols/cli/doc/02-result-v2.md`.

**`task:end`**: `identity`, `task` (status, reuse, exit code, timings,
diagnostics, `error` when it did not succeed, and `testCasesDropped` when the
task ran cases that no `test:case` record carries)

**`session:end`**: bounded `run` verdict plus `machineOutput` budget, exact
elision, sanitizer version, and complete-artifact reference. The general JSON
envelope remains the surface for aggregate `failures`, `publications`, and
`cache`; JSONL keeps their complete per-task evidence in `events.jsonl`.

`identity` is the typed task identity: `key` (exactly
`project.id + ":" + task.name`), `scope`, `project`, `task`, `provider`. Each
record variant pins an exact member set, so a reader can switch on `record` alone
and know what is present.

### Usage in CI

JSONL output is ideal for CI pipelines:

```bash
# Stream events for programmatic consumption
putnami build --all --output=jsonl | process-events.sh

# --verbose selects the larger bounded live profile; the artifact is complete
putnami test --all --no-cache --output=jsonl --verbose
```

## Cloud Logging Renderer

Produces Google Cloud Logging structured JSON, auto-detected when running on Cloud Run (`K_SERVICE` env var).

### Format

```json
{"severity":"INFO","message":"my-lib build started","time":"2025-03-02T14:15:30Z"}
{"severity":"INFO","message":"my-lib build succeeded (0.8s, cached)","time":"2025-03-02T14:15:31Z"}
{"severity":"ERROR","message":"my-app test failed: 2 assertions failed","time":"2025-03-02T14:15:35Z"}
```

### Severity Mapping

| Job Event | Severity |
|-----------|----------|
| Job start | `INFO` |
| Job success | `INFO` |
| Job cached | `INFO` |
| Job coalesced | `INFO` |
| Job failed | `ERROR` |
| Job skipped | `WARNING` |
| Diagnostic (warn) | `WARNING` |
| Diagnostic (error) | `ERROR` |

## Job Event Types

Events emitted by job subprocesses via the JSONL protocol:

| Type | Fields | Description |
|------|--------|-------------|
| `log` | `level`, `message` | Freeform log message (`info`, `warn`, `error`) |
| `progress` | `current`, `total`, `label` | Progress reporting (drives progress bars) |
| `phase` | `name`, `status` | Lifecycle phase (`start`, `end`) with optional result |
| `metric` | `name`, `value`, `unit` | Measurable value (e.g., binary size, test count) |
| `diagnostic` | `severity`, `message`, `file`, `line`, `column`, `code` | Structured warning or error with source location |
| `artifact` | `id`, `name`, `kind`, `path` | Output artifact reference |
| `summary` | `message` | Human-readable summary for job recap |
| `result` | `status`, `data`, `error` | Final job result (always the last event) |

## Watch Mode Output

Watch mode wraps the base renderer with status information:

```
  lib build done  0.8s
  app build done  1.2s

  2 succeeded  0 cached  1.2s

  [watch] waiting for changes...  (1.2s)

  [watch] change detected — re-running
    src/lib.ts
  [watch] affected: lib, app
```

See [09-watch-mode.md](09-watch-mode.md) for full details.
