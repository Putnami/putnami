# Migrations

`go.putnami.dev/migration` defines the backend-neutral registry and runner
contracts. `go.putnami.dev/database` owns SQL execution, while
`go.putnami.dev/migratecli` drives the production application graph as a
one-shot command without starting listeners or workloads.

Both public modules are **stable** and owned by the Go SDD surface. Their
migration-execution and CLI specifications live next to their package sources.

## Ownership model

- A feature plugin contributes `migration.Source` values.
- A backend plugin registers one `migration.Runner` for each kind it owns.
- The registry processes kinds in lexical order and fails on orphan sources
  before runtime execution, except during explicit source-only metadata work.
- Each runner owns its atomic boundary. The SQL runner applies the SQL and
  records that one migration in the same database transaction.

There is no cross-kind or cross-datasource distributed transaction. A failure
stops the current registry operation and returns the records already completed.

## Service migration command

Reuse the exact builder used by the server:

```go
package main

import (
    "os"

    "go.putnami.dev/migratecli"

    "example.com/service/internal/wire"
)

func main() {
    os.Exit(migratecli.Run(wire.BuildApp))
}
```

The CLI calls `Application.Prepare`, discovers the resulting registry, and
never calls `Start`. Because preparation can open eager dependencies, every
return path explicitly closes the prepared dependency container.

| Command | Result |
|---------|--------|
| `up [to name]` | Apply pending work, optionally through one target |
| `down [to name]` | Roll back the latest reversible work, optionally to one target |
| `status` | Show known and recorded migrations |
| `verify` | Report pending or drifted migrations |
| `inspect` | Dump the prepared registry without executing migration-state operations |

`up`, `down`, and `status` accept table, JSON, or JSONL output. Exit code `1`
means invalid command input, `2` an operational failure, and `3` detected
drift. `inspect` is not guaranteed offline because application preparation may
open eager dependencies.

## Drift and rollback

Versioned migration drift fails closed. Repeatable migration drift is applied
again. Rollback is available only when the migration declares a reverse
operation; applied SQL rollback remains possible because the runner persists
the reverse SQL with the migration record.

Accepted decision records next to the registry and CLI package sources document
these boundaries.

## Related guides

- [Persistence](/docs/frameworks/go/persistence) — SQL definitions and pools
- [Plugins & Lifecycle](/docs/frameworks/go/plugins-and-lifecycle) — application preparation and shutdown
- [Testing](/docs/frameworks/go/testing) — project gates
