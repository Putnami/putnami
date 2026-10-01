# Pipelines

Commands compose tasks into execution pipelines — DAGs of steps with dependency edges, input bindings, and conditional execution.

## Pipeline Steps

Each step in a command's `run` array references a task and declares its position in the DAG:

```json
{
  "run": [
    { "id": "generate", "task": "codegen", "dependsOn": ["^generate"] },
    { "id": "transpile", "task": "build-transpile", "dependsOn": ["generate"] },
    { "id": "types", "task": "build-types", "dependsOn": ["generate", "transpile"] },
    { "id": "compile", "task": "build-compile", "dependsOn": ["generate"] }
  ]
}
```

### Step Fields

| Field | Required | Description |
|-------|----------|-------------|
| `id` | **yes** | Unique step identifier (used by downstream bindings) |
| `task` | **yes** | Name of a task in the extension's `tasks` section |
| `dependsOn` | no | Step IDs or external references this step depends on |
| `with` | no | Input bindings passed to the task |
| `optional` | no | If true, failure marks step as skipped (not failed) |
| `if` | no | Plan-time condition over params, top-level commands, and resolved project facts — excludes step from DAG |
| `runOn` | no | Lifecycle condition: `success` (default), or `finally` with an explicit `finalizes` relation |
| `finalizes` | with `runOn: finally` | Invocation-resource producer and complete downstream consumer frontier |
| `activation` | no | Plan-time gate (project files) — splices step out of the DAG when the project doesn't match |
| `cache` | no | Per-step cache override |
| `timeoutMs` | no | Per-step timeout override |
| `heavy` | no | Marks the step CPU/memory heavy for worker tuning, overriding the command's `heavy` trait |
| `cpuWeight` | no | Relative multiplier on deterministic, history-derived CPU demand (default 1, capped by machine capacity). A project's `putnami.json` `tasks` entry overrides it. Execution hint only — never part of cache keys |

## Dependency References

The `dependsOn` field supports several reference types:

| Pattern | Meaning |
|---------|---------|
| `"transpile"` | Wait for step `transpile` in the same command |
| `"^generate"` | Wait for each upstream project's `generate` step of the same command |
| `"/workspace-install"` | Wait for the workspace-once command's `workspace-install` step |
| `"*generate"` | Wait for every other job that is the `generate` step, or belongs to the `generate` command |

The `^` prefix is the most common — it creates cross-project dependency chains so that downstream projects wait for their dependencies to complete the same phase.

References resolve by **exact identity**: the step id a job *is*, within the
command the reference is written in (`*` additionally matches a command name).
A job's display name, its internal plan name (`build~generate`) and synthesized
`project:name` keys are not addressable. A reference that matches nothing is a
no-op — an upstream project that does not run the step simply contributes no
edge — but a reference that matches nothing exactly while a removed name-based
spelling would have matched is a plan-time error naming what it would have
bound, so a stale spelling fails loudly instead of binding to a different job.

## Input Bindings

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

Three binding types:

| Type | Fields | Description |
|------|--------|-------------|
| Value | `value` | Literal value |
| Context | `from`, `path` | Value from command params or context |
| Step result | `fromStep`, `output` | Named output port from a previous step |

## Conditional Execution

### `if` (plan-time)

Evaluated before the pipeline runs. Removes the step entirely from the execution graph:

```json
{ "id": "types", "task": "build-types", "if": "params.emitTypes == true" }
```

The context has four plan-time roots:

| Root | Meaning |
|---|---|
| `params.<name>` | Fully merged command parameter |
| `commands.<name>` | Whether `<name>` is an effective top-level command for this provider and selected project after matching and disable rules |
| `commandParams.<command>.<name>` | Fully resolved parameter `<name>` for an effective command in the same provider/project scope |
| `project.type` | Target project's resolved classification, including provider probing and authored overrides |

`commands` is a set, so command ordering and duplicates do not affect a
condition. `commandParams` can compare compile-affecting settings before one
command treats another as replacement evidence, for example
`params.race != commandParams.test.race`. A planner that cannot supply the
newer invocation/project context keeps a step that references it instead of
pruning from an unknown fact; older params-only expressions retain their
historical behavior.

### `runOn` (execution)

Where `if` and `activation` decide whether a step is **planned**, `runOn`
selects its lifecycle condition:

| Value | Meaning |
|-------|---------|
| `success` | *(default)* Run only if every dependency succeeded |
| `finally` | The containing step is an invocation-resource finalizer and must carry `finalizes` |

```json
{
  "id": "teardown",
  "task": "database-down",
  "runOn": "finally",
  "finalizes": {
    "producer": "setup",
    "consumers": ["test"]
  }
}
```

`finally` is the finalizer form: the teardown half of an
[invocation-scoped resource](07-lifecycles.md) must run exactly once whenever
its producer started, or the resource outlives the invocation that created it.
`finalizes.producer` must use a task that declares an invocation-scoped output;
`finalizes.consumers` is the complete downstream terminal frontier cleanup
waits for. A finalizer has no ordinary `dependsOn`, accepts no step-result input,
and exports no task or command result. It locates cleanup state through the
explicit `{invocationArtifactRoot}` argument described in the lifecycle
contract.

This is the only runtime step gate. A `when` field — a runtime expression over
step results — existed in the schema, had no consumers and no manifest usage,
and was deleted: two overlapping runtime gates is one more than any pipeline
needs.

### `activation` (plan-time, project-aware)

Where `if` sees resolved planner facts but does not inspect source contents,
`activation` is evaluated against the target project's files, so the planner
can drop steps that would deterministically skip at runtime for an entire class
of projects:

```json
{
  "id": "describe",
  "task": "build-describe",
  "dependsOn": ["^describe", "generate"],
  "activation": { "contains": { "go.mod": "go.putnami.dev/app" } }
}
```

| Field | Meaning |
|-------|---------|
| `files` | Active only if at least one project file matches one of these globs |
| `contains` | Map of project-relative file path → required substring; active only if every file exists and contains its substring |

A step is active only when **every** declared condition holds, and conditions fail closed — a missing or unreadable file makes the step inactive, mirroring the runtime skip checks so pruning never removes a step that would have done real work.

Unlike `if` (which drops an excluded step's edges), an inactive `activation` step is **spliced out**: its dependents inherit its dependencies, so ordering and data-flow through the remaining steps are preserved. This removes a no-op node — and its process spawn and cache lookup — from the DAG instead of scheduling it just to skip. Older toolchains that predate `activation` ignore the field and fall back to scheduling-and-skipping, so adding a gate is backward compatible.

## Output Bindings

Commands can project results from steps as named outputs:

```json
{
  "outputs": {
    "bundle": { "fromStep": "transpile", "path": "data.outputPath" }
  }
}
```

These outputs are accessible by downstream commands in the workspace dependency graph.
