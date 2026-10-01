# Getting Started with @putnami/migration

`@putnami/migration` is the transversal, backend-free migration framework. It
hosts the cross-kind contracts — `MigrationSource`, `MigrationRunner`,
`Record`, `MigrationRegistry` — that backend-specific runners implement. SQL is
the first kind (via `@putnami/database`); GCS, document, cache, and event-topic
kinds slot in later without modifying this package.

The package is intentionally backend-free: no `postgres`, no `pgx`, no
filesystem APIs. Those live in the kind-owning packages.

## Mental model

- A **feature plugin** owns its migrations. It contributes one or more
  `MigrationSource` values by implementing `MigrationContributor`.
- A **kind plugin** (e.g. `@putnami/database`) registers exactly one
  `MigrationRunner` per `kind` into the per-application `MigrationRegistry`.
- At lifecycle time the framework iterates `registry.kinds()` in lexicographic
  order and calls the matching runner's `apply()` / `status()` / `rollback()` /
  `verify()`.

The migrate CLI drives the **same** registry — the same `Application` graph
production uses, with no HTTP listener — so adding a feature plugin
automatically widens what the CLI applies on the next boot.

## Authoring a MigrationContributor

A plugin contributes migrations by implementing `MigrationContributor`
alongside the normal `Plugin` interface. `migrationSources()` is called once,
between the warmup and migrate phases of the application lifecycle.

```ts
import type { MigrationContributor, MigrationSource } from '@putnami/migration';
import { sqlSourceInline } from '@putnami/database';
import { iamMigrations } from './.gen/migrations.gen';

class IamPlugin implements Plugin, MigrationContributor {
  name = 'iam';

  migrationSources(): MigrationSource[] {
    return [
      sqlSourceInline({
        namespace: 'iam',
        datasource: { name: 'default', schema: 'identity' },
        definitions: iamMigrations,
      }),
    ];
  }
}
```

Returning multiple sources is the supported stacking pattern: a single feature
can mix codegen-backed SQL sources with inline definitions, and can contribute
to several kinds in the same plugin.

`isMigrationContributor(value)` is the runtime type guard the lifecycle uses to
filter the plugin list; you rarely call it yourself.

## Every source needs a kind and a namespace

`MigrationSource` is the minimal contract:

```ts
interface MigrationSource {
  readonly kind: Kind;          // 'sql' today; KnownKind documents the set
  readonly namespace: string;   // usually Plugin.name; drives naming + diagnostics
  infraDatabase?(): InfraDatabaseRequirement | undefined; // optional, see infra doc
}
```

An empty `kind` or blank `namespace` throws `MigrationConfigError` at
`addSource`. The recommended migration naming convention is
`${namespace}/${basename}`, e.g. `iam/20260520120000_create_users`.

## Running migrations from the CLI

A service binary's `bin/migrate.ts` is about five lines — it reuses the same
`buildApp` the production server uses:

```ts
#!/usr/bin/env bun
import { runMigrate } from '@putnami/migration/cli';
import { buildApp } from '../src/app';

process.exit(await runMigrate(buildApp, process.argv.slice(2)));
```

Then:

```bash
bun bin/migrate.ts status   # what's applied / pending across every kind
bun bin/migrate.ts up       # apply all pending migrations
bun bin/migrate.ts verify   # drift report (exit 3 on drift)
```

See [The migrate CLI](./03-cli.md) for the full subcommand and exit-code
reference.

## Next steps

- [Runners and the registry](./02-runners-and-registry.md) — implement a kind.
- [The migrate CLI](./03-cli.md) — subcommands, flags, exit codes.
- [Infra requirements sidecar](./04-infra-sidecar.md) — declaring databases.
- [Cross-language contract](./05-cross-language-contract.md) — Go parity.
