# ADR 0003 — The wire contract is a shared protocol module; the runtime is TypeScript only

- **Status**: accepted
- **Scope**: `@putnami/analytics` (`typescript/framework/analytics`)

## Context

[`protocols/analytics`](../../../../../protocols/analytics/doc/adr/0001-putnami-owned-wire-with-closed-vocabulary.md)
owns the wire: the closed vocabulary, the JSON shapes, the error codes and the
fixture corpus. Its Go code is the contract, not a runtime. Both the producer
(the browser tracker) and the consumer (the ingest route) are TypeScript, and
there is no Go web layer. An unexercised Go runtime would drift from the
contract silently.

Bounds are easy to get wrong across languages. Go's `len()` counts UTF-8 bytes;
JavaScript's `String.length` counts UTF-16 code units. A sanitizer written with
`.length` accepts documents the Go validator rejects, and no ASCII fixture
reveals it.

## Decision

The runtime is TypeScript only: tracker, sanitizer, enrichment, sink and plugin.
No Go analytics runtime exists while there is no Go web layer.
`@putnami/analytics` owns the feature; the protocol package declares none.

The TypeScript sanitizer runs the protocol's corpus: every
`fixtures/batch/valid/*.json` is accepted, and every
`fixtures/batch/invalid/*.json` is rejected with the code its file name states.

Every contract bound is a UTF-8 byte bound on both sides. The TypeScript side
hand-rolls the byte measurement rather than allocating a `TextEncoder` buffer
per event. The client truncates an over-long value to its byte bound without
splitting a code point, because the server rejects an over-long value outright
and one campaign name would drop the whole page view.

A guard test requires every persisted column name to be a token the protocol
declares.

## Invariants

- Every emitted attribute key and string vocabulary value is declared in
  `protocols/analytics`.
- Every persisted column name resolves to a protocol token.
- Length bounds are UTF-8 byte bounds on both sides of the wire.
- `protocols/analytics` contains no analytics runtime.

## Rejected alternatives

- **A Go ingest package for symmetry.** Nothing exercises it, and it doubles the
  cost of every contract change.
- **Defining the contract in TypeScript and generating Go.** Every other
  protocol is a Go module, and the Go validator is what makes parity checkable.
- **Measuring bounds in characters.** Identical for ASCII, so the mismatch
  survives an ASCII-only suite.
- **Fixtures inside the TypeScript package.** The corpus would prove the
  implementation against itself.

## Consequences

- A contract change is one edit in `protocols/analytics` plus fixtures; the
  TypeScript conformance test fails until the runtime follows.
- A future Go producer implements an already-proven contract.
- No contract bound may use `String.length`; the byte helpers are required, not
  an optimisation.
