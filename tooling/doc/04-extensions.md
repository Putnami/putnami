# Extensions

Extensions teach the CLI new capabilities. Each extension provides commands — build, test, lint, serve, publish — for a specific language, framework, or tool. The extension model is open: you can write extensions in any language and distribute them as workspace packages, npm packages, or standalone directories.

## Official extensions

Putnami ships three language extensions:

| Extension | Purpose | Commands | Details |
|-----------|---------|----------|---------|
| `@putnami/typescript` | TypeScript/JavaScript toolchain | build, test, lint, serve, run, package, generate | [TypeScript extension](/docs/frameworks/typescript/extension) |
| `@putnami/go` | Go toolchain | build, test, lint, serve, run, package | [Go extension](/docs/frameworks/go/extension) |
| `@putnami/python` | Python toolchain | test, lint, serve | [Python extension](/docs/frameworks/python/extension) |

Each language extension page documents its commands, flags, caching behavior, and templates in full.

Beside them, three cross-language extensions serve every ecosystem:

| Extension | Purpose | Commands | Activates on |
|-----------|---------|----------|--------------|
| `@putnami/scaffold` | Packages a project template into a distributable archive | package | `putnami.template.json` |
| `@putnami/clientgen` | Generates typed REST clients for a service's consumers from its OpenAPI document | clientgen | A provider project |
| `@putnami/sdd` | Specification-driven development: authored features, durable specs, executable architecture, generated contracts, a CODEOWNERS file generated from declared owners | validate, validate-workspace, features, specs, architecture, contracts | `putnami.features.json` or `specs/*.json` |

None is tied to one language: a template is packaged the same way whatever it
renders into, a client is generated from the provider's published contract
rather than from either side's source, and a spec is prose about intent. See
[Templates](/docs/tooling-&-workspace/templates) for scaffolding and
[Write a feature spec](/docs/how-to/write-a-feature-spec) for SDD.

`@putnami/sdd` is **experimental** and entirely opt-in. Its four command groups
and its four `sdd.*` MCP tools used to be compiled into the CLI; they are not
any more. A workspace that does not declare the extension has none of them, and
`putnami` prints a hint naming the extension if you ask for one. Activation is
two conditions: the files must match **and** the project must name the extension
in its own `putnami.json`.

## Installing extensions

```bash
putnami deps add @putnami/typescript @putnami/go @putnami/python
```

Extensions are auto-discovered from:

1. **Workspace-level** entries in `putnami.workspace.json` → `extensions` field
2. **Scope-level** entries in scope `putnami.json` → `extensions` field
3. **Project-level** `devDependencies` in `package.json`

No manual registration is needed for npm-based extensions.

## Extension caching

Discovered extensions are cached in `.putnami/extensions.cache.json` for fast startup. The cache auto-invalidates when dependencies change.

## The extension manifest

Every extension is defined by a `putnami.extension.json` file. The manifest declares **commands** (the public API) and **tasks** (atomic executable units).

### Minimal manifest

```json
{
  "$schema": "https://putnami.dev/schemas/putnami-extension.json",
  "commands": {
    "build": {
      "run": [{ "id": "build", "task": "build-exec" }]
    }
  },
  "tasks": {
    "build-exec": {
      "kind": "command",
      "command": "echo",
      "args": ["built"]
    }
  }
}
```

### Top-level fields

