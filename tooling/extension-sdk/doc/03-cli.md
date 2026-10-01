# CLI Runner & Flags

The `cli` package provides the standard entry point for extension binaries and a lightweight flag parser. It handles context loading, meta emission, job execution, and result reporting — the full job lifecycle.

## Overview

Every extension binary follows the same lifecycle: parse `--putnamiContext`, load the context, emit a `meta` event, run the job, and emit a `result` event. The `cli` package encapsulates this pattern in `Run` and `RunSubcommand`, so extension authors only need to implement the job logic.

The package also provides `ParseFlags` for parsing job-specific flags from remaining arguments.

## Usage

### Single-Job Binary

For extensions with a single job:

```go
package main

import (
    "go.putnami.dev/sdk/extension/cli"
    pctx "go.putnami.dev/sdk/extension/context"
    "go.putnami.dev/sdk/extension/jsonl"
)

func main() {
    cli.Run(buildJob)
}

func buildJob(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
    emit.PhaseStart("compile")
    // ... do work ...
    emit.PhaseEnd("compile", "success")
    return "OK", nil, nil
}
```

### Multi-Job Binary (Subcommands)

For extensions with multiple jobs dispatched via subcommand:

```go
package main

import "go.putnami.dev/sdk/extension/cli"

func main() {
    cli.RunSubcommand(map[string]cli.JobFunc{
        "build":   buildJob,
        "test":    testJob,
        "lint":    lintJob,
        "serve":   serveJob,
    })
}
```

The binary is invoked as `putnami-go build --putnamiContext /tmp/ctx.json`. `RunSubcommand` strips the subcommand argument and forwards the rest to `Run`.

### Workspace-Level Jobs

By default, `Run` skips execution (emits `SKIP`) when the project name is empty. For extensions that run at the workspace level (e.g., Python `install`, CI `setup`), disable this:

```go
cli.RunSubcommand(commands, cli.WithSkipEmptyProject(false))
```

### Parsing Job Flags

Use `ParseFlags` to parse job-specific arguments passed after `--putnamiContext`:

```go
func buildJob(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
    flags := cli.ParseFlags(args)

    target := cli.FlagString(flags, "target", "linux/amd64")
    race := cli.FlagBool(flags, "race", false)
    parallel := cli.FlagInt(flags, "parallel", 4)

    // ...
}
```

Supported flag formats:
- `--flag value` — key-value pair
- `--flag=value` — key-value with equals
- `--flag` — boolean true
- `--no-flag` — boolean false

## API Reference

### Types

#### `JobFunc`

```go
type JobFunc func(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (status string, data map[string]any, err error)
```

The function signature for job implementations.

| Parameter | Type | Description |
|-----------|------|-------------|
| `ctx` | `*context.Context` | Parsed job context |
| `emit` | `*jsonl.Emitter` | JSONL event emitter |
| `args` | `[]string` | Remaining arguments after standard flags |

**Returns:**

| Value | Type | Description |
|-------|------|-------------|
| `status` | `string` | `"OK"`, `"FAILED"`, or `"SKIP"` |
| `data` | `map[string]any` | Structured result data (nil to omit) |
| `err` | `error` | Error (causes `FAILED` result and error log) |

### Runner Functions

#### `Run(job JobFunc, opts ...RunOption)`

Standard entry point for extension jobs. Performs:

1. Parses `--putnamiContext` and `--output` from `os.Args`
2. Loads and parses the context file
3. Skips if project is empty (unless `WithSkipEmptyProject(false)`)
4. Emits `meta` event
5. Calls the job function
6. Emits `result` event
7. Exits with code 1 on `FAILED`, 0 otherwise

If the job returns an error, `Run` emits an error log and a `FAILED` result before exiting.

#### `RunSubcommand(commands map[string]JobFunc, opts ...RunOption)`

Dispatches to a named job based on the first positional argument. Prints usage and exits with code 2 if no subcommand is provided or the subcommand is unknown.

#### `WithSkipEmptyProject(skip bool) RunOption`

Controls whether `Run` automatically emits `SKIP` when the project name is empty. Default: `true`. Set to `false` for workspace-level jobs.

### Flag Functions

#### `ParseFlags(args []string) map[string]string`

Parses arguments into a flag map. Supports `--key value`, `--key=value`, `--flag` (boolean true), and `--no-flag` (boolean false). Non-flag arguments are ignored.

#### `FlagString(flags map[string]string, key, defaultVal string) string`

Returns the string value of a flag, or `defaultVal` if not set.

#### `FlagBool(flags map[string]string, key string, defaultVal bool) bool`

Returns the boolean value of a flag. Recognizes `"true"`, `"1"`, `"yes"`, `"on"` (true) and `"false"`, `"0"`, `"no"`, `"off"` (false). Returns `defaultVal` if not set or unrecognized.

#### `FlagInt(flags map[string]string, key string, defaultVal int) int`

Returns the integer value of a flag, or `defaultVal` if not set or not a valid integer.

## Job Lifecycle

```
os.Args → Run() → parse --putnamiContext
                 → Parse(contextFile)
                 → skip if empty project?
                 → emit Meta(extension, job)
                 → job(ctx, emit, args)
                 → emit Result(status, data)
                 → os.Exit(0 or 1)
```

## Boundaries

- **Scope**: Job lifecycle management, context loading, flag parsing
- **Out of scope**: Job implementation logic, subprocess execution, JSONL protocol details
- **Dependencies**: [context](./02-context.md), [jsonl](./01-jsonl-emitter.md)
