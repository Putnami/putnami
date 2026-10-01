# ADR 0004 — Integer width is declared, never inferred

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/clientcontract`, both projections, both
  emitters

## Context

An integer's width must not depend on the language that projects it or the
transport that carries it. If a Go `int` is 64 bits over REST and 32 bits over
Connect, a value above 2³¹ crosses one transport and is truncated or refused by
the other. If one provider's field becomes a `bigint` and another's a `number`,
one contract yields two client types. An emitter that picks a width for an
unformatted `integer` hides the same loss. Bounds carried as floating-point
values round above 2⁵³.

## Decision

- **Every `type: "integer"` schema carries a `format` from the closed set
  `int32 | int64 | uint32 | uint64`.** An `integer` without a format is a
  strict generation error in both emitters. Neither emitter picks a default.
- **A contract `int` field is `int64` on every transport**, including proto map
  keys and values. A 32-bit width needs a dedicated field type. It is never
  deduced from the transport.
- **Bounds are explicit and exact.** The projection emits the `minimum` and
  `maximum` of the declared format, plus any narrower author bound, as exact
  decimal text (`ClientExactNumber` in TypeScript, `*json.Number` here). The
  corpus field `WidgetEvent.sequence` (`uint64`, maximum
  `18446744073709551615`) round-trips unchanged.
- **Emission follows the declared width.** `int64`/`uint64` become Go
  `int64`/`uint64` and TypeScript `bigint`. `int32`/`uint32` become Go
  `int32`/`uint32` and TypeScript `number`. The TypeScript JSON codec applies
  the same rule.
- **No implicit safe-integer bounds.** `MIN_SAFE_INTEGER` and
  `MAX_SAFE_INTEGER` describe a JavaScript runtime, not a contract. TypeScript
  exactness comes from `bigint`.

## Consequences

One declaration produces the same width in both languages and on all four
transports. A guard test fails if a corpus schema declares `type: "integer"`
without a format from the closed set or without exact bounds.
