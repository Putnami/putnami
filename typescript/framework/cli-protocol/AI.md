# @putnami/cli-protocol

TypeScript binding of the Putnami CLI machine-output contract: the documents
every command emits under `--output=json` and `--output=jsonl`, plus the session,
plan, and report files.

Use this package when you need:

- to read a `putnami <command> --output=json` result from TypeScript
- to validate a session stream, an MCP result, or a report file before trusting it
- the exact violation list for a document that failed validation

Do not use this package for:

- defining or extending the contract — that is `protocols/cli`
- running the CLI; this package only reads what it produced
- asserting that a given CLI build emits the bounded JSONL profile

## Reading one result

```ts
import { DOCUMENT_KIND, validateDocument, type ResultV2 } from '@putnami/cli-protocol';

const line = await runPutnami('build', '--output=json');
const violations = validateDocument(DOCUMENT_KIND.resultEnvelope, line);
if (violations.length > 0) {
  throw new Error(violations.map((violation) => `${violation.path}: ${violation.code}`).join(', '));
}
const result = JSON.parse(line) as ResultV2;
```

`validateDocument` returns data, never throws. Violations are sorted by path and
then by code, with paths compared by code unit, so two runs over the same
document produce the same list.

For `--plan` or plan-only `--dry-run`, JSON uses `result.plan` and JSONL emits
one `plan:end` record. That record proves planning, not execution: it has no
`run` or session artifact and must be validated as one
`sessionStreamRecord`, not with `validateSessionStream`. An empty plan remains
typed with `metrics.tasks === 0` and `tasks: []`.

## Deciding whether a run succeeded

Use `runSucceeded` rather than reading `status` directly: success is a strict
unified verdict over the run summary, and `aborted` is a third outcome that
still lists the failures the run accumulated.

```ts
import { runSucceeded } from '@putnami/cli-protocol';

if (!runSucceeded(result.run)) {
  for (const failure of result.run?.failures ?? []) reportFailure(failure);
}
```

## Versions

`protocolVersion: 2` marks a version-2 document. A document **without** the field
is version 1 — the `Result` / `ResultError` types are kept exported for reading
output recorded by older CLI builds, not for producing anything new.

## Contract ownership

The wire contract lives in `protocols/cli` and is published as JSON schemas.
This package mirrors it and is kept honest by two mechanisms:

- every exported document type's member set is asserted field-by-field against
  the published schema;
- every bound constant (report limits, machine-output budgets) is asserted
  against the schema's own value instead of being restated.

Both readers — this one and the Go one — execute the same committed corpus,
`protocols/cli/conformance/manifest.json`, which pins the exact violation set of
every fixture. A change to the contract lands in `protocols/cli` first; the
corpus test here fails until this binding follows.

## Maintained contract

This public package is `stable` in the workspace [support
catalog](../../../putnami.support.json). Its behavior is defined by the
[CLI machine-output specification](specs/cli-machine-output.json) and the
[shared-corpus ADR](doc/adr/0001-binding-validates-against-the-shared-corpus.md).
Before v1.0, follow the workspace
[migration-based compatibility policy](../../../RELEASE.md); do not infer strict
compatibility between every `0.x` minor.

## Detailed Documentation

- `protocols/cli/doc/02-result-v2.md` — the version-2 contract
- `protocols/cli/doc/03-report.md` — the report document
- `protocols/cli/doc/04-machine-output.md` — the bounded JSONL profile
