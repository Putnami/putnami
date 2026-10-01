# go.putnami.dev/migratecli

Kind-agnostic migrate command-line driver. `cmd/migrate/main.go` calls
`Run(AppBuilder)` and exits with the returned code.

## When to touch

- Adding a subcommand: edit `cli.go`. Mirror the dispatch pattern of
  `runUp`/`runDown`/etc. — they're small enough that one function per
  subcommand stays readable.
- Adjusting exit codes: keep the contract documented in the package
  doc-comment and README in sync (1=user, 2=operational, 3=drift).
- Changing the `inspect` JSON shape: bump it deliberately — external
  CI scripts parse this with `jq`. `specs/migration-cli.json` and
  `doc/adr/0001-prepare-production-graph-without-runtime-start.md` are the
  durable source of truth.

## When NOT to touch

- Per-runner behavior (SQL apply logic, etc.) — that's
  `go.putnami.dev/database` (or whatever package owns the kind).
- Plugin discovery — that lives in `go.putnami.dev/app`'s
  `collectMigrationSources` lifecycle hook. The CLI just calls
  `app.Prepare(ctx)` and reads the registry.

## Discoverability invariants

The CLI must not silently swallow errors:

- `up` surfaces runner errors with exit 2 and prints the records that
  did succeed before the failure.
- `verify` exits 3 with a per-row breakdown when drift is detected.
- `inspect` is `jq`-clean JSON: every `up`/`status` user can verify
  what the binary thinks it owns before it runs. It drives no migrations,
  but like every subcommand it first runs `Prepare`, so a database plugin
  may open a connection — it is not a guaranteed offline command.

## Tests

`cli_test.go` covers every subcommand against a fake `migration.Runner`
and a fake `app.MigrationContributor`. The seam is intentionally small:
`AppBuilder` → `*app.Application`, the rest is registry plumbing.

## Contract invariants

- Every command prepares the production application graph but never starts
  listeners, consumers, invokers, or application migration hooks.
- User, operational, and drift outcomes retain distinct exit codes and
  structured output preserves records completed before a later error.
- Returning from a command explicitly closes the dependency container created
  by `Prepare`; `Application.Stop` alone is insufficient because the prepared
  application never became running.

Support is **stable** in `../../../../putnami.support.json`; the normative
feature contract is `specs/migration-cli.json`, with the decision in
`doc/adr/0001-prepare-production-graph-without-runtime-start.md`.
