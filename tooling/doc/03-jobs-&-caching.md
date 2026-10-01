# Jobs & Caching

Extensions expose functionality through **commands** (the user-facing API) and implement it with **tasks** (atomic executable units). The CLI orchestrates tasks across projects and uses caching to avoid redundant work.

## Commands and tasks

A command is a user-facing operation (e.g., `putnami build`). Each command defines a pipeline of steps that reference tasks:

```json
{
  "commands": {
    "build": {
      "run": [
        { "id": "generate", "task": "build-generate", "dependsOn": ["^generate"] },
        { "id": "transpile", "task": "build-transpile", "dependsOn": ["generate"] }
      ]
    }
  },
  "tasks": {
    "build-generate": {
      "kind": "command",
      "command": "{extensionRoot}/bin/build",
      "args": ["generate"],
      "cwd": "{projectRoot}",
      "timeoutMs": 300000,
      "inputs": {
        "source": { "from": "project", "files": ["src/**/*.ts"] },
        "config": { "from": "project", "files": ["package.json"] }
      },
      "cache": { "enabled": true, "deterministic": true }
    }
  }
}
```

**Command fields:**

| Field | Required | Description |
| ----- | -------- | ----------- |
| `run` | **yes** | Pipeline step DAG |
| `description` | no | Human-readable description |
| `activationFiles` | no | File patterns that must exist for the command to activate |
| `flags` | no | CLI flags the command accepts |
| `priority` | no | Higher priority wins when multiple extensions provide the same command |
| `quiet` | no | Suppress from job recap |
| `channel` | no | Execution channel for concurrency control |

**Task fields:**

