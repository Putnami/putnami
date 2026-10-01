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

# Kill any process using the port before starting
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
| `--watch` / `-w` | `boolean` | `true` | Auto-restart on file changes (Bun's built-in watch mode) |
| `--port` | `number` | `3000` | Server port. Also exported as `PORT` env var to the application |
| `--kill-port` | `boolean` | — | Kill any process using the port before starting. Not available on Windows: it prints a warning instead |
| `--debug` | `boolean` | — | Set `LOG_LEVEL=debug` in the application environment |
| `--inspect` | `boolean` | — | Activate Bun's debugger (WebSocket inspector) |
| `--inspect-wait` | `boolean` | — | Activate debugger and wait for a connection before running |
| `--inspect-brk` | `boolean` | — | Activate debugger and break on the first line |

## Boundaries

- **Scope**: Starting a development server, port management, log forwarding, debugger integration
- **Out of scope**: Building the project (handled by the build pipeline dependency), hot module replacement (Bun handles this natively), multi-service orchestration (handled by the SDK scheduler)
- **Dependencies**: Requires Bun runtime. The project must have a `./serve` export in `package.json` or use `--entrypoint`.
- **Extension points**: None. Serve behavior is controlled through CLI flags and environment variables.
