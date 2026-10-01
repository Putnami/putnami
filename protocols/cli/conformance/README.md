# CLI result v2 conformance corpus

`manifest.json` is the canonical **cross-language corpus** for the version 2
machine result contract: one file, executed by both runtimes, pinning the exact
validation verdict of every fixture.

- **Go** — `go.putnami.dev/protocol/cli`, `TestConformanceCorpus`
  (`../conformance_v2_test.go`), driving `ValidateDocument`.
- **TypeScript** — `@putnami/cli-protocol`,
  `test/result-v2-conformance.test.ts`, driving `validateDocument`.

The manifest's `streamCases` are also shared by both runtimes and drive
`ValidateSessionStream` / `validateSessionStream` against complete live and
artifact JSONL strings. They pin whole-sequence rules a per-line schema cannot
express: budgets, hard partitions, deterministic selection, sanitization,
terminal identity and exact split elision.

The contract it certifies is specified in
[`../doc/02-result-v2.md`](../doc/02-result-v2.md) and published as
[`../schemas/result-v2.json`](../schemas/result-v2.json).

## Why a corpus and not two test suites

The v2 contract has a Go implementation and a TypeScript implementation, and
`--output=json` is parsed by both. Two independently authored test suites would
each be self-consistent while the two implementations quietly disagreed about,
say, whether an aborted run with failures is a failure. This corpus makes that
impossible: both runtimes assert against the **same expectations**, so a
divergence fails one runtime's tests against a corpus the other still passes,
and the failure names the disagreement.

That gap is not hypothetical here. `RESULT_ERROR_CODE` in `@putnami/cli-protocol`
was missing the `signal` class that [`../schemas/result.json`](../schemas/result.json)
had carried since v1 — a drift that survived because nothing forced the two
sides to be checked together.

## Case shape

```jsonc
{
  "id": "invalid.run.lenient-success-over-reused-failure",
  "document": "sessionStreamRecord",   // one of the six v2 documents
  "expect": "reject",                  // "accept" | "reject"
  "summary": "…what invariant this case pins…",
  "value": { /* the document, embedded so both runtimes read the same bytes */ },
  "violations": [                      // reject only: the EXACT expected set
    { "code": "cli.result.outcome_mismatch", "path": "run.outcome" }
  ]
}
```

- Exactly one of `value` (a JSON document) or `raw` (text that is deliberately
  not a single JSON value — trailing data, a truncated line).
- `expect: "accept"` declares no violations; the runners require an **empty**
  result, so an over-strict validator fails just as loudly as a lax one.
- `expect: "reject"` declares the complete violation set, **sorted by path then
  code** — the same order both validators emit. Equality is exact: an extra
  violation, a different path, or a different code all fail.
- Violation codes must come from the closed vocabulary
  (`ValidViolationCodes` / `VIOLATION_CODE`); a case naming an unknown code is
  itself a failure.

Both runtimes additionally ratchet coverage: every document kind and every
violation code must appear in at least one case, so a rule added without a
fixture fails the suite.

## What the cases pin

Beyond structural shape, the corpus fixes the six decisions this contract
exists to settle (each has both an accepted and a rejected fixture):

| Decision | Accepted | Rejected |
|----------|----------|----------|
| Unified success is strict | `envelope.run.success-counts-reuse-as-success`, `envelope.run.reused-failure-fails-the-run` | `invalid.run.lenient-success-over-reused-failure` |
| Abort wins, all surfaces agree | `envelope.run.aborted-keeps-failures-visible` | `invalid.run.v1-abort-ordering`, `invalid.envelope.aborted-with-exit-1` |
| Identity is typed and its key derived | `stream.task-start`, `stream.task-start.workspace-scope` | `invalid.identity.key-not-derived`, `invalid.identity.display-name-member` |
| The report is a bounded projection ([`../doc/03-report.md`](../doc/03-report.md)) | `report.file`, `report.file.absent-is-not-zero` | the `invalid.report.*` cases |
| Machine output is bounded and sanitized ([`../doc/04-machine-output.md`](../doc/04-machine-output.md)) | `stream-sequence.*` accepted cases | the `invalid.stream-sequence.*` cases |
| A test case is one bounded, attributed record ([`../doc/02-result-v2.md`](../doc/02-result-v2.md#test-case-records)) | the `stream.test-case.*` cases, `stream.task-end.test-cases-dropped`, `stream-sequence.normal-elides-every-test-case`, `stream-sequence.verbose-admits-every-test-case` | the `invalid.test-case.*` cases, `invalid.task.test-cases-dropped-zero` |

### What the corpus does NOT hold

The report's three **caps** (64 jobs, 16 diagnostics per job, 1024-byte
messages) are exercised by documents the two runtimes BUILD from their exported
constants — `../result_v2_report_test.go` and
`typescript/framework/cli-protocol/test/result-v2.test.ts` — because a fixture
proving "65 jobs is too many" would put 65 near-identical rows in a file whose
whole value is that a reviewer can read it. The constants themselves are pinned
against the schema's `maxItems`/`maxLength` on both sides, which is what ties
the two runtimes' numbers together the way a shared fixture otherwise would.

The `test:case` bounds follow the same pattern. The corpus holds one fixture
over the output byte bound; the edge of every byte bound is exercised by
documents built from `TestCaseMaxTextBytes` and `TestCaseMaxOutputBytes` in
`../test_case_test.go` and `test/result-v2.test.ts`. The 1,000-case cap spans
records, so no per-record validator checks it: `../test_case_test.go` pins what
`BoundTestCases` keeps, and both runtimes pin the constant against the schema's
value-only `$defs.testCaseMaxPerTask`.

## Update procedure

A contract change lands as **one commit** touching three places: this corpus,
the Go validator (`../result_v2_rules.go`), and the TypeScript validator
(`typescript/framework/cli-protocol/src/result-v2.ts`). A failure in one
runtime means either the change forgot the twin runtime or it forgot this
corpus — the Go failure message says so. Never loosen a case to make one runtime
pass; that defeats the corpus's purpose.

Adding or renaming a member additionally touches `schemas/result-v2.json`, the
Go struct in `../result_v2.go`, and the TypeScript interface — the drift tests
in both runtimes pin the schema `$defs` against the validator member lists and
the language types, so none of the four can move alone.

## Pack manifest

[`pack.json`](./pack.json) follows the pack-manifest convention
([`../../transaction/conformance/README.md`](../../transaction/conformance/README.md)):
`id` `putnami.cli.result.conformance`, `corpus` `manifest.json`, `languages`
`["go", "typescript"]`. `capabilityKinds` is empty — this pack certifies a
machine contract, not a capability kind.
