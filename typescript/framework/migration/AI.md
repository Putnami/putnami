# @putnami/migration

Transversal migration framework — kind-agnostic. SQL is the first kind;
GCS / document / cache / event-topic kinds slot in later without
modifying this package.

## When to touch

- Adding a new migration **kind**: define a new `Kind` value in your
  own package, implement `MigrationRunner` there, and register it into
  the per-app `MigrationRegistry`.
- Tweaking the cross-kind `Record` / `DriftReport` shape: review every
  Runner implementation in lockstep (today: `@putnami/database`).
- Changing the boot-log surface: discoverability is a contract (see §15
  of the redesign proposal); keep the per-source `INFO` event and the
  per-runner `INFO` event.

## When NOT to touch

- Adding SQL-specific behavior (file loaders, postgres-js bits, drift
  detection): lives in `@putnami/database`.
- Wiring the registry into DI: that is the `@putnami/application`
  package's job (the Application class creates the registry in its
  constructor and publishes it into the container).

## Public surface (stable contract)

- `Kind`, `KnownKind`, `KindSQL` — kind identity. `KnownKind` is the closed
  union of shipped kinds (`sql`, `gcs`, `document`, `cache`, `event-topic`)
  for autocomplete/typo hints; `Kind` keeps a `string & {}` escape hatch so
  out-of-tree packages can define their own kinds.
- `MigrationSource { kind, namespace, infraDatabase? }` — what plugins
  contribute. `infraDatabase()` is the optional infra hook a kind-specific
  source implements (SQL → `{ name, engine: 'postgres', schemas }`).
- `MigrationRunner { kind, apply, status, rollback, verify }` — what
  kind packages implement.
- `Record`, `RecordStatus`, `ApplyOpts`, `RollbackOpts`, `DriftReport`,
  `HashDrift` — value types.
- `MigrationRegistry` — `addSource`, `registerRunner`, `sourcesFor`,
  `runnerFor`, `kinds`, `applyAll`, `statusAll`, `verifyAll`,
  `infraManifest`.
- `MigrationContributor` + `isMigrationContributor` — plugin contract.
- `canonicalMigrationId`, `sha256Hex`, `sha256HexSync` — cross-language
  helpers (output matches the Go runner byte-for-byte).
- `MigrationConfigError`, `UnknownKindError` — typed failure surface.
- `resolveDatasourceSchemas`, `DatasourceSchemaClaim`,
  `DatasourceSchemaConflictError` — the one-schema-per-datasource rule the SQL
  runner (at apply) and `emitMigrationBundle` (at build) both run;
  `checkBundleSchemas` runs it over a bundle's operations. The message matches
  Go's `migration.DatasourceSchemaConflictError`.
- Bundle (`migration-bundle.v1`): `Bundle`, `BundleOperation`, `PayloadRef`,
  `Capabilities`, `BundlePayload`, `MigrationBundleContributor` types, plus
  `computeBundleDigest`, `computePayloadHash`, `normalizeBundle`, `writeBundle`,
  and `BUNDLE_PROTOCOL`. The digest is byte-for-byte identical to the Go
  protocol package (`go.putnami.dev/protocol/migration`); parity is pinned by a
  shared golden fixture (see `doc/05-cross-language-contract.md`).

## Infra requirements (`src/infra.ts`)

Migration is the authoritative source for the `schemas` list inside each
declared database. It walks its sources' `infraDatabase()` contributions,
merges them by `(name, engine)` (sorted union of schemas), and emits a
per-project scratch fragment at `<project>/.gen/infra/migration.json`.
The TypeScript generator syncs that fragment into committed
`<project>/infra/requirements.json`, which the build aggregator reads. The
fragment is a v1 hand-port of the Go protocol (`protocols/infra/`)
and carries no contributor field; provenance is assigned when the committed
requirements file is merged into the workload artifact.

- `InfraEngine`, `InfraDatabaseRequirement`, `PerProjectInfraManifest`,
  `INFRA_PROTOCOL_VERSION`, `INFRA_PER_PROJECT_SCHEMA_URL`.
- `mergeInfraDatabases`, `buildInfraManifest` — pure derivation.
- `writeInfraSidecar` (atomic temp+rename), `removeInfraSidecar`,
  `infraSidecarPath`, `emitInfraRequirements` — sidecar reconciliation.
- `infraRequirements()` — a generate-lifecycle plugin that collects
  `MigrationContributor` sources from the module tree and emits the sidecar.

## CLI (`@putnami/migration/cli`)

`runMigrate(buildApp, argv, streams?)` and `runMigrateAndExit(buildApp)`.
Subcommand parsing and dispatch is in `src/cli/index.ts`.

## Discoverability invariants

Every `addSource` and `registerRunner` emits a structured log line via
`useLogger('database.migration')` (the pinned logger name of the migration
boundary, shared with the SQL migrator/runner — see
`protocols/logging/conformance`), with its identifiers in a camelCase
`migration` group. Empty kind / empty namespace / nil
source / duplicate runner all throw with descriptive messages — the
"§15.6 failure modes are loud" contract.

## Maintained contract

This public package is `stable` in the workspace [support
catalog](../../../putnami.support.json). It owns the
[migration-orchestration specification](specs/migration-orchestration.json) with
its [kinds-are-contracts ADR](doc/adr/0001-kinds-are-contracts-not-backends.md).

Facts to rely on when generating code:

- The package is backend-free. Never add a driver, connection, or filesystem
  call here; a kind is served by a runner registered from the owning package.
- One runner per kind. A second `registerRunner` for the same kind throws.
- Sources for a kind with no runner fail explicit apply/status/verify with
  `UnknownKindError`; only the automatic start-up apply sets `allowSourceOnly`.
- `kinds()` is lexicographic, and every cross-kind operation iterates in that
  order.
- The record type is `MigrationRecord`. The deprecated `Record` alias — which
  shadowed the global `Record<K, V>` utility type — has been removed as a
  documented pre-1.0 breaking change.

Before v1.0, follow the workspace
[migration-based compatibility policy](../../../RELEASE.md); do not infer strict
compatibility between every `0.x` minor.
