# ADR 0001 — The sample library is the Python executable-spec fixture

- **Status**: accepted
- **Scope**: `py_example_library` (`python/samples/library`)

## Context

The executable-spec gate needs one conformance fixture per runtime: a project
whose feature declaration names acceptance checks, whose tests bind to those
checks through the runtime's native producer, and whose normal `test` run
publishes the verification report the gate joins. The Python extension ships no
framework package that could host one.

## Decision

`py_example_library` carries the fixture. It declares the
`python/example-library-utilities` feature with one executable requirement and
binds its clamp tests with the `putnami_proves` marker. The project stays on
the inherited workspace `report` mode: it proves the loop end to end without
gating anyone.

## Rejected alternatives

- **A synthetic project under the extension's test tree.** It would never run
  through the real workspace gate, the path the fixture exists to prove.
- **A pip-installable helper package.** Providing a Python framework is a
  non-goal of the extension, and the marker needs no import.
- **`enforce` mode here.** Rollout goes `report`, then complete mapping, then
  `enforce` per project; a sample gating contributors inverts that order.

## Consequences

The sample names `@putnami/sdd`, so `validate` checks its spec and every `test`
run emits the verification report. Renaming or deleting a marked test shows as
an unresolved requirement in `putnami specs verify` (visible, never blocking).
Whoever moves the sample's tests keeps the marker bindings with them.
