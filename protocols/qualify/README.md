# Workload qualification contract

This package owns the two documents of workload qualification: the **smoke
contract** the CLI derives from a workload's existing contracts, and the
**verdict** it emits after running that contract against a target.
`putnami qualify` is the producer and the first consumer.

## Why

`putnami lint,test,build,validate` proves a change statically. It does not prove
that a workload starts through its production entrypoint, reaches readiness and
answers a real request, and a local-only proof misses deploy-class failures.
Qualification closes that gap with data rather than a scenario: the smoke is
derived from what the workload already declares, it runs in seconds against any
HTTP target, and it ends in one verdict an agent or a CI check can read.

## What

### Contract

A `Contract` (`schemas/contract.json`) has `protocolVersion: 1`, the workload's
canonical `project` ID, the `derivedFrom` documents, the ordered `requests`, and
a `digest`.

Version 1 derives from the `putnami.http-routes.v1` inventory only. A route is
eligible when it matches exactly, accepts `GET` or `HEAD`, is `publicEdge`, and
is none of the platform paths (`/livez`, `/healthz`, `/readyz`, `/version`, bare
or under the platform prefix) and not under `/debug/pprof` or `/_putnami/`.
Eligible routes are sorted by path and capped at 25. Each becomes one request
(`GET` preferred) with `maxStatus: 499`: the assertion is "no server error".
Read-only requests are the only set that is safe by construction; mutations
need a safety annotation the inventory does not carry.

`digest` is `sha256:` plus the lowercase hex SHA-256 of the canonical JSON of
`requests`: an array (`[]` when empty, never `null`) of objects whose members
are sorted by name, with no insignificant whitespace and no HTML escaping
(`&`, `<`, `>` stay literal). `ContractDigest` computes it; the TypeScript twin
computes the same bytes, and the fixture corpus pins the values.

### Verdict

A `Verdict` (`schemas/verdict.json`) records the `target`, the `binding`, the
executed `contract` (digest, request count, sources), every phase, every
request, the final `state`, and the start and finish times. `cleanup` is present
for a local target only.

`state` is a closed vocabulary: `passed`, `failed`, `unsupported`, `not_run`,
`timed_out`, `canceled`, `target_unreachable`, `digest_mismatch`,
`composition_failed`. **Only `passed` is a pass.** `unsupported` and `not_run`
read like excuses and are deliberately non-passes: absence of proof is never
proof.

Phases run in the order `resolve-target`, `readiness`, `version-binding`,
`smoke`, `teardown`, and a verdict lists all five. A phase that never started is
`not_run`. The verdict state is the first phase state that is neither `passed`
nor `not_run`, or `passed`.

A `binding` states which build the verdict proves:

- `tree` (local target): `fingerprint`, `dirty` and `headSHA` of the worktree,
  read before the workload is built. `version-binding` passes only when the
  worktree still has that fingerprint once the workload is ready; a changed tree
  is `digest_mismatch`, and the binding keeps the fingerprint the run started
  from.
- `artifact` (deployed target): `expectedSHA` is what the caller requires,
  `observedSHA` and `version` are what `/version` reported. The observed sha must
  start with the expected one (at least 7 characters); a mismatch or a missing
  sha is `digest_mismatch`, because a green verdict on a stale deployment proves
  nothing.

### Strict parsing

`ParseAndValidateContract` and `ParseAndValidateVerdict` refuse a document that
is not one JSON object, a member the schema does not declare (compared
case-sensitively), and a member of the wrong type. `ValidateContract` checks the
request shape (read-only method, absolute path, `id` equal to
`"<method> <path>"`, unique ids, an HTTP `maxStatus`, a provenance) and
recomputes the digest. `ValidateVerdict` checks the closed vocabularies and the
binding rules, and refuses a `passed` verdict without its proof: all five phases
passed, at least one request and every request passed, an observed sha starting
with the expected one, and a clean teardown.

Every diagnostic code carries the `qualify.` prefix. `ValidErrorCodes` is the
closed validation set; the `PhaseCode*` constants are what a producer records on
a non-pass phase.

## How

- `fixtures/valid/` and `fixtures/invalid/` hold the corpus; a file whose name
  starts with `contract-` is a contract, every other file a verdict.
  `fixtures/expectations.json` lists every file and pins the exact distinct codes
  each invalid fixture produces.
- `conformance_test.go` runs the corpus, pins `ProtocolVersion`, the state
  vocabulary and the digest bytes, and holds the fail-closed rule in
  `TestPassedRequiresEveryPhaseAndRequestPassed`.
- `drift_test.go` keeps the schemas, the case-sensitive member walk and the Go
  types on one member set.
- `@putnami/cli-protocol` (`typescript/framework/cli-protocol/src/qualify.ts`)
  is the TypeScript twin; `test/qualify-conformance.test.ts` runs the same
  corpus and must produce the same codes.

Run `./putnamiw test --projects go.putnami.dev/protocol/qualify,@putnami/cli-protocol --enforce-coverage`.

## Support

`preview`, as recorded in the [reviewed support catalog](../../putnami.support.json).
`@putnami/cli` produces both documents through `putnami qualify`, and the
TypeScript twin in `@putnami/cli-protocol` reads the same corpus. Stable support
still requires a consumer outside this repository (the PR-preview check) and an
explicit compatibility window.
