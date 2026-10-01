# ADR 0001 — A committed capability manifest states only what its own project can justify

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/capabilities` (`protocols/capabilities`)

## Context

`schema/capabilities.json` is tracked, reviewed, and content-addressed:
producers emit it under `.gen/schema/`, the codegen committer promotes it, and
downstream systems index and diff it. Every byte is a commitment about when the
file changes.

Resolved dependency versions, a source-state binding, and the reachable module
closure each make a manifest change when its capabilities have not. Consumers
also need a contribution identity that survives copying into an aggregating
manifest, and a migration kind, because registries scope migration namespaces
by kind and `MigrationBundle` carries none.

## Decision

1. **v2 is a separate strict wire shape.** v1 stays frozen with
   `ParseManifest`, `ValidateManifest`, and `ParseAndValidateManifest`. v2 has
   its own types, schema, strict readers, and diagnostics.
   `ParseManifestDocument` and `ParseAndValidateManifestDocument` dispatch on
   the exact JSON integer token `1` or `2`; `1.0`, `2.0`, and `2e0` are invalid,
   and the version is never inferred from shape. Readers never rewrite one
   version into the other; an aggregation boundary that can project only one
   version reports a no-lossy-projection error instead of dropping entries.
2. **Identity is owner-scoped.** Every v2 entry starts with
   `identity: { ownerProject, kind, subkind?, key }`, complete on its own.
   `ResolveContribution` coalesces byte-identical copies and reports divergent
   ones. A v1 migration returns `capabilities.v1_unreferenceable`.
3. **Canonical emission only drops.** `CanonicalManifestV2` drops resolved
   dependency versions, the source binding, and packages outside the project's
   capability surface (`CapabilitySurfaceV2`, via
   `ScopePackagesToContributionsV2`). It never adds facts. Validation and
   `ResolveContribution` read a document as written, so older manifests with
   those fields keep validating.
4. **Workspace state lives in the ephemeral stamp.** The module closure is
   recorded as `capabilityPackages` in `.gen/version.json`, never committed.
   Freshness of an indexed manifest belongs to the system that selected the
   revision.

## Rejected alternatives

- **Optional v2 fields on v1.** Optional identity is not identity; a document
  could pass the schema and still be unusable.
- **Keep the module closure in `packages`.** Any dependency edit re-stamps
  every workload manifest.
- **Embed a source binding.** A comment change would move a capability
  artifact. `ComputeSourceBinding` stays a primitive for evidence and index
  snapshots, with enumeration owned by the caller.
- **Record a version of first integration.** Build state cannot infer it
  truthfully.
- **Key v1 migrations by name alone.** Namespaces are scoped by kind, so this
  manufactures collisions. Go and TypeScript emitters validate typed migration
  sources by `(kind, namespace)` before projecting into v1.
- **Rebuild runtime config validators from `configDefinitions[].fields`.**
  That projection is lossy review metadata. Startup loads the declared server
  modules, which run their real `configToken()` calls.
- **Additive canonicalization.** Two runs with different workspace state would
  stop being byte-identical.

## Consequences

- A dependency-only upgrade does not rewrite `capabilities.json`.
- "What modules does this workload link" is answered by the stamp or the index
  snapshot, not the committed manifest.
- Two live versions mean two schemas, corpora, and validators.
  `version_test.go` pins each schema to its constant and checks the TypeScript
  producer stamps an accepted version.
- v1 migrations stay unreferenceable; no shim invents the kind.
- Diffing an old manifest against a regenerated one shows removals that are not
  capability losses; the canonical form is the baseline.
