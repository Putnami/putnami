# The migrate CLI

`@putnami/migration/cli` is a kind-agnostic driver. It builds the production
application graph, calls `prepare()` so the migration registry is populated,
then dispatches the requested subcommand directly against the registry — no
HTTP listener starts, no `start()` runs.

## Wiring

A service binary's `bin/migrate.ts`:

```ts
#!/usr/bin/env bun
import { runMigrate } from '@putnami/migration/cli';
import { buildApp } from '../src/app';

process.exit(await runMigrate(buildApp, process.argv.slice(2)));
```

`runMigrate(build, argv, streams?)` returns the exit code; the caller wraps it
with `process.exit`. `runMigrateAndExit(build)` is the one-line convenience that
reads `process.argv.slice(2)` and exits for you.

- `build` is an `AppBuilder` — a zero-arg function returning an `AppLike`
  (`prepare()`, `stop()`, `getMigrationRegistry()`). The production `application()`
  satisfies this.
- `streams` is optional; override `stdout` / `stderr` to capture output (used in
  tests).

## Subcommands

| Subcommand | Effect |
|------------|--------|
| `up` | Apply all pending migrations across kinds (`force`). |
| `up to <name>` | Apply forward through `<name>`, inclusive. |
| `down` | Roll back the most recently applied migration. |
| `down to <name>` | Roll back every migration applied after `<name>`. |
| `status` | Per-row state of every kind. |
| `verify` | Drift report; exits `3` when any kind has drift. |
| `inspect` | JSON dump of the registry view (no database access). |

`<name>` may be the fully namespaced form (`iam/20260520120000_create_users`) or
the bare basename when unambiguous across namespaces. A bare argument without
the `to` keyword (e.g. `up iam/001`) is rejected as a user error.

`up` passes `force: true`, overriding any per-runner auto-apply gating.

`down` is destructive, so a partial failure is never reported as success: every
kind with a runner is attempted, the records that *did* roll back are still
printed, and every kind's error is surfaced (aggregated when more than one kind
fails) with a non-zero exit.

## Output format

`--output table|jsonl` (default `table`) selects the format and may appear
anywhere in the arguments.

- `table` — aligned columns (`KIND NAMESPACE NAME STATUS TARGET SOURCE`) plus
  human-readable prose for empty results and drift.
- `jsonl` — one JSON object per line (one `Record` per line for `status` / `up` /
  `down`; one `DriftReport` per line for `verify`; one kind summary per line for
  `inspect`). Suited to piping into `jq`.

An invalid value exits `1`.

## Exit codes

| Code | Meaning |
|------|---------|
| `0` | Success. |
| `1` | User error — unknown subcommand, missing/extra argument, bad `--output`, missing `AppBuilder`. |
| `2` | Operational error — `prepare()` failed, or a runner threw during the command. |
| `3` | Drift detected by `verify`. |

`help`, `-h`, and `--help` print usage and exit `0`. Running with no subcommand
prints usage to stderr and exits `1`.

These codes match the Go migrate driver, so a CI pipeline can treat both the
same way (notably: gate a deploy on `verify` returning `3`).
