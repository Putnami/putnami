# JSONL Event Format (v1)

Putnami jobs communicate with the orchestrator through structured JSONL on stdout. Each line is a self-contained JSON object conforming to the event protocol.

The Putnami CLI reads lines of at most 16 MiB. It reads a longer line to its end, discards it, and reports an `error` log in its place. A task that discarded a line and reported no `result` fails, since its result may have been the discarded line. The same framing holds at v2.

> Version 2 is documented in [`05-event-v2.md`](05-event-v2.md): the same
> envelope plus the typed `ready` event. Everything below stays true for both —
> only the `v` stamp and the extra type differ. **What an emitter writes is
> negotiated, not fixed**: a job stream handled by a CLI that requires the
> current extension contract is `v: 2`, and that CLI rejects a `v: 1` line.
> `v: 1` remains what an emitter writes when nothing advertises a version. The
> rule and its alternatives are recorded in
> [`adr/0001-one-stream-one-negotiated-version.md`](adr/0001-one-stream-one-negotiated-version.md).

## Envelope

Every event has the same envelope:

```json
{
  "v": 1,
  "type": "<event-type>",
  "time": "2024-01-15T10:30:00.000Z",
  "level": "info",
  "message": "Human-readable description"
}
```

| Field | Required | Description |
|-------|----------|-------------|
| `v` | yes | Protocol version. Must be `1`. |
| `type` | yes | Event type (see below). |
| `time` | no | ISO 8601 UTC timestamp. |
| `level` | no | Log level (`debug`, `info`, `warn`, `error`). |
| `message` | no | Human-readable message. |
| `data` | no | Type-specific structured payload. |

Lines that don't parse as JSON or lack `v=1` and a `type` field are silently ignored by the orchestrator.

## Event Types

### `meta`

Emitted once at the start of job execution. Identifies the extension and job.

```json
{
  "v": 1, "type": "meta", "time": "...", "level": "info",
  "message": "Starting build",
  "data": { "extension": "@putnami/typescript", "job": "build" }
}
```

### `log`

Freeform log output.

```json
{
  "v": 1, "type": "log", "time": "...", "level": "info",
  "message": "Compiling 42 files..."
}
```

Optional fields: `context` (structured key-value data), `error` (with `message` and `stack`).

### `phase`

Marks the start or end of a named execution phase within a job.

```json
{ "v": 1, "type": "phase", "time": "...", "name": "compile", "action": "start" }
{ "v": 1, "type": "phase", "time": "...", "name": "compile", "action": "end", "status": "success" }
```

`action` is `"start"` or `"end"`. `status` is only present on `action=end` and is one of `"success"`, `"failed"`, or `"skipped"`.

### `progress`

Progress indicator for operations with known bounds.

```json
{
  "v": 1, "type": "progress", "time": "...",
  "current": 15, "total": 42, "message": "Compiling files..."
}
```

### `diagnostic`

Structured warning or error with optional source location. Used by linters, compilers, and test runners.

```json
{
  "v": 1, "type": "diagnostic", "time": "...",
  "severity": "error",
  "message": "Cannot find name 'foo'",
  "code": "TS2304",
  "location": { "file": "src/app.ts", "line": 10, "column": 5 }
}
```

`severity` is one of: `"error"`, `"warning"`, `"info"`, `"hint"`.

### `metric`

Named measurement.

```json
{
  "v": 1, "type": "metric", "time": "...",
  "name": "binary-size", "value": 4096, "unit": "bytes"
}
```

### `artifact`

Output file or directory produced by the job.

```json
{
  "v": 1, "type": "artifact", "time": "...",
  "id": "bundle", "name": "app.js", "kind": "bundle", "path": "dist/app.js"
}
```

### `summary`

Human-readable summary label shown in the job recap.

```json
{ "v": 1, "type": "summary", "time": "...", "message": "42 tests passed" }
```

### `result`

Final result event. Exactly one must be emitted per job execution. The orchestrator uses this to determine job success or failure.

```json
{
  "v": 1, "type": "result", "time": "...",
  "level": "info", "message": "Job OK",
  "data": {
    "status": "OK",
    "data": { "outputPath": "dist/" }
  }
}
```

`status` values: `"OK"`, `"FAILED"`, `"SKIP"`.

On failure, the `data` object may include an `error` field:

```json
{
  "data": {
    "status": "FAILED",
    "error": { "message": "Compilation failed", "code": "BUILD_ERROR" }
  }
}
```

## Flat vs. Nested Fields

Job subprocesses may use flat top-level fields (e.g., `"severity"`, `"location"`, `"current"`) rather than nesting under `"data"`. The orchestrator normalizes both forms — all non-envelope fields are accessible via `event.Data[key]`.

## Schema

The formal definition is in [`schemas/event.json`](../schemas/event.json).
