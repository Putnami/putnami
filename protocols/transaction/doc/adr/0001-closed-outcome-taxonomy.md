# ADR 0001 — A closed outcome taxonomy whose retryable advisory cannot lie

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/transaction` (`protocols/transaction`)

## Context

Two runtimes run the same transactional primitives (compare-and-set,
consume-once, rotate) against the same database, and callers must react
identically. Driver errors and message strings differ per language, so callers
end up matching text like `could not serialize access` to decide whether to
retry. A driver reword then changes behaviour in one language only.

## Decision

1. `Outcome` has exactly four values: `applied`, `already-consumed-conflict`,
   `not-found`, and `retryable-serialization-failure`. A value outside the set
   is a validation error. Callers switch exhaustively.
2. The strict validator enforces `retryable == Outcome.Retryable()`. Only
   `retryable-serialization-failure` is retryable.
3. `message` is diagnostic detail, never control flow.
4. The request shape `UnitOfWork` (`propagation`, optional `isolation`,
   `readOnly`) carries no result and is validated separately.
5. The ordered corpus in `conformance/manifest.json` runs through both language
   adapters against a real Postgres and asserts the identical `Outcome` per
   scenario, including multiset outcomes for concurrent groups.
6. An adapter maps a database condition outside the taxonomy onto an existing
   outcome or propagates it as an error; it never mints a code. A new outcome
   is a `ProtocolVersion` change moving the enum, the retryable mapping, both
   schemas, the corpus, and both adapters together.

## Rejected alternatives

- **Driver errors classified by callers.** Classification in every caller, in
  two languages, keyed on text.
- **An open outcome vocabulary.** No exhaustive switch, and one runtime can
  invent an outcome the other cannot produce.
- **Caller-supplied `retryable`.** An advisory that contradicts the outcome
  invites endless retries or dropped failures.
- **Drop `retryable` and derive it on read.** The field documents intent on the
  wire for non-Go readers; the validator makes it safe.
- **HTTP status codes.** The primitives are not HTTP, the frameworks' boundary
  policies differ by design, and a status cannot separate
  `already-consumed-conflict` from `not-found`.

## Consequences

- The corpus asserts outcomes at the primitive level, not through HTTP
  middleware, so parity is exact where it matters and each framework keeps its
  own boundary policy.
