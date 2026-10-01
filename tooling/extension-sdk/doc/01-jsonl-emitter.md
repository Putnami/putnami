# JSONL Emitter

The `jsonl` package emits the Putnami runtime event protocol for structured communication between extension jobs and the orchestrator. Every event is a self-contained JSON line written to stdout.

The stream's protocol version is **negotiated once**, when the emitter is built: `jsonl.New()` reads the invoking CLI's `PUTNAMI_RUNTIME_EVENTS` advertisement and stamps that version on every line it writes, because the protocol rejects a stream that mixes versions. A CLI that requires extension contract 3 advertises — and accepts only — `v: 2`; with no advertisement the emitter falls back to `v: 1`. Use `jsonl.NewForVersion` only when you have already resolved the negotiation yourself.

The wire format is owned by `go.putnami.dev/protocol/runtime` (`protocols/runtime`); this package is a stdout-bound convenience wrapper around the protocol emitter, and a drift test validates every emitted event against the protocol parser and validator. Metric values must be numeric per the protocol schema — non-numeric values are dropped with a debug log.

## Overview

Extension jobs communicate with the Putnami orchestrator by emitting JSONL events to stdout. The orchestrator parses these events to track progress, collect diagnostics, display metrics, and determine job outcomes. The emitter provides type-safe methods for each event kind.

## Usage

### Basic Usage

```go
import "go.putnami.dev/sdk/extension/jsonl"

emit := jsonl.New()

emit.Meta("@putnami/go", "build")
emit.PhaseStart("compile")
emit.Progress(1, 3, "Compiling...")
emit.Log("info", "Build started")
emit.PhaseEnd("compile", "success")
emit.Result("OK", nil)
```

### Structured Logging

Use convenience methods for common log levels:

```go
emit.Info("Starting build")
emit.Warn("Deprecated flag used")
emit.Error("Compilation failed")
emit.Debug("Resolved path: /usr/local/go")
```

For structured log forwarding from child processes, use `LogEvent`:

```go
emit.LogEvent("error", "connection refused",
    map[string]any{"host": "localhost", "port": 5432},
    map[string]any{"message": "dial tcp: connection refused"},
)
```

### Reporting Diagnostics

Report compiler errors, lint warnings, or test failures with source locations:

```go
// Simple diagnostic
emit.Diagnostic("error", "undefined: foo", "./main.go", 42)

// Diagnostic with column and rule code
emit.DiagnosticWithCode("warning", "unused variable", "./main.go", 10, 5, "SA4006")
```

### Emitting Metrics and Artifacts

```go
emit.Metric("binary-size", 4096, "bytes")
emit.Metric("coverage", 87.5, "percent")
emit.Metric("tests-passed", 42, "count")

emit.Artifact("coverage", "Coverage Profile", "coverage", "/out/coverage.out")
emit.ArtifactWithData("binary", "App Binary", "executable", "/out/app",
    map[string]any{"platform": "linux/amd64"},
)
```

### Forwarding Child Process Output

When running subprocesses, use `ForwardPipe` to stream their output as JSONL events. It automatically detects structured JSON logs (with `severity` + `message` fields) and preserves their context:

