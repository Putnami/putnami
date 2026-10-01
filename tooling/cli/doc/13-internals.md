# Internals

This document covers the Go-specific patterns, architectural decisions, concurrency model, error handling, and testing approach used in the CLI implementation.

## Module and Package Layout

The CLI is two modules, not one. [ADR 0006](adr/0006-cli-model-and-command-verticals.md) records why.

### `go.putnami.dev/cli/model` — `tooling/cli-model/`

The data model, extracted so it can be read, tested and depended on without the orchestrator that drives it. Three packages, each named after the internal package it came from so call sites read the same either way (`workspace.Project`, `jobs.JobResult`):

| Package | Holds |
|---------|-------|
| `workspace` | `Workspace`, `Project`, `ScopeContribution`, the dependency graph, target/filter/include resolution, identity, the probe view, change→project impact mapping, auto-selection inputs |
| `extension` | `ExtensionDescription`, `JobDefinition`, manifest and contract types, pipeline expansion, the expression evaluator, flag declarations, reserved-provider rules |
| `jobs` | `ScheduledJob`, `JobResult`, `Execution`, the plan contract, the canonical result reducer, the runtime-event parser, invocation and identity types, task resource profiles, the job-context shape |

Type declarations and pure methods over them, and nothing else. **`tooling/cli-model/go.mod` requires only `go.putnami.dev/protocol/*`** — a model package that reaches back into the CLI does not build. Anything with an effect (loading a `putnami.json`, discovering extensions, spawning a job, computing a cache entry, rendering a run) stays in `tooling/cli`.

Each origin package in `tooling/cli` keeps a `model_alias.go` of `type X = model.X` aliases, so a consumer moves onto the model one import line at a time.

### `go.putnami.dev/tooling/cli` — `tooling/cli/`

```
cmd/putnami/main.go           Entry point
internal/
├── cli/                       Argument parsing, routing, help, the structural ratchets
├── commands/                  Container for the command verticals (see below)
├── engine/                    Engine.Run — the single run lifecycle
├── extension/                 Extension discovery, install, integrity, registry, lockfile I/O
├── git/                       Git operations (branch, diff, merge-base)
├── hooks/                     Lifecycle hooks: extension (onInstall, preBuild) and workspace
├── jobs/                      Planning, DAG scheduling, subprocess execution, caching
├── output/                    Renderers (text, live TUI, JSONL, cloud-logging)
├── machine/                   Machine-readable run/report documents
├── changeplan/                CI change plans
├── profiler/                  Chrome trace format profiling
├── store/                     Content-addressed cache store (local, remote, chained)
├── telemetry/                 Anonymous opt-in metrics
├── watch/                     File watching, change classification, serve lifecycle
├── workspace/                 Workspace detection, project discovery, probing, syncing
└── workspace_state/           Session recording and audit trails
```

Principal packages only. `internal/` holds 52 production packages in all — the rest are small, single-purpose support packages (`abort`, `agentartifacts`, `cmderr`, `env`, `flock`, `iox`, `launch`, `layout`, `lockfile`, `mcp`, `progress`, …). `core_isolation_ratchets_test.go` pins the count as a ceiling, so a new package is a reviewed event.

### Command verticals — `internal/commands/`

`internal/commands` is a container directory, not a package with logic of its own (see its `doc.go`). One package per command surface:

| Vertical | Owns |
|----------|------|
| `agentctx` | Agent context generation, the context pack, agent workflows, MCP config, the workspace map |
| `cachecmd` | `cache` and its verification/analysis subcommands |
| `ci` | `ci` and CI change plans |
| `completion` | bash/zsh/fish completion generators and structured help |
| `configcmd` | `config` and `telemetry` |
| `doctor` | `doctor`, its check families, the gate and waivers |
| `extensions` | `extensions`, `templates`, artifacts, validation |
| `lifecycle` | `install`, `upgrade`, `init`, `deps`, `infra`, `projects*`, `scopes`, `workspace`, bootstrap and ensure |
| `migrate` | `migrate vnext`, the lock-file format migration |
| `sdd` | `contracts`, `features`, `specs` and their diffs |
| `sessions` | `sessions`, the gate, inspection, reports |
| `versioncmd` | `version`, `pin`, release/source install, the Go/Bun toolchain lock |

Plus two landing zones for what verticals share:

