# ADR 0001 — One database contract, projected into infra without secrets

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/database` (`protocols/database`)

## Context

Per-language database configuration shapes drift, cannot describe a workload
the same way in both runtimes, and do not model several logical datasources.

Deployers read `protocol/infra`, whose requirements are committed. Any design
that lets a resolved connection reach an infra requirement commits a DSN,
password, or SSL flag to git.

## Decision

`protocols/database` owns the single cross-language contract:

1. Three layered shapes are keyed by **logical datasource name**:
   `RequirementManifest` (secret-free needs), `Binding` (manifest plus a
   resolved `Connection`), and `TestBinding` (binding plus test policy:
   `mode`, `isolation`, `reuse`, `applyMigrations`).
2. A `Connection` declares **exactly one** transport: `dsn`, `host`, or
   `instance` (`database.invalid_connection` otherwise). Adapters render driver
   configuration; application code stays driver-neutral.
3. A `RequirementManifest` has **no `connection` field**, so strict parsing
   rejects one. `RequirementManifest.Project()` flattens it to
   `{Name, Engine, Schemas}`, which `infra.DatabasesFromManifest` maps onto an
   infra requirement. Secrets cannot cross because no type carries them.
4. The package names no driver and opens no connection, so projects, deployers,
   adapters, and the test provider depend on it without a cycle.
5. The engine enum is closed (`postgres` only); a new engine is a
   version-bumping change on both sides of the infra bridge.

## Rejected alternatives

- **Per-language shapes reconciled in docs.** The drift this removes.
- **`protocol/infra` owns the rich shape.** It would commit connection data and
  give infra a database vocabulary to version.
- **Optional `connection` on a requirement.** An optional secret field is a
  secret field.
- **A single DSN convention.** Cannot express several datasources. A shorthand
  may exist as CLI sugar that expands to a binding.
- **Free-form provider options.** Makes "exactly one transport" unenforceable.

## Consequences

- A new transport changes the exactly-one rule and both language adapters.
- A deployer needing more than `{name, engine, schemas}` extends the
  projection here, never infra.
- A test provider in any language reads one binding document.
