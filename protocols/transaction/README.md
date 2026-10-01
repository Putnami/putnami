# `go.putnami.dev/protocol/transaction`

The Putnami **canonical transaction / unit-of-work protocol**: the single
cross-language contract for describing how a transactional unit of work runs —
its propagation relative to an enclosing transaction and its isolation level —
and for reporting the typed **Outcome** of running it.

The point of this package is that `transaction` itself owns the shared contract.
Go, TypeScript, and (later) Python adapters consume the *same* shapes and result
codes instead of each modelling transaction semantics and error taxonomy
differently.

This package is **declaration + validation only**. It has no dependency on a
database driver or a runtime, so a framework, a deployer, and the conformance
corpus can all depend on it without a cycle. It defines the shapes, the closed
enums, and the strict parse/validate helpers; it opens no connection and runs no
transaction.

## Two layered shapes

The protocol splits the request side from the result side:

### 1. Unit of work — `ParseAndValidateUnitOfWork`

The request-side descriptor a caller hands to a transaction runner: how the unit
of work should be run. It carries no result and no connection. `propagation` is
required; `isolation` defaults to the runner's configured level when omitted.

```json
{
  "$schema": "https://putnami.dev/schemas/putnami-transaction.json",
  "protocolVersion": 1,
  "name": "charge-card",
  "propagation": "requires-new",
  "isolation": "serializable",
  "readOnly": false
}
```

