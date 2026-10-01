# ADR 0001 — A migration kind is a contract, and this package ships none of them

- **Status**: accepted
- **Scope**: `@putnami/migration` (`typescript/framework/migration`)

## Context

A relational schema, an object-storage layout, a document collection, a cache
namespace, and event topics all need a named, ordered, recorded change that is
applied or not. Only the mechanics differ. Machinery that holds a SQL connection
forces every other kind to duplicate it or pretend to be SQL.

Migrations arrive from plugins the application never names. The collector must
tell "nobody contributed anything" from "somebody contributed migrations for a
backend this application never wired"; only the first is fine.

## Decision

1. **Backend-free by construction.** The package holds the contracts (`Kind`,
   `MigrationSource`, `MigrationRunner`, `MigrationRecord`, `DriftReport`) and an
   in-process registry: no driver, no connection, no SQL. `@putnami/database`
   registers the `sql` runner. The other `KnownKind` values (`gcs`, `document`,
   `cache`, `event-topic`) have no runner yet. `Kind` also accepts any string, so
   out-of-tree kinds work while known kinds keep autocomplete.
2. **Exactly one runner per kind; a missing one is loud.** A second registration
   for a kind throws. Sources for a kind with no runner raise `UnknownKindError`
   naming the contributing namespaces (typically a plugin with SQL migrations in
   an app without `.use(sql())`). Only the automatic start-up apply tolerates
   source-only kinds, because a source can be static metadata; every explicit
   migration entry point keeps the check.
3. **The registry is per boot, in DI.** No module-level singleton, so two
   applications in one process never share contributions.
4. **Cross-kind order is lexicographic.** Apply, status, and verify iterate
   `kinds()` in sorted order, independent of registration order. Order within a
   kind belongs to its runner.
5. **Infrastructure is derived.** Each source reports the database it migrates;
   the registry merges them by `(name, engine)` into one per-project fragment.
   Migration is the authority for the schema list because it creates the schemas.
6. **Bundle identity is canonical** and matches Go byte for byte, as
   [the migration protocol](../../../../../protocols/migration/doc/adr/0001-bundle-digest-excludes-provenance.md)
   defines.

## Rejected alternatives

- **Put the SQL runner here.** Every consumer of the contracts would import a
  driver, and every other kind would have to look like SQL.
- **Last registered runner wins.** Plugin order silently decides which
  migrations run.
- **Ignore sources without a runner.** A wiring mistake becomes an unmigrated
  database.
- **Module-level registry.** One test suite's contributions leak into another.
- **Order kinds by registration.** Changes whenever the plugin list changes.
- **Each package emits its own infra requirements.** Two sources for the schema
  list can disagree.

## Consequences

- A new kind is a new package: define the source shape, implement
  `MigrationRunner`, register it. Nothing here changes.
- Wiring order matters: a kind with sources and no runner fails at startup.
- A typo in a kind string is caught by the orphan-source check, not the compiler.
- Import `MigrationRecord`; there is no `Record` alias, because it shadowed the
  global `Record<K, V>` type.
