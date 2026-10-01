# Output Contract

Every Putnami workload communicates through well-defined output channels. This document describes what outputs a workload produces and how they are consumed.

## Output Channels

| Channel | Format | Purpose |
|---------|--------|---------|
| `stdout` | JSONL | Structured events for the orchestrator |
| `stderr` | Text | Human-readable output, debug info, crash traces |

The orchestrator reads `stdout` line by line, parsing each line as a JSONL event. Lines that don't parse are silently ignored. All human-readable output (progress bars, colored text) goes to `stderr`.

## Output Modes

The CLI supports three output modes, selectable via `--output` or the `PUTNAMI_OUTPUT` environment variable:

| Mode | Description |
|------|-------------|
| `jsonl` | Streams raw JSONL events as jobs emit them. One event per line. |
| `text` | Renders human-readable output with progress bars, colors, and formatting. |
| `cloud-logging` | Google Cloud Logging structured JSON with `severity` + `message` fields. |

Auto-detection: TTY → `text`, `K_SERVICE` env var → `cloud-logging`, otherwise → `jsonl`.

## Output Categories

### Logs

Freeform log messages emitted via `log` events. Every workload should emit logs at appropriate levels:

- `debug` — Detailed internal state (only shown with `--debug`)
- `info` — Normal operation progress (shown with `--verbose`)
- `warn` — Potential issues that don't prevent completion
- `error` — Failures that affect the job outcome

### Metrics

Named measurements emitted via `metric` events. Metric values must be numeric. Well-known metrics that tools can aggregate:

| Metric | Unit | Description |
|--------|------|-------------|
| `binary-size` | bytes | Size of compiled binary |
| `bundle-size` | bytes | Size of bundled output |
| `compile-time` | ms | Time spent compiling |
| `tests-total` | count | Number of tests executed |
| `tests-passed` | count | Number of tests passed |
| `tests-failed` | count | Number of tests failed |
| `tests-skipped` | count | Number of tests skipped |
| `coverage` | percent | Code coverage percentage |
| `lint-errors` / `lint-warnings` / `lint-infos` | count | Lint findings by severity |

Extensions may emit additional custom metrics.

### Typed per-verb payloads

Verb-specific outcomes also travel as **typed payloads** so consumers can aggregate across languages without re-parsing metrics (formal definition: [`schemas/payloads.json`](../schemas/payloads.json), Go types in `payloads.go`):

