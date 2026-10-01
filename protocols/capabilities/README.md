# Capability Manifest Protocol

> **Status: v2 producers, v1/v2 readers.** This package freezes the v1 wire
> contract for compatibility and implements the current v2 contract. A complete
> legacy scheduler stamp may still select the documented v1 compatibility
> behavior.

Both wire versions are live. v1 is frozen: its types, API, diagnostics, and
fixture corpus stay as they were so existing documents keep validating and
resolving. v2 is what current producers stamp, and it adds what v1 could not
express — uniform semantic contribution identities, migration-kind identity,
ownership independent of the aggregating manifest, and precise
declaration/artifact provenance that cannot self-reference generated output.

Why v2 is a separate wire shape rather than optional fields on v1, and why
canonical emission drops content instead of enriching it, is recorded in
[`doc/adr/0001-committed-manifest-scope.md`](doc/adr/0001-committed-manifest-scope.md).
The longer design record it came from — including the feature-evidence work that
shares its identity model — is
[`protocols/features/design.md`](../features/design.md#1-capability-manifest-v2).

## Why

A Putnami project contributes a lot to the workload it is part of: config
blocks, HTTP routes and schemas, source and config discoverers, database
migrations, infrastructure requirements, health probes, and lifecycle hooks.
Today that knowledge is scattered across
per-domain artifacts and language-specific reflection. There is no single,
reviewable document that says "here is everything this project brings, and here
is the evidence for each item."

This protocol fixes one deterministic artifact — the **capability manifest** —
that aggregates every contribution kind, each entry carrying **provenance**
back to the project, package, discovery mechanism, and the source file
that declared it. A reviewer can read one file and trace any capability to its
origin; a deploy target can reason about a workload from one document instead
of re-deriving it per language.

## What

The manifest is emitted once per project to
`<project>/.gen/schema/capabilities.json`, from which the codegen committer
promotes it into the tracked tree at `<project>/schema/capabilities.json` — the
`.gen/schema/` prefix is what makes it a committed, shipped artifact rather than
ephemeral build scratch. It is versioned (`protocolVersion`, pinned by
[`schemas/capabilities.json`](schemas/capabilities.json) for v1 and
[`schemas/capabilities-v2.json`](schemas/capabilities-v2.json) for v2) and
strict: unknown fields are rejected.

The v1 contribution kinds, each an array of entries with a `provenance` block:

- `configDefinitions` — `{ path, fields: [{ name, type?, sensitive? }], provenance }`
- `schemas` — `{ name, kind ∈ {route, openapi, proto}, path?, provenance }`
- `discoverers` — `{ name, kind ∈ {source, config}, provenance }`
- `migrations` — `{ name, datasource?, digest?, provenance }`
- `infraRequirements` — `{ name, kind ∈ {database, events, storage, secret, scheduledJob}, provenance }`
- `healthContributors` — `{ name, probe ∈ {health, liveness, readiness}, provenance }`
- `lifecycleHooks` — `{ name, phase ∈ {starter, stopper}, provenance }`
- `packageVersions` — `{ package, version, provenance }`
- `requiredCapabilities` — `{ name, requires: [capabilityKind], provenance }`

For TypeScript protocol v1, an importable generated server module that owns
routes (`api-loader`, `react-loader`, or `static-loader`) is carried by a
`schemas` entry with `kind: "route"`, the module-registry key in `name`, and a
project-relative TypeScript/JavaScript module in `path`. Other generated server
loaders (for example SQL table or event-handler discovery) are represented
without fabricating route schemas: they use a `discoverers` entry with
`kind: "source"`, the registry key in `name`, and their project-relative module
in `provenance.evidencePath`. Activation entries have
`provenance.sourceKind: "generated"`. Client loaders and non-code generate
assets are not activatable.

The TypeScript application hook emits the first manifest, then the TypeScript
extension reconciles the merged exports from every pre-build hook before it
generates the bundled entrypoint. This captures loaders emitted by later hooks
such as `@putnami/web`'s `react-loader`. Reconciliation strictly parses and
semantically validates the document it finds and reconciles loader activation in
that same version, rejects registry-key or path conflicts, canonical-sorts every
collection, and publishes the result atomically. The rolling v1 aggregation path
in the application producer is reachable only under a complete legacy scheduler
stamp; it refuses a v2 dependency manifest with an explicit
no-lossy-projection error rather than downgrading it.

This convention deliberately does not reconstruct runtime config validators
from `configDefinitions[].fields`: that projection is review metadata and is
lossy. Loading the manifest-declared server modules executes their real
`configToken()` calls, dependency `ConfigContributor`s are then registered from
the constructed application graph, and startup verifies every manifest config
path is present before the DI container is built.

**V1 provenance** is `{ project, package?, version?, sourceKind ∈ {framework, manual, generated}, evidencePath? }`.
The wire protocol requires `project` and `sourceKind`; when the Go and
TypeScript application emitters take the v1 compatibility path they apply the
stronger inventory contract that every emitted contribution also has `package`,
resolved `version`, and `evidencePath`.

A **`requiredCapability`** expresses a logical capability that depends on a set
of other capability kinds. Example: a `sql` capability `requires`
`[datasource, migration, readiness]`. The `requires` vocabulary is a closed
enum (`config, schema, discoverer, migration, datasource, infra, health,
liveness, readiness, lifecycle, package`); it must be non-empty.

All closed enums are frozen: extending any of them requires a `ProtocolVersion`
bump so consumers can decide how to react.

## How

Tooling consumes the manifest through three entry points:

- `ParseManifest(data) → *Manifest, []Diagnostic` — strict parse (unknown fields rejected).
- `ValidateManifest(m) → []Diagnostic` — structural and semantic validation
  (protocol version, project, per-entry enums/provenance, provider uniqueness,
  and required-provider completeness).
- `ParseAndValidateManifest(data) → *Manifest, []Diagnostic` — parse then validate.

Those entry points remain the frozen v1 API. Version-neutral readers use
`ParseManifestDocument` / `ParseAndValidateManifestDocument`, which inspect the
exact integer `protocolVersion` and dispatch to `Manifest` or `ManifestV2`.
`ParseManifestV2`, `ValidateManifestV2`, and `MarshalManifestV2` expose the v2
path directly. Both versions reject unknown fields; missing, non-integer, or
unsupported versions are never inferred from document shape.
Only the lexical JSON integer tokens `1` and `2` are accepted; numeric
equivalents such as `1.0`, `2.0`, and `2e0` are intentionally invalid in both
Go and TypeScript.

### `domainAccess` — the v2-only contribution kind

`domainAccess` is the one contribution kind v1 has no counterpart for, and it is
a different kind of statement from the others. Every other row says *this project
contributes this capability*; a `domainAccess` row says *a running component of
this project enforces this declared cross-domain contract*:

```json
{
  "identity": { "ownerProject": "example", "kind": "domainAccess", "subkind": "projection", "key": "example.workspace-context.v1" },
  "import": "example.workspace-context.v1",
  "mode": "projection",
  "status": "active",
  "transports": [{ "role": "bootstrap", "kind": "api", "contract": "runtime.bindings.v1", "availability": "active" }],
  "enforced": { "maxStaleness": "5m", "onMissing": "fail-closed", "writer": "example.projector" },
  "provenance": { "project": "example", "sourceKind": "framework", "declaration": { "root": "project", "path": "darc.go" } }
}
```

It is **evidence, never authority**. The contract itself is declared and reviewed
in a `putnami.architecture.json`; `putnami architecture validate` reads these rows
beside the declared imports so it can tell a declared active contract nothing
implements from an implemented one nobody declared. Emitting a row cannot create
a cross-domain permission — that is the anti-pattern
[`protocols/architecture` ADR 0001](../architecture/doc/adr/0001-declarations-are-authority-observations-are-evidence.md)
exists to forbid.

The vocabulary — `mode`, `status`, transport kinds and availabilities, and every
member of `enforced` — is carried **verbatim** from the architecture contract and
is deliberately unconstrained here. `protocols/architecture` owns what those
values mean, and a second copy of the vocabulary is exactly the drift a checker
joining the two documents exists to catch. The mode is the identity subkind, so
one project enforcing two modes of one import keeps two distinct identities.

Every v2 entry begins with `identity: { ownerProject, kind, subkind?, key }`.
The complete tuple is owner-scoped and independent of the containing
`ManifestV2.project`, so verbatim dependency copies retain one identity.
`ResolveContribution` normalizes referenceable v1 entries from
`provenance.project`, coalesces byte-identical copies across sorted containers,
applies the deterministic mixed v1/v2 projection rule, and reports divergent
copies. V1 migrations return `capabilities.v1_unreferenceable` because their
wire shape has no migration kind.

V2 provenance requires an exact `declaration`; optional generated `artifacts`
are separate locations. A `package`-root location is resolved against the root
of the package named by `provenance.package` — the project root when that
package is a workspace project, or the module/package root selected by the
consumer's build/index snapshot when it is a published dependency. `package`
carries the durable producer identity; the exact resolved version is deliberately
not part of the stable capability declaration.

Resolved dependency state belongs to `.gen/version.json` and index/build
snapshots. V2 readers still accept historical `provenance.version` and
`packageVersions` fields. Canonicalization removes every version and migrates
historical package entries to the stable `packages` collection; current
producers write `{ identity, package, provenance }` there. This keeps the
`package` provider available to `requiredCapabilities` without coupling it to
dependency resolution. Consequently a dependency-only version upgrade does not
rewrite `capabilities.json`; only a semantic capability or stable provenance
change does. A version of first integration is not inferred from build state —
if such product history is needed later, it should be modeled as separate,
intentional metadata.

`packages` is scoped to the manifest's own **capability surface**: the project
itself, the owner projects that declare a contribution in this manifest, and the
packages those contributions were declared by (`CapabilitySurfaceV2`).
Canonicalization drops every other package entry, so the manifest can never
enumerate the reachable module closure. The closure is workspace state — adding
a dependency edge anywhere would otherwise rewrite every workload's committed
manifest at once — and stays in the ephemeral scheduler stamp
(`.gen/version.json` `capabilityPackages`), which is never committed. The rule is
a subset rule: emission drops, it never invents an entry. Scoping applies to
EMISSION only; validation and `ResolveContribution` still observe a document
exactly as written, so historical manifests carrying closure entries keep
validating and resolving.

Capability manifests intentionally contain no source-state binding. Embedding
one made otherwise stable manifests change after every source commit. Systems
that index capabilities own freshness and integrity checks against the revision
they selected. Readers accept the former v2 field for historical revisions but
canonicalization discards it, and producers never write it. `ComputeSourceBinding` remains the protocol-layer source-v1
primitive for feature evidence and index snapshots: callers supply canonical Git-like input
records (`path`, `mode`, `digest`) after enumerating tracked and non-ignored
untracked files. The protocol package validates/excludes/sorts those records,
serializes canonical JSON, applies the domain separator, and hashes it. CLI
worktree/Git-tree/package loaders own enumeration; keeping enumeration outside
this package prevents hidden filesystem or current-checkout state from entering
historical evaluation.

Diagnostics use the shared `go.putnami.dev/protocol/diagnostic` shape with a
stable error-code taxonomy (`ValidErrorCodes`), e.g. `capabilities.unknown_field`,
`capabilities.invalid_source_kind`, `capabilities.missing_provenance`.
Semantic failures use `capabilities.missing_required_provider`,
`capabilities.duplicate_provider`, and `capabilities.conflicting_provider`.
For v1, a datasource provider is a migration with a non-empty `datasource`;
health, liveness, and readiness requirements match their corresponding probe.
Infra provider identity is `(kind, name)`, so resources of different kinds may
share a name. V2 required-provider completeness remains workload-container
scoped, matching v1: any contribution copied into the same aggregate may
satisfy a requirement regardless of semantic owner, while a provider found
only in another manifest cannot. A v1 migration's complete identity cannot be
derived because registries scope namespaces by migration kind but
`MigrationBundle` does not carry that kind. Go and TypeScript emitters therefore
validate typed migration sources by `(kind, namespace)` before projecting them
into v1; the shared v1 validator deliberately does not key migrations by name
alone.

V2 diagnostics are sorted bytewise by `(severity, code, field, message)` after
canonical contribution/provenance ordering. This keeps invalid-manifest output
stable when equivalent input collections are shuffled without changing the
frozen v1 diagnostic API.

### Determinism and cross-language byte parity

Field order in every Go struct is deliberate. The canonical serialization is
`json.MarshalIndent(m, "", "  ")+"\n"`. The TypeScript emitter mirrors its
field order, two-space indentation, `omitempty` behavior, HTML and JavaScript
separator escaping, and trailing newline.
`fixtures/valid/full.json` is that exact canonical form for a representative
manifest; `determinism_test.go` pins it and asserts serialization is stable
across 100 marshals and idempotent under round-trip. The actual Go and
TypeScript application emitters independently produce and byte-compare the
shared `fixtures/equivalence/capabilities.golden.json`, which covers every v1
collection and infra kind with complete provenance.

### Fixtures

[`fixtures/valid/`](fixtures/valid) holds manifests that parse and validate
clean; [`fixtures/invalid/`](fixtures/invalid) holds manifests that must produce
at least one diagnostic. The frozen v1 corpus remains in those directories
unchanged. V2 parity and invalid cases live under [`fixtures/v2/`](fixtures/v2),
including the Go/TypeScript canonical golden at
[`fixtures/v2/equivalence/capabilities-v2.golden.json`](fixtures/v2/equivalence/capabilities-v2.golden.json).
The `no-source-claim.*` files beside it are two scheduler stamps of one
workload, one bound and one with an unavailable binding, and the one manifest
both emitters produce from either.
This corpus is the cross-language contract — `conformance_test.go` runs it
through the Go parser and validator, and the TypeScript emitter reads the same
files by relative path. [`conformance/`](conformance/README.md) additionally
publishes the manifest-determinism pack a downstream project runs against its
own committed manifest.

### Versioning

`version_test.go` asserts [`schemas/capabilities.json`](schemas/capabilities.json)
accepts exactly `ProtocolVersion` and
[`schemas/capabilities-v2.json`](schemas/capabilities-v2.json) exactly
`ProtocolVersionV2`. It also scans the TypeScript producer and asserts the
version it stamps is a version the parser accepts — the guard against a producer
stamping a version a reader would silently drop.

**Compatibility.** Readers accept the exact integer tokens `1` and `2` and
nothing else; every closed enum in both versions is frozen, so extending one is a
version bump rather than an additive edit. v1 documents keep parsing, validating,
and resolving unchanged. Because the manifest is committed, a version bump is
also a migration of every tracked `schema/capabilities.json`, which is why v2
arrived as its own shape instead of as optional v1 fields.

V2 is a separate strict wire shape rather than adding fields to v1; the decision
and its rejected alternatives are in
[`doc/adr/0001-committed-manifest-scope.md`](doc/adr/0001-committed-manifest-scope.md).
Its read-before-write migration order, v1 normalization limits (notably
migrations, whose kind is absent in v1), owner-scoped identity/copy convergence,
self-reference-free `source-v1` binding, and compatibility tests are recorded in
the [design record](../features/design.md#1-capability-manifest-v2). Readers
accept v1 and v2 without rewriting one into the other, and the legacy v1
aggregation boundary recognizes a valid v2 input and reports an explicit
no-lossy-projection compatibility error rather than downgrading it.

## Producers and consumers

**Producers** emit one manifest per project to
`<project>/.gen/schema/capabilities.json`, which the codegen committer promotes
to `<project>/schema/capabilities.json`:

- the Go framework describe/build emitter (`go/framework/app`), and
- the `@putnami/application` capabilities producer, whose activation metadata is
  then reconciled by the TypeScript extension's build step.

Both stamp `ProtocolVersionV2` unless a complete legacy scheduler stamp selects
the v1 compatibility path.

**Consumers** read the committed manifest, never a re-derivation of it:

- the CLI's feature aggregation and contribution resolution, `putnami context
  pack` (which references the manifest by path and digest), and the doctor
  stability checks;
- the TypeScript extension reconciler, which merges post-hook loader activation
  into the already validated document;
- any deploy target or index that wants one document per project instead of
  per-language reflection.

## Ownership and support status

- **Owner**: `go.putnami.dev/protocol/capabilities` (`protocols/capabilities`).
- **Status**: `preview` — recorded in the workspace-root
  [`putnami.support.json`](../../putnami.support.json) under the classification
  contract in
  [`protocols/support`](../support/doc/adr/0001-support-entry-ownership-and-evidence.md).
- **Evidence**: producers in both languages, in-repo consumers listed above, two
  fixture corpora with Go/TypeScript byte-parity goldens, canonical-bytes
  determinism tests, and both schemas pinned to their constants by
  `version_test.go`. It is not `stable` because two wire versions are live while
  v1 readers remain in service, so the surface can still move.
- **User-facing feature**: none. A capability manifest is a build artifact that
  other tools read; the user-visible behavior it enables belongs to the CLI and
  framework features that produce and consume it, and inventing a product feature
  per wire contract would put product intent in the wrong place. The durable
  decisions for this module live in [`doc/adr/`](doc/adr/), which is the form
  [`protocols/features`](../features/README.md#specs-and-durable-decisions)
  expects a spec to link to.