- **`shared`** — production helpers two or more verticals need (`ResolveProjectSelector`, `WithResultData`, `AtomicWriteFile`, `RunGroupCombined`, …).
- **`sharedtest`** — test-only helpers (`CaptureStdout`, `WriteLock`, `WriteJSONConfig`, …). Kept separate on purpose: putting them in `shared` made `shared` import `testing`, which linked the testing runtime into the shipped binary.

A vertical may import `shared`, `sharedtest`, `go.putnami.dev/cli/model/*` and CLI support packages, but **never a sibling vertical** without a written exception. Ten of the twelve import no sibling at all; the exceptions — `lifecycle` orchestrating `agentctx`/`extensions`/`versioncmd`/`completion`, and `agentctx` reading `sdd`'s design summaries — are listed with their reasoning in `internal/cli/vertical_isolation_ratchet_test.go`. The graph is acyclic, with `lifecycle` on top.

## Go Patterns

### Stdlib Only

The CLI has zero third-party Go dependencies. `go.mod` requires only in-repo modules — `go.putnami.dev/protocol/*` and `go.putnami.dev/cli/model` — each `replace`d to a local path, so nothing is fetched from a registry:

```
module go.putnami.dev/tooling/cli
go 1.25.7

require (
    go.putnami.dev/cli/model v0.0.0
    go.putnami.dev/protocol/... v0.0.0
)
```

Every capability — JSON parsing, SHA-256 hashing, file watching, HTTP client, process management, ANSI terminal output — uses the Go standard library. This eliminates supply chain risk and ensures the binary is fully self-contained.

### Context-Based Cancellation

The CLI uses `context.Context` for structured cancellation throughout:

```go
// main.go — top-level signal handling
ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
defer cancel()
```

Context flows through:
- **Scheduler** — Cancels in-flight jobs on failure or Ctrl+C.
- **Subprocess execution** — `exec.CommandContext()` kills child processes when context is cancelled.
- **Watch mode** — Cancels the current iteration when new changes arrive.
- **Hook execution** — Respects the parent context's timeout.

### Interface-Based Design

Key abstractions use interfaces for testability and extensibility:

```go
// Renderer — output formatting
type Renderer interface {
    Start(planned []*ScheduledJob)
    JobStart(job *ScheduledJob)
    JobEvent(job *ScheduledJob, event RawJobEvent)
    JobComplete(job *ScheduledJob, result *JobResult)
    Finish(results map[string]*JobResult)
}

// Store — cache storage
type Store interface {
    Has(hash string) (bool, error)
    Get(hash string) (*Entry, error)
    Put(hash string, entry *Entry) error
}
```

Implementations (local store, remote store, chained store; text renderer, live renderer, JSONL renderer) are swapped at runtime based on configuration.

### No Framework

The CLI does not use Cobra, urfave/cli, or any other CLI framework. Argument parsing is a custom implementation (`internal/cli/args.go`) that handles:

- Multi-command syntax (`lint,test,build`)
- Alias resolution with chain detection
- Global flag extraction with remaining passthrough
- `--flag=value` and `--flag value` forms
- Typo suggestions via Levenshtein distance

This avoids framework overhead and provides exact control over the parsing behavior needed for Putnami's unique command model.

## Concurrency Model

### Worker Pool Pattern

The scheduler uses a fixed-size goroutine pool:

```go
readyCh := make(chan *ScheduledJob, maxWorkers)
doneCh := make(chan jobDone, maxWorkers)

// Workers
for i := 0; i < maxWorkers; i++ {
    go func() {
        for job := range readyCh {
            result := executeJob(ctx, job)
            doneCh <- jobDone{job, result}
        }
    }()
}

// Coordinator: single goroutine managing the DAG
```

The coordinator avoids deadlock by using non-blocking sends with a pending queue fallback. It alternates between sending jobs and receiving completions.

### DAG State Machine

Job readiness is tracked with dependency counters:

```go
type dagState struct {
    remaining  map[string]int              // jobKey → unresolved dependency count
    dependents map[string][]string         // jobKey → dependent job keys
    jobs       map[string]*ScheduledJob    // jobKey → job
}
```

When a job completes, its dependents' counters are decremented. Jobs reaching zero become ready. This provides O(1) scheduling decisions.

### Mutex Usage

Mutexes are minimized and scoped:

