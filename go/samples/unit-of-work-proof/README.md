# unit-of-work-proof

A Go executable proof for the unit-of-work and consume-once primitives in the
transaction stack. It exercises, against a
real Postgres, the two safety properties those primitives exist to guarantee and
doubles as their acceptance proof.

The owner contract is
[`go/persistence-transactions`](../../framework/database/specs/persistence-transactions.json),
with the pool-scoped atomicity decision in its
[ADR](../../framework/database/doc/adr/0001-pool-scoped-transaction-boundaries.md).

| Property | Primitive | What the proof asserts |
| --- | --- | --- |
| Consume-once credential | `Repository.ConsumeOnce` | A device code is redeemable **exactly once**: `applied` on the first redemption, `already-consumed-conflict` on every later one, `not-found` for an unknown code — and under 16 racing redemptions **exactly one** observes `applied`. |
| Atomic two-repository rotation | `Repository.Rotate` inside `database.WithTx` | Revoking a predecessor signing key, installing its successor, and binding the successor to the tenant is **all-or-nothing**: on success all three are committed together; when the second-repository write fails the whole unit rolls back, leaving the predecessor `active` and no successor/binding — no transient unbound state. |

`uow.go` holds the reusable service code (`DeviceCodeService`, `KeyRotationService`)
so the proof is real, compiled code a reader can copy — the same shape as the
[TypeScript guide](../../../typescript/framework/database/doc/transactions.md).

## Typed Outcome taxonomy

Every helper returns the closed, cross-language `transaction.Outcome`
(`go.putnami.dev/protocol/transaction`) rather than a bare row count, so callers
switch exhaustively on a stable set of result codes:

- `applied` — the unit of work committed.
- `already-consumed-conflict` — the target was already consumed (a redeemed
  code, or a predecessor key already rotated).
- `not-found` — the target row did not exist.
- `retryable-serialization-failure` — a serialization/deadlock abort the caller
  may retry (the only retryable outcome).

The identical taxonomy is reported by the TypeScript adapter, and the shared
behavioral corpus at `protocols/transaction/conformance/manifest.json` pins that
both runtimes agree scenario-for-scenario.

## How it runs

`uow_test.go` provisions an isolated Postgres through the shared test provider
(`go.putnami.dev/database/testprovider`) and runs the scenarios for real. It is
gated behind `DATABASE_TEST_BINDINGS` exactly like the framework's conformance
corpus: **with no binding it SKIPS**, so the local build gate stays green with no
Postgres, and CI injects the binding to run the assertions. Run it locally with:

```sh
DATABASE_TEST_BINDINGS='{"protocolVersion":1,"mode":"require","databases":{"default":{"engine":"postgres","connection":{"dsn":"postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable"}}}}' \
  ./putnamiw test --projects go.putnami.dev/examples/unit-of-work-proof
```

This proof is a library, not a runnable binary, on purpose: the deterministic
proof lives in the test, mirroring `go/samples/capabilities-proof`.
