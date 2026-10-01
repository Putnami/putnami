# ADR 0001 — Bind to the CLI contract through the shared corpus, not a second reader

- **Status**: accepted
- **Scope**: `@putnami/cli-protocol` (`typescript/framework/cli-protocol`)

## Context

`go.putnami.dev/protocol/cli` owns the machine-output contract (result envelope,
session stream, MCP result, session and report files) and publishes it as JSON
schemas. A TypeScript consumer needs the answer the Go reader gives: is this
document valid, and which clauses did it break? A hand-written reader drifts from
the Go one, and a `JSON.parse(...) as Result` cast reports nothing until an
`undefined` field surfaces far from the malformed input.

## Decision

This package is a binding, not an implementation. The contract lives in
`protocols/cli`; the types here mirror it and the validator reports violations
against it.

- **Conformance is proved by execution.** The binding runs the Go reader's
  committed corpus, `protocols/cli/conformance/manifest.json`, which pins the
  exact violation set per fixture. The pack asserts that every document kind and
  every violation code has a fixture. Every exported member set is pinned
  field by field against the schema, and every bound constant (report limits,
  machine-output budgets) is asserted against the schema value, never restated
  as a literal.
- **Violations are data**, ordered by path then code, comparing code units, not
  locale, so two runs diff cleanly. Consumers get violations, not exceptions.
- **The version token alone decides the version**: `protocolVersion: 2` is
  version 2; no field is version 1. Version 1 types stay exported so recorded
  sessions and archived logs stay readable.
- **Success is the contract's strict unified verdict**, exposed as one helper.
  An interrupted run is a distinct outcome and still reports its failures.
- **The bounded JSONL profile is a reader-side contract.** This package makes no
  claim about which producer builds emit it.

## Rejected alternatives

- **Generate types from the schema.** Generated unions read poorly and put a
  generator in every consumer's build; hand-written types plus a drift test give
  the same guarantee.
- **Ship a JSON Schema validator.** It says "invalid", not which clause failed.
- **TypeScript-specific fixtures.** The only useful question is whether this
  reader matches the Go one, which needs one shared corpus.
- **Drop version-1 types.** Recorded documents outlive the build that wrote them.

## Consequences

- A contract change lands in `protocols/cli` first; the corpus test here fails
  until this package follows.
- Adding a field means editing two places; the drift test catches the second.
