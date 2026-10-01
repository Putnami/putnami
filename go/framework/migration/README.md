# go.putnami.dev/migration

Transversal migration framework. Hosts the cross-kind contracts — `Source`,
`Runner`, `Record`, `Registry` — that backend-specific runners
(`go.putnami.dev/database` for SQL today; GCS, document collections,
cache topology, and event-topic provisioning later) implement.

This package is intentionally backend-free: no pgx, no Bun, no embed
helpers. It defines the surface every kind plugs into and the lifecycle
the app builder + migrate CLI orchestrate.

## Mental model

- A **feature plugin** owns its migrations. It contributes `Source` values
  via `app.MigrationContributor.MigrationSources()`.
- A **kind plugin** (e.g. `go.putnami.dev/database`) registers a `Runner`
  for its `Kind` into the per-app `*migration.Registry`.
- At lifecycle time, the framework iterates `Registry.Kinds()` in
  lexicographic order and calls `Runner.Apply` (or `Status`, `Rollback`,
  `Verify`) for each one.

The `migrate` CLI binary in a service uses the same registry — same
plugin chain as production, no HTTP listeners — so adding a feature plugin
to a service automatically widens what the CLI applies on the next boot.

## Subcommands the CLI dispatches

| Subcommand        | What it calls                                  |
| ----------------- | ---------------------------------------------- |
| `up`              | `Registry.ApplyAll(ctx)`                       |
| `down`            | `Registry.RollbackAll(ctx, RollbackOpts{})`         |
| `down to <name>`  | `Registry.RollbackAll(ctx, RollbackOpts{To: name})` |
| `status`          | `Registry.StatusAll(ctx)`                      |
| `verify`          | `Registry.VerifyAll(ctx)` (drift report)       |
| `inspect`         | Registry-side dump (read-only; still runs Prepare) |

## Infra requirements

Migration is the cross-kind coordinator that knows which schemas a project
actually migrates, so it is the authoritative source for the `schemas` list
of each database it touches. A `Source` opts in by implementing
`SchemaContributor`:

```go
type SchemaContributor interface {
    InfraDatabases() []infra.Database // (name, engine) → schemas it owns
}
```

`Registry.InfraRequirements()` walks every registered `Source`, collects the
declarations from those implementing `SchemaContributor`, and folds them into
a deterministic `infra.PerProjectManifest`: one entry per `(name, engine)`,
each `Schemas` list the sorted, deduplicated union across sources. Sources
that don't implement the interface contribute nothing.

During the application's generate/describe build phase (`app.Describe`),
`Registry.WriteInfraRequirements(outputDir)` emits that manifest atomically to
the framework-generated scratch fragment `<project>/.gen/infra/migration.json`.
The Go generator syncs that fragment into committed `infra/requirements.json`,
which the build aggregator merges across the workload's dependency graph,
additively with the database framework's pool-level declaration. When no source
declares a database, no scratch file is written and any stale one is removed.

## Failure modes

- A `Source` whose `Kind` has no registered `Runner` (e.g., plugin ships
  SQL migrations but the app forgot `Use(database.NewPlugin())`) →
  `CodeUnknownKind` for apply unless `AllowSourceOnly` is set, rollback,
  `StatusAll`, and `VerifyAll`, naming every contributing plugin. Only the
  automatic lifecycle apply sets `AllowSourceOnly` so build metadata can be
  declared independently of the runtime backend.
- Two `Runner`s registered for the same `Kind` → `CodeDuplicateRunner`.
- A `Source` with empty `Kind` or empty `Namespace` → `CodeInvalidSource`
  at `AddSource` time.

## See also

- `go.putnami.dev/protocol/migration` — the cross-language SQL contract
  (state-store schema, hash policy, drift policy).
- `go.putnami.dev/database` — the SQL runner implementation.

## Support and contract

The SDD owner is `go`. `go.putnami.dev/migration` is public, documented,
maintained, and classified **stable** in the repository
[support catalog](../../../putnami.support.json). Before v1.0, minor releases
may still require documented migrations; see the repository
[release policy](../../../RELEASE.md).

The durable cross-kind execution contract is
[`go/migration-execution`](specs/migration-execution.json). Deterministic order
and backend-owned atomicity are recorded in
[ADR 0001](doc/adr/0001-deterministic-per-migration-atomicity.md) and protected by
[`registry_test.go`](registry_test.go) and the SQL runner tests in
[`../database/migration_test.go`](../database/migration_test.go).