```go
import "sync"

cmd := exec.Command("go", "run", ".")
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

### `New() *Emitter`

Creates a new JSONL emitter that writes to stdout.

### `(*Emitter) Meta(extension, job string)`

Emits a `meta` event identifying the extension and job. Must be the first event emitted.

| Parameter | Type | Description |
|-----------|------|-------------|
| `extension` | `string` | Extension name (e.g. `"@putnami/go"`) |
| `job` | `string` | Job name (e.g. `"build"`) |

### `(*Emitter) Log(level, message string)`

Emits a `log` event.

| Parameter | Type | Description |
|-----------|------|-------------|
| `level` | `string` | `"debug"`, `"info"`, `"warn"`, or `"error"` |
| `message` | `string` | Log message |

### `(*Emitter) Info(message string)`

Shorthand for `Log("info", message)`.

### `(*Emitter) Warn(message string)`

Shorthand for `Log("warn", message)`.

### `(*Emitter) Error(message string)`

Shorthand for `Log("error", message)`.

### `(*Emitter) Debug(message string)`

Shorthand for `Log("debug", message)`.

### `(*Emitter) LogEvent(level, message string, context map[string]any, errInfo map[string]any)`

Emits a `log` event with optional structured context and error information. Used primarily by `ForwardLine` to re-emit parsed JSON logs from child processes.

| Parameter | Type | Description |
|-----------|------|-------------|
| `level` | `string` | Log level |
| `message` | `string` | Log message |
| `context` | `map[string]any` | Additional context fields (nil to omit) |
| `errInfo` | `map[string]any` | Error details with `message` key (nil to omit) |

### `(*Emitter) PhaseStart(name string)`

Emits a `phase` event with action `"start"`. Phases group related work for progress display.

### `(*Emitter) PhaseEnd(name, status string)`

Emits a `phase` event with action `"end"`.

| Parameter | Type | Description |
|-----------|------|-------------|
| `name` | `string` | Phase name (must match the corresponding `PhaseStart`) |
| `status` | `string` | `"success"`, `"failed"`, or `"skipped"` |

### `(*Emitter) Progress(current, total int, message string)`

Emits a `progress` event for progress bar display.

| Parameter | Type | Description |
|-----------|------|-------------|
| `current` | `int` | Current step (1-based) |
| `total` | `int` | Total steps |
| `message` | `string` | Human-readable progress label |

### `(*Emitter) Diagnostic(severity, message, file string, line int)`

Emits a `diagnostic` event with optional file location.

| Parameter | Type | Description |
|-----------|------|-------------|
| `severity` | `string` | `"error"`, `"warning"`, `"info"`, or `"hint"` |
| `message` | `string` | Diagnostic message |
| `file` | `string` | File path (empty to omit location) |
| `line` | `int` | Line number (ignored if `file` is empty) |

### `(*Emitter) DiagnosticWithCode(severity, message, file string, line, column int, code string)`

Emits a `diagnostic` event with optional file location, column, and diagnostic code.

| Parameter | Type | Description |
|-----------|------|-------------|
| `severity` | `string` | `"error"`, `"warning"`, `"info"`, or `"hint"` |
| `message` | `string` | Diagnostic message |
| `file` | `string` | File path (empty to omit location) |
| `line` | `int` | Line number (0 to omit) |
| `column` | `int` | Column number (0 to omit) |
| `code` | `string` | Diagnostic code/rule (empty to omit) |

### `(*Emitter) Metric(name string, value any, unit string)`

Emits a `metric` event for measurable values.

| Parameter | Type | Description |
|-----------|------|-------------|
| `name` | `string` | Metric name (e.g. `"binary-size"`, `"coverage"`) |
| `value` | `any` | Numeric value |
| `unit` | `string` | `"ms"`, `"bytes"`, `"count"`, or `"percent"` |

### `(*Emitter) Artifact(id, name, kind, path string)`

Emits an `artifact` event referencing a build output.

| Parameter | Type | Description |
|-----------|------|-------------|
| `id` | `string` | Unique artifact identifier |
| `name` | `string` | Human-readable artifact name |
| `kind` | `string` | Artifact kind (e.g. `"executable"`, `"coverage"`) |
| `path` | `string` | Absolute file path |

### `(*Emitter) ArtifactWithData(id, name, kind, path string, data map[string]any)`

Like `Artifact`, but merges additional key-value pairs into the event.

### `(*Emitter) Summary(message string)`

Emits a `summary` event with a human-readable label shown in the job recap.

### `(*Emitter) Result(status string, data map[string]any)`

Emits the final `result` event. Must be the last event emitted.

| Parameter | Type | Description |
|-----------|------|-------------|
| `status` | `string` | `"OK"`, `"FAILED"`, or `"SKIP"` |
| `data` | `map[string]any` | Structured result data (nil to omit) |

### `ForwardPipe(emit *Emitter, pipe io.ReadCloser, fallbackLevel string, wg *sync.WaitGroup)`

Reads lines from a pipe and forwards each as a JSONL log event. Calls `wg.Done()` when the pipe is closed. Designed to run as a goroutine.

| Parameter | Type | Description |
|-----------|------|-------------|
| `emit` | `*Emitter` | Target emitter |
| `pipe` | `io.ReadCloser` | Subprocess stdout or stderr pipe |
| `fallbackLevel` | `string` | Log level for plain text lines |
| `wg` | `*sync.WaitGroup` | Wait group to signal completion |

### `ForwardLine(emit *Emitter, line, fallbackLevel string)`

Parses a single output line and re-emits it as a JSONL log event. If the line is a JSON object with `severity` and `message` fields, it is parsed as a structured log and context fields are preserved. Otherwise, the line is emitted as a plain log at `fallbackLevel`.

### `MapSeverity(severity, fallback string) string`

Normalizes a severity string (case-insensitive) to a standard log level. Maps `"ERROR"` to `"error"`, `"WARNING"`/`"WARN"` to `"warn"`, `"INFO"` to `"info"`, `"DEBUG"` to `"debug"`. Returns `fallback` for unrecognized values.

## Event Protocol

All events share these common fields:

| Field | Type | Description |
|-------|------|-------------|
| `v` | `int` | Protocol version (always `1`) |
| `type` | `string` | Event type |
| `time` | `string` | ISO 8601 timestamp (UTC) |

Event types: `meta`, `log`, `phase`, `progress`, `diagnostic`, `metric`, `artifact`, `summary`, `result`.
