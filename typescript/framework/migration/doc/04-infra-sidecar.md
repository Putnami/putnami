# Infra requirements sidecar

Migration is the authoritative source for the `schemas` list inside each
declared database. During a project's build/generate phase it walks its
contributed sources, asks each for its infra contribution, merges them, and
emits a per-project sidecar that the build aggregator reads across the workload
dependency graph.

This is the TypeScript counterpart of the Go infra work; the shapes are a
hand-port of the v1 protocol defined in Go at `protocols/infra/`.

## What a source contributes

A kind-specific source that maps to a deployable database implements the
optional `infraDatabase()` hook:

```ts
interface InfraDatabaseRequirement {
  name: string;            // logical database id (the datasource / pool name)
  engine: InfraEngine;     // 'postgres' | 'mysql' | 'sqlite' | 'firestore'
  schemas?: string[];      // schema names this source migrates
}
```

SQL sources return `{ name, engine: 'postgres', schemas }`. Kinds with no infra
footprint leave `infraDatabase` undefined and are skipped.

## Merge semantics

`mergeInfraDatabases(requirements)` and `buildInfraManifest(sources)` are pure:

- Entries are keyed by `(name, engine)` — the protocol's merge identity — so
  multiple sources targeting the same database collapse into one entry whose
  `schemas` are the sorted union.
- The result is sorted by `(name, engine)` and every schema list is sorted and
  deduplicated.

The output is therefore **byte-identical for a given input**, regardless of
source ordering — the idempotency the sidecar contract requires of concurrent
producers. `buildInfraManifest` returns `undefined` when nothing is contributed,
signalling the caller to emit no sidecar.

## The sidecar file

```ts
infraSidecarPath(projectRoot); // <projectRoot>/.gen/infra/migration.json
```

- `protocolVersion` must match the Go reference; the aggregator's strict parser
  drops sidecars declaring a different version.
- The manifest carries **no contributor field** — provenance
  (`framework:migration`, derived from the sidecar's slug) is assigned by the
  aggregator.

### Atomic, idempotent writes

`writeInfraSidecar(projectRoot, manifest)` writes to a per-call random temp file
then `rename`s it into place, so a concurrent reader never observes a torn file
and two concurrent writers never clobber each other's temp file. On a rename
failure the temp file is removed and the error rethrown. `removeInfraSidecar`
deletes a stale file (a missing file is fine).

`emitInfraRequirements(sources, projectRoot)` ties it together: it builds the
manifest and reconciles the sidecar — writing it when there is something to
declare, otherwise removing any stale file from a prior generate.

## The generate plugin

`infraRequirements()` is a generate-lifecycle plugin that collects
`MigrationContributor` sources from the module tree and emits the sidecar during
`app.build()`:

```ts
import { application } from '@putnami/application';
import { sql } from '@putnami/database';
import { infraRequirements } from '@putnami/migration';

application()
  .use(sql())
  .use(infraRequirements());
```

The project root defaults to `PUTNAMI_PROJECT_ROOT` or the generate hook's
working directory (which the build pipeline sets to the project being built);
pass `{ projectRoot }` to override it.

## Reading the manifest from the registry

If you already hold a `MigrationRegistry`, `registry.infraManifest()` aggregates
every contributed source's `infraDatabase()` into the same manifest shape — the
"walk the registry" entry point used outside the generate phase.
