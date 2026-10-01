# Cross-language DARC conformance fixtures

Each fixture here is one **behavioral** corpus that every language runtime
enforcing a Domain Access & Replication Contract must reproduce exactly.

The `equivalence/` fixtures of other protocols pin a document: given the same
input, both producers must emit the same bytes. These pin something a document
cannot — what a contract *does*. A projection declaring `onStale: fail-closed`
and a five-minute bound has to refuse the same read in every language, and a
`lateEvents: ignore-older` strategy has to drop the same update. Two runtimes
that agreed on the JSON and disagreed on the behavior would be worse than one
runtime, because the manifest would describe neither.

## Who runs them

| Corpus | Go runner | TypeScript runner |
|---|---|---|
| `contract-validation.json` | `protocols/architecture/cross_language_conformance_test.go` | `test/architecture/conformance.test.ts` |
| `*-behavior.json` | `go/framework/app/darc/conformance_test.go` | `test/darc/conformance.test.ts` |
| `typescript-emitted-capabilities.json` | `tooling/sdd-extension/internal/sdd/architecture_engine_evidence_test.go` | `test/darc/evidence.test.ts` |

TypeScript paths are relative to `typescript/framework/application/`.

Both read these files by relative path. A case added here fails both runners
until both implement it, which is the point: the corpus is the specification,
and neither runtime is allowed to be its own oracle.

## Fixtures

| File | What it pins |
|---|---|
| `contract-validation.json` | Which contracts the protocol refuses, by diagnostic code and field |
| `projection-behavior.json` | Ordering, idempotency, late events, freshness, the single writer, rebuild, deletion |
| `snapshot-behavior.json` | Version addressing, immutability, and the latest-read consistency verdict |
| `command-reference-behavior.json` | Fact minimization, and the send/emit split under a planned or active carrier |
| `typescript-emitted-capabilities.json` | The bytes a TypeScript workload emits, read back by the Go evidence detector |

`typescript-emitted-capabilities.json` is the one fixture that is not authored:
it is captured from `@putnami/application`'s capability producer for a workload
enforcing one contract of each shape. TypeScript asserts the producer still emits
those exact bytes; Go feeds them to the real detector and asserts the rows land
in the right domain. It is the only artifact that proves the loop closes — every
other test on either side could pass while the two languages disagreed about the
manifest that carries evidence between them, and the gate would then read nothing
from a TypeScript project that enforces its contracts perfectly. Regenerate it by
running the TypeScript evidence test and copying the emitted manifest; never edit
it by hand.

## Case format

Every fixture is `{ "protocolVersion": 1, "cases": [ … ] }`. A case names the
contract it runs and a list of ordered steps:

```json
{
  "name": "an update older than the local copy is dropped under ignore-older",
  "contract": { "…": "an archproto.Import document" },
  "now": "2026-01-01T00:00:00Z",
  "steps": [
    { "op": "apply", "id": "a", "value": "v2", "sourceVersion": "2", "expect": { "changed": true } },
    { "op": "apply", "id": "a", "value": "v1", "sourceVersion": "1", "expect": { "changed": false } },
    { "op": "get", "id": "a", "expect": { "found": true, "value": "v2" } }
  ]
}
```

The projected value is always a string, so the corpus stays language-neutral:
`Projection[string]` in Go and `Projection<string>` in TypeScript project the
same fixture. Clocks are explicit — `now` seeds the runtime clock and the
`advance` step moves it — because a freshness bound measured against a real
clock is not reproducible.

### Steps

| Op | Applies to | Fields |
|---|---|---|
| `construct` | all | `expect.error` — the refusal a bad contract must produce |
| `apply` | projection | `id`, `value`, `sourceVersion`, `idempotencyKey`, `observedAt`, `expect.changed` |
| `delete` | projection | `id`, `sourceVersion` |
| `get` | projection | `id`, `expect.found`, `expect.value`, `expect.freshness` |
| `all` | projection | `expect.ids` |
| `rebuild` | projection | `source` — the named update batch the fixture supplies |
| `writer` | projection | `name`, to prove the single-writer refusal |
| `attach` | snapshot | `version`, `value`, `observedAt` |
| `at` | snapshot | `version`, `expect.found`, `expect.value`, `expect.freshness` |
| `latest` | snapshot | `expect.found`, `expect.value`, `expect.freshness` |
| `versions` | snapshot | `expect.versions`, oldest first |
| `send` / `emit` | command | `payload`, `fail` — whether the supplied carrier rejects |
| `stats` | command | `expect.attempted`, `expect.failed` |
| `fact` | reference | `name`, `expect.provenance` |
| `advance` | all | `seconds` |

`expect.error` is a stable token, never a message: `missing`, `stale`,
`late-update`, `writer-claimed`, `not-the-writer`, `deletion-not-applicable`,
`immutable`, `not-active`, `fact-not-imported`, `contract`. Messages are
deliberately not pinned — they are written for a person reading a terminal and
each language phrases them idiomatically. What must agree is *which* clause of
the contract refused.

## Adding a case

1. Add it here first, with the behavior you believe both runtimes should have.
2. Run both runners. A case that passes in one language and fails in the other
   is the finding — decide which runtime is right before changing either.
3. Never weaken a case to make a runtime pass. The corpus is the contract's
   meaning; a runtime that cannot meet it is the thing that is wrong.
