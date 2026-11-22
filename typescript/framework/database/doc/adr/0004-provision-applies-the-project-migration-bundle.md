# ADR 0004 — `provision()` applies the project's migration bundle by default

- **Status**: accepted
- **Scope**: `@putnami/database` (`typescript/framework/database`) and the test
  binding `putnami test` synthesizes (`tooling/extension-sdk/dbtestenv`)

## Context

`provision()` created an isolated test database and applied migrations only
when its caller passed `sources` and the test binding set `applyMigrations`.
The binding `putnami test` passes never set it, so a suite that followed the
docs got an empty database and failed with `relation "…" does not exist`.

The project's migrations are not reachable from a test file: plugins construct
their `SQLSource` values lazily, inside `migrationSources()`, and nothing in
the test process calls it before `provision()`. The build already has them:
`putnami build` writes the sources every plugin contributes into the migration
bundle under `<projectRoot>/.gen/migration-bundle/`, the same document the Go
provider takes as `Options.Bundle`.

A process-wide registry of constructed sources was considered and rejected: it
sees no lazily constructed source, and since `bun test` runs every test file of
a project in one process, it would apply whatever earlier test files built.

## Decision

1. **Default sources.** With no `sources`, `provision()` loads the project's
   migration bundle, checks every payload against its pinned hash, and groups
   the SQL operations back into sources. `sources: []` applies none.
2. **Protocol meaning kept.** `applyMigrations` keeps its protocol meaning:
   the provider applies only when it is `true`. `SynthesizeBinding` sets it on
   every binding `putnami test` writes; the Go provider still applies only the
   bundle its caller passes.
3. **Visible failure modes.** `provision()` warns when the bundle is missing,
   older than a file under `src/`, targets none of the provisioned datasources,
   or the binding does not ask for migrations. Each warning names the remedy.
4. **Schema handling.** As in Go's `scopeSources`, each applied source loses its
   declared schema, so the binding's schema is the search_path the migrations
   run in. Under schema isolation, where every suite migrates the same database
   and its shared `migration.migrations` store, each suite records its rows
   under its isolated schema's name, and its teardown deletes them.

## Consequences

- A suite needs `putnami build` once, and again after a migration changes; the
  staleness warning says so.
- The application under test no longer applies the migrations: tests start it
  with `sql()`, not `sql({ autoApply: true })`.
- A suite under schema isolation that also runs the application's `autoApply`
  still shares state-store rows under the datasource's name, as before this
  record.
