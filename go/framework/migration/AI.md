# go.putnami.dev/migration

Transversal migration framework — kind-agnostic. SQL is the first kind;
GCS / document / cache / event-topic kinds slot in later without
modifying this package.

## When to touch this package

- Adding a new migration **kind** (GCS, document, etc.): define a new
  `Kind` constant in your own package, implement `migration.Runner`
  there, and register it into the per-app `*Registry`.
- Tweaking the cross-kind `Record` / `DriftReport` shape: review every
  Runner implementation in lockstep (today: `go.putnami.dev/database`).
- Changing the boot-log surface: discoverability is a contract; keep the
  per-source `INFO` event and the per-runner `INFO` event.

## When NOT to touch this package

- Adding SQL-specific behavior (file loaders, advisory locks, drift
  detection): that lives in `go.putnami.dev/database`. This package
  must stay free of pgx and other backend SDKs.
- Wiring the `Registry` into DI: that is the `go.putnami.dev/app`
  package's lifecycle responsibility.

## Public surface (stable contract)

- `Kind`, `KindSQL` — kind identity.
- `Source { Kind, Namespace }` — what plugins contribute.
- `Runner { Kind, Apply, Status, Rollback, Verify }` — what kinds implement.
- `Record`, `RecordStatus`, `ApplyOpts`, `RollbackOpts`, `DriftReport`, `HashDrift` — value types.
- `Registry` + `NewRegistry`, `AddSource`, `RegisterRunner`, `Sources`,
  `Runner`, `Kinds`, `ApplyAll`, `RollbackAll`, `StatusAll`, `VerifyAll`,
  `InfraRequirements`, `WriteInfraRequirements`.
- `SchemaContributor { InfraDatabases }` — optional `Source` interface that
  declares the `(database, engine) → schemas` mapping a source migrates.
  Migration is the authoritative source for the schemas list; sources that
  don't implement it contribute nothing to the infra manifest.
- `ResolveDatasourceSchemas`, `DatasourceSchemaClaim`,
  `DatasourceSchemaConflictError` — the one-schema-per-datasource rule the SQL
  runner (at apply) and the bundle describer (at build) both run;
  `CheckBundleSchemas` runs it over a bundle's operations. The message matches
  TypeScript's `DatasourceSchemaConflictError`.
- Error codes: `CodeInvalidSource`, `CodeDuplicateRunner`, `CodeUnknownKind`,
  `CodeInfraEmit`, `CodeApplyFailed`, `CodeRollbackFailed`, `CodeDriftDetected`.

## Discoverability invariants

Every contribution emits a structured log line (`database.migration`
logger — the pinned name of the migration boundary, shared with the SQL
migrator/runner; see `protocols/logging/conformance`), carrying its
identifiers in a camelCase `migration` group. Every runner registration emits one too. `Registry.Kinds()`
returns lexicographically sorted output. `assertNoOrphanSources` fires
for every apply unless the automatic Start lifecycle explicitly sets
`AllowSourceOnly`, and for every rollback/status/verify, when a `Source` is
contributed for a `Kind` with no `Runner` — that catches the "I shipped
migrations but forgot the backend plugin" case without making source-only
build metadata fatal during lifecycle startup.

## Tests

Stub `Source` and `Runner` types live in `registry_test.go` — reuse them
for any new test that needs a fake kind/runner.

## Contract invariants

- Registry order is deterministic and a source without its owning runner fails
  before execution unless source-only metadata collection is explicit.
- Each runner owns its backend atomicity; the SQL runner applies and records one
  migration in one database transaction.
- Repeatable drift is reapplied, versioned drift fails closed, and rollback is
  limited to migrations that declare a reverse operation.

Support is **stable** in `../../../putnami.support.json`; the normative feature
contract is `specs/migration-execution.json`, with the decision in
`doc/adr/0001-deterministic-per-migration-atomicity.md`.
