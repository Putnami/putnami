# migrations-feature

End-to-end sample of per-feature migration ownership.

The owner contracts are
[`go/migration-execution`](../../framework/migration/specs/migration-execution.json)
and [`go/migration-cli`](../../framework/migration/migratecli/specs/migration-cli.json),
with their durable decisions in the
[registry ADR](../../framework/migration/doc/adr/0001-deterministic-per-migration-atomicity.md)
and [CLI ADR](../../framework/migration/migratecli/doc/adr/0001-prepare-production-graph-without-runtime-start.md).

## Layout

```
.
├── iam/                        ← feature package (one of many in a real service)
│   ├── plugin.go               ← implements app.MigrationContributor
│   ├── users.go                ← GET /users, a read from the migrated iam_users table
│   └── migrations/             ← paired NNN_name.up.sql / NNN_name.down.sql
│       ├── 20260520120000_create_users.up.sql
│       ├── 20260520120000_create_users.down.sql
│       ├── 20260521093000_add_email_index.up.sql           ← non-reversible
│       └── 20260601150000_partition_users.up.sql           ← non-reversible
├── internal/wire/wire.go       ← single source of the app graph
├── cmd/server/main.go          ← wire.BuildApp().ListenAndServe()
├── cmd/migrate/main.go         ← migratecli.Run(wire.BuildApp)
└── cmd/bootstrap/main.go       ← escape hatch: database.ApplyToPool with pre/post SQL
```

## The wiring

A feature plugin lives entirely inside its package:

```go
//go:embed migrations/*.sql
var migrationsFS embed.FS

type Plugin struct{}

func (p *Plugin) Name() string { return "iam" }

func (p *Plugin) MigrationSources() []migration.Source {
    return []migration.Source{
        database.NewSQLSource("iam", database.Datasource{Name: "default", Schema: "public"}, migrationsFS),
    }
}
```

Adding a second feature is one `.Use(secrets.New())` line in `internal/wire/wire.go`.
Nothing in `cmd/migrate/main.go` changes — the CLI self-collects every
contributor in the plugin chain.

## Running the migrate CLI

```sh
export DATABASE_URL="postgres://postgres@localhost:5432/migrations_sample?sslmode=disable"

# From the repository root. Inspect runs no migration-state operations, though
# application preparation can still open eager dependencies.
./putnamiw run --projects go.putnami.dev/examples/migrations-feature --entrypoint ./cmd/migrate --args "inspect"

# Apply everything pending.
./putnamiw run --projects go.putnami.dev/examples/migrations-feature --entrypoint ./cmd/migrate --args "up"

# Apply forward through a specific migration.
./putnamiw run --projects go.putnami.dev/examples/migrations-feature --entrypoint ./cmd/migrate --args "up to 20260521093000_add_email_index"

# Tabular state of every migration.
./putnamiw run --projects go.putnami.dev/examples/migrations-feature --entrypoint ./cmd/migrate --args "status"

# Roll back the most recent reversible migration.
./putnamiw run --projects go.putnami.dev/examples/migrations-feature --entrypoint ./cmd/migrate --args "down"

# Drift check (exit 3 on drift, exit 0 on clean).
./putnamiw run --projects go.putnami.dev/examples/migrations-feature --entrypoint ./cmd/migrate --args "verify"
```

`down` against `20260601150000_partition_users` will refuse — it has no
`.down.sql` sibling, so the migration is non-reversible by design (the
inverse of a `region` column addition would silently drop data).

## Running the server

The same plugin graph; `AutoApply: true` means migrations apply during
startup before HTTP traffic is served:

```sh
./putnamiw serve --projects go.putnami.dev/examples/migrations-feature
```

The server mounts the platform endpoints (`/livez`, `/healthz`, `/readyz`,
`/version`), the single-endpoint probe `/_/health`, and one business route, `GET /users`, which lists users from the
`iam_users` table the migrations create. To prove the server boots against a
fresh database, applies its migrations and answers that route, compose it and
run its derived smoke in one command:

```sh
./putnamiw qualify /go/samples/migrations-feature --target local
```

## Adding a new migration

1. Drop `NNN_short_description.up.sql` into your feature's `migrations/`.
2. (Optional) drop a matching `.down.sql`. Absence = non-reversible.
3. Restart the service or run the migrate project entrypoint with `up`.

That's it. There is no registration code to update.

## Bootstrap pattern (escape hatch)

`cmd/migrate` is the right entry point for almost every production
scenario. If you find yourself reaching past it, it's usually because
privileged SQL has to run *around* the schema-apply step — e.g. a
Cloud Run Job that creates a shared application role, applies the
schema as the role's grantor, and transfers ownership of the new
tables to the role. None of those bracket statements belong inside a
migration file (they have to be re-run idempotently across every Job
invocation), but the schema apply in the middle is just the same
SQLSource the feature plugin already exports.

`cmd/bootstrap/main.go` shows the shape:

```go
pool, _ := database.NewPool(ctx, database.PoolConfig{DSN: dsn})
defer pool.Close()

pool.Exec(ctx, preBootstrapSQL)                        // CREATE ROLE, GRANTs
database.ApplyToPool(ctx, pool, iam.Source())          // schema migrations
pool.Exec(ctx, postBootstrapSQL)                       // ALTER OWNER
```

The same `iam.Source()` the lifecycle path consumes via
`MigrationSources()`. Two helpers from `go.putnami.dev/database` make
this work without the app builder:

- `database.LoadSQLSource(src) ([]Definition, error)` — materialize a
  source into the same `[]Definition` the SQLRunner would (useful for
  tests, dry-runs).
- `database.ApplyToPool(ctx, pool, sources...) ([]Record, error)` —
  spin up a one-off `migration.Registry` + `SQLRunner` with
  `Force=true` and apply against a pool the caller owns. Closes the
  internal `*sql.DB` before return; the pool stays open.

Use `cmd/migrate` unless you have a real reason to reach for this.
Both paths share the same SQLSource, so the migration hashes and
state-store rows match either way.
