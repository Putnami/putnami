# Migrations

`@putnami/database` is the SQL runner that plugs into the transversal
migration framework (`@putnami/migration`). Per-feature ownership: each
feature plugin colocates its migrations alongside its code, contributes
them via `MigrationContributor.migrationSources()`, and the framework's
Migrate lifecycle phase applies them.

## Contents

- [Authoring](#authoring) — migration definitions, inline or imported from `.sql` files.
- [Feature plugin wiring](#feature-plugin-wiring) — one method on your plugin.
- [Inline definitions](#inline-definitions) — when SQL files are overkill.
- [Running migrations](#running-migrations) — auto on startup vs. the CLI.
- [Targeting many schemas](#targeting-many-schemas-with-one-migration-set) — one set, per-datasource search_path.
- [Rollback](#rollback) — how down SQL is persisted and resolved.
- [Drift verification](#drift-verification) — `verify` and the state store.
- [Discoverability](#discoverability) — boot log, `inspect`, describer.

## Authoring

A migration is a `SQLDefinition`: a `name`, its `sql`, and an optional `down`.
Declare the definitions in TypeScript:

```ts
import type { SQLDefinition } from '@putnami/database';

export const iamMigrations: SQLDefinition[] = [
  {
    name: '20260520120000_create_users',
    sql: 'CREATE TABLE users (id UUID PRIMARY KEY DEFAULT gen_random_uuid(), email TEXT NOT NULL UNIQUE);',
    down: 'DROP TABLE users;', // optional; without it the migration is not reversible
  },
];
```

To keep the SQL in `.sql` files, import their text yourself. Bun inlines a
text import into the bundle, so it survives `bun build --compile` and container
images:

```ts
import createUsers from '../migrations/20260520120000_create_users.up.sql' with { type: 'text' };
import dropUsers from '../migrations/20260520120000_create_users.down.sql' with { type: 'text' };

export const iamMigrations: SQLDefinition[] = [
  { name: '20260520120000_create_users', sql: createUsers, down: dropUsers },
];
```

The build generates no migration module and loads no `migrations/` directory
for you: each file needs its import. (Go loads `*.up.sql` files from an
`embed.FS`; TypeScript does not.)

Naming: `YYYYMMDDHHmmss_short_description`. Names sort in byte order, which is
the apply order within the feature's namespace.

## Feature plugin wiring

```ts
import type { Plugin } from '@putnami/application';
import { sqlSourceInline } from '@putnami/database';
import type { MigrationContributor, MigrationSource } from '@putnami/migration';

import { iamMigrations } from './migrations';

export class IamPlugin implements Plugin, MigrationContributor {
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

A plain object works too: `{ name: 'iam', migrationSources: () => [source] }`.

## Inline definitions

For one-off boot data (rare; mostly tests), add definitions next to the
imported ones:

```ts
sqlSourceInline({
  namespace: 'iam',
  datasource: { name: 'default', schema: 'identity' },
  definitions: [
    ...iamMigrations,
    { name: '20260601_seed_root_role', sql: 'INSERT INTO roles VALUES (...)' },
  ],
});
```

The loader namespaces inline names with the source's `namespace` prefix
if you don't include one yourself.

## Running migrations

**On startup (auto-apply):** set `autoApply: true` on the SQL plugin.
The framework's Migrate lifecycle phase runs every contributor's
sources before HTTP starts:

```ts
import { application } from '@putnami/application';
import { sql } from '@putnami/database';
import { IamPlugin } from './iam/plugin';

const app = application()
  .use(sql({ autoApply: true }))
  .use(new IamPlugin());

await app.start();
```

**Via the migrate CLI (recommended for production):** leave
`autoApply: false` (the default) and ship a `bin/migrate.ts` binary in
the service that reuses the same `buildApp`:

```ts
#!/usr/bin/env bun
import { runMigrate } from '@putnami/migration/cli';
import { buildApp } from '../src/app';

process.exit(await runMigrate(buildApp, process.argv.slice(2)));
```

Subcommands: `up`, `up to <name>`, `down`, `down to <name>`, `status`,
`verify`, `inspect`. Exit codes: `0` success, `1` user error, `2`
operational error, `3` drift detected.

## Direct Migrator use (tests)

For unit and integration tests that just need a table to exist, drive
the engine directly with explicit definitions:

```ts
import { Migrator } from '@putnami/database';

const migrator = new Migrator('tx-test', [
  {
    name: 'tx-test/create-tx-test-table',
    sql: `CREATE TABLE IF NOT EXISTS tx_test_items (
      id TEXT PRIMARY KEY,
      name TEXT NOT NULL
    );`,
  },
]);

await migrator.up();
```

## Targeting many schemas with one migration set

`datasource.schema` is honoured at apply time: when a source declares a
`schema`, the per-datasource `Migrator` sets it as the transaction-local
`search_path` (`set_config('search_path', …, true)`) before each up/down body,
reverting at commit. Write **unqualified DDL** — `CREATE TABLE users (…)`, never
`CREATE TABLE identity.users (…)` — and the target schema is configuration, not
SQL. The `migration.migrations` state store is fully qualified, so the
search_path never disturbs bookkeeping.

To point one set at several schemas, contribute it under several datasources,
each owning its schema:

```ts
migrationSources(): MigrationSource[] {
  return ['put', 'oci', 'npm', 'gomod'].map((proto) =>
    sqlSourceInline({
      namespace: 'registry',
      datasource: { name: `registry_${proto}`, schema: `registry_${proto}` },
      definitions: registryMigrations, // one shared, schema-agnostic set
    }),
  );
}
```

The runner fans out one `Migrator` per datasource, each applying into its own
schema through a **single connection** — no per-schema pool needed. The
datasource **names must differ**: `migration.migrations` keys each row on
`(datasource, name)`, so reusing one name across schemas makes the second target
look already-applied and skip it. Two sources mapping one datasource to
different schemas throws at materialization, and earlier, at `putnami build`:
the bundle generator runs the same `resolveDatasourceSchemas` from
`@putnami/migration`. The owning schemas are also what the
source's `infraDatabase()` declares, so a deployer provisions them first.

Driving the engine directly (tests), the schema is the `Migrator`'s third arg:

```ts
await new Migrator('registry_put', registryMigrations, 'registry_put').up();
```

The declared schema also travels in the **published bundle** (it is part of each
bundle operation, byte-identical to Go), so a runtime that applies the bundle
reconstructs schema-aware sources and routes each datasource into its own schema
automatically — no per-pool wiring. A schema-less source omits the field, so
schema-less bundles stay digest-identical across both runtimes.

**Alternative — per-connection search_path.** When each schema lives in its own
database, bind each datasource's `schema` in `DATABASE_BINDINGS`; the factory
sets it as the connection `search_path` (see `connections.md`) and the
unqualified DDL lands in it. Within a *single* database prefer the per-datasource
form above — the state store keys on datasource.

## Rollback

Each apply records the `down` SQL into `migration.migrations.down_sql`
and its hash into `down_hash`. That means `down` works **even after the
code that defined the migration has been reverted** — the rollback SQL
comes from the database row, not the current registry.

Roll back one migration:

```sh
./migrate down
```

Roll back every migration applied after a named one (the named one
stays applied):

```sh
./migrate down to iam/20260520120000_create_users
```

Migrations without a `.down.sql` are non-reversible by design (the
inverse of `ADD COLUMN region` would silently drop data; `CREATE TABLE`
inverts to `DROP TABLE CASCADE`, which destroys it). `down` against
such a migration refuses with a clear error.

## Drift verification

```sh
./migrate verify
```

Compares the per-app registry against the state store and reports:

- **hash drift**: a migration in the state store whose hash no longer
  matches the current registry's hash.
- **missing-from-registry**: an applied row whose name is no longer in
  the registry (someone removed a migration that had been applied).
- **pending**: a migration in the registry that has not yet been
  applied (normal state of pending changes).

Exit code 3 when any drift is present; 0 otherwise.

## Discoverability

Every contribution and every runner registration emits a structured
`database.migration` log event at boot, and every applied migration emits an
`info` `migration applied` record (`migration:{name, datasource, durationMs,
outcome}`) — a failure emits `migration failed` at `error` with the structured
cause. No silent registration or silent failure is possible.

`./migrate inspect` dumps the per-app registry as JSON — no DB access
required, useful in CI:

```sh
./migrate inspect | jq '.kinds[] | select(.kind=="sql") | .sources | length'
```

`PUTNAMI_DESCRIBE=migrations` (or `=all`) emits `.gen/migrations.json`
during the existing describe build phase, baking the registry into
the artifacts the workspace already commits.

## See also

- The transversal framework: `@putnami/migration` and its CLI
  subpath `@putnami/migration/cli`.
- The Go counterpart: `go.putnami.dev/database` + `go.putnami.dev/migration`
  + `go.putnami.dev/migratecli`. Both runtimes share the
  `migration.migrations` state store and the
  `hashtext('putnami.migration:<datasource>')` advisory lock.
