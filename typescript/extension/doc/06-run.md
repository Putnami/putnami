# Run

**Command:** `putnami run [project]`

Runs a TypeScript workload **once** for the host and forwards the child process
exit code. Unlike `serve` — which keeps a resident dev server alive and restarts
it on file changes — `run` is one-shot: it is the verb for validating one-shot
Jobs (a migration runner, a batch worker, a CLI) end-to-end locally.

## Overview

- Runs `bun run <entrypoint>` once — Bun transpiles on the fly, so this is the
  "build for the host" step
- Streams the workload's stdout/stderr live as it runs
- Passes the ambient environment through to the child (`NODE_ENV=development`
  is defaulted only when unset)
- **Forwards the child's exact exit code** as the `putnami run` exit code, so a
  gate that returns `3` surfaces as `3` (not a generic failure)
- Runs the same `generate` codegen step as `serve` before executing

**Activation:** Any project containing `package.json` or `src/**/*.ts`. Library
projects are skipped (no runnable entrypoint).

## Entrypoint Resolution

When `--entrypoint` is not provided, `run` resolves the entrypoint in order:

1. The `./run` export in `package.json` (`exports`)
2. The package's `main` field
3. `src/main.ts`

If none resolve, the job fails with `NO_RUN_ENTRYPOINT`.

## Usage

```bash
# Run the workload once and forward its exit code
putnami run my-job

# Explicit entrypoint
putnami run my-app --entrypoint src/cli.ts

# Pass a port and extra arguments
putnami run my-job --port 3000 --args "--check --dry-run"

# Assert a specific exit code from a gate
putnami run my-migrator --args "--gate"; echo "exit: $?"
```

## Exit Codes

| Exit code | Meaning |
|-----------|---------|
| `0` | The workload exited successfully |
| `N` (non-zero) | The workload's own exit code, forwarded verbatim |
| `130` | The run was interrupted by `SIGINT`/`SIGTERM` |

## API Reference

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--entrypoint <path>` | `./run` export → `main` → `src/main.ts` | File Bun should run |
| `--port <n>` | — | Sets the `PORT` environment variable when provided |
| `--args <string>` | — | Extra arguments passed to the program |

## Boundaries

- **Scope**: project-only — workspace-level runs are not supported
- **One-shot**: `run` does not watch or restart; use `serve` for a resident dev
  server
- **Library projects**: skipped, as they have no runnable entrypoint
