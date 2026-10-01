# Transaction Conformance Corpus

`manifest.json` is the **single, ordered behavioral conformance corpus** that
both the Go database adapter (`go.putnami.dev/database`) and the TypeScript
adapter (`@putnami/database`) execute against a real Postgres, asserting the
identical typed [`Outcome`](../transaction.go). It is the artifact behind the
acceptance line *"Go and TypeScript pass the same transaction and concurrency
fixture corpus."*

The corpus drives the tx/CAS primitives **directly** — `WithTx` /
`CompareAndSet` / `ConsumeOnce` / `Rotate` in Go, and `runInTransaction` /
`compareAndSet` / `consumeOnce` / `rotateRow` in TypeScript — **not** through the
HTTP middleware. The two languages' HTTP boundary policies differ by design (TS
rolls back on a thrown error, Go classifies on response status), so the
primitive level is where cross-language outcome parity is meaningful and exact.

## Structure

The manifest is a typed, closed-enum document parsed and validated by
[`conformance.go`](../conformance.go) and pinned by
[`schemas/conformance-manifest.json`](../schemas/conformance-manifest.json). Each
`case` has:

- **`setup.tables`** — table DDL (`columns`: closed `text` / `boolean` /
  `integer` types) and `seed` rows. Both runners render byte-identical
  `CREATE TABLE` / `INSERT` statements from it.
- **`operations`** — primitive invocations. Each names a `primitive`
  (`compare-and-set` / `consume-once` / `rotate`), the `table`, a `boundary`
  (`none` / `transaction` / `nested-transaction`), an optional `fault`
  (`callback-error`), a `repeat` count, and typed `args`. A **sequential**
  operation carries an `expect` Outcome; a **concurrent** operation is referenced
  by a concurrency group and carries none.
- **`concurrency`** — groups of operations run in **parallel** (real goroutines /
  `Promise.all`), asserting an **outcome multiset** (`expect`, e.g. exactly one
  `applied` + one `already-consumed-conflict`) rather than a per-operation
  outcome.
- **`asserts`** — post-condition row counts, the observable proof a boundary
  committed or rolled back.

### Boundary rule (identical in both runners)

A `transaction` (or `nested-transaction`) boundary **commits iff** the
operation's Outcome is `applied` and no fault was injected; any other outcome (or
an injected fault) rolls the boundary back. This keeps a conflict from
half-committing and makes the rollback assertions deterministic across both
languages. A `nested-transaction` opens an inner transaction joined to the outer
on the same pool (**join-outer, no savepoints**), so an outer rollback undoes the
inner write.

## Scenarios

| Case | Invariant |
|------|-----------|
| `concurrent.consume.one-credential` | Two concurrent consumers of one credential → exactly one `applied`, the other `already-consumed-conflict`. |
| `sequential.consume.device-code` | A device code issues its token only once: `applied`, then `already-consumed-conflict`, then `not-found`. |
| `concurrent.rotate.refresh-once` | Refresh rotation revokes the predecessor and installs a successor atomically; two concurrent rotations → one `applied` + one conflict, and exactly one successor exists. |
| `atomic.rotate.all-or-nothing` | A two-write unit whose second write fails rolls back the first — no transient revoked state is observable. |
| `nested.uow.join-outer-rollback` | Nested unit-of-work join-outer semantics: an outer rollback undoes the inner's applied write. |
| `boundary.rollback-releases-connection` | An abnormal exit (callback error) rolls back and releases the connection; repeated failures leave the row unchanged and a follow-up query still succeeds. |

Panic (Go) and timeout/cancellation (both) are not portable failure triggers, so
the corpus pins the portable one (`callback-error`) and the shared invariant
(rollback + connection release). Each adapter keeps its native panic / timeout
tests locally.

## Runners

The execution machinery is **exported** so a downstream project opts into the
whole corpus with one committed line, instead of copying a runner:

- **Go:** `go.putnami.dev/database/conformance` — call `conformance.Run(t)` from a
  test (`go/framework/database/conformance_test.go` is that thin caller). `Run`
  loads the corpus from the bytes embedded and exported by this protocol module
  (`transaction.ConformanceManifestJSON()`), so it never reads a repo-relative
  path.
- **TypeScript:** `@putnami/database/conformance` — call `registerConformanceTests()`
  from a `bun:test` file (`typescript/framework/database/test/conformance.integration.test.ts`
  is that thin caller). Because a TypeScript package cannot reach into the Go
  module, `@putnami/database` ships a **byte-identical committed copy** of
  `manifest.json` under `src/conformance/`, guarded against drift by
  `test/conformance-drift.test.ts` (this file stays the single source of truth).

Both provision Postgres via the shared test provider (`DATABASE_TEST_BINDINGS`)
and skip when no binding is present — so the local unit gate stays green with no
Postgres while CI runs the corpus for real. The manifest's own schema-validity is
exercised by the non-gated guard tests in
[`../conformance_manifest_test.go`](../conformance_manifest_test.go), so the
corpus structure is checked even without a database.

## Pack manifest

[`pack.json`](./pack.json) is the committed **pack-manifest convention**: a
minimal, forward-stable descriptor a project references (by `id`) to declare that
it runs this conformance pack. Aggregating packs across suites is out of scope
here; this file just names the pack and points at its corpus.

| Field | Meaning |
|-------|---------|
| `id` | Stable pack identifier (`putnami.transaction.conformance`). Projects reference a pack by this id. |
| `corpus` | **Optional.** The corpus file next to `pack.json` — the single source of truth (`manifest.json`) — when the pack ships a committed corpus. Omit it when the pack has no committed corpus file (see below). |
| `capabilityKinds` | The [`protocols/capabilities`](../../capabilities) capability kinds this pack certifies (`["datasource"]`). |
| `languages` | The runtimes with an exported runner for the pack (`["go", "typescript"]`). |

The shape is intentionally small and stable: keep speculative fields out so
downstream aggregation can rely on it. A guard test
([`../pack_test.go`](../pack_test.go)) pins the id, languages, and corpus pointer,
and checks every `capabilityKinds` value against the capabilities vocabulary.

### Optional `corpus`

`corpus` is **optional** and forward-compatible. This pack carries a committed
behavioral corpus (`manifest.json`), so it names it. Packs whose "corpus" is not a
committed fixture omit the field entirely:

- a **pure behavioral pack** that asserts in-process behavior — e.g. the health
  pack (`go/framework/app/conformance`), which drives the liveness/readiness/version
  endpoints — has no data corpus; and
- a **re-emit-and-compare pack** — e.g. the capability-manifest determinism pack
  (`protocols/capabilities/conformance`), whose corpus is the emitted manifest
  re-checked at run time, not a file on disk.

Aggregation is by `id`, so an omitted `corpus` does not affect it; the field stays
a strict superset of the original shape. A pack that omits `corpus` still declares
`id`, `capabilityKinds`, and `languages`, and its guard test asserts the pointer is
absent so the omission is intentional rather than a typo.
