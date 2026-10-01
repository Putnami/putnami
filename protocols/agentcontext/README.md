# Agent Context Protocol

> **Status: adopted.** This package pins the v1 wire contract, strict parsing,
> semantic validation, and the fail-closed publish-safety gate for the
> per-project agent-context artifact. The deterministic aggregator (`putnami
> context pack` / `--check`) and the read-only MCP `agent_context` tool have
> shipped; see [Using it](#using-it).

Why the document references instead of embedding, why it stays ephemeral, and
why publication fails closed is recorded in
[`doc/adr/0001-aggregate-by-reference-fail-closed.md`](doc/adr/0001-aggregate-by-reference-fail-closed.md).

## Why

Putnami's MCP/intelligence tools orient in a project by reading source: to find
composition roots, capabilities, public contracts, representative code, and
tests, an agent otherwise enumerates files by hand. This package fixes one
deterministic, **redaction-safe** artifact — the agent-context **Document** — so
those tools can answer "what is this project" from framework-owned facts without
rescanning the repository, while a hosted index can consume it without leaking
anything sensitive.

The document is **ephemeral**: a producer writes it to
`<project>/.gen/agent-context.json` (the `.gen/` root, gitignored, never
promoted), because it embeds the workspace revision and content digests and
would churn every commit. Determinism is pinned byte-for-byte at a fixed tree by
tests, not by committing the artifact.

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
- **tests** — an OPTIONAL section: a `policy` (`auto`/`require`/`skip`, mirroring
  `database` `TestMode`), referenced conformance `packs` (id + capabilityKinds +
  languages, matching `conformance/pack.json`), and `fixtureDigests`. When it
  lists no packs it MUST carry a machine-readable `absenceReason`
  (`not-collected`/`no-packs`/`unsupported-project-type`).
- **docs** — adjacent documentation paths with a `checked`/`unchecked`
  relationship status.
- **config** — a config-schema reference (`path` + digest) and refs-only
  operational-surface hints (e.g. whether platform endpoints are present).
- **provenance** — the workspace revision, the generator identity/version, and
  the aggregation method (`by-reference`). Token-budget entries carry their
  `method` (`bytes/4`).

Every closed enum (`RootKind`, `SourceReason`, `DocRelationship`, `TestPolicy`,
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
framework-owned facts.

## Using it

The contract in this package is consumed end to end by shipped tooling:

- **`putnami context pack [--project <id>]`** — the deterministic aggregator.
  It walks committed facts and writes one document per project to
  `<project>/.gen/agent-context.json` (the gitignored, ephemeral `.gen/` root),
  running the fail-closed publish-safety gate before anything leaves the
  workspace.
- **`putnami context pack --check`** — a freshness gate that re-derives the
  document and exits non-zero (2) on drift without rewriting the artifact, so a
  stale context is caught in pre-flight or CI.
- **The read-only MCP `agent_context` tool** — serves the same document
  in-memory per request (plus on-disk freshness), so an agent gets a project's
  composition roots, contracts, representative source ranges, and tests in one
  structured call instead of enumerating files. It complements
  `describe_project`.
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
pins exactly `ProtocolVersion` for both the `document` and `overridesFile`
shapes.

**Compatibility.** `ProtocolVersion` is the exact integer `1`; readers accept no
other token and reject unknown fields. Every enum listed above is closed, so
adding a composition-root kind, a selection reason, an absence reason, or a
token method is a version bump rather than an additive edit. The artifact is
ephemeral, so a bump migrates producers and consumers only — there is no tracked
document to rewrite — and `putnami context pack --check` exits non-zero (2) on
drift, which is how a stale document is caught instead of being trusted.

## Producers and consumers

- **Producer**: `putnami context pack` — the only writer. It aggregates
  committed facts by reference, runs the publish-safety gate, and writes
  `<project>/.gen/agent-context.json`.
- **Author input**: `<project>/schema/agent-context.overrides.json`, strict
  parsed and reviewed like any other source.
- **Consumers**: `putnami context pack --check` (freshness), the read-only MCP
  `agent_context` tool (serves the document per request and complements
  `describe_project`), and any authorized index that consumes a
  publish-safety-checked document. The referenced artifacts —
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
- **Evidence**: a shipped producer and two shipped consumers, strict parse and
  validation for both shapes, a fail-closed publish-safety gate with its codes
  exercised directly, one invalid fixture per structural diagnostic code,
  canonical-byte determinism pinned across 100 marshals and round-trips, and a
  schema pinned to `ProtocolVersion` by `version_test.go`. One Go implementation
  exists and there is no second-language twin, so no cross-implementation parity
  is promised. It is not `stable`: the consumers are all in-repo, and an
  agent-facing surface this young should be able to change.
- **User-facing feature**: none of its own. The user-visible behavior is the
  CLI command and the MCP tool that produce and serve the document; a wire
  contract is not a product feature, and minting one per protocol would scatter
  product intent across artifacts nobody operates directly. The durable
  decisions for this module live in [`doc/adr/`](doc/adr/), the form
  [`protocols/features`](../features/README.md#specs-and-durable-decisions)
  expects a spec to link to.
