# @putnami/migration

Transversal migration framework. Hosts the cross-kind contracts —
`MigrationSource`, `MigrationRunner`, `MigrationRecord`, `MigrationRegistry` —
that backend-specific runners (SQL today via `@putnami/database`; GCS,
document, cache, event-topic later) implement. The package is
intentionally backend-free: no `postgres`, no `pgx`, no fs APIs.

## Mental model

- A **feature plugin** owns its migrations. It contributes
  `MigrationSource` values via `MigrationContributor.migrationSources()`.
- A **kind plugin** (e.g. `@putnami/database`) registers a
  `MigrationRunner` for its `kind` into the per-app `MigrationRegistry`.
- At lifecycle time, the framework iterates `registry.kinds()` in
  lexicographic order and calls `runner.apply()` / `status()` /
  `rollback()` / `verify()` for each.

The migrate CLI uses the same registry — the same Application graph
production uses, no HTTP listener — so adding a feature plugin
automatically widens what the CLI applies on the next boot.

## CLI (`@putnami/migration/cli`)

```ts
#!/usr/bin/env bun
import { runMigrate } from '@putnami/migration/cli';
import { buildApp } from '../src/app';

process.exit(await runMigrate(buildApp, process.argv.slice(2)));
```

Subcommands: `up`, `up to <name>`, `down`, `down to <name>`, `status`,
`verify`, `inspect`. Exit codes: `0` success, `1` user error, `2`
operational error, `3` drift detected.

## Failure modes

- A source whose `kind` has no registered `Runner` (e.g., plugin ships
  SQL migrations but the app forgot `.use(databasePlugin())`) →
  `UnknownKindError` for apply unless `allowSourceOnly` is set, `statusAll`,
  and `verifyAll`, naming every contributing plugin. Only the automatic
  lifecycle apply sets `allowSourceOnly` so build metadata can be declared
  independently of the runtime backend.
- Two runners for the same kind → `MigrationConfigError`.
- Empty `kind` or `namespace` on a source → `MigrationConfigError`.

## Documentation

- [Getting started](./doc/01-getting-started.md) — mental model, authoring a `MigrationContributor`, running the CLI.
- [Runners and the registry](./doc/02-runners-and-registry.md) — implement a kind, the `MigrationRegistry`, failure modes.
- [The migrate CLI](./doc/03-cli.md) — subcommands, `--output`, exit codes.
- [Infra requirements sidecar](./doc/04-infra-sidecar.md) — declaring databases for the build aggregator.
- [Cross-language contract](./doc/05-cross-language-contract.md) — hash and canonical-id parity with the Go runner.

## Maintained contract

This package is `stable` in the workspace
[support catalog](../../../putnami.support.json). It owns one public promise:

- **[Migration orchestration](specs/migration-orchestration.json)**, with the
  [kinds-are-contracts ADR](doc/adr/0001-kinds-are-contracts-not-backends.md).
  The package stays backend-free; exactly one runner may register per kind;
  sources contributed for a kind with no runner fail explicit operations by
  name; cross-kind iteration is lexicographic so aggregated output is
  reproducible; and the bundle identity is byte-identical to the one the Go
  build produces.

Before v1.0.0, follow the workspace
[compatibility policy](../../../RELEASE.md): a breaking change is allowed in a
minor `0.x` release when the migration is documented. Do not infer strict
compatibility between every `0.x` minor.

**Removed in this release**: the deprecated `Record` type alias, which shadowed
the global `Record<K, V>` utility type. Import `MigrationRecord` instead.

## See also

- `@putnami/database` — the SQL runner implementation.