| Resource | Protection | Scope |
|----------|-----------|-------|
| Job results map | `sync.Mutex` | Scheduler-level |
| Cache hash map | Same mutex | Scheduler-level |
| DAG state | `sync.Mutex` | Per DAG state |
| Session events | `sync.Mutex` | Per session |
| Hook tracking | `sync.Mutex` | Per scheduler |
| Renderer output | None (single-threaded calls from coordinator) | N/A |

The renderer is called exclusively from the coordinator goroutine, so it doesn't need synchronization.

### Graceful Shutdown

On SIGINT/SIGTERM:

1. Context cancellation propagates to all goroutines.
2. In-flight jobs are killed via `exec.CommandContext()`.
3. The scheduler drains `doneCh` to collect in-flight results.
4. Remaining planned jobs are marked as `skipped`.
5. Session is finalized with partial results.

## Error Handling

### Error Wrapping

Errors use `fmt.Errorf` with `%w` for wrapping:

```go
if err != nil {
    return nil, fmt.Errorf("expand pipeline for %s/%s: %w", proj.Name, cmdName, err)
}
```

This preserves the error chain for debugging while providing context at each layer.

### Exit Codes

Exit codes are determined at the top level in `app.go`:

| Condition | Exit Code |
|-----------|-----------|
| All jobs succeeded | 0 |
| CLI error (parsing, discovery) | 1 |
| Any job failed | 2 |
| No jobs matched filters | 3 |
| Contract validation error | 4 |
| Signal received | 130 |

### Subprocess Error Handling

Job subprocesses may fail in several ways:

1. **Clean failure** — Process exits with non-zero code and emits a `result` event with `status: "failed"`.
2. **Crash** — Process exits without emitting a `result` event. Stderr is captured as the error message.
3. **Timeout** — Process exceeds `timeoutMs`. Context cancellation kills it. Status is `"failed"`.
4. **Signal** — Parent context cancelled (Ctrl+C). Process is killed. No error reported.

The runner normalizes all failure modes to a `JobResult` with `status: "failed"` and an appropriate error message.

### Nil Safety

The codebase defensively checks for nil pointers at boundary points:

- Extension discovery returns empty slices, not nil.
- Config loading returns zero-value structs, not nil.
- Session operations are no-ops when session is nil.
- Cache operations are no-ops when cache manager is nil.
- Version info is nil-safe — `GetVersionInfo` returns nil if git is unavailable, and the context propagates nil cleanly.

## Version Computation

The CLI computes git metadata once per session (`GetVersionInfo` in `internal/git/git.go`) and combines it with each job's effective project version:

1. Read the current branch name (`git rev-parse --abbrev-ref HEAD`).
2. Read the short commit SHA (`git rev-parse --short HEAD`).
3. Determine the release channel: `"canary"` on `main`/`master`, `"dev"` on other branches.
4. Detect dirty state: run `git status --porcelain`. If non-empty, compute a 7-character SHA-256 hash of `git diff HEAD` as the dirty hash.
5. Resolve the base version for each project: project config, nearest scope config, then workspace config.
6. Build the full version: `{base}-{sha}` when clean, `{base}-{sha}.{dirtyHash}` when dirty.

The git portion is computed once in `app.go` and passed to the scheduler config. The scheduler rewrites the base/full version per job before writing each `JobCommandContext`. Jobs use it for publish versioning, build metadata, Docker tags, etc.

## Process Management

### Subprocess Execution

Jobs run as isolated subprocesses:

1. Build a `JobContext` struct with all necessary metadata (including version info).
2. Serialize to a temporary JSON file.
3. Spawn the process with `exec.CommandContext()`.
4. Pass the context file path via `--putnamiContext` flag.
5. Stream stdout line-by-line, parsing JSONL events.
6. Capture stderr for error reporting.
7. Clean up the temporary context file.

### Template Variable Resolution

Task commands, args, cwd, and env values use template variables:

| Variable | Resolution |
|----------|------------|
| `{workspaceRoot}` | Absolute workspace root path |
| `{projectRoot}` | Absolute project directory path |
| `{extensionRoot}` | Absolute extension directory path |
| `{outputRoot}` | `.putnami/out/{project}/{command}` |
| `{cacheRoot}` | `.putnami/cache/` (per-workspace mutable scratch, **not** the content-addressed store) |
| `{selectedProjects}` | Comma-separated selected project names for workspace-once jobs |
| `{selectedProjectIDs}` | Comma-separated selected project IDs for workspace-once jobs |
| `{selectedProjectPaths}` | Comma-separated selected project paths for workspace-once jobs |
| `{selectedProjectRoots}` | Comma-separated selected project roots for workspace-once jobs |

