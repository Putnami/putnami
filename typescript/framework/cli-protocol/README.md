# @putnami/cli-protocol

The package also exports `SessionReportingChunk`, `SessionReportingAck`,
`parseSessionReportingChunk` (async SHA-256 validation),
`parseSessionReportingAck` and `sessionReportingAckMatches` for the
[native session reporter protocol](../../../protocols/cli/doc/04-session-reporting.md).
Go and TypeScript execute one shared conformance corpus. The reporter names
(`SESSION_REPORTER_*`, `LOG_REPORTER_*`) and `sessionReportingArtifacts`
state which artifacts the session reporter and the log reporter receive.

It also exports `SessionSubscribersFile` and `parseSessionSubscribersFile` for
the [session subscriber evidence](../../../protocols/cli/doc/05-session-subscribers.md)
document (`subscribers.json`), with its own shared corpus.

TypeScript binding of the **Putnami CLI result-envelope contract** — the shape
every command emits under `--output=json`.

It mirrors the Go module `go.putnami.dev/protocol/cli` and the canonical JSON
schema at `protocols/cli/schemas/result.json`
(`$id` `https://putnami.dev/schemas/putnami-cli-result.json`). Types are
hand-authored and kept in lock-step with the schema by a conformance test (the
TypeScript analog of the Go `drift_test.go`).

```ts
import { type Result, isResult, RESULT_STATUS } from '@putnami/cli-protocol';

const parsed: unknown = JSON.parse(await runPutnami('build', '--output=json'));
if (isResult(parsed) && parsed.status === RESULT_STATUS.failure) {
  console.error(`${parsed.command} failed (${parsed.error?.code}): ${parsed.error?.message}`);
  if (parsed.error?.next) console.error(`Next: ${parsed.error.next}`);
  process.exit(parsed.exitCode);
}
```

When present, `ResultError.next` is a suggested recovery command or documentation link.

## Surface

| Symbol | Purpose |
|--------|---------|
| `Result` / `ResultError` | The `--output=json` envelope types |
| `ResultStatus` / `RESULT_STATUS` | `success` \| `failure` |
| `ResultErrorCode` / `RESULT_ERROR_CODE` | `usage` \| `auth` \| `api` \| `signal` \| `failure` |
| `isResult` / `isResultError` | Runtime type guards |
| `RESULT_SCHEMA_ID` | `$id` of the canonical JSON schema |

## Machine contract version 2

Version 2 stamps `protocolVersion: 2` on all four machine surfaces — the
`--output=json` envelope, the `--output=jsonl` run stream or terminal
`plan:end`, the recorded session event log, the MCP `run_jobs`/`plan_jobs`
results, and the session files. A preview `plan:end` is live only and never a
persisted session event. **A document without the field is version 1.** Both versions are
exported for readers; current machine documents use version 2.

```ts
import { DOCUMENT_KIND, validateDocument, type ResultV2 } from '@putnami/cli-protocol';

const line = await runPutnami('build', '--output=json');
const violations = validateDocument(DOCUMENT_KIND.resultEnvelope, line);
if (violations.length > 0) {
  throw new Error(`not a v2 result envelope: ${violations.map((v) => `${v.path}: ${v.code}`).join(', ')}`);
}
const result = JSON.parse(line) as ResultV2;
// result.status is "success" | "failure" | "aborted" — an interrupted run is
// reported distinctly, with exitCode 130, and still lists run.failures.
```

