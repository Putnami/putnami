# Subprocess Execution

The `exec` package provides a functional-options API for running subprocesses. It captures stdout/stderr, handles exit codes, and supports timeouts, context cancellation, custom working directories, and environment variables.

## Overview

Extension jobs frequently shell out to tools like `go build`, `bun test`, `golangci-lint`, or `git`. The `exec` package wraps `os/exec` with a consistent API that returns a `Result` struct instead of requiring manual error/exit-code handling.

## Usage

### Basic Execution

```go
import "go.putnami.dev/sdk/extension/exec"

result, err := exec.Run("go", []string{"build", "./..."})
if err != nil {
    // Command could not start (binary not found, permission denied, etc.)
    return "FAILED", nil, err
}
if !result.Success {
    emit.Error("Build failed: " + result.Stderr)
    return "FAILED", nil, nil
}
emit.Info("Build output: " + result.Stdout)
```

### With Options

```go
result, err := exec.Run("go", []string{"test", "./..."},
    exec.Dir("/path/to/project"),
    exec.Env(map[string]string{
        "CGO_ENABLED": "0",
        "GOOS":        "linux",
    }),
    exec.Timeout(5 * time.Minute),
)
```

### With Context Cancellation

```go
ctx, cancel := context.WithCancel(context.Background())
defer cancel()

result, err := exec.Run("long-running-tool", nil,
    exec.WithContext(ctx),
)
```

### With Stdin

```go
result, err := exec.Run("jq", []string{".name"},
    exec.Stdin(`{"name": "putnami", "version": "1.0.0"}`),
)
// result.Stdout == "\"putnami\"\n"
```

### Pipe-Based Streaming

For long-running processes where you need real-time output (e.g., serve jobs), use `os/exec` directly with [jsonl.ForwardPipe](./01-jsonl-emitter.md):

```go
cmd := exec.Command("bun", "run", entrypoint)
stdoutPipe, _ := cmd.StdoutPipe()
stderrPipe, _ := cmd.StderrPipe()
cmd.Start()

var wg sync.WaitGroup
wg.Add(2)
go jsonl.ForwardPipe(emit, stdoutPipe, "info", &wg)
go jsonl.ForwardPipe(emit, stderrPipe, "warn", &wg)
wg.Wait()
cmd.Wait()
```

## API Reference

### `Run(name string, args []string, opts ...Option) (*Result, error)`

Executes a command and returns the result.

| Parameter | Type | Description |
|-----------|------|-------------|
| `name` | `string` | Command name or path |
| `args` | `[]string` | Command arguments |
| `opts` | `...Option` | Execution options |

**Returns:** A `Result` with captured output, or an error if the command could not start. Non-zero exit codes are **not** returned as errors — check `Result.Success` instead.

### `Result`

| Field | Type | Description |
|-------|------|-------------|
| `ExitCode` | `int` | Process exit code |
| `Stdout` | `string` | Captured standard output |
| `Stderr` | `string` | Captured standard error |
| `Success` | `bool` | `true` if `ExitCode == 0` |

### Options

#### `Dir(dir string) Option`

Sets the working directory for the command.

#### `Env(env map[string]string) Option`

Adds environment variables. These are appended to the current process environment (`os.Environ()`), so existing variables are inherited.

#### `UnsetEnv(names ...string) Option`

Removes inherited environment variables from the subprocess environment — the counterpart to `Env`, which can only add.

The filter applies to the inherited environment and runs **before** `Env`'s additions, so `UnsetEnv("X")` together with `Env{"X": "v"}` yields `X=v`. Naming a variable that is not set is a no-op, repeated calls accumulate, and matching is exact and case-sensitive.

The canonical caller is a test harness scrubbing host platform identity; the option itself knows nothing about which names those are (see [Host platform identity](./06-hostenv.md)).

```go
result, err := exec.Run("bun", []string{"test"},
    exec.Dir(projectPath),
    exec.UnsetEnv(hostenv.PlatformIdentityVars()...),
    exec.Env(map[string]string{"FORCE_COLOR": "1"}),
)
```

#### `Timeout(d time.Duration) Option`

Sets a timeout. The command is killed if it exceeds the duration.

#### `WithContext(ctx context.Context) Option`

Sets a context for cancellation. The command is killed when the context is cancelled.

#### `Stdin(s string) Option`

Provides string content as standard input to the command.

## Error Handling

The `exec` package distinguishes between two failure modes:

1. **Start failure** — the command could not be started (binary not found, permission denied). Returns `(nil, error)`.
2. **Non-zero exit** — the command ran but exited with a non-zero code. Returns `(*Result, nil)` with `Result.Success == false`.

```go
result, err := exec.Run("might-not-exist", nil)
if err != nil {
    // Binary not found or permission denied
    return "FAILED", nil, err
}
if !result.Success {
    // Command ran but failed (exit code != 0)
    emit.Error(result.Stderr)
    return "FAILED", nil, nil
}
```

## Boundaries

- **Scope**: Synchronous subprocess execution with captured output
- **Out of scope**: Interactive processes, pipe-based streaming (use `os/exec` directly with [jsonl.ForwardPipe](./01-jsonl-emitter.md) for that), process pools
- **Dependencies**: Standard library only (`os/exec`, `context`, `bytes`)