Variables are resolved via simple string replacement before process creation.

### Watch Mode Process Lifecycle

For `putnami serve --watch`, the CLI manages a long-running server process:

1. **Start** — Spawn with process group isolation (`Setpgid: true`).
2. **Stop** — Send SIGTERM to the process group. Wait up to 5 seconds.
3. **Force kill** — If still running, send SIGKILL.
4. **Restart** — Stop then start.

Process group isolation ensures that child processes spawned by the server are also terminated.

## File System Operations

### Atomic Cache Writes

Cache entries are written atomically to prevent corruption:

1. Create entry under `tmp/{uuid}/`.
2. Write `meta.json`, `result.json`, and copy `files/`.
3. `os.Rename()` the directory to the final `blobs/` path.

Since `os.Rename()` is atomic on most filesystems when source and destination are on the same mount, this prevents partial entries.

### Glob Pattern Matching

Go's `filepath.Glob()` does not support `**` (recursive matching). The CLI implements custom recursive matching:

1. Split the pattern on `**`.
2. Extract prefix (before `**`) and suffix (after `**`).
3. `filepath.WalkDir()` from the prefix directory.
4. Match each file's name against the suffix pattern using `filepath.Match()`.
5. Skip common non-project directories (`node_modules`, `.git`, `dist`, `vendor`).

### File Watching

The watcher uses polling-based change detection:

1. **Snapshot** — Record all file paths and modification times.
2. **Poll** — Periodically re-scan and compare against the snapshot.
3. **Debounce** — Coalesce rapid changes within a 150ms window.
4. **Classify** — Map changed files to affected projects.

Polling avoids platform-specific inotify/kqueue differences and works reliably on all operating systems and network file systems.

## Testing

### Test Framework

Tests use Go's standard `testing` package:

```go
func TestParseArgs(t *testing.T) {
    parsed := ParseArgs([]string{"build", "--all"}, nil, nil)
    if !parsed.Global.All {
        t.Error("expected --all to be set")
    }
}
```

### Test Organization

Test files are colocated with their source files following Go convention:

```
internal/cli/args.go       → internal/cli/args_test.go
internal/store/cache.go    → internal/store/cache_test.go
internal/jobs/planner.go   → internal/jobs/planner_test.go
```

### Running Tests

```bash
# Via putnami CLI
putnami test tooling/cli

# Directly with Go
cd tooling/cli && go test ./...

# With verbose output
go test -v ./internal/...

# Specific package
go test -v ./internal/cli/
```

## Key Design Decisions

### Why No CLI Framework?

Putnami's command model is unusual: multi-command execution (`lint,test,build`), dynamic job commands from extensions, alias resolution chains, and pass-through flags for job processes. No existing CLI framework supports this natively. A custom parser provides exact control with less code than adapting a framework.

### Why Polling for File Watching?

Platform-specific file system notification APIs (inotify, kqueue, FSEvents) have different behaviors, buffer limits, and failure modes. Polling at 100ms intervals with 150ms debounce provides consistent behavior everywhere, including Docker volumes and network mounts where notification APIs are unreliable.

### Why Content-Addressed Caching?

Hash-based keys provide automatic deduplication and correct invalidation without tracking file modification times (which are unreliable across machines). The SHA-256 hash of actual file contents guarantees that identical inputs always produce cache hits, regardless of timestamps.

### Why Subprocess-Based Job Execution?

Running each job as a subprocess provides:
- **Crash isolation** — A segfault in a build tool doesn't crash the CLI.
- **Language independence** — Jobs can be TypeScript, Go, Python, or shell scripts.
- **Resource isolation** — Each job gets its own memory space and file descriptors.
- **Clean termination** — Process groups enable reliable cleanup on abort.

The JSONL protocol over stdout adds minimal overhead while providing structured, streaming communication.

### Why Session Recording?

Sessions provide post-mortem debugging without re-running builds. When a CI failure occurs, inspecting the session reveals the exact execution plan, timing, and error without reproducing the environment. The append-only JSONL format ensures no event is lost, even on crash.