| Field | Required | Description |
|-------|----------|-------------|
| `$schema` | no | JSON Schema reference for editor support |
| `commands` | **yes** | Public commands exposed to users |
| `tasks` | no | Atomic executable units referenced by commands |
| `hooks` | no | Lifecycle hooks (e.g., `preBuild`) |
| `templates` | no | Project templates (see [Templates](/docs/tooling-&-workspace/templates)) |
| `contracts` | no | Input/output schemas for task validation |
| `extensionDependencies` | no | Other extensions this one depends on |
| `workspaceDevDependencies` | no | NPM devDependencies required at workspace root |
| `autoServe` | no | Whether `serve` auto-discovers projects with this extension |
| `runtime` | no | The extension's own executable and how to prepare it (see [Lifecycle primitives](#lifecycle-primitives)) |
| `workspace` | no | Project discovery and metadata this extension owns (see [Lifecycle primitives](#lifecycle-primitives)) |

### Commands

A command is a user-facing operation invoked via `putnami <command>`. It defines activation rules, CLI flags, and a pipeline of steps:

```json
{
  "commands": {
    "build": {
      "description": "Build the project",
      "activationFiles": ["tsconfig.json"],
      "flags": {
        "target": {
          "type": "string",
          "default": "production",
          "choices": ["development", "production"]
        }
      },
      "run": [
        { "id": "generate", "task": "codegen", "dependsOn": ["^generate"] },
        { "id": "transpile", "task": "build-transpile", "dependsOn": ["generate"] },
        { "id": "types", "task": "build-types", "dependsOn": ["generate"] }
      ]
    }
  }
}
```

**Command fields:**

| Field | Required | Description |
|-------|----------|-------------|
| `run` | **yes** | Pipeline step DAG |
| `description` | no | Human-readable description |
| `activationFiles` | no | File patterns that must exist for the command to activate |
| `flags` | no | CLI flags the command accepts |
| `defaults` | no | Default parameter values |
| `priority` | no | Higher priority wins when multiple extensions provide the same command |
| `quiet` | no | Suppress from job recap |
| `visibility` | no | `public` (default) or `internal` |
| `outputs` | no | Named outputs projected from step results |
| `channel` | no | Execution channel for concurrency control |

Commands only activate for projects that match `activationFiles` patterns. If no patterns are specified, the command activates for all projects that use the extension.

**Flag properties:**

| Property | Type | Description |
|----------|------|-------------|
| `type` | `"string" \| "boolean" \| "number" \| "array"` | Value type |
| `short` | `string?` | Short alias (e.g., `"-r"`) |
| `description` | `string?` | Help text |
| `default` | `any?` | Default value |
| `required` | `boolean?` | Whether the flag must be provided |
| `choices` | `string[]?` | Valid values for `string` or `array` types |

### Tasks

A task is an atomic executable unit — a subprocess with defined inputs, outputs, and caching:

```json
{
  "tasks": {
    "build-transpile": {
      "kind": "command",
      "command": "bun",
      "args": ["run", "{extensionRoot}/bin/build"],
      "cwd": "{projectRoot}",
      "timeoutMs": 300000,
      "inputs": {
        "source": { "from": "project", "files": ["src/**/*.ts", "src/**/*.tsx"] },
        "config": { "from": "project", "files": ["tsconfig.json", "package.json"] }
      },
      "outputs": {
        "dist": { "kind": "directory", "path": "dist/" }
      },
      "cache": {
        "enabled": true,
        "deterministic": true
      }
    }
  }
}
```

**Task fields:**

| Field | Required | Description |
|-------|----------|-------------|
| `kind` | **yes** | Execution kind (currently only `command`) |
| `command` | **yes** | Command to execute |
| `args` | no | Command arguments |
| `cwd` | no | Working directory (supports template variables) |
| `env` | no | Environment variables |
| `timeoutMs` | no | Execution timeout (default: 300000ms) |
| `inputs` | no | Input port declarations |
| `outputs` | no | Output port declarations |
| `writes` | no | Resources written; conflicting accesses are serialized (project- or workspace-scoped) |
| `reads` | no | Resources read; serialized against conflicting writers |
| `cache` | no | Cache policy (`{ enabled, deterministic }`) |
| `inputSchemaRef` | no | Reference to a contract schema for input validation |
| `outputSchemaRef` | no | Reference to a contract schema for output validation |

**Template variables** in task fields:

| Variable | Expands to |
|----------|------------|
| `{extensionRoot}` | Absolute path to the extension directory |
| `{projectRoot}` | Absolute path to the project directory |
| `{workspaceRoot}` | Absolute path to the workspace root |
| `{selectedProjects}` | Comma-separated selected project names for workspace-once jobs |
| `{selectedProjectIDs}` | Comma-separated selected project IDs for workspace-once jobs |
| `{selectedProjectPaths}` | Comma-separated selected project paths for workspace-once jobs |
| `{selectedProjectRoots}` | Comma-separated selected project roots for workspace-once jobs |

### Input ports

Tasks declare their inputs via `inputs`. Each input has a source:

| Source | Description |
|--------|-------------|
| `project` | Files from the project directory (use `files` for globs) |
| `task` | Output from another task in the pipeline |
| `params` | Command parameters |
| `env` | Environment variables |
| `runtime` | Runtime context (platform, arch, versions) |

### Pipeline steps

Each step in a command's `run` array references a task and declares its position in the execution DAG:

| Field | Required | Description |
|-------|----------|-------------|
| `id` | **yes** | Unique step identifier |
| `task` | **yes** | Name of a task in the extension's `tasks` section |
| `dependsOn` | no | Step IDs or external references this step depends on |
| `with` | no | Input bindings passed to the task |
| `optional` | no | If true, failure marks step as skipped (not failed) |
| `if` | no | Plan-time condition over params, effective commands and their resolved params, and project type — excludes step from DAG |
| `runOn` | no | Lifecycle condition: `success` (default), or `finally` with an explicit `finalizes` relation |
| `finalizes` | with `runOn: finally` | Invocation-resource producer and complete downstream consumer frontier |
| `cache` | no | Per-step cache override |
| `timeoutMs` | no | Per-step timeout override |

**Dependency references** in `dependsOn`:

| Pattern | Meaning |
|---------|---------|
| `"transpile"` | Wait for step `transpile` in the same command |
| `"^generate"` | Wait for each upstream project's `generate` step of the same command |
| `"/workspace-install"` | Wait for the workspace-once command's `workspace-install` step |
| `"*generate"` | Wait for every other job that is the `generate` step, or belongs to the `generate` command |

The `^` prefix is the most common — it creates cross-project dependency chains so downstream projects wait for their dependencies to complete the same phase.

References resolve by **exact identity**: the step id a job *is*, within the
command the reference is written in (`*` additionally matches a command name).
Display names, internal plan names (`build~generate`) and synthesized
`project:name` keys are not addressable — a reference that only a name-based
match would have resolved is a plan-time error naming what it would have bound.

Command-level `dependsOn` also supports release gates. Bare command names are
same-project prerequisites; `!command` is a session barrier across the current
project selection:

```json
{
  "publish": { "dependsOn": ["!lint", "!test", "!build", "!package"] },
  "deploy": { "dependsOn": ["!publish"] }
}
```

`!command` dependencies are functional: if a barrier job fails, downstream
side-effecting jobs are skipped unless `--continue-on-error` is set.

**Conditional execution:**

- `if` (plan-time): evaluated before the pipeline runs, removes the step entirely from the graph
- `runOn` (execution): `success` (default) runs only after successful dependencies; `finally` marks the containing step as the finalizer and requires `finalizes.producer` plus the complete downstream `finalizes.consumers` frontier

```json
{ "id": "types", "task": "build-types", "if": "params.emitTypes == true" }
{
  "id": "teardown",
  "task": "database-down",
  "runOn": "finally",
  "finalizes": { "producer": "setup", "consumers": ["test"] }
}
```

Producer start arms the finalizer exactly once; cleanup waits until every listed
consumer is terminal. The producer task must declare an invocation-scoped
output, and the finalizer has no ordinary `dependsOn` or result exports.

**Input bindings** via `with`:

```json
{
  "id": "transpile",
  "task": "build-transpile",
  "with": {
    "target": { "value": "production" },
    "config": { "from": "command", "path": "flags.target" },
    "schemas": { "fromStep": "generate", "output": "schemas" }
  }
}
```

## The JSONL event protocol

Extensions communicate with the CLI via JSON Lines on stdout. Each line is a JSON object with protocol version `v: 1` and a `type` field. Stderr is reserved for human-readable diagnostic output.

### Invocation

The CLI spawns task commands as:

```text
{command} [args...] --putnamiContext <file> --output jsonl [job-flags...]
```

The `--putnamiContext` argument points to a temporary JSON file with workspace, project, extension, and parameter context:

```json
{
  "workspaceRoot": "/home/user/my-workspace",
  "workspace": { "name": "my-workspace", "rootPath": "/home/user/my-workspace" },
  "project": {
    "name": "@myorg/my-app",
    "path": "apps/my-app",
    "fullPath": "/home/user/my-workspace/apps/my-app"
  },
  "extension": { "name": "my-extension", "path": "./extensions/my-extension" },
  "job": { "name": "build" },
  "params": { "target": "linux/amd64" },
  "outputPath": "/tmp/putnami/my-extension/my-app/build",
  "cacheRoot": "/tmp/putnami/my-extension/my-app"
}
```

The `project` field is absent for workspace-level jobs.

### Event types

| Event | Description | Key fields |
|-------|-------------|------------|
| `meta` | Job metadata (emit once at start) | `data: { extension, job }` |
| `log` | Structured log message | `level`, `message` |
| `phase` | Lifecycle phase markers | `name`, `action` (`start`/`end`), `status` on end |
| `progress` | Progress reporting | `current`, `total`, `message` |
| `diagnostic` | Errors/warnings with source location | `severity`, `message`, `code`, `location: { file, line, column }` |
| `metric` | Performance and size metrics | `name`, `value`, `unit` |
| `artifact` | Output artifact registration | `id`, `name`, `kind`, `path` |
| `summary` | Human-readable label for job recap | `message` |
| `result` | Job result (must be last event) | `data: { status: "OK" \| "FAILED" \| "SKIP" }` |

### Example JSONL output

```jsonl
{"v": 1, "type": "meta", "time": "...", "level": "info", "message": "Starting build", "data": {"extension": "my-ext", "job": "build"}}
{"v": 1, "type": "phase", "time": "...", "name": "compile", "action": "start"}
{"v": 1, "type": "log", "time": "...", "level": "info", "message": "Compiling 42 files..."}
{"v": 1, "type": "progress", "time": "...", "current": 21, "total": 42, "message": "Compiling..."}
{"v": 1, "type": "diagnostic", "time": "...", "severity": "warning", "message": "unused import", "code": "TS2304", "location": {"file": "src/main.go", "line": 5, "column": 1}}
{"v": 1, "type": "metric", "time": "...", "name": "binary-size", "value": 4096, "unit": "bytes"}
{"v": 1, "type": "artifact", "time": "...", "id": "binary", "name": "App Binary", "kind": "file", "path": "bin/app"}
{"v": 1, "type": "summary", "time": "...", "message": "Build complete (4.1 KB)"}
{"v": 1, "type": "phase", "time": "...", "name": "compile", "action": "end", "status": "success"}
{"v": 1, "type": "result", "time": "...", "level": "info", "message": "Job OK", "data": {"status": "OK"}}
```

## Lifecycle primitives

Beyond commands and tasks, an extension can take ownership of three generic
lifecycle surfaces. There is no fourth: anything that does not fit one of them
stays an ordinary typed task.

### Runtime preparation

Declare the executable your tasks run on, and how to build it, instead of the
CLI knowing which wrapper binary your ecosystem ships:

```json
{
  "runtime": {
    "executable": "bin/putnami-ts",
    "prepare": {
      "command": "{extensionRoot}/bin/prepare",
      "args": ["--output", "{runtimeOutput}"],
      "inputs": ["cmd/**", "internal/**", "go.mod", "go.sum"]
    }
  }
}
```

Tasks then reference it as `{extensionRuntime}`. Preparation runs once per run,
before any job is scheduled; it resolves dependencies from the extension's own
module alone, so a locally prepared runtime and one shipped inside a published
archive are the same artifact. The prepared artifact's digest is part of every
cache key for tasks that run on it. `prepare` is optional — omit it when each
platform archive already carries the binary.

### Workspace adapter

Declare which directories are your projects, and what makes their metadata
stale:

```json
{
  "workspace": {
    "markers": ["package.json"],
    "inputs": ["package.json", "bun.lock", "tsconfig*.json"],
    "excludes": ["node_modules", "dist"],
    "syncTask": "workspace-sync"
  }
}
```

The CLI asks your extension — it never parses a language manifest itself. Every
`marker` must also appear in `inputs`, because the content digest of `inputs` is
what decides whether a recorded answer is still valid. Answers are cached in
`.putnami/workspace-index.json`, so an unchanged workspace starts **zero** probe
processes; only the providers whose own declared inputs moved are re-asked.
Provider-specific metadata is namespaced under your extension's name, and
`syncTask` is where you make your own manifest edits (`putnami projects sync`
runs it). An extension that declares no `workspace` block is never probed.

### Machine caches

An extension that keeps a machine-global cache owns it end to end. The CLI hands
it one stable directory (`extension.cacheRoot` in the job context) and never
looks inside. Declare the two reserved internal commands to participate in the
cache lifecycle:

| Command | Invoked by |
|---------|-----------|
| `cache-clean` | `putnami cache clean` — a cold reset |
| `cache-gc` | `putnami cache gc`, and the CLI's throttled opportunistic collection |

Both are ordinary commands running ordinary tasks, so cache policy is declared
and validated like the rest of your manifest.

### Invocation-scoped resources and cleanup

Use this for anything that exists only for the duration of one CLI invocation —
a throwaway database, a temporary credential, a socket:

```json
{
  "tasks": {
    "test-database-setup": {
      "kind": "command",
      "command": "{extensionRuntime}",
      "args": ["database", "up", "--dsn-file", "{invocationArtifactRoot}/database/dsn.env"],
      "cache": false,
      "declares": {
        "outputs": {
          "dsn": { "kind": "runtime-file", "scope": "invocation", "sensitive": true, "path": "database/dsn.env" }
        }
      }
    }
  },
  "commands": {
    "test": {
      "run": [
        { "id": "setup", "task": "test-database-setup" },
        { "id": "run", "task": "test-run", "dependsOn": ["setup"] },
        { "id": "teardown", "task": "test-database-teardown", "runOn": "finally",
          "finalizes": { "producer": "setup", "consumers": ["run"] } }
      ]
    }
  }
}
```

- `scope: "invocation"` keeps the output in the invocation's private scratch: it
  is never captured into the cache and never restored from it.
- `sensitive: true` guarantees its path and bytes never reach an event, a
  result, a session record, telemetry or cache traffic.
- `runOn: "finally"` plus `finalizes` makes a step the finalizer. The producer's
  start arms it exactly once, and it runs after every named consumer reaches a
  terminal state — including on failure and on Ctrl-C.
- A task with an invocation-scoped output must set `cache: false`: a cache hit
  would report success without ever creating the resource.

**Crash recovery.** A `SIGKILL` cannot run a finalizer, so cleanup is
contractual. The CLI holds a non-secret lease (invocation ID, PID, provider,
action digest, creation time); stamp the matching non-secret identity onto any
external resource you create, and reap resources whose lease has no live owner
before provisioning or reusing anything. The next invocation after a kill is
then the one that recovers the orphan, with no GC interval to wait for.
Credentials never belong in a lease, label, name, filter, log or error.

The full contract — every field, merge rule, lifecycle state and failure code —
is in `protocols/extension/doc/07-lifecycles.md`.

## Any-language extensions

Extensions can be written in **any language** — Go, Python, shell scripts, Rust — without requiring npm, TypeScript, or Bun. The only requirements are a `putnami.extension.json` manifest and executables that speak the JSONL protocol on stdout.

For an extension written in Go, the **extension SDK** (`putnami-extension-sdk`,
Go module `go.putnami.dev/sdk/extension`) supplies the orchestrator-facing half
so the extension only has to supply what its own ecosystem knows: the JSONL
emitter and structured log forwarding, the job context and flag parsing, the
manifest builder that runs the same validation the CLI runs *at build time*,
subprocess execution, and the neutral half of every shared lifecycle primitive —
machine-cache eviction, infra aggregation, database test environments,
checkout-independent generation paths, and the single writer of the
package→publish metadata index. Extensions in other languages implement the
manifest and the JSONL protocol directly, as the shell example below does.

```text
my-extension/
├── putnami.extension.json    # Manifest with commands and tasks
└── bin/
    ├── build                 # Executable (any language)
    ├── test
    └── lint
```

### Example: Shell script extension

**`putnami.extension.json`:**

```json
{
  "$schema": "https://putnami.dev/schemas/putnami-extension.json",
  "commands": {
    "build": {
      "flags": {
        "target": { "type": "string", "default": "release", "choices": ["debug", "release"] },
        "clean": { "type": "boolean", "default": false, "description": "Clean before building" }
      },
      "run": [{ "id": "build", "task": "build-exec" }]
    }
  },
  "tasks": {
    "build-exec": {
      "kind": "command",
      "command": "{extensionRoot}/bin/build",
      "cwd": "{projectRoot}",
      "inputs": {
        "source": { "from": "project", "files": ["src/**/*", "Makefile"] }
      },
      "cache": { "enabled": true }
    }
  }
}
```

**`bin/build`:**

```bash
#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/putnami-jsonl.sh"

CONTEXT_FILE="" TARGET="release" CLEAN="false"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --putnamiContext) CONTEXT_FILE="$2"; shift 2 ;;
    --output)        shift 2 ;;
    --target)        TARGET="$2"; shift 2 ;;
    --clean)         CLEAN="true"; shift ;;
    --no-clean)      CLEAN="false"; shift ;;
    *)               shift ;;
  esac
done

[ -z "$CONTEXT_FILE" ] && { echo "Error: --putnamiContext required" >&2; exit 2; }

parse_context "$CONTEXT_FILE"

emit_meta "$PUTNAMI_CTX_EXTENSION_NAME" "$PUTNAMI_CTX_JOB_NAME"
emit_phase_start "build"
emit_log "info" "Building $PUTNAMI_CTX_PROJECT_NAME (target=$TARGET)..."

if make -C "$PUTNAMI_CTX_PROJECT_PATH" "$TARGET" 2>/dev/stderr; then
  emit_phase_end "build" "success"
  emit_result "OK"
  exit 0
else
  emit_phase_end "build" "failed"
  emit_result "FAILED"
  exit 1
fi
```

### Example: Python extension

**`putnami.extension.json`:**

```json
{
  "$schema": "https://putnami.dev/schemas/putnami-extension.json",
  "commands": {
    "test": {
      "run": [{ "id": "test", "task": "test-exec" }]
    }
  },
  "tasks": {
    "test-exec": {
      "kind": "command",
      "command": "{extensionRoot}/bin/test",
      "cwd": "{projectRoot}",
      "inputs": {
        "source": { "from": "project", "files": ["**/*.py"] }
      },
      "cache": { "enabled": true }
    }
  }
}
```

**`bin/test`:**

```python
#!/usr/bin/env python3
import argparse, json, subprocess, sys
from datetime import datetime, timezone

def emit(event):
    event.setdefault("v", 1)
    event.setdefault("time", datetime.now(timezone.utc).isoformat())
    print(json.dumps(event), flush=True)

def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--putnamiContext", required=True)
    parser.add_argument("--output", default="jsonl")
    parser.add_argument("--verbose", "-V", action="store_true")
    args = parser.parse_args()

    with open(args.putnamiContext) as f:
        ctx = json.load(f)

    project = ctx.get("project")
    if not project:
        emit({"type": "result", "data": {"status": "SKIP"}})
        sys.exit(0)

    emit({"type": "meta", "level": "info", "message": "Starting test",
          "data": {"extension": ctx["extension"]["name"], "job": "test"}})
    emit({"type": "phase", "name": "test", "action": "start"})

    result = subprocess.run(
        ["python", "-m", "pytest", "-v" if args.verbose else "-q"],
        cwd=project["fullPath"], capture_output=True, text=True,
    )

    if result.returncode == 0:
        emit({"type": "phase", "name": "test", "action": "end", "status": "success"})
        emit({"type": "result", "level": "info", "message": "Job OK", "data": {"status": "OK"}})
    else:
        for line in result.stdout.splitlines():
            if "FAILED" in line:
                emit({"type": "diagnostic", "severity": "error", "message": line.strip()})
        emit({"type": "phase", "name": "test", "action": "end", "status": "failed"})
        emit({"type": "result", "level": "info", "message": "Job FAILED", "data": {"status": "FAILED"}})
        sys.exit(1)

if __name__ == "__main__":
    main()
```

## Lifecycle hooks

Extensions can define hooks for cross-cutting concerns like code generation.

### Command hooks

Command hooks execute as subprocesses:

```json
{
  "hooks": {
    "preBuild": {
      "kind": "command",
      "command": "bunx",
      "args": ["@my/extension:generate"],
      "cwd": "{projectRoot}",
      "timeoutMs": 120000
    }
  }
}
```

When several extensions declare the same hook for one project they share that
project's generated tree, so the order matters. Hooks run in ascending
`order` (default `0`), ties broken by extension name. Leave `order` unset for a
hook that only writes generated sources, and raise it for a hook that must
observe what the others generated — for example one that imports the workload
entry point.

### Module hooks

Module hooks import JavaScript modules directly:

```json
{
  "hooks": {
    "onLoad": "./hooks/on-load",
    "onBeforeJob": "./hooks/on-before-job",
    "onAfterJob": "./hooks/on-after-job"
  }
}
```

## Registering local extensions

Add extension paths to `putnami.workspace.json`:

```json
{
  "extensions": [
    "./my-extension",
    "/opt/shared-extensions/custom-ext"
  ]
}
```

Paths can be relative (resolved from workspace root) or absolute. Each path must contain a `putnami.extension.json`.

## Command priority

When multiple extensions provide the same command name, priority determines which one runs:

```json
{
  "commands": {
    "build": {
      "priority": 10,
      "run": [{ "id": "build", "task": "build-exec" }]
    }
  }
}
```

Higher priority value wins (default: 0). Only the highest-priority matching command runs for each project.

## Managing extensions

```bash
putnami extensions list              # List all active extensions
putnami extensions install           # Install extensions from workspace config
putnami extensions update            # Update to latest compatible versions
putnami extensions remove <ext>      # Remove an installed extension
putnami extensions validate [path]   # Validate an extension manifest
putnami extensions test [path]       # Run extension tests
putnami extensions package [path]    # Package for distribution
```

To materialize artifacts for a platform other than the invoking machine's — pre-warming an image layer, for example — name the target and a destination:

```bash
putnami extensions install --platform linux/amd64 --dest ./.gen/warm-artifacts
```

`--dest` receives a drop-in artifact-store root (`<dir>/sha256/<xx>/<digest>/`) that can be copied into `~/.putnami/artifacts` on the target machine. A materialization skips install hooks, leaves the stable extension links and `putnami.lock.json` untouched, fails closed when the lock has no integrity for the requested platform, and produces a byte-reproducible tree.

## Validating extensions

`putnami extensions validate` checks:

- JSON Schema conformance
- All pipeline steps reference defined tasks
- Pipeline DAG is acyclic
- Activation file patterns are valid globs
- Contract schema references are valid

```bash
putnami extensions validate ./my-extension
```
