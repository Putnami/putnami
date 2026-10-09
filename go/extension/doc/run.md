# Run

**Command:** `putnami run [project]`

Builds a Go workload for the host, runs it **once**, and forwards the child
process exit code. Unlike `serve` — which keeps a resident dev server alive and
restarts it on file changes — `run` is one-shot: it is the verb for validating
one-shot Jobs (a migration runner, a batch worker, a CLI) end-to-end locally
without falling back to a raw `go build`/`go run`.

## Overview

- Compiles a **host** binary with `go build`, then executes it a single time
- Streams the workload's stdout/stderr live as it runs
- Passes the ambient environment through to the child unchanged
- **Forwards the child's exact exit code** as the `putnami run` exit code, so a
  gate that returns `3` surfaces as `3` (not a generic failure)
- Runs the same `generate`/`describe` codegen pipeline as `serve`, so
  framework apps that embed generated artifacts run correctly

> `run` compiles and executes a real binary rather than using `go run`, because
> `go run` collapses every non-zero program exit to `1` — which would defeat
> exit-code forwarding.

**Activation:** Any project containing `go.mod` or `*.go` files. Library
projects are skipped (no runnable entrypoint).

## Entrypoint Auto-Detection

`run` resolves the entrypoint exactly like [`serve`](./serve.md): explicit
`--entrypoint`, then a root `main.go`, then a single `cmd/<name>/`, then the
preference heuristics (`serve`, `server`, `<project-name>`, `api`).

## Usage

```bash
# Run the workload once and forward its exit code
putnami run my-job

# Explicit entrypoint (cmd/ layout)
putnami run my-service --entrypoint ./cmd/migrate

# Pass arguments and a port
putnami run my-job --args "--check --dry-run" --port 8080

# Assert a specific exit code from a gate (e.g. a migration guard)
putnami run my-migrator --args=--gate; echo "exit: $?"
```

`--args` is split on whitespace into the program's arguments. A one-word value
that begins with a hyphen needs the `=` spelling (`--args=--gate`): the CLI
reads `--args --gate` as two flags. See
[Job-Specific Flags](../../../tooling/cli/doc/03-commands.md#job-specific-flags).

## Exit Codes

| Exit code | Meaning |
|-----------|---------|
| `0` | The workload exited successfully |
| `N` (non-zero) | The workload's own exit code, forwarded verbatim |
| `130` | The run was interrupted by `SIGINT`/`SIGTERM` |

A stop request (`SIGINT`/`SIGTERM`, or the console's `CTRL_BREAK_EVENT` or
Ctrl+C on Windows) reaches the workload. A workload still running 3 seconds
after the request is killed, on every OS, and the run exits with `130`. This
ends before the CLI's own force kill of the job's process tree, 5 seconds after
the request.

## API Reference

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--entrypoint <path>` | auto-detect | Package path to run (e.g. `./cmd/migrate`) |
| `--port <n>` | — | Sets the `PORT` environment variable when provided |
| `--race` | `false` | Run with the Go race detector |
| `--args <string>` | — | Extra arguments passed to the program, split on whitespace |

## Boundaries

- **Scope**: project-only — workspace-level runs are not supported
- **One-shot**: `run` does not watch or restart; use `serve` for a resident dev
  server
- **Library projects**: skipped, as they have no runnable entrypoint
