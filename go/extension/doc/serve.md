# Serve

**Command:** `putnami serve [project]`

Runs a Go application as a server. In development mode the server restarts automatically when source files change. In production mode it runs without file watching.

`serve` is **build-and-exec**: it compiles the entrypoint it is about to run, for
the host platform, and runs it. It emits no binary — not into `.gen`, not into a
command output directory. A served process is not an artifact.

## Overview

- **Development mode**: auto-detects the entrypoint and uses `go run` with file watching and automatic restart (500 ms debounce)
- **Production mode**: compiles the same entrypoint into a scratch directory, execs it, and deletes it on exit
- Requires no prior `putnami build` in either mode, and schedules no compile step of its own
- Supports both simple projects (root `main.go`) and `cmd/` layouts (`cmd/serve/main.go`, `cmd/api/main.go`, etc.)
- Monitors `.go` files in the project and all dependency projects
- Ignores `vendor/`, test files, and common generated directories
- Tracks `go.sum` and `go.work` changes (dependency graph updates trigger a restart)
- Sets `PORT` environment variable from `--port` flag
- Runs the resident server as `NO-CACHE`; a cache hit cannot reproduce a live process or bound port

**Activation:** Any project containing `go.mod` or `*.go` files.

## Entrypoint Auto-Detection

When `--entrypoint` is not provided, the serve command automatically resolves which package to run:

1. **Root `main.go`** — if the project root contains `main.go`, uses `.` (simple project layout)
2. **Single `cmd/` subdirectory** — if only one `cmd/<name>/` contains Go files, uses it automatically
3. **Multiple `cmd/` subdirectories** — applies preference heuristics in order:
   - `cmd/serve/` — conventional name for the server binary
   - `cmd/server/` — alternative conventional name
   - `cmd/<project-name>/` — matches the project identity
   - `cmd/api/` — common API server convention
4. **No match** — fails with an error listing available options, prompting you to use `--entrypoint`

Resolution runs **once**, before mode selection, and both modes run the same
package. They differ only in how they get from source to a running process.

## Execution Flow

1. **Setup**: resolve port from `--port`, `PORT` env, or default `8080`; set `PORT` environment variable
2. **Kill port** (if `--kill-port`): terminate the process listening on that port using `lsof`/`fuser`
3. **Resolve entrypoint**: see [Entrypoint Auto-Detection](#entrypoint-auto-detection); a failure here fails the job before anything is compiled
4. **Mode selection**:
   - **Development** (`NODE_ENV != production` AND `--watch` is `true`): compile and run with `go run`; watch for file changes and restart
   - **Production** (`NODE_ENV=production` OR `--watch=false`): `go build` the entrypoint into a temporary directory, exec the result, and remove the directory when serve returns

## Usage

### Development (default)

```bash
putnami serve .
```

### Projects with cmd/ layout

```bash
# Auto-detects cmd/serve/ or cmd/api/ automatically
putnami serve my-service

# Explicit entrypoint if auto-detection doesn't pick the right one
putnami serve my-service --entrypoint ./cmd/api
```

### Custom port

```bash
putnami serve . --port 3000
```

### Kill existing process on port first

```bash
putnami serve . --port 8080 --kill-port
```

### With race detector

```bash
putnami serve . --race
```

### Pass arguments to the program

```bash
putnami serve . --args "--config ./config.yaml --debug"
```

### Production (compiled binary, no watching)

```bash
NODE_ENV=production putnami serve .

# Or disable watch explicitly
putnami serve . --watch=false
```

## Development Mode Details

Development mode uses `go run <entrypoint>` to compile-and-run the application in a single step. The entrypoint is resolved automatically (see [Entrypoint Auto-Detection](#entrypoint-auto-detection)) or set explicitly with `--entrypoint`.

**File watching behaviour:**
- Watches all `*.go` files in the project directory (recursive)
- Also watches `*.go` files in all dependency projects (from `putnami.json` `dependencies`)
- Tracks `go.sum` and `go.work` — changes here trigger a restart
- Ignores `vendor/`, `.git/`, `node_modules/`, `.putnami/`, `dist/`, `bin/`
- Uses a content-hash snapshot (SHA256 of `path + mtime`) to detect changes
- 500 ms debounce after the last change before restarting

**Signal handling:**
- `SIGINT`/`SIGTERM` gracefully stop the running process. On Windows the stop request is the console's `CTRL_BREAK_EVENT` or Ctrl+C
- A process still running 3 seconds after the stop request is killed, on every OS. This ends before the CLI's own force kill of the job's process tree, 5 seconds after the request
- Exit codes 0 and 128–143 are treated as normal shutdown (no error)

## Production Mode Details

Production mode compiles the resolved entrypoint for the host with `go build`,
into a temporary directory, and execs the result. The directory is removed when
serve returns, so nothing accumulates on disk between runs.

```bash
NODE_ENV=production putnami serve .
```

A compile failure fails the job with the toolchain's own diagnostics; the process
is never started against a stale binary.

Two boundaries are worth naming, because they are the reason the mode looks the
way it does:

- **The compile is workspace-scoped, the run is not.** The build re-points
  `GOWORK` at the governing `go.work` so workspace `replace` directives resolve
  (an inherited `GOWORK=off` would otherwise send framework `v0.0.0` placeholders
  to the module proxy). The exec inherits your environment verbatim, because the
  server — and any tooling it spawns — must see the environment you gave it. The
  boundary is between *building* and *running*, not between dev and prod.
- **Nothing is emitted.** `serve` previously ran `putnami build`'s compile step
  ahead of itself, wrote a binary tree into the command's output directory, and
  then ignored it — `go run` compiled the program a second time. That was one
  entire discarded compile per served project, and it coupled `serve` to another
  command's artifact layout. If you want a binary on disk, that is
  [`putnami build`](./build.md) or [`putnami package`](./package.md); `serve`
  produces a running process.

## Environment Variables

| Variable | Set by | Description |
|----------|--------|-------------|
| `PORT` | `--port` flag or default `8080` | Server port — read this in your Go code |
| `NODE_ENV` | caller | Set to `production` to enable production mode |

Read the port in your Go application:

```go
port := os.Getenv("PORT")
if port == "" {
    port = "8080"
}
log.Fatal(http.ListenAndServe(":"+port, nil))
```

## API Reference

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--watch` / `-w` | `true` | Watch for changes and restart (disabled in production mode) |
| `--port <n>` | `8080` | Port number; sets the `PORT` environment variable |
| `--kill-port` | `false` | Terminate any process on the port before starting |
| `--race` | `false` | Run with the Go race detector |
| `--args <string>` | — | Extra arguments passed to the program |
| `--entrypoint <path>` | auto-detect | Package path to run (e.g. `./cmd/api`). Auto-detected from project layout if not set |

## Boundaries

- **Scope**: project-only — workspace-level runs are not supported
- **No prerequisite**: both modes compile what they run; a prior `putnami build` is neither required nor consumed
- **No emitted artifact**: the served binary lives in a temporary directory and is deleted on exit
- **Host platform only**: both modes build for the host; cross-compilation is a build-time concern
- **Libraries cannot be served**: a project with no `package main` plans no `serve` (see [Project classification](./build.md#project-classification))
- **Port detection**: `--kill-port` uses `lsof` or `fuser`; neither is available on all systems. On Windows it kills nothing and prints a warning
