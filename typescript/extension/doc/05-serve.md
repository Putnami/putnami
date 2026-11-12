# Serve

The serve command starts a development server with hot-reload using Bun's runtime. It resolves the application entrypoint, manages port allocation, and forwards structured logs from the application process.

## Overview

- Resolves the serve entrypoint from `package.json` exports or an explicit flag
- Runs a generate phase before serving to ensure generated code is current
- Manages port allocation with conflict detection
- Forwards JSON-structured logs from the application as JSONL events
- Supports Bun's built-in debugger/inspector
- Handles signal forwarding (SIGINT/SIGTERM) to the child process
- Runs the resident server as `NO-CACHE`; a cache hit cannot reproduce a live process or bound port

## Usage

### Basic Usage

```bash
putnami serve .
```

### Common Patterns

```bash
# Serve on a specific port
putnami serve . --port 8080

# Serve with debug-level logging
putnami serve . --debug

# Kill the process listening on the port before starting
putnami serve . --kill-port

# Serve with a custom entrypoint
putnami serve . --entrypoint src/server.ts

# Attach Bun debugger
putnami serve . --inspect
```

## Execution Flow

```text
generate ──→ resolve entrypoint ──→ resolve port ──→ start bun
```

1. **Generate** — runs the build generate phase (pre-build hooks, version info)
2. **Resolve entrypoint** — determines which file to run
3. **Resolve port** — determines the port number
4. **Start** — executes `bun run --port=<port> <entrypoint>` with the appropriate environment

### Entrypoint Resolution

The entrypoint is resolved in this order:

1. Explicit `--entrypoint` flag value
2. `./serve` export from `package.json` exports

If no entrypoint is found, the command fails with an error.

### On the Node runtime

A project that declares `"runtime": "node"` under
`options["@putnami/typescript"]` in its `putnami.json` serves on Node instead
of Bun:

```json
{
  "options": { "@putnami/typescript": { "runtime": "node" } }
}
```

```text
generate ──→ resolve entrypoint ──→ bundle for node ──→ start node --enable-source-maps
```

1. **Entrypoint** — `--entrypoint`, else the `./serve` export, else the
   [run entrypoint](06-run.md#entrypoint-resolution): `./run`, `main`,
   `src/main.ts`.
2. **Bundle** — Bun bundles the entrypoint for Node, as the compile step does
   (see [Build](02-build.md#the-node-runtime)), into the command's output
   directory, `.putnami/out/<project>/~serve/node/`. The bundle resolves the
   packages it leaves as imports from the project's `node_modules`.
3. **Start** — `node --enable-source-maps <bundle>`, with the environment, the
   port (`PORT`) and the log forwarding below. `--inspect`, `--inspect-wait`
   and `--inspect-brk` pass to node.
4. **Watch** — on by default outside production. Every half second the command
   reads the files the bundle was built from, as bun's metafile lists them,
   and the project's `package.json`; files under `node_modules` are not read.
   A workspace library the bundle includes is watched like the project's own
   sources, and a file the workload writes at run time, such as a database,
   a log or an upload, restarts nothing. After a change it bundles again into
   a staging directory and restarts node on the new bundle. A change that does
   not bundle is reported, and node keeps serving the previous bundle, which
   stays in place. When node exits on its own, the next change that bundles
   starts it again. Node stays in the job's process group, so ending the job
   ends it. A restart stops node alone: SIGTERM, then SIGKILL after 3
   seconds. Once node exited, the command waits at most 2 seconds more for
   its output, so a process node started that keeps that output open holds
   no restart, and what that process writes afterwards is not forwarded.

The bundle resolves the packages it leaves as imports through
`node_modules` in the bundle directory, a link (a junction on Windows) to the
project's `node_modules`, then through each directory above it up to the
workspace root. A package that only a workspace library depends on, and that
the install placed under that library's own `node_modules` (pnpm does), is
not found there.

`node` must be on `PATH`; otherwise serve fails with
`typescript.node_not_found`. The Putnami server framework
(`@putnami/application` and the packages built on it) calls Bun APIs and stays
on Bun: a Node workload brings its own server, such as Express, Fastify or
NestJS.

### Port Resolution

The port is resolved in this order:

1. Explicit `--port` flag value
2. `PORT` environment variable
3. Default: `3000`

### Environment

The serve command sets:

| Variable | Value | Condition |
|----------|-------|-----------|
| `NODE_ENV` | `development` | Only if not already set and not in production mode |
| `PORT` | Resolved port | Always |
| `LOG_LEVEL` | `debug` | Only when `--debug` is set |

**Production mode** is detected when `NODE_ENV=production` or `ENV=production`. In production mode:
- Debugger flags are ignored
- Bun runs with `--no-install --smol` for smaller memory footprint
- `NODE_ENV` is not overridden

### Log Forwarding

The serve command captures stdout/stderr from the application process and re-emits them as structured JSONL events.

When the application outputs JSON logs (as produced by the framework's `JSONSink` logger), the command:
- Parses the `severity` and `message` fields
- Maps severity: `ERROR` → error, `WARNING`/`WARN` → warn, `INFO` → info, `DEBUG` → debug
- Preserves additional context fields from the JSON log
- Extracts `error` objects with message and stack trace

Non-JSON output is forwarded as plain text log events.

### Signal Handling

The serve command forwards `SIGINT` and `SIGTERM` to the child process. Signal exits (exit codes 128–143) are treated as successful termination.

## API Reference

### Flags

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--entrypoint` | `string` | — | Explicit entrypoint file to serve. Overrides `./serve` export resolution |
| `--watch` / `-w`, `--no-watch` | `boolean` | `true` | Auto-restart on file changes (Bun's built-in watch mode); `--no-watch` or `--watch=false` runs the server once, with no restarts |
| `--port` | `number` | `3000` | Server port. Also exported as `PORT` env var to the application |
| `--kill-port` | `boolean` | — | Best effort: kill the process listening on the port before starting; a client connected to the port keeps running. It uses `lsof`, or `fuser` when `lsof` is absent, and kills nothing when neither is installed or `lsof` is the BusyBox applet of Alpine images. Not available on Windows: it prints a warning instead |
| `--debug` | `boolean` | — | Set `LOG_LEVEL=debug` in the application environment |
| `--inspect` | `boolean` | — | Activate Bun's debugger (WebSocket inspector) |
| `--inspect-wait` | `boolean` | — | Activate debugger and wait for a connection before running |
| `--inspect-brk` | `boolean` | — | Activate debugger and break on the first line |

## Boundaries

- **Scope**: Starting a development server, port management, log forwarding, debugger integration
- **Out of scope**: Building the project (handled by the build pipeline dependency), hot module replacement (Bun handles this natively), multi-service orchestration (handled by the SDK scheduler)
- **Dependencies**: Requires Bun, and on the Node runtime also `node` on `PATH`. A project on Bun must have a `./serve` export in `package.json` or use `--entrypoint`.
- **Extension points**: None. Serve behavior is controlled through CLI flags and environment variables.