- `result.data.testSummary` — `{ total, passed, failed, skipped, failureDetailsTruncated? }`. The optional count records causal failure diagnostics omitted by a bounded producer; the complete transcript travels separately as `debug` log events.
- `result.data.testCases` — one entry per test case the job ran, and `result.data.testCasesDropped` — how many cases it left out, present only when it left some out. The entry shape and its bounds belong to [`go.putnami.dev/protocol/cli`](../../cli/doc/02-result-v2.md#test-case-records) (`TestCase`, bounded with `BoundTestCases`), not to this protocol. The CLI writes each entry as one `test:case` session-stream record before the task's `task:end` and removes `testCases` from the result it copies into the stream. A runtime without structured per-case output omits both members; it never guesses. In a batched run each member's entries travel in its own `batchResults[i].data` ([batched execution](../../extension/doc/02-commands-and-tasks.md)), bounded with `BoundTestCasesWithin` and `TestCaseBatchMemberBytes`, so the cases take at most 8 MiB of the batch's one result line, which the CLI reads whole up to 16 MiB.
- `result.data.coverageSummary` — `{ percentage, granularity, covered?, total?, threshold? }`. Languages measure coverage at different granularities (Go: `statements`, TypeScript: `lines`); the payload declares its granularity instead of pretending the units agree, and consumers must not mix counts across granularities.
- `result.data.lintSummary` — `{ errors, warnings?, infos? }`. Individual findings are emitted as **per-issue diagnostic events** with severity, message, rule code, and location — one aggregate diagnostic of raw tool output is not conformant.
- `result.data.releaseSet` — the sole successful release-set publish outcome.
  Its shape is owned by
  [`go.putnami.dev/protocol/distribution`](../../distribution), not restated by
  this protocol. In a raw runtime event the complete path is
  `event.data.data.releaseSet`: the first `data` is the `ResultData` envelope,
  the second is its typed per-verb map. The v2 outcome carries the stored set's
  `{id, digest}` plus `current`, the head each advanced channel now points at
  with its generation — one release advances several channels, so a consumer
  reads that map rather than a channel name. It is emitted exactly once and only
  after immutable storage plus a `released` or `already-current` answer on
  **every** listed channel; dry-run, partial failure, provider failure, or a CAS
  `conflict` on any one channel must omit it.
- Compiled binaries are emitted as `artifact` events with kind `binary`; published artifacts carry the `PublishRecord` fields (`registry`, `name`, `version`, `dryRun`) as extras on a kind `published` artifact event.
- A publisher reports each artifact it published with a kind `published-member`
  artifact event carrying `ecosystem`, `coordinate`, `version`,
  `artifactDigest`, and optional per-platform digests. The release-set
  coordinator reconciles those events against its plan to build the snapshot it
  releases.
- A publisher that runs under `--dry-run` reports, with a kind `member-probe`
  artifact event per member, whether the registry already holds the member at
  the version it would publish: `ecosystem`, `coordinate`, `version`,
  `registry`, and a `state` of `absent`, `identical`, `conflict` or
  `unverified`. The orchestrator fails the dry run on `conflict` and
  `unverified`. The event is never publication evidence.

**Parsing a typed payload out of an artifact event.** `ParseRawEvent` merges
every top-level runtime-event field into `Data`, so the map a consumer receives
carries the envelope beside the payload. Before decoding a typed payload out of
it — a `published-member`, for instance — drop the envelope keys `kind`, `type`,
`time`, `level`, and `message`; a strict decoder rejects the object otherwise.

### Diagnostics

Structured warnings and errors with source location, emitted via `diagnostic` events. Diagnostics enable IDE integration and structured error reporting.

Every diagnostic has:
- `severity` — `error`, `warning`, `info`, or `hint`
- `message` — Human-readable description
- `code` (optional) — Machine-readable code (e.g., `TS2304`, `lint/no-unused-vars`)
- `location` (optional) — Source file, line, and column

### Artifacts

Named output files emitted via `artifact` events. The orchestrator can cache, display, and pipeline artifacts between jobs.

Common artifact kinds:
- `binary` — Compiled executable
- `bundle` — Bundled JavaScript/CSS
- `package` — Distributable package (tarball, container image)
- `report` — Test or coverage report
- `coverage` — Coverage data file
- `published-member` — One published release-set member: its ecosystem,
  coordinate, version, and verified artifact digest
- `member-probe` — One registry answer of a dry-run publish: whether the
  registry already holds a member at the version the publish would write

### Traces (future)

OpenTelemetry trace integration is reserved for future use. When enabled, workloads will export trace spans to an OTLP endpoint for distributed tracing.

## Session stream (`--output=jsonl`)

The CLI re-publishes job events to external consumers as a **session
stream**, and that stream is **not owned by this protocol**. Each subprocess
runtime event travels inside a `task:event` record whose outer envelope —
`task:start` / `task:event` / `task:end` / `session:end`, stamped
`protocolVersion: 2` — is
[`protocols/cli`](../../cli/doc/02-result-v2.md)'s `SessionStreamRecord`. CI
systems and agents should consume that contract rather than parsing subprocess
stdout directly.

This package used to declare an unversioned envelope of its own (`job:start` /
`job:event` / `job:end` / `session:end`, in `stream.go`). When the CLI's v1
JSONL renderer — its last producer — was removed, the envelope, its
`schemas/stream.json` and its `fixtures/stream/` corpus were deleted with it: a
protocol with no producer and no consumer is a liability, not a compatibility
promise. The decision and the alternatives are recorded in
[`adr/0002-delete-the-producerless-stream-envelopes.md`](adr/0002-delete-the-producerless-stream-envelopes.md).
Consumers still reading those lines must pin a CLI build published before the
removal.

The events this document specifies are unchanged: they are what a `task:event`
record carries in its `event` member.

### Interrupted sessions

A signal (Ctrl-C, or a supervisor's `SIGTERM`) stops the run before its plan
finishes. Work still in flight ends as a `task:end` record with
`status: "canceled"`, and the closing `session:end` reports the run's own
verdict — `aborted` is never `success`, because the cancelled tasks were killed
before they could pass or fail. The process exits `130` for an interrupted run:
a **CI gate must not read an aborted session as a pass**, and must not confuse
it with the `1` of a genuine build failure. The record shapes and the abort
precedence rules are specified in
[`protocols/cli/doc/02-result-v2.md`](../../cli/doc/02-result-v2.md).

## Schema

The formal definition is in [`schemas/output.json`](../schemas/output.json); the
session stream envelope is defined by
[`protocols/cli/schemas/result-v2.json`](../../cli/schemas/result-v2.json).