| Symbol | Purpose |
|--------|---------|
| `ResultV2`, `SessionStreamRecord`, `BoundedSessionEndRecord`, `MCPResult`, `SessionFile`, `SessionPlanFile`, `ReportFile` | The v2 documents and opt-in bounded JSONL terminal |
| `TaskIdentity`, `RunSummary`, `TaskRecord`, `PlanSummary` | Typed task identity, executed-run verdict, and non-executed preview plan |
| `validateDocument`, `validateSessionStream` / `Violation` / `VIOLATION_CODE` | Per-document and whole bounded-stream validation, mirroring Go |
| `runSucceeded` / `derivedKey` | The strict success rule and the derived identity key |
| `RESULT_PROTOCOL_VERSION` / `RESULT_V2_SCHEMA_ID` | The version stamp and the v2 schema `$id` |
| `REPORT_MAX_JOBS`, `REPORT_MAX_JOB_DIAGNOSTICS`, `REPORT_MAX_MESSAGE_BYTES` | The report's bounds — contract clauses, pinned against the schema |
| `TEST_FAILURE_DETAILS_TRUNCATED_CODE`, `BATCH_PROJECT_LOG_CONTEXT_KEY`, `BATCH_PROJECT_LOGS_CONTEXT_KEY` | Shared test-output accounting and batch-routing spellings |
| `MACHINE_OUTPUT_BUDGETS`, `isMachineOutputDebugDetail`, `sanitizeMachineOutputValue` | Fixed live-stream policy/budgets and sanitization-v1 helper |
| `TestCase`, `TEST_CASE_STATUS`, `TEST_CASE_MAX_PER_TASK`, `TEST_CASE_MAX_OUTPUT_BYTES`, `TEST_CASE_MAX_TEXT_BYTES`, `TEST_CASE_MAX_BYTES_PER_TASK`, `TEST_CASE_MAX_BYTES_PER_BATCH` | The `test:case` record's payload, statuses and bounds, pinned against the schema |

The contract is specified in `protocols/cli/doc/02-result-v2.md`, and the report
document in `protocols/cli/doc/03-report.md`. This binding
and the Go implementation execute the **same** corpus,
`protocols/cli/conformance/manifest.json`, which pins the exact violation set of
every fixture — so a divergence between the two is a test failure here.

The bounded JSONL profile is specified in
`protocols/cli/doc/04-machine-output.md`. It is additive: legacy per-line
records remain valid, while `validateSessionStream(live, artifact)` requires the
bounded final accounting and validates the complete retained artifact against
the live selection. Current CLI JSONL streams implement this profile: normal
suppresses debug task detail, verbose admits it under the larger fixed budget,
and both name the complete sanitized session artifact. Every `test:case`
record is debug task detail, whatever its status.

For a plan-only JSON result, read `ResultV2.plan`; it is exclusive with `run`,
`data`, and `error` and requires `status: "success"` / `exitCode: 0`. For JSONL,
validate the standalone `plan:end` as `DOCUMENT_KIND.sessionStreamRecord`.
`validateSessionStream` intentionally rejects it because that function proves a
persisted session pair ending in `session:end`.

`SessionFile.placement`, when present, records `requested` and `actual` as
`local` or `remote`. A remote request can have `actual: "local"` when no
execution provider is available. Older sessions omit placement; absence means
unknown. This metadata does not alter task identities, cache keys or the
session tree fingerprint.

## Workload qualification

`putnami qualify` prints a verdict (`QualifyVerdict`) in the result envelope's
`data`, and `--print-contract` prints the derived smoke contract
(`QualifyContract`). `validateQualifyVerdict` and `validateQualifyContract`
apply the rules of `go.putnami.dev/protocol/qualify` and return the same
`qualify.*` codes, proven by `test/qualify-conformance.test.ts` over the
fixture corpus in `protocols/qualify/fixtures`. Only `state === 'passed'`
(`isQualifyPass`) is a pass; a `passed` verdict without every phase and request
passed, a matching sha and a clean teardown is refused.

## Maintained contract

This package is `stable` in the workspace
[support catalog](../../../putnami.support.json). What that promises is the
behavior stated in the
[CLI machine-output specification](specs/cli-machine-output.json) and the
[shared-corpus ADR](doc/adr/0001-binding-validates-against-the-shared-corpus.md):
this is a binding, not a second contract. The contract itself belongs to
`go.putnami.dev/protocol/cli`; the types here are pinned field-by-field against
its published schemas, and the same committed corpus that the Go reader executes
runs in this package's tests, so a divergence between the two readers fails
here.

Before v1.0.0, follow the workspace
[compatibility policy](../../../RELEASE.md): a breaking change is allowed in a
minor `0.x` release when the migration is documented. Do not infer strict
compatibility between every `0.x` minor.

## License

[FSL-1.1-MIT](../../../LICENSE.md)