| Field | Required | Description |
| ----- | -------- | ----------- |
| `kind` | **yes** | Execution kind (currently only `command`) |
| `command` | **yes** | Command to execute |
| `args` | no | Command arguments |
| `cwd` | no | Working directory (supports template variables) |
| `env` | no | Environment variables |
| `timeoutMs` | no | Execution timeout (default: 300000ms) |
| `inputs` | no | Input port declarations for cache invalidation |
| `outputs` | no | Output port declarations |
| `writes` | no | Resources written; conflicting accesses are serialized (see [Write-resource serialization](#write-resource-serialization)) |
| `reads` | no | Resources read; serialized against conflicting writers |
| `cache` | no | Cache policy (`{ "enabled": true }`) |

**Template variables** for `command`, `args`, `cwd`, and `env`:

| Variable | Description |
| -------- | ----------- |
| `{workspaceRoot}` | Absolute path to workspace root |
| `{projectRoot}` | Absolute path to project root |
| `{extensionRoot}` | Absolute path to extension package |
| `{selectedProjects}` | Comma-separated selected project names for workspace-once jobs |
| `{selectedProjectIDs}` | Comma-separated selected project IDs for workspace-once jobs |
| `{selectedProjectPaths}` | Comma-separated selected project paths for workspace-once jobs |
| `{selectedProjectRoots}` | Comma-separated selected project roots for workspace-once jobs |

## Pipeline steps

Each step in a command's `run` array references a task and declares its position in the DAG:

### Step fields

| Field | Required | Description |
| ----- | -------- | ----------- |
| `id` | **yes** | Unique step identifier |
| `task` | **yes** | Name of a task in the extension's `tasks` section |
| `dependsOn` | no | Step IDs or external references this step depends on |
| `with` | no | Input bindings passed to the task |
| `if` | no | Plan-time condition (excludes step from DAG when false) |
| `runOn` | no | Lifecycle condition: `success` (default), or `finally` with an explicit `finalizes` relation |
| `finalizes` | with `runOn: finally` | Invocation-resource producer and complete downstream consumer frontier |
| `optional` | no | If true, failure marks step as skipped instead of failed |

### Dependency references

The `dependsOn` field supports several reference types:

| Pattern | Meaning |
| ------- | ------- |
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

### Command barriers

Command-level `dependsOn` auto-plans prerequisite commands. Bare command names
are scoped to the same project:

```json
{ "publish": { "dependsOn": ["build"] } }
```

Use `!command` for a session barrier:

```json
{
  "publish": { "dependsOn": ["!lint", "!test", "!build", "!package"] },
  "deploy": { "dependsOn": ["!publish"] }
}
```

`!build` plans `build` across the current project selection and makes each root
step of the dependent command wait for every `build` leaf job in the final plan.
These are functional dependencies: if a barrier job fails, downstream
side-effecting jobs are skipped unless `--continue-on-error` is set.

### Embedded flags

Append flags after a dependency reference to request a specific subset of work:

```json
"dependsOn": ["^build --generate"]
```

The planner splits each string on whitespace — the first token is the reference, the rest are forwarded as arguments. When multiple steps schedule the same dependency with different flags, the planner upgrades to an unrestricted execution so the result satisfies all dependents.

### Task-level upstream dependencies

When a `^` ref matches a step in the current pipeline, the planner emits only that individual task for each upstream project — not the full pipeline:

```json
{
  "commands": {
    "package": {
      "run": [
        { "id": "generate", "task": "build-generate", "dependsOn": ["^generate"] },
        { "id": "transpile", "task": "build-transpile", "dependsOn": ["generate", "^transpile"] },
        { "id": "docker", "task": "package-docker", "dependsOn": ["transpile"], "if": "params.docker" }
      ]
    }
  }
}
```

Running `putnami package my-app` (where my-app depends on sdk which depends on utils):

- utils and sdk get only `generate` and `transpile` tasks
- my-app gets the full pipeline (generate, transpile, docker)
- No upstream docker tasks are scheduled

When the `^` ref does NOT match any step in the pipeline, the planner schedules the full command on upstream projects.

### Input bindings

Steps can pass data to tasks via `with`:

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

### Conditional execution

- `if` — evaluated at plan-time. Removes the step entirely from the DAG: `"if": "params.compile"`
- `runOn` — evaluated at execution time. `success` (the default) runs only after successful dependencies. `finally` marks the containing step as the finalizer; `finalizes.producer` start arms it once and cleanup waits for every downstream step in `finalizes.consumers` to become terminal. A finalizer uses no ordinary `dependsOn` and exports no result

```json
{
  "id": "teardown",
  "task": "database-down",
  "runOn": "finally",
  "finalizes": { "producer": "setup", "consumers": ["test"] }
}
```

## Write-resource serialization

`dependsOn` is for **functional** ordering — a step needs another step's output.
Some ordering, though, exists only to keep two jobs from writing the same place
at once: several commands regenerate the same `.gen` tree, and multiple projects
may publish to one shared output root. Expressing that as `dependsOn` would make
the DAG needlessly conservative and blur the line between "needs this output" and
"must not write at the same time."

Instead, a task declares the resources it touches:

```json
"tasks": {
  "build-generate": { "kind": "command", "command": "...", "writes": ["gen"] },
  "build-compile":  { "kind": "command", "command": "...", "reads":  ["gen"] }
}
```

The planner then serializes only **conflicting** accesses, independently of
functional dependencies:

| Access pair (same resource) | Result |
| --------------------------- | ------ |
| writer + writer | serialized — never run concurrently |
| writer + reader | serialized — the reader observes the writer it derives from |
| reader + reader | independent — run in parallel |

A resource is a bare string (project-scoped, e.g. each project's own `.gen`) or
an object with an explicit scope:

```json
"writes": [{ "id": "registry", "scope": "workspace" }]
```

| Scope | Conflict domain |
| ----- | --------------- |
| `project` (default) | Only within the same project |
| `workspace` | Across every project sharing the id |

Because these serialize edges are separate from `dependsOn`, they never enter
cache keys and a serialized predecessor failing does not skip its successor.
A read-only step (a no-fix lint check, or a build/test task reading project
sources) declares `reads` but no `writes`, so it stays parallel with other
readers and only serializes against a planned writer of the same resource. Split
mode-dependent tasks when needed: fix-mode lint can declare
`writes: ["sources"]`, while no-fix lint declares `reads: ["sources"]`.

## Orchestration

Job orchestration happens in two phases:

**Planning:**

1. Resolve target projects (`--all`, `--impacted`, explicit names, etc.)
2. Load extension command and task definitions
3. Expand `dependsOn` into concrete task nodes (project + task pairs)
4. Build a dependency DAG

**Execution:**

1. Start all tasks concurrently
2. Each task waits on its dependencies before running
3. Results and events are collected into a single session

Parallelism is automatic — tasks without mutual dependencies run at the same time. Use `--max-parallel auto`, `eco`, `max`, or an explicit number to tune concurrency.

If a dependency is already in the scheduling stack, the edge is skipped to avoid cycles. Planning continues, but the cycle indicates a configuration problem.

Use `--plan` to preview the execution tree without running:

```bash
putnami build --impacted --plan
```

## Caching

Caching avoids re-executing tasks whose inputs have not changed.

**How it works:**

1. Tasks declare `inputs` with file globs (`from: "project"`, `files: [...]`)
2. The runner computes a hash from matched files
3. Cache keys combine:
   - Task identity (extension, task name, project)
   - Task parameters
   - Dependent task hashes
   - Project file hash

If any file matched by the input declaration changes, the hash changes and the cache is invalidated.

**Flags:**

- `--no-cache` — Skip cache reads but still write results
- `putnami cache clean` — Delete all cached data

## Watch mode

The `--watch` flag enables pipeline-aware file watching. When files change, running tasks are aborted and the pipeline is re-executed. Watch mode forces `--no-cache` to ensure fresh results on every iteration.

```bash
putnami test . --watch
putnami build --all --watch
putnami serve my-app --watch
```

**Behavior:**

- Polling-based file system monitoring (100ms intervals)
- 150ms debounce for rapid edits
- Automatically excluded: `.git/`, `node_modules/`, `.putnami/`, `dist/`, `build/`, `__pycache__/`, `.venv/`
- Changed files are mapped to projects, and only affected tasks re-run
- Dependencies propagate transitively — if a library changes, dependent apps re-run

### Multi-service sessions

When running `putnami serve`, watch mode discovers and starts all runtime service dependencies alongside the primary target. Services are found via `putnami.runsWith` in `package.json` or by walking the dependency graph for projects with a `./serve` export.

```bash
putnami serve web-app              # Starts web-app + discovered services
putnami serve web-app --no-services  # Only the target project
```

On file change, only affected services are selectively restarted. Unaffected services keep running.

## Disabling jobs

Jobs can be disabled at workspace or project level. Disabled jobs are excluded during planning.

**Workspace level** — in the root `package.json`:

```json
{
  "putnami": {
    "disable": {
      "jobs": ["lint"]
    }
  }
}
```

**Project level** — in the project's `package.json`:

```json
{
  "putnami": {
    "disable": {
      "jobs": ["@putnami/typescript:lint"]
    }
  }
}
```

Unqualified names (e.g., `"lint"`) disable all jobs with that name. Qualified names (e.g., `"@putnami/typescript:lint"`) disable only that extension's job.

## Overriding extension jobs

A project can override extension-provided jobs in its `putnami.json` under the `jobs` key:

```json
{
  "name": "my-custom-app",
  "extensions": ["@putnami/typescript"],
  "jobs": {
    "build": {
      "kind": "command",
      "command": "bun",
      "args": ["run", "{projectRoot}/scripts/build.ts"],
      "cwd": "{projectRoot}",
      "dependsOn": ["^build"]
    }
  }
}
```

Overrides only affect the specific project. The project acts as its own job provider — `{extensionRoot}` resolves to `{projectRoot}`. You can also define entirely new job names that don't exist in any extension.

## Job status

Jobs return one of: `OK`, `FAILED`, or `SKIP` (and may be shown as `cached` when served from cache).

Common skip cases:

- A dependency failed
- The job decides to no-op in its own implementation
- The job is listed in `disable.jobs`

## Command naming scheme

Putnami uses a consistent naming scheme across all extensions:

| Command | Scope | Description |
| ------- | ----- | ----------- |
| `build` | project | Compile and bundle |
| `test` | project | Run tests |
| `lint` | project | Format and lint code |
| `serve` | project | Run development server |
| `package` | project | Create distribution packages |
| `publish` | project | Publish to registries |
