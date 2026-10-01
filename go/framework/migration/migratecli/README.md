# go.putnami.dev/migratecli

Kind-agnostic CLI driver for the transversal migration framework. A
service's `cmd/migrate/main.go` is ~3 lines:

```go
package main

import (
	"os"

	"go.putnami.dev/migratecli"

	"example.com/ledger/internal/wire"
)

func main() {
	os.Exit(migratecli.Run(wire.BuildApp))
}
```

`wire.BuildApp` is the same function the service's `cmd/server` already
imports to construct `*app.Application` — typically
`app.New("ledger").Use(database.NewPlugin(...)).Use(iam.NewPlugin()).Use(ledger.NewPlugin())`.
The migrate binary reuses it verbatim; only `Prepare()` runs (no HTTP,
no event consumers, no Invoke functions).

## Subcommands

| Command           | Behavior                                                       |
| ----------------- | -------------------------------------------------------------- |
| `up`              | Apply all pending migrations across kinds.                     |
| `up to <name>`    | Apply forward through `<name>` (inclusive).                    |
| `down`            | Roll back the most recently applied migration per kind.        |
| `down to <name>`  | Roll back every migration applied after `<name>`.              |
| `status`          | Tabular per-row state of every kind.                           |
| `verify`          | Drift report (hash drift, missing rows, pending).              |
| `inspect`         | JSON dump of the registry view (runs no migrations; `Prepare` still runs). |

Every subcommand, including `inspect`, runs the app's `Prepare` phase before
dispatching. `inspect` performs no migration-state I/O of its own, but plugins
that open connections during `Configure` will still do so, so it is not
guaranteed to run without a reachable database.

### Machine-readable output

`up`, `down`, and `status` accept `--output=table|json|jsonl` (also the
space-separated `--output json` form). The default is `table` (the human
tabwriter rendering). For CI/deploy pipelines that assert on which migrations
ran, prefer the structured formats:

- `--output=json` — a single JSON array of records (empty result is `[]`).
- `--output=jsonl` — one JSON record per line (streamable; empty result is no
  lines).

Both reuse the `migration.Record` JSON shape (`kind`, `namespace`, `name`,
`status`, `target`, `source`, …), the same shape `inspect` already emits.

## Exit codes

- `0` — success.
- `1` — user error (unknown subcommand, missing argument).
- `2` — operational error (DB unreachable, runner error).
- `3` — drift detected by `verify`.

## Why a separate module?

`migratecli` imports both `go.putnami.dev/app` and
`go.putnami.dev/migration`. The `app` package itself depends on
`migration`. If the CLI lived inside the `migration` module, that module
would gain an `app` dep, which would form a circular module
relationship. Splitting `migratecli` out keeps the dependency direction
clean: `migratecli → app → migration`, never the other way.

## Support and contract

The SDD owner is `go`. `go.putnami.dev/migratecli` is public, documented,
maintained, and classified **stable** in the repository
[support catalog](../../../../putnami.support.json). Before v1.0, minor releases
may still require documented migrations; see the repository
[release policy](../../../../RELEASE.md).

The durable command contract is [`go/migration-cli`](specs/migration-cli.json).
Production-graph preparation and explicit prepared-container cleanup are
recorded in [ADR 0001](doc/adr/0001-prepare-production-graph-without-runtime-start.md)
and protected by [`cli_test.go`](cli_test.go).
