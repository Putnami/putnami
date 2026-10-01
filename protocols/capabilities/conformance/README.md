# Capability-Manifest Determinism Pack

This pack certifies that a project's committed capability manifest
(`schema/capabilities.json`) is **byte-deterministic** and **complete**: the one
deterministic artifact this protocol aggregates must re-emit to the exact same
bytes on every run and in every runtime, or a downstream cannot content-address
or diff it.

It reuses the byte-stability machinery pinned by
[`../determinism_test.go`](../determinism_test.go): re-emit the manifest in the
canonical `json.MarshalIndent` form (two-space indent + trailing newline) and
**byte-compare** it to the committed file. The comparison is deliberately exact,
not semantic — the canonical form is the cross-language contract (the Go emitter
in `go.putnami.dev/app` and the TypeScript emitter in `@putnami/application` must
both reproduce it byte-for-byte, guarded on the TS side by
`@putnami/application`'s `test/capabilities/cross-language.test.ts` against the
same [`../fixtures/equivalence/capabilities.golden.json`](../fixtures/equivalence)
golden).

## Runner

The machinery is **exported** so a downstream opts in with one committed line
instead of copying a comparison:

- **Go:** `go.putnami.dev/protocol/capabilities/conformance` — call
  `conformance.RunFile(t, "schema/capabilities.json")` (or
  `conformance.Run(t, bytes)`) from a test. `Run`:
  - dispatches by exact protocol version and strict-parses/validates v1 or v2
    with no error diagnostics
    (**completeness** — a valid `protocolVersion`, closed-enum kinds, and
    complete provenance on every contribution),
  - re-emits that same version canonically and asserts the bytes match the committed file
    (**byte-determinism**), and
  - asserts the parse → marshal round-trip is a fixed point (**idempotent**).

`conformance/conformance.go` deliberately imports `testing` in non-test source:
it exists to be called by a downstream project's own test binary, mirroring the
other exported protocol packs (see
[`protocols/logging/conformance`](../../logging/conformance)). This is a
**pure** pack — no external service, no skip gate — so it runs in the normal
unit gate. The pack's own guard
(`../pack_test.go`, `TestManifestDeterminismRunner`) runs it against the shared
golden so the runner is exercised in-repo, not just published.

## Pack manifest

[`pack.json`](./pack.json) is the committed **pack-manifest convention** shared
by the protocol packs: a minimal, forward-stable descriptor a project references
(by `id`) to declare it runs this pack. `../pack_test.go` strict-parses it, pins
the id and languages, and checks every `capabilityKinds` value against the
capabilities vocabulary.

| Field | Meaning |
|-------|---------|
| `id` | Stable pack identifier (`putnami.capabilities.manifest-determinism`). Projects reference a pack by this id, and the [agent-context document](../../agentcontext/README.md) lists referenced packs by the same id. |
| `corpus` | **Optional.** Omitted here: this pack's "corpus" is the emitted manifest re-checked at run time, not a committed fixture file. |
| `capabilityKinds` | The [`protocols/capabilities`](..) capability kinds this pack certifies. The determinism pack is **manifest-scoped** — it certifies the aggregate artifact, which can carry every capability kind — so it lists the full vocabulary rather than one kind. |
| `languages` | The runtimes with an exported runner for the pack (`["go"]`). The canonical byte form is a cross-language contract, but the determinism runner is Go; the TypeScript emitter's parity is guarded by `@putnami/application`'s cross-language golden test. |

### Why `corpus` is optional

The convention's base shape is `{ id, corpus, capabilityKinds, languages }`,
with `corpus` pointing at a committed fixture next to `pack.json` — that is how
[`protocols/logging/conformance/pack.json`](../../logging/conformance/pack.json)
uses it. Making `corpus` optional keeps the shape forward-compatible for packs
with no committed corpus file: a pure behavioral pack, or a re-emit-and-compare
pack like this one. Aggregation by `id` is unaffected; omitting the field is a
strict subset of the original shape, never a different one.
