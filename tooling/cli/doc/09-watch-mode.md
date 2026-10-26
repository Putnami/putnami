# Watch Mode

The `--watch` flag enables file-watching mode for any job command. When active, the CLI monitors the workspace for file changes, determines which projects and tasks are affected, and re-executes only what's needed.

## Usage

```bash
# Watch and re-run tests on change
putnami test . --watch

# Watch and re-lint on change
putnami lint . --watch

# Watch and rebuild on change
putnami build . --watch

# Watch with serve (hot-reload)
putnami serve my-app --watch
```

## How It Works

### File Watching

The watcher polls the workspace for file system changes using a configurable interval (default: 100ms). Changes are debounced with a 150ms window — rapid edits within this window are coalesced into a single batch. The following directories are automatically excluded:

- `.git/`
- `node_modules/`
- `.putnami/`
- `dist/`
- `build/`
- `__pycache__/`
- `.venv/`

A change to a path Git ignores (through `.gitignore`, `.git/info/exclude` or the global excludes file) is dropped as well, so a log or a browser snapshot written into `.context/`, `log/` or `.playwright-mcp/` re-runs nothing. A dotenv file, any file whose name starts with `.env`, still counts wherever it is: `.env` and `conf/.env.local.yaml` are ignored because they hold local values, and a runtime or a config task reads them. A tracked file always counts, and outside a Git repository every change counts.

### Change Classification

When changes are detected, the classifier:

1. **Maps files to projects** — each changed file is associated with its containing project based on directory paths.
2. **Matches task inputs** — the classifier checks task `inputs.*.files` globs to determine which specific tasks are affected.
3. **Propagates transitively** — if project A changes and project B depends on A, B's downstream tasks are also re-run.

### Execution

- **Initial run**: the full plan executes on startup.
- **Subsequent runs**: only affected projects/tasks are re-planned and re-executed.
- **Cache disabled**: watch mode forces `--no-cache` to ensure fresh results.
- **Session recording**: each iteration creates a child session for the audit trail.

## Serve Mode

A command whose traits declare the `serve` workload, such as `serve`, watches without `--watch`, whatever its name. A `serve` that declares no workload does not. To run the server once, with no restarts, turn the command's own `watch` flag off:

```bash
putnami serve my-app --no-watch
putnami serve my-app --watch=false   # the same
```

When watching with the `serve` command, the behavior is specialized:

1. **Dependency rebuild** — if a library dependency changes, it is rebuilt first.
2. **Server restart** — the server process is gracefully restarted (SIGTERM → wait → SIGKILL if needed).
3. **Logs preserved** — server output remains visible between restarts (the terminal is not cleared).

### Readiness gating

A serve iteration does not arm the file watcher until the server reports that it
is listening — otherwise the watcher's own startup writes would restart the
server it just started.

That report is the typed `ready` event of runtime event protocol v2
([`protocols/runtime`](../../../protocols/runtime/doc/05-event-v2.md)), which a
serve job emits once per iteration, on the initial start and on every restart.
The watcher arms on the iteration's **first** `ready` event and re-arms from
scratch for the next one.

Readiness travels because every loadable extension speaks v2 (see
[04-job-execution.md](04-job-execution.md#protocol-version-negotiation)): CLI
contract 3 requires it, and the CLI rejects a v1 stream outright. The window in
which a v1 extension ran its serve iteration normally but never armed the
watcher — the server started, file changes did not restart it, and nothing said
so — closed with the contract-3 flip: such an extension no longer loads, and the
message names the fix. Before 0.3.0 readiness was inferred from the workload's
log text (`listening http://`), which made any wording change a silent
hot-reload outage.

### Process Lifecycle

The managed process lifecycle follows this sequence:

- **Start**: spawn the server subprocess with process group isolation.
- **Graceful stop**: send SIGTERM to the process group, wait up to 5 seconds.
- **Force kill**: if the process hasn't exited, send SIGKILL.
- **Restart**: stop the current process, then start a new one.

### Port Management

The port manager detects conflicts and suggests alternatives:

```
port 3000 is in use, try port 3001
```

Between restarts, the serve loop waits up to 2 seconds for the stopped
server's port to free up before it starts the next one. The port it waits on
is:

- the `--port` value, when it names a port;
- the default port, 3000, when the run sets no `--port`;
- with `--port 0`, the port of the last `ready` claim that announced one in
  the stopped iteration: the first `http` or `https` endpoint, else the first
  endpoint. A claim without an endpoint leaves it. When no claim announced a
  port, the loop waits on nothing.

## Output

Watch mode renders status information around the base renderer's job output:

```
  lib build done  0.8s
  app build done  1.2s

  2 succeeded  0 cached  1.2s

  [watch] waiting for changes...  (1.2s)

  [watch] change detected — re-running
    src/lib.ts
  [watch] affected: lib, app

  lib build done  0.3s
  app build done  0.5s

  2 succeeded  0 cached  0.5s

  [watch] waiting for changes...  (0.5s)
```

## Architecture

```
internal/watch/
├── watcher.go       # Polling-based file system watcher with debounce
├── classifier.go    # Maps changed files → affected projects/tasks
├── session.go       # Watch session lifecycle and execution loop
├── lifecycle.go     # Process lifecycle management (start/stop/restart)
├── ports.go         # Port allocation and conflict detection
└── renderer.go      # Watch-specific output wrapper
```

Watch is a **replan policy over the engine**, not a second run loop: every iteration goes back through `engine.Engine.Run` (the seam is bound in `internal/engine/watch.go`), which owns planning, scheduling and the canonical result. Watch itself contributes change detection, the per-iteration flag policy (no run marker, no remote cache, no preflight), and a renderer wrapper that adds watch-specific status output.
