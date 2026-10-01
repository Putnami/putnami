---
order: 14
---

# Run Command

**Command:** `putnami run [project]`

**Purpose:** Run a Python workload **once** for the host and forward the child
process exit code. Unlike `serve` (a resident dev server that restarts on file
changes), `run` is one-shot — the verb for validating one-shot Jobs (a
migration runner, a batch worker, a CLI) end-to-end locally.

## Usage

```bash
putnami run <project> [options]
```

## Arguments

- `project`: The name or directory of the package to run.

## Options

- `--entrypoint <path>`: Entry point to run (default: `src/main.py`).
- `--port <number>`: When provided, exposed as the `PORT` environment variable.

## Behavior

- Runs `uv run <entrypoint>` once — UV builds/resolves the environment for the
  host and executes the program a single time.
- Streams the workload's stdout/stderr live as it runs.
- Passes the ambient environment through to the child.
- **Forwards the child's exact exit code** as the `putnami run` exit code, so a
  gate that returns `3` surfaces as `3` (not a generic failure).

## Exit Codes

| Exit code | Meaning |
|-----------|---------|
| `0` | The workload exited successfully |
| `N` (non-zero) | The workload's own exit code, forwarded verbatim |

## Examples

```bash
putnami run api-server
putnami run migrate-runner --entrypoint src/migrate.py
putnami run batch-job --port 8080

# Assert a specific exit code from a gate
putnami run migrate-runner; echo "exit: $?"
```
