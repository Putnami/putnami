# ADR 0026 — Test outcomes are summaries; transcripts are session detail

- **Status**: accepted
- **Scope**: Go and TypeScript test producers, the CLI session recorder and
  report (`protocols/cli` `result-v2`)

## Context

A human needs a concise test answer, an automation client needs stable typed
counts, and a debugging session needs the tool's complete transcript. A large
failing suite must not turn every assertion into an unbounded diagnostic
stream, and Go and TypeScript must tell the same machine story.

The runtime protocol already separates `summary`, `diagnostic`, `log` and
`result` events. `testSummary` is the typed place for cross-runtime test
accounting, and its members are additive. The CLI report bounds each job at 16
diagnostics and 1,024 bytes per message.

## Decision

Go and TypeScript test producers apply this policy to solo and batched runs:

1. **One summary vocabulary.** The default recap is one deterministic summary
   in this order: `passed`, `failed`, `skipped`, a runtime-failure fallback when
   a nonzero exit has no failed-test count, omitted failure details, then
   coverage. Go's fallback is package-aware (`N package(s) failed` or `go test
   failed`); TypeScript's is `test run failed`.
2. **The transcript is `debug` detail.** Every non-empty transcript line is a
   runtime `log` event at `debug`. `--test-verbose` promotes the same lines to
   `info`; it changes no result data, verdict or exit code. Go passes `-v` so
   the transcript holds Go's verbose detail; TypeScript does not change Bun's
   arguments.
3. **Failure diagnostics are a bounded causal projection.** Each producer emits
   at most 16 failure diagnostics, including one final
   `TEST_FAILURE_DETAILS_TRUNCATED` accounting diagnostic when needed. Every
   message is valid UTF-8 and at most 1,024 bytes.
4. **Omissions are counted on the wire.** The omitted count is
   `result.data.testSummary.failureDetailsTruncated`. The report projects it
   onto each affected job and the command aggregate, so its validator can
   require full diagnostics for report-side omissions and still accept
   producer-side bounding. No parallel result type exists. The member is
   additive and optional, so it does not increment `protocolVersion`.
5. **Batched transcripts keep project ownership.** A batched producer tags
   transcript logs in the log `context` with the owning project. A group-level
   failure names all affected projects once. The CLI splitter strips those
   routing hints and attaches each log only to its project's event stream.

The producer stream is the complete input to the recorder. The recorder
sanitizes and persists every event to `events.jsonl`, suppresses `debug`
detail from normal live JSONL, admits it under the verbose budget
(`--output=jsonl --verbose`), and counts every live omission. It never
reconstructs a transcript from the bounded diagnostics.

## Consequences

- Default terminal and machine output is summary-first.
- A large failure cannot force an unbounded diagnostic payload, and no
  transcript line is discarded at the producer.
- `--test-verbose` is a task cache input. The result cache keeps
  report-relevant events and excludes transcript `log` events, so a warm hit
  replays the summary and verdict without a transcript; the flag shows the
  transcript only when the task executes. A fresh execution keeps the complete
  transcript in the session's `events.jsonl`.
- A consumer validating the closed report schema must adopt the `result-v2`
  revision that carries `failureDetailsTruncated`.
