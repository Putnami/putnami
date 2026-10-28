# Agent Context Protocol

> **Status: adopted.** This package pins the version 2 wire contract, strict
> parsing, semantic validation, and the fail-closed publish-safety gate for the
> per-project agent-context document. The deterministic aggregator (`putnami
> context pack`) has shipped; see
> [Using it](#using-it).

Why the document references instead of embedding, why no producer writes it to
a file, and why publication fails closed is recorded in
[`doc/adr/0001-aggregate-by-reference-fail-closed.md`](doc/adr/0001-aggregate-by-reference-fail-closed.md).

## Why

Putnami's agents and intelligence tools orient in a project by reading source: to find
composition roots, capabilities, public contracts, representative code, and
tests, an agent otherwise enumerates files by hand. This package fixes one
deterministic, **redaction-safe** artifact — the agent-context **Document** — so
those tools can answer "what is this project" from framework-owned facts without
rescanning the repository, while a hosted index can consume it without leaking
anything sensitive.

The document is **built in memory and never written**: a producer builds it
from committed facts and serves it, because it embeds the workspace revision and
content digests and would churn every commit. Determinism is pinned
byte-for-byte at a fixed tree by tests, not by storing the document.

This package is the contract, not the pipeline.

## What

### Aggregation is by reference

Every fact is aggregated **by reference**, never by embedding content:

- **identity/graph** — project id, name, path, type, tags, languages, and the
  ids of its dependencies and dependents.
- **compositionRoots** — the application main, describe entrypoint, and other
  roots (closed `RootKind` enum), each with a path and a provenance tag.
- **capabilities / contracts / infra / migrations** — arrays of `ArtifactRef`
  (`path` + `sha256:<64hex>` digest, optional `kind`, optional `sensitive`) that
  point at the committed `schema/capabilities.json`, `schema/contracts.json`,
  `infra/requirements.json`, and migration bundles.
- **representativeSources** — ordered `SourceRange` entries (`path`, 1-based
  `startLine`/`endLine`, a `why` reason, and a `tokens` estimate). **Ranges
  only**, never file content.
- **tests** — an OPTIONAL section: referenced conformance `packs` (id +
  capabilityKinds + languages, matching `conformance/pack.json`) and
  `fixtureDigests`. When it lists no packs it MUST carry a machine-readable
  `absenceReason` (`not-collected`/`no-packs`/`unsupported-project-type`). It
  carries no test policy: how test infrastructure is provisioned is a setting of
  the test jobs, not a fact about the project.
- **docs** — adjacent documentation paths with a `checked`/`unchecked`
  relationship status.
- **config** — a config-schema reference (`path` + digest) and refs-only
  operational-surface hints (e.g. whether platform endpoints are present).
- **provenance** — the workspace revision, the generator identity/version, and
  the aggregation method (`by-reference`). Token-budget entries carry their
  `method` (`bytes/4`).

Every closed enum (`RootKind`, `SourceReason`, `DocRelationship`,
`AbsenceReason`, `TokenMethod`, `AggregationMethod`) is frozen: extending one
requires a `ProtocolVersion` bump.

### The publish-safety gate (acceptance-critical)

`ValidatePublishSafety(doc, opts)` is the fail-closed redaction gate a producer
runs before a document leaves the workspace for an authorized index. It reports,
as hard errors:

- a reference to a caller-flagged sensitive path (`opts.SensitivePaths`:
  gitignored paths, files backing `sensitive` config fields, infra `secret`-kind
  entries, keyring material) that is not marked `sensitive`
  (`agentcontext.unredacted_sensitive`);
- any string field long enough to look like embedded file content
  (`agentcontext.embedded_content`) — the types have no content slot, so this is
  defense in depth;
- any non-workspace-relative path — absolute or `..`-escaping
  (`agentcontext.invalid_path`); and
- any duplicated reference (`agentcontext.duplicate_ref`).

A `nil` document fails the gate, so it never passes vacuously.

### Author overrides

`<project>/schema/agent-context.overrides.json` is the strict-parsed,
author-owned **OverridesFile** (`OverridesPath`). It is deliberately narrow: it
only adds/removes representative-source and doc entries and force-flags paths as
sensitive. It never overrides identity, capabilities, or provenance — those are
framework-owned facts. It carries its own version, `OverridesProtocolVersion`,
apart from the document's, so a change to the document shape leaves a committed
overrides file readable.

## Using it

The contract in this package is consumed end to end by shipped tooling:

- **`putnami context pack [--project <id>] [--output=json]`** — the
  deterministic aggregator. It walks committed facts, runs the fail-closed
  publish-safety gate, and prints one document per project; `--output=json`
  returns them as `data.documents`. It writes no file.
- **`bench/`** — the orientation-reads benchmark (`bash
  protocols/agentcontext/bench/orient-bench.sh`) dogfoods `context pack` across
  Go and TypeScript samples and shows fewer orientation round-trips than manual
  reconstruction. See [`bench/README.md`](./bench/README.md).

## How

Tooling consumes each shape through parse + validate entry points:

- `ParseDocument` / `ParseOverrides` — strict parse (unknown fields rejected).
- `ValidateDocument` / `ValidateOverrides` — structural and semantic validation.
- `ParseAndValidateDocument` / `ParseAndValidateOverrides` — parse then validate.
- `ValidatePublishSafety(doc, opts)` — the fail-closed redaction gate.

Diagnostics use the shared `go.putnami.dev/protocol/diagnostic` shape with a
stable taxonomy (`ValidDiagnosticCodes`).

### Determinism

Field order in every struct is deliberate; the canonical serialization is
`json.MarshalIndent(v, "", "  ")+"\n"`. `fixtures/valid/full.json` and
`fixtures/overrides/valid/full.json` are the exact canonical forms;
`determinism_test.go` pins them (regenerate with `go test -run CanonicalByteForm
-update`) and asserts serialization is stable across 100 marshals and idempotent
under round-trip.

### Fixtures

The two shapes have separate corpora so each parser is exercised in isolation:

- [`fixtures/valid/`](fixtures/valid) and [`fixtures/invalid/`](fixtures/invalid)
  — Documents (one invalid fixture per structural diagnostic code).
- [`fixtures/overrides/valid/`](fixtures/overrides/valid) and
  [`fixtures/overrides/invalid/`](fixtures/overrides/invalid) — OverridesFiles.

`conformance_test.go` runs every valid fixture clean and asserts every invalid
fixture produces at least one coded diagnostic. The publish-safety codes are
exercised directly in `strict_test.go` because the gate takes caller options.

### Versioning

`version_test.go` asserts [`schemas/agent-context.json`](schemas/agent-context.json)
pins exactly `ProtocolVersion` for the `document` shape and exactly
`OverridesProtocolVersion` for the `overridesFile` shape.

**Compatibility.** `ProtocolVersion`, the document version, is the exact integer
`2`. `OverridesProtocolVersion`, the overrides-file version, is the exact
integer `1`. Readers accept no other token for either shape and reject unknown
fields. Document version 2 deletes `tests.policy`, so a version 1 document is
rejected. The overrides shape did not change, so it stays at version 1 and every
existing overrides file still reads. Every enum listed above is closed, so adding a
composition-root kind, a selection reason, an absence reason, or a token method
is a version bump rather than an additive edit. No producer stores the document,
so a bump migrates producers and consumers only: there is no stored document to
rewrite or to go stale.

## Producers and consumers

- **Producer**: `putnami context pack` aggregates committed facts by reference
  and runs the publish-safety gate; it writes the document to no file.
- **Author input**: `<project>/schema/agent-context.overrides.json`, strict
  parsed and reviewed like any other source.
- **Consumers**: an agent that reads the printed or served document, the
  orientation benchmark under [`bench/`](./bench/README.md), and any authorized
  index that consumes a publish-safety-checked document. The referenced artifacts —
  `schema/capabilities.json`, `schema/contracts.json`, `infra/requirements.json`,
  migration bundles, conformance packs — are owned by
  [`capabilities`](../capabilities/README.md),
  [`contracts`](../contracts/README.md), and their protocols; this document only
  points at them.

## Ownership and support status

- **Owner**: `go.putnami.dev/protocol/agentcontext` (`protocols/agentcontext`).
- **Status**: `preview`, `parity: unsupported` — recorded in the workspace-root
  [`putnami.support.json`](../../putnami.support.json) under the classification
  contract in
  [`protocols/support`](../support/doc/adr/0001-support-entry-ownership-and-evidence.md).
- **Evidence**: two shipped producers sharing one builder, strict parse and
  validation for both shapes, a fail-closed publish-safety gate with its codes
  exercised directly, one invalid fixture per structural diagnostic code,
  canonical-byte determinism pinned across 100 marshals and round-trips, and a
  schema pinned to `ProtocolVersion` by `version_test.go`. One Go implementation
  exists and there is no second-language twin, so no cross-implementation parity
  is promised. It is not `stable`: the consumers are all in-repo, and an
  agent-facing surface this young should be able to change.
- **Feature and spec**: `agent-context/aggregated-agent-context`, specified in
  [`specs/aggregated-agent-context.json`](specs/aggregated-agent-context.json).
  The protocol owns the feature of its contract. The Go tests here and the
  CLI producer tests under `tooling/cli/internal/commands/agentctx` prove its
  requirements. The CLI command keeps its own feature,
  `cli/context-discovery`. The durable decisions for this module live in
  [`doc/adr/`](doc/adr/), the form
  [`protocols/features`](../features/README.md#specs-and-durable-decisions)
  expects a spec to link to.