| Field | Values | Meaning |
| --- | --- | --- |
| `propagation` | `required` \| `requires-new` \| `nested` | `required` joins the enclosing transaction or starts one; `requires-new` always starts an independent transaction; `nested` joins the enclosing transaction as an outer participant **without a savepoint** (join-outer, no savepoints — the frameworks' current behaviour; a nested rollback rolls back the whole enclosing transaction). |
| `isolation` | `read-committed` \| `repeatable-read` \| `serializable` | Isolation level, mapped one-to-one onto the Postgres isolation levels. Optional. |
| `readOnly` | bool | When true, the unit of work performs no writes and the runner may open a read-only transaction. |

### 2. Transaction result — `ParseAndValidateResult`

The result-side envelope a runner returns: the typed, closed **Outcome** result
code plus a `retryable` advisory that mirrors `protocol/cache` and
`protocol/events`.

```json
{
  "$schema": "https://putnami.dev/schemas/putnami-transaction-result.json",
  "protocolVersion": 1,
  "outcome": "retryable-serialization-failure",
  "retryable": true,
  "message": "could not serialize access due to concurrent update"
}
```

## The Outcome taxonomy

`Outcome` is a closed enum so callers can switch exhaustively on a stable set of
result codes across languages:

| Outcome | Retryable | Meaning |
| --- | --- | --- |
| `applied` | no | The unit of work committed successfully. |
| `already-consumed-conflict` | no | The work targeted a resource that was already consumed (e.g. an idempotency key or once-only token); rejected as a conflict rather than re-applied. |
| `not-found` | no | A required target row/resource did not exist. |
| `retryable-serialization-failure` | **yes** | The transaction aborted with a serialization/deadlock failure the caller may safely retry. |

`retryable` is advisory metadata, but it is **not free-form**: the strict
validator enforces `retryable == Outcome.Retryable()`, so the advisory can never
contradict the result code. Only `retryable-serialization-failure` is retryable;
every other outcome is terminal. This keeps the two runtimes from disagreeing on
whether a given outcome may be retried. The reasoning, and the alternatives that
lost, is recorded in
[`doc/adr/0001-closed-outcome-taxonomy.md`](doc/adr/0001-closed-outcome-taxonomy.md).

## Usage

```go
import transaction "go.putnami.dev/protocol/transaction"

u, diags := transaction.ParseAndValidateUnitOfWork(uowBytes)
if diag.HasErrors(diags) { /* reject */ }

r, diags := transaction.ParseAndValidateResult(resultBytes)
```

Both shapes are strict-parsed (`DisallowUnknownFields`), validated against the
closed enums and the retryable/outcome consistency rule, and guarded by JSON
schemas (`schemas/transaction.json`, `schemas/transaction-result.json`) kept in
lockstep with the Go types by `drift_test.go`. A conformance fixture corpus under
`fixtures/` pins the accepted and rejected shapes, and
`fixtures/equivalence/transaction.golden.json` pins the byte-identical
cross-language serialization.

## Behavioral conformance corpus

Beyond the wire-shape fixtures, `conformance/manifest.json` is a **single,
ordered behavioral scenario corpus** that both the Go and TypeScript database
adapters execute against a real Postgres, asserting the identical `Outcome` for
transaction, concurrency, and rotation scenarios. It is declared by the typed,
closed-enum `Manifest`/`Case` shapes in `conformance.go` (strict
`ParseAndValidateManifest`), pinned by `schemas/conformance-manifest.json`, and
conformance-checked — even without a database — by the non-gated guard tests in
`conformance_manifest_test.go` plus the valid/invalid fixtures under
`fixtures/conformance-manifest/`. See [`conformance/README.md`](conformance/README.md)
for the scenario list, the schema, and the shared boundary rule the two runners
interpret identically.

## Non-goals

- **No driver coupling.** The package opens no connection and imports no driver;
  isolation values name Postgres levels but the protocol runs nothing.
- **No savepoint nesting in v1.** `nested` is join-outer-no-savepoints to match
  the frameworks' current behaviour; true savepoint-scoped nesting would be a new
  value and a `ProtocolVersion` bump.
- **No message-driven control flow.** `message` is diagnostic detail only;
  callers switch on `outcome`, never on `message`.

## Producers and consumers

| Shape | Produced by | Consumed by |
| --- | --- | --- |
| `UnitOfWork` | a caller describing how a unit of work should run | a transaction runner in `go.putnami.dev/database` / `@putnami/database` |
| `Result` | that runner, after running the unit of work | the caller, which switches on the closed `Outcome` |
| `conformance/manifest.json` | this module (the single source of truth; `@putnami/database` ships a byte-identical committed copy guarded by a drift test) | `go.putnami.dev/database/conformance` and `@putnami/database/conformance`, executed against a real Postgres |

The package itself produces nothing at runtime: it parses, validates, and pins
the vocabulary. It opens no connection and runs no transaction.

## Versioning and compatibility

Both shapes carry `protocolVersion` and readers accept exactly
`transaction.ProtocolVersion`; anything else is rejected with
`transaction.invalid_protocol_version`. `Propagation`, `Isolation`, and
`Outcome` are closed enums, so an unknown value is a diagnostic instead of a
tolerated string, and the retryable/outcome consistency rule is enforced at
validation time rather than trusted. Adding an outcome, a propagation mode, or
savepoint-scoped nesting changes what a conforming caller must handle and is a
`ProtocolVersion` bump; adding an optional field inside an existing shape is not.

The JSON schemas in [`schemas/`](schemas) — `transaction.json`,
`transaction-result.json`, and `conformance-manifest.json` — are kept in lockstep
with the Go types by `drift_test.go`; the corpus under [`fixtures/`](fixtures)
(`valid/`, `invalid/`, `conformance-manifest/`, and
`equivalence/transaction.golden.json`) is the cross-language surface.

## Durable decisions

- [`doc/adr/0001-closed-outcome-taxonomy.md`](doc/adr/0001-closed-outcome-taxonomy.md)
  — why `Outcome` is a closed enum and why the `retryable` advisory is validated
  against it instead of being caller-supplied.

## Product feature linkage

No user-facing product feature owns this module, and none should be minted for
it. It is a contract between application code and a transaction runner; what a
developer experiences is `WithTx` / `runInTransaction` and the CAS,
consume-once, and rotate helpers in the framework database packages. Per the
spec contract in [`protocols/features`](../features/README.md) a spec details an
already-authored feature and never mints one, so the durable design intent lives
in [`doc/adr/0001-closed-outcome-taxonomy.md`](doc/adr/0001-closed-outcome-taxonomy.md).
A product feature that later owns transactional data access links to that record
rather than restating it.

## Support

- **Status:** `stable`, recorded as
  `{"id": "go.putnami.dev/protocol/transaction", "kind": "protocol", "status": "stable"}`
  in the workspace-root [`putnami.support.json`](../../putnami.support.json)
  catalog (contract: [`protocols/support`](../support/README.md)).
- **Owner:** the Putnami maintainers, as repository owners of `protocols/`.
  Support entries carry no owner field, so the single owner is stated here.
- **Evidence:** both language adapters map their primitives onto the closed
  `Outcome` taxonomy in shipped code, both execute the same ordered behavioral
  corpus against a real Postgres and assert identical outcomes scenario by
  scenario, in-repo dogfood proofs exercise the primitives end to end
  (`go/samples/unit-of-work-proof`, `typescript/samples/06-database`), and the
  schemas, drift test, fixture corpus, and equivalence golden pin the wire.

## Adoption

The Go and TypeScript database adapters both
consume this contract: each maps its CAS / consume-once / rotation helpers onto
the closed `Outcome` taxonomy (Go `go.putnami.dev/database`, TypeScript
`@putnami/database`), and both execute the shared behavioral conformance corpus
(`conformance/manifest.json`) against a real Postgres, asserting the identical
`Outcome` scenario-for-scenario. In-repo dogfood proofs exercise the primitives
end-to-end (`go/samples/unit-of-work-proof`, `typescript/samples/06-database`).

The package itself remains **declaration + validation only** — the `UnitOfWork`
and `Result` shapes with strict parse + validate, the
`Propagation`/`Isolation`/`Outcome` closed enums, JSON schemas, drift and version
guards, the conformance fixture corpus, and the cross-language equivalence
golden; the adapters and runners live in the frameworks.
