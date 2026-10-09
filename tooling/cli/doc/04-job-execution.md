# Job Execution

Job execution is the core workflow of the CLI. When a user runs `putnami build --impacted`, the CLI plans which jobs to run, resolves their dependency order, and executes them as subprocesses with parallel scheduling and caching.

## Execution Pipeline

```
Command(s) + Project Selection
        │
        ▼
  ┌─────────────┐
  │  Discovery   │  Load workspace, discover extensions
  └──────┬──────┘
         │
         ▼
  ┌─────────────┐
  │  Validation  │  Contract validation on task I/O wiring
  └──────┬──────┘
         │
         ▼
  ┌─────────────┐
  │  Selection   │  Filter projects (--impacted, --tag, ...)
  └──────┬──────┘
         │
         ▼
  ┌─────────────┐
  │   Planning   │  Match commands → extensions, expand pipelines, resolve deps
  └──────┬──────┘
         │
         ▼
  ┌─────────────┐
  │   Baseline   │  Capture generated artifacts before anything writes
  └──────┬──────┘
         │
         ▼
  ┌─────────────┐
  │  Scheduling  │  DAG execution with parallel workers, cache, hooks
  └──────┬──────┘
         │
         ▼
  ┌─────────────┐
  │  Reporting   │  Render results, record session, emit telemetry
  └─────────────┘
```

## Planning

The planner (`internal/jobs/planner.go`) produces a flat list of `ScheduledJob` entries with resolved dependencies.

### Job Matching

For each `(command, project)` pair, the planner finds every compatible extension job:

1. **Collect candidates** — All extensions providing this command name.
2. **Filter** — Remove disabled jobs/extensions (workspace or project level). Check channel compatibility. Check activation files.
3. **Sort** — By priority (descending), then project extension match, then activation file specificity, then extension name.
4. **Compose** — Every remaining candidate contributes its `run` steps to the plan. A single matching extension keeps the historical job names; multiple matching extensions get internal namespaced DAG keys while the displayed command/step names remain readable.

Activation files are glob patterns (e.g., `tsconfig.json`, `go.mod`, `**/*.go`) that must exist in the project directory for a job to activate. This enables multi-language workspaces where only the relevant extension handles each project.

When multiple active contributors define the same flag for a command, the definitions must be byte-for-byte equivalent after manifest parsing. Identical shared flags are accepted and merged once. Incompatible shared flags fail planning with an error naming the command, flag, and extensions. Flags that only one contributor declares remain available to that contributor through normal command params.

### Pipeline Expansion

When a matched job has `pipelineSteps`, each step is expanded into an individual scheduled job:

1. **Activation pruning** — Steps that declare an `activation` gate are dropped when the target project doesn't match. Activation probes the project's files, so the planner can remove steps that would deterministically skip at runtime — e.g. a Go `describe` step for a project that doesn't import the app framework. A pruned step is *spliced out* rather than dropped: its dependents inherit its dependencies, so ordering and data-flow through the remaining steps are preserved. Conditions fail closed (a missing/unreadable file makes the step inactive), mirroring the runtime skip checks so pruning never removes a step that would have done real work.
2. **Evaluate conditions** — Plan-time `if` expressions are evaluated against merged params, the order-independent effective command set for the current provider/project (`commands.<name>`), each effective command's fully resolved params (`commandParams.<command>.<name>`), and the target's resolved classification (`project.type`). Steps whose condition is false are excluded (their edges are dropped, not bridged). Disabled, unmatched, or dependency-synthesized commands do not become replacement evidence, and a context-free compatibility caller keeps conditions that need planner facts rather than pruning from an unknown value.
3. **Dead-code elimination** — Walk backward from surviving leaves to collect only needed dependencies. Unreachable steps are pruned.
4. **Name qualification** — Each step gets a displayed name: `{commandName}~{stepId}` (e.g., `build~generate`, `build~transpile`). If multiple extensions contribute to the same command for the same project, scheduler keys are internally namespaced by extension identity to avoid collisions.
5. **Dependency resolution** — Intra-pipeline references become qualified names. External references (`^`, `/`, `*`) are passed through for cross-project resolution.

#### Step activation

A pipeline step may declare an `activation` gate in the extension manifest:

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
| `files` | Step is active only if at least one project file matches one of these globs. |
| `contains` | Map of project-relative file path → required substring; the step is active only if every listed file exists and contains its substring. |

Use `activation` when the decision requires probing project files. Use `if`
when it is a function of resolved params, top-level invocation commands, or
project classification. Older toolchains that predate `activation` ignore the
field and fall back to scheduling-and-skipping, so adding a gate is backward
compatible.

### Parameter Merging

Job parameters are merged from multiple sources in a defined order (later overrides earlier):

1. **Workspace config defaults** — `options.*`, `options.{command}`, `options.{extension}`, `options.{extension}:{command}`
2. **Manifest flag defaults** — Default values declared in the extension manifest's `flags` field
3. **Project options** — `options.{command}`, `options.{extension}`, `options.{extension}:{command}` from `putnami.json`
4. **CLI flags** — Flags passed on the command line (highest priority)

This merged parameter set is used alongside the planner-owned command/project
facts for plan-time `if` evaluation and is passed to job subprocesses.

### Cross-Project Dependencies

After expanding all pipelines, the planner resolves external dependency references:

| Reference | Meaning | Resolution |
|-----------|---------|------------|
| `^stepId` | Same step in upstream dependency projects | Resolves to `{depProject}:{command}~{stepId}` for each dependency |
| `/stepId` | Same step at workspace level | Resolves to `{workspaceName}:{command}~{stepId}` |
| `*stepId` | Same step in all other projects | Resolves to all matching jobs across the plan |

Upstream emission ensures transitive dependencies get their required pipeline steps planned, even if those projects weren't in the original selection.

### Command Barriers

Command-level `dependsOn` can also auto-plan prerequisite commands:

```json
{
  "publish": { "dependsOn": ["!lint", "!test", "!build", "!package"] },
  "deploy": { "dependsOn": ["!publish"] }
}
```

Bare command names are same-project prerequisites. `dependsOn: ["build"]`
plans `build` for each project that has the dependent command, then the
dependent command's root steps wait for that project's `build` leaf steps.

The `!` prefix creates a session barrier. `!build` plans `build` across the
current project selection, then root steps of the dependent command wait for
every `build` leaf job in the final plan. These are functional dependencies, so
a failing barrier job skips downstream side-effecting jobs unless
`--continue-on-error` is set.

For a dependent-owned conditional prerequisite, `sessionPrerequisites` adds a
selection override, per-project policy, gate commands, and invocation-local
parameter bindings. The planner first filters the projects, expands all
contributors to the prerequisite command with those local params, wires every
prerequisite root after every declared gate leaf, then wires the dependent roots
after every prerequisite leaf. Constant bindings use `{ "value": ... }`;
`{ "fromProjectParam": "path" }` copies a dotted path from the dependent
command's parameters for that project. The bindings do not alter a separate,
explicit invocation of the prerequisite command. If that invocation, an
ordinary `dependsOn`, or another relation already planned the same command with
different local parameters, planning fails; keys are never first-wins.
Relation-local params cannot overwrite literal `run[].with` bindings, and a
missing `fromProjectParam` path also fails closed. Workspace-once contributors
accept shared constants and merge compatible project selections, but reject a
project-derived binding across multiple selected projects.

### Write-Resource Serialization

Functional dependencies (`dependsOn`) express correctness ordering: a step needs
another step's *output*. They are deliberately distinct from ordering that exists
only to stop two jobs from writing the same place at once. Tasks declare the
resources they touch so the planner can add the latter without conflating it with
the former:

```jsonc
"build-generate": { "writes": ["gen"] },              // rewrites the .gen tree
"build-compile":  { "reads":  ["gen"] },              // consumes generated code
"publish-push":   { "writes": [{ "id": "registry", "scope": "workspace" }] }
```

A resource reference is a bare string (shorthand for project scope) or an object
with an explicit `scope`:

| Scope | Conflict domain |
|-------|-----------------|
| `project` (default) | Two accesses conflict only within the same project (e.g. each project's own `.gen`). |
| `workspace` | Accesses to the same id conflict across every project (e.g. a shared registry or output root). |

After functional dependencies are resolved, `serializeWriteResources`
(`internal/jobs/planner_resources.go`) inspects declared accesses and adds
**serialize edges** (`ScheduledJob.SerializeAfter`) so that, per resource:

- two **writers** never run concurrently;
- a **reader** never overlaps a writer, and observes the writer it derives from;
- two **readers** stay independent and run in parallel.

Edges are emitted along a deterministic topological order of the functional
graph, so they can never introduce a cycle. They are kept separate from
`DependsOn`: they order execution but are **not** functional dependencies — they
do not contribute to cache keys, and a serialize predecessor failing does not
skip its successor. Read-only steps (a no-fix lint check, a compile that only
reads generated code or project sources) declare `reads` but no `writes`, so
they only serialize when another planned task writes the same resource. A
fix-mode linter that mutates project files should be a distinct task from the
no-fix check and declare `writes: ["sources"]`; source-consuming build/test/lint
tasks should declare `reads: ["sources"]`.

This replaces an earlier heuristic that ordered every non-build `generate` step
after the whole build command; serialization is now driven by declared conflicts,
which keeps the shared `.gen` tree safe while letting independent work start
sooner.

### Generated-Client Ordering

A generated client is a project of its own whose provider writes its directory.
Resources cannot express that relation, because a resource conflicts only
within one project. After write serialization, `orderContractClients`
(`internal/jobs/planner_deps.go`) adds serialize edges from each job of a
contract client to its provider's jobs, under two rules:

- **Step rule.** A client step that references `^<step>` also waits for the
  provider's step of that name. A client's `build~generate` waits for the
  provider's `build~generate`.
- **Tree rule.** Every client job waits for every provider job whose task
  declares a durable `directory` output with `pathFrom`. That output is how a
  generator declares a client whose directory the provider's configuration
  chooses, such as `clientgen~generate-ts` or the TypeScript `build~generate`.
  A project-rooted output lies inside the provider's directory, so the rule
  applies only when the client's directory and the provider's directory nest.
  A workspace-rooted output applies to every client. The command-output and
  invocation roots apply to none. A file output never applies.

Both rules add ordering only. They never add a job to the plan, so an
unplanned provider adds no edge. They skip finalizers, which the invocation
runtime dispatches. They also skip any edge that would close a cycle, for
example when a provider imports its own client: the client jobs that the
provider's writer depends on already run before it, through the writer's
dependencies, and the other client jobs wait. The check covers functional, write-serialization, and contract edges, so
the plan stays a DAG.

### Cross-Command Shared Executions

Several commands can schedule separate DAG nodes over the **same** manifest
task, project, declared inputs and functional producer chain. The cache cannot
collapse that duplication: a task-owned entry is addressed partly by the job
name, so one command's node cannot reach another's entry no matter how
identical the work is.

`attachSharedExecutions` (`internal/jobs/plan_shared.go`) groups those nodes at
plan time and stamps each group with a shared id, visible in `--plan` as
`[shared-1]`. At run time the first member to reach a cache **miss** executes;
every later member adopts its result instead of spawning an identical subprocess
and reports the outcome as `coalesced`.

What the grouping does **not** touch: the plan has the same nodes, the same
`dependsOn`/`serializeAfter` edges and the same cache keys it had without it. Each
member still computes its own key, publishes its own entry and emits its own
session row, so a **warm** run is exactly what it was before — only a **cold** run
loses the duplicate subprocess. The shared subprocess appears once in the session's
physical execution ledger, referenced by every logical row that came out of it.

A node joins a shared group only when all of the following hold, and each
condition removes a way one physical run could fail to be what the second node
would have produced:

| Condition | Why |
|-----------|-----|
| Same project, extension, manifest task and task-contract digest | The identity of the work itself. |
| Same recursive functional-producer identity | Downstream nodes are not unified when any direct or transitive producer has different declared work or literal task params. |
| No `with:` binding on the candidate step | Literal values are live task params; context/result bindings are not reducible to one plan-time value, so input-bound candidates stay separate. |
| Task declares `cache.deterministic` | The manifest's own statement that identical inputs give identical outputs. |
| Task declares a complete `inputs` contract | "The same declared inputs" is only a complete statement when the task declares them; contract-declared params and explicit step literals are compared. |
| Task declares at least one `writes` resource, and none of them is the project `gen` tree | A shared run's side effects must be ones the planner can serialize, and `.gen` is a subtree other tasks write into without owning it. |
| Same resolved deadline, file/env patterns, runtime inputs, selected projects | Everything else a cache key would have distinguished, minus the command name. |

Nothing designates a leader in advance, which is what keeps the mechanism correct
on partial plans: a member served from its own cache entry takes no slot, and a
member skipped by a failed dependency never arrives, so the surviving node
executes exactly as it would have alone. A failure is adopted like a success —
two nodes over identical inputs fail identically, and each command's dependents
then behave exactly as they would have without the sharing.

### Scheduled Job Structure

```go
type ScheduledJob struct {
    Project        *Project              // Target project
    Extension      *ExtensionDescription // Providing extension
    JobDef         *JobDefinition        // Job definition with command, args, cache policy
    Step           *PipelineStep         // Pipeline step (nil for single-step jobs)
    DependsOn      []string              // Functional dependency keys (data/correctness)
    SerializeAfter []string              // Write-serialization edges (ordering only)
}
```

Job keys use the format `{projectName}:{jobName}` (e.g., `my-app:build~transpile`).

## Declared-Output Drift

A generator that writes into the worktree — a client, a schema sidecar —
produces bytes the repository commits. Whether those bytes still equal what
the current inputs generate is answered by the task that writes them, at the
moment it writes: a verifier that runs later in the session reads a tree the
generator, or a cache restore of its entry, has already rewritten.

A declared output carrying `drift: "warn" | "fail"` asks the scheduler for that
comparison. It snapshots the output path immediately before the task writes —
in `openTask`, before the preBuild hook and the subprocess, or in
`restoreDeclaredCacheHit`, before the recorded tree is swapped in — and walks
it again afterwards. A difference is the diagnostic `generated-output-drift`,
naming the added, removed and changed paths; under `fail` the task also fails
with that code, its outputs still written or restored so the worktree holds
the regenerated bytes to commit. A `pathFrom` output is judged inside a digest
of its project root, taken before the run. Nothing about drift reaches a cache
key, an entry or the failure cache. See
[10-caching.md](10-caching.md#declared-output-drift-committed-generated-bytes)
and ADR [0034](adr/0034-a-declared-output-polices-its-own-drift.md).

## Scheduling

The scheduler (`internal/jobs/scheduler.go`) executes the planned DAG using a goroutine worker pool.

### DAG State Machine

The scheduler tracks unresolved dependency counts for each job:

1. **Initialization** — Count dependencies for each job. Jobs with zero dependencies are immediately ready.
2. **Dispatch** — Admit ready jobs against the run's CPU and usable-memory pool,
   then send them to workers through a buffered channel. The pending queue is
   ordered by each job's historical duration (longest first), so the task that
   bounds the makespan starts immediately instead of behind the short tail. If
   that task is temporarily too large for the remaining pool, the coordinator
   waits for a completion instead of backfilling shorter work into every newly
   released slot and starving the critical-path chain. Jobs without history
   keep deterministic key order.
3. **Completion** — When a job finishes, decrement its dependents' counters. Newly-ready jobs enter the pending queue.
4. **Termination** — When all jobs complete (or context is cancelled).

### Worker Pool

```
                ┌──────────┐
 pending ─────▶ │ readyCh  │ ◀──── coordinator
                └────┬─────┘
                     │
         ┌───────────┼───────────┐
         ▼           ▼           ▼
    ┌─────────┐ ┌─────────┐ ┌─────────┐
    │ worker1 │ │ worker2 │ │ worker3 │  (resolved worker policy)
    └────┬────┘ └────┬────┘ └────┬────┘
         │           │           │
         └───────────┼───────────┘
                     ▼
                ┌──────────┐
                │ doneCh   │ ────▶ coordinator
                └──────────┘
```

- **Max workers** defaults to the `auto` policy, configurable with `--max-parallel auto|eco|max|<n>`.
- **Resource admission** independently bounds physical subprocesses. The
  coordinator owns every reservation, attaches it to the dispatched group, and
  releases it when that group returns on success, failure, or cancellation.
  Worker timing therefore cannot race to choose admission order.
- **Named resource budgets** are the second admission dimension, for a scarce
  thing *outside* the machine — connections, ports, bandwidth. A task declares
  what it consumes (`resources: { "db-connections": 230 }` in the manifest); the
  run declares what exists (`--resource db-connections=400`, repeatable). A task
  starts when the worker count allows it and every resource it claims still has
  budget, so one task kind's ceiling no longer caps tasks that never touch it. A
  resource with no budget on the run is unlimited, and a single claim larger than
  the whole budget is refused at plan time rather than waiting forever. See
  [Resource Budgets](07-extensions.md#resource-budgets).
- The coordinator uses non-blocking sends to avoid deadlock between the ready and done channels.
- Workers pull jobs from `readyCh` and push results to `doneCh`.

### Opportunistic task batching

Cacheable per-project tasks can opt into same-key batching through the extension
manifest's `batchable` policy. The coordinator groups jobs only when at least
two compatible peers are already ready; it never waits on a coalescing window.
The group shares one extension invocation, whose `batchResults` are split back
into normal per-project results, cache entries, session rows, and dependency
outcomes.

The batch key includes the tool, task identity, effective parameters,
toolchain, resolved configuration, synthesized execution environment, and
policy bounds. `maxProjects` caps group size. `maxProjectsParam` lets a
workspace or project replace that cap through its options: the resolved value
takes the static cap's place in the batch key, an unset option leaves the key
unchanged, `1` keeps singleton dispatch, and a value that is not a positive
integer fails the plan. The parameter never enters a task cache key. `maxWorkers`
disables batching when the resolved scheduler concurrency exceeds a tool's
measured economic envelope. Extension tasks without the policy retain singleton
dispatch.

Batching comes in two forms. A **shared-config** tool (Biome lint, Ruff) passes
all grouped projects to one tool invocation, so its `configFiles` must resolve
to the same source before peers may group. An **extension-loop** producer (the
TypeScript build producers — `generate`, `transpile`, `types`, `compile`) runs
each project's own transpile/tsc/generate with its own config discovery inside
the shared process; it declares only a stable `tool` label and no `configFiles`,
so config compatibility never gates grouping — only same task kind, parameters,
and toolchain. Each project still writes its own outputs and gets its own cache
entry; the batch is execution-grouping only. A producer whose Data is nil (e.g.
transpile) carries its per-project counters through the wire result's `metrics`
field so the reconstructed terminal rows and metrics match a singleton run.
Because an extension-loop producer runs its projects **sequentially** under the
task's single timeout — and a timeout emits no `batchResults`, failing every
member including already-built peers — each producer sets a `maxProjects` bound
so a large or slow ready group cannot exceed that one window. Per-project tool
failures stay isolated (the loop records the failure and continues); only a
whole-batch timeout or crash fails peers together, which the bound and the
scheduler's one-shot batch retry contain.

Batching preserves the scheduler's longest-first queue: compatible peers may
join the job at the head of the ready queue, but a later batch never jumps
ahead of earlier ordinary work. A lone batch candidate may yield to ready
ordinary work when that work can unblock a compatible peer; the scheduler does
not otherwise wait for a batch to fill.

Batching amortizes only the extension-process overhead `F` (fork/exec +
extension boot + config + toolchain probe, ~0.18s/task). Each project still
pays its own tool startup: a single-tool task such as `lint` runs the tool once
over every grouped path, while an **extension-loop** task such as `test-run`
runs each project's suite in turn inside the shared process. Because grouping
serializes N suites onto one worker, heavy extension-loop tasks (`test-run`
carries `heavy: true`) set a conservative `maxWorkers`/`maxProjects` so grouping
only engages when workers are scarce and never serializes an unbounded cohort —
the amortization wins under constrained parallelism without regressing wall time
when workers are idle. Each grouped project keeps its own cache key/entry, its
own captured output directory (coverage, JUnit) via the per-project
`selectedProjects[].outputPath`, its own coverage-threshold evaluation, and its
own pass/fail: one project's failing or crashing suite is recorded as that
project's `FAILED` result and never fails or masks a batch-mate. A suite that
crashes non-gracefully (the tool dies or hangs past its per-project timeout) is
respawned once, then isolated as that project's failure.

### Resource admission and CPU ceilings

Worker count is only an opportunity bound. Two separate quantities govern each
physical subprocess:

- The **CPU ceiling** is exported to the child and limits tool-native
  parallelism. It is deterministic for the same machine, task history, and
  plan; live peers and acquisition order never change it. How it is *derived*
  is a policy — see below.
- The **CPU/RAM reservation** is scheduler-only. The coordinator subtracts it
  from stable machine capacity before dispatch and restores it exactly once
  when the group completes. Ceilings may exceed reservations so a task can use
  otherwise-idle cores for bursts without letting every worker claim that burst
  capacity concurrently.

CPU capacity is the machine's logical CPUs clamped to its captured cgroup
bandwidth quota. At the opening environment sample, memory capacity captures
physical RAM and any finite cgroup limit with their provenance; its effective
value is the smaller available bound. That exact `effectiveBytes` value sizes
the scheduler's worker and admission pools and is persisted in
`session.json.environment.memoryCapacity` with `effectiveSource`. Admission
retains 25% as headroom and deliberately does not use instantaneous
`MemAvailable`: co-tenant activity must not change the schedule for an
otherwise-identical machine, history, and plan.

The other recorded memory facts are observe-only. Closing cgroup
current/anon/file/shmem gauges, the cgroup-lifetime peak, and session-window
event and PSI deltas do not resize the pool or trigger reclamation.
`executions[].maxRssBytes` remains the peak of one physical
subprocess tree and continues to feed the existing task-history admission
estimate; shared-cgroup gauges are never attributed back to a task. Cross-runner
qualification and the evidence threshold for a later policy change are defined
in [Profiling and Telemetry](12-profiling-and-telemetry.md#qualifying-memory-across-runner-substrates).

#### Ceiling policy: `--cpu-policy`

Two quantities can size a ceiling deterministically, and which one is right
depends on the repository's shape, so it is configurable — flag, then
`PUTNAMI_CPU_POLICY`, then `options.<command>.cpu-policy` in the workspace
config, then the default. An unrecognized value is an error, never a silent
fallback: this policy is precisely the variable a measurement campaign controls.

- **`critical-path`** (default) sizes the ceiling by the task's share of the
  plan's makespan — its expected wall time relative to the plan's longest job,
  squared, scaled over machine capacity — and takes the **maximum** of that and
  measured occupancy. Occupancy therefore only ever raises a ceiling, never
  lowers one.
- **`measured`** uses measured occupancy alone (CPU ms / wall ms, rounded up).

`critical-path` first asks whether the plan is bounded by one task at all: the
lift applies only when the longest job's wall exceeds what the rest of the plan
can absorb in parallel (`longest > (total − longest) / workers`). Below that
line the run is throughput-bound, no single ceiling can move the makespan, and
the policy degrades to `measured`. That is what lets one default serve
repositories of different shape — a repo whose makespan is one long chain gets
the lift, a repo of many comparable tasks does not, with nothing to configure.

The default is `critical-path` because measured occupancy alone is a feedback
loop with a starved fixed point. Occupancy measures what a task was *allowed* to
do, not what it could use: a task held at one core reports one core of demand
forever, and nothing in its own history can lift it back. Some tasks read far
below their real appetite for unrelated reasons — this CLI's own test suite
reads 0.73 because it is dominated by re-exec'd child processes — so on a plan
whose makespan is one long chain, occupancy hands the long pole a single core
and lengthens the whole run. The asymmetry decides it: over-granting a *ceiling*
is close to free, because the task simply does not use the cores and admission
still bounds real concurrent pressure, while under-granting the long pole costs
makespan one for one.

`measured` remains the honest choice for a plan of many comparable tasks, for a
shared machine where per-task accounting matters more than one chain's makespan,
or as the control arm of a scheduler experiment.

The criticality share is **plan-scoped and never persisted**. The same task is a
long pole under `--all` and an ordinary job in a single-project run; freezing one
plan's shape into task history would make a grant depend on which projects the
previous run selected. Only the occupancy-derived, plan-independent ceiling
travels into `.putnami/stats/tasks.json`. Both policies are equally
deterministic: the same machine, history and plan give the same grants.

The bootstrap distinguishes two kinds of cache miss:

- **First cold** means no task resource profile exists. The subprocess keeps a
  portable full-machine CPU ceiling (naturally smaller under a cgroup quota),
  while admission charges one neutral CPU per physical process and uses the
  declared resource class for memory: 512 MiB for a light task or 1280 MiB for
  a declared-heavy task, clamped to the machine. `heavy` is useful memory-risk
  evidence, not a measured CPU demand; an explicit CPU weight can raise the CPU
  reservation. This permits more work as CPU and RAM grow but still reduces to
  one process on a one-core or low-memory shape. The fallback
  reservation/ceiling is never persisted as proven CPU demand; the execution
  still records its measured RSS and actual observation ceiling so the next
  regular-cold run has contextual evidence.
- **Regular cold** means the build/task cache missed but a learned profile is
  present. The ceiling comes from CPU/wall demand and configured/learned weight;
  CPU admission uses observed occupancy rather than treating that ceiling as a
  reservation. RAM admission uses peak RSS from the nearest observed ceiling,
  retains the measured high-water mark when the new ceiling is lower, scales it
  when the ceiling is higher, and adds confidence/saturation headroom.

Admission occurs before cache lookup, so a hit can hold a reservation briefly;
it releases with the same terminal group path and never records a CPU budget.

This has a known cost, stated here because it is not visible from the code
alone. A job's cache key depends on its dependencies' outputs, so it cannot be
computed until they finish — the lookup therefore has to happen in the worker,
after admission, and a group that turns out to be entirely cached still holds a
CPU and memory reservation for the whole lookup, restore included. Worker count
and pool capacity are also sized from different numbers: `auto` workers are a
multiple of the machine's logical CPUs, while pool capacity is logical CPUs
clamped to the cgroup quota. The pool is consequently the binding constraint on
a quota-limited runner, and it binds cache hits along with real subprocesses.
Making admission follow the lookup would need reservation ownership to move to
the workers, which is exactly what the longest-first priority fix above removed;
that trade needs a controlled cache-hit-heavy measurement before it is made.

For a batch, members share the maximum CPU reservation because there is one
tool-native pool. First-cold memory similarly uses one shared physical-process
class envelope rather than multiplying it by the number of logical rows.
Learned singleton memory estimates add because shared-config tools may retain
several projects' inputs concurrently. A learned batch RSS is already one
physical-process envelope, so its observation retains batch size: the next
comparable batch apportions that envelope across its members, keeps at least one
full envelope for fixed process memory, and a later singleton still reserves
the full measured envelope.

Before dispatch, every batch-compatible planned class receives one fixed CPU
ceiling: the maximum recommendation among all members with the same
`readyBatchKey`. Each member receives that ceiling whether runtime timing runs
it alone, in a partial batch, or with the full compatible class. The shared
subprocess is stamped only after its members miss cache lookup.

The budget reaches the subprocess as `PUTNAMI_CPU_BUDGET` (all extensions) and
as `GOMAXPROCS` for `@putnami/go` jobs, which bounds `go test`/`go build -p`,
golangci-lint, and staticcheck. The TypeScript extension maps the budget to
`RAYON_NUM_THREADS` for Biome. An operator-set `GOMAXPROCS` or
`RAYON_NUM_THREADS` always wins.

A job's weight combines two inputs, both execution hints that never affect
cache keys:

- **Configured** — `cpuWeight` on the extension's pipeline step, overridden by
  the project's `putnami.json`:

  ```json
  { "tasks": { "lint": { "cpuWeight": 4 } } }
  ```

  Keys are a full step name (`build~transpile`) or a command name (`lint`,
  which covers all of that command's steps unless a step entry exists). The
  value is a relative multiplier on history-derived demand: otherwise-identical
  history gets 4× the budget at weight 4 versus weight 1, up to machine capacity.
- **Learned** — the scheduler records each task's CPU and wall time in
  `.putnami/stats/tasks.json` (machine-local, exponential moving average).
  A task whose CPU cost is far above the median of same-named tasks across
  projects gets a proportional boost, capped at 8×. CPU time (not wall time)
  drives the boost; CPU divided by wall also records parallelism the task
  demonstrably consumed. That measured demand is rounded up to a whole-core
  ceiling before the current weight is applied. This deliberately keeps the
  measured EMA within its grant without an unmeasured fractional tolerance; a
  tighter rule for cases such as 1.02 measured cores needs benchmark evidence.
  The store retains the largest successful, history-backed singleton ceiling
  **before** configured or learned weight is applied, so a throttled observation
  cannot lower the next grant and starve a known long pole, while changing or
  removing a weight affects the next comparable run. Version-1 stores retain
  their CPU/wall EMAs during migration but drop the old post-weight concurrency
  floor because its multiplier cannot be reconstructed. A task with no history
  receives the whole machine for its first execution, avoiding the cold-history
  critical-path regression; that fallback (and a batch's fixed class grant) is
  not persisted as proven singleton demand, so the next run uses the
  measurement it just learned. Version 3 also retains a bounded set of at most
  eight concurrency-conditioned observations per task: CPU, wall, peak RSS,
  granted concurrency, physical batch size, sample count, confidence, and
  saturation. Measurements at different ceilings or batch sizes do not get
  averaged into a fictitious common RSS.
  Version-2 stores migrate their usable CPU/wall and unweighted-ceiling evidence
  without inventing RSS or an observation concurrency; corrupt observations are
  discarded and fall back to the first-cold resource class. A history-backed
  singleton that receives a higher class ceiling persists only its own
  pre-weight recommendation, never the class inflation.
  Wall-time history remains separate and supplies the expected durations behind
  longest-first dispatch and cache waiting decisions.

### Scheduler Observability

The scheduler records how it tuned itself and how the DAG behaved, so the choice
of worker count and any waiting is visible without external trace analysis.

- **Parallelism decision** — mode (`auto`/`eco`/`max`/`numeric`), selected worker
  count, detected logical CPUs, quota-clamped CPU capacity, stable total memory,
  its physical/cgroup provenance in the adjacent environment block, usable
  memory after headroom, the memory-derived worker cap, and the heavy-job ratio
  that drove the choice.
- **Ready wait** — cumulative time jobs spent ready (all dependencies satisfied)
  but waiting for a free worker, grouped by command. High values point to worker
  starvation rather than dependency stalls.
- **Critical path** — the longest dependency chain weighted by measured job
  durations, with its total duration and the jobs along it.
- **CPU budgets** — every deterministic singleton or shared-batch subprocess
  grant (job/group, effective weight, expected CPU work, cores), so "why did
  this task only get one core" is
  answerable from the run itself.
- **Resource reservations** — the CPU/RAM admission claim for each dispatched
  job/group (retries reuse the same claim), sorted by public group key and
  bounded to keep session metadata finite. Internal reservation identifiers are
  never serialized.

Where it surfaces:

- **Human output** — `--verbose`/`--debug` print a compact parallelism summary;
  `--debug` (and `--trace-profile`) also print the critical path and ready wait.
- **Session metadata** — `session.json` includes a machine-readable `scheduler`
  object, and `events.jsonl` records a `scheduler:parallel` event at run start.
  Inspect with `putnami sessions show --output=jsonl`.

### Failure Handling

- **Default** — On first failure, cancel the context. In-flight jobs are drained. Remaining jobs are marked as `skipped`.
- **`--continue-on-error`** — Failures don't cancel execution. Dependent jobs of failed jobs are still skipped (dependency cascade).
- **Retry** — `--retry N` re-executes failed jobs up to N additional times before marking them as failed. Context cancellation is checked between retries.

### Hook Execution

Before the first job of each `(extension, project)` pair, the scheduler runs the extension's `preBuild` hook (if defined). Hooks run at most once per pair, tracked by a concurrent-safe map.

## Subprocess Execution

Each job executes as a child process (`internal/jobs/runner.go`).

### Process Setup

1. **Context serialization** — Build a `JobContext` struct with workspace, project, extension, job metadata, and version info. Write it to a temporary JSON file. Runtime-backed jobs receive the synchronized executable as `extension.runtimePath`; `extension.cacheRoot` stays empty until the extension-cache ownership contract assigns one. Every job also receives `selection`: how this invocation chose its scope (`mode`, `scoped`, the `--impacted` `baseline` and `baselineSource`, and the sorted `projects` ids). It is run state, never a cache-key input — a task keyed on it would miss on every `--impacted` run. The `--impacted` fallback-to-all branch reports `mode: "all"`, because the run really did cover the whole workspace. A job that runs once for the whole workspace — a `workspace-once` task, or an interactive extension subcommand — also receives `selectedProjects`, one entry per selected project, each carrying the id, the resolved and declared names, the resolved `version` (project > scope > workspace) and both path forms. `selection` says which ids are in scope; `selectedProjects` is what a task opens a file with. Order is run order: the planned path reports the engine's, the interactive path reports the selection's canonical id order because it runs one process. EVERY job additionally receives `workspaceProjects`: the complete resolved membership in canonical id order, each entry carrying the resolved direct `dependencies` ids. It is a different question from `selectedProjects` — that one is what the run acts on, this one is what the workspace contains — and a task cannot substitute one for the other: under `--impacted` a project outside the selection reads as absent and an edge into it is never walked. It reaches project-scoped jobs too, because a rule can report on one project and resolve against the whole workspace (a feature relation naming an identity a sibling declares). A task that reads it must declare `from: "workspace"` cache inputs covering what it opens, or a sibling's file changes its verdict without moving its key.
2. **Template expansion** — Resolve `{workspaceRoot}`, `{projectRoot}`, `{extensionRoot}`, `{outputRoot}`, and selection variables in the task's command, args, cwd, and env.
3. **Flag injection** — Convert merged params to CLI flags (`--name value` or `--name=value`).
4. **Process spawn** — Use `exec.CommandContext()` with timeout, working directory, and environment.

An interactive subcommand declared `workspace: "optional"` can run outside any
workspace, from an extension pinned in the user scope (see
[The User Scope](07-extensions.md#the-user-scope)). Its job differs in three
ways:

- The workspace is a synthetic one rooted at `~/.putnami/user`, with a single
  project named `user` at path `.`. `workspaceRoot`, the project paths, the
  output directory, the scratch space and the caches all live there or in the
  machine-wide caches.
- The context carries `userScope: {"callerDir": "<absolute path>"}`, the
  directory the command was invoked from, and the environment carries the same
  value as `PUTNAMI_CALLER_DIR`. The member is present only outside a
  workspace, so its presence is how a job knows that no workspace exists. A
  workspace job never receives it, even when its parent process had
  `PUTNAMI_CALLER_DIR` set.
- The process runs in `callerDir` instead of the project root. A task that
  declares its own `cwd` still runs there.

`userScope` is run state, like `selection`: nothing derived from it reaches a
cache key or a session record.

### Process Teardown

A job that is not interactive runs as the root of its own process tree: a
process group on Unix, a Job Object on Windows. A process the job starts joins
that tree unless it leaves it. The runner stops the tree:

1. **Timeout or cancellation** — The runner asks the tree to exit: SIGTERM on
   Unix, CTRL_BREAK_EVENT on Windows, or an immediate kill on Windows when the
   CLI has no console to send it through. When the root still runs 5 seconds
   later, the runner kills the tree.
2. **Root exit** — Once the root exits, for any reason, the runner asks the
   rest of the tree to exit. When a process still holds the job's stdout or
   stderr after 2 seconds, the runner kills the tree. When any process of the
   tree still runs 5 seconds after the job's output closed, the runner kills
   it.

A process that a task needs after its job ends must leave the tree: on Unix by
starting a new session or process group, on Windows by breaking away from the
Job Object. A container that a daemon runs was never in the tree.
`proctree.StartDetached` leaves the tree on Windows only. An interactive job
shares the CLI's process group, and the runner signals only its root. A
timed-out job stays failed with its error, and a canceled job stays canceled,
whatever the teardown had to do.

### Version Info

The CLI computes git metadata once per session and combines it with each project's effective version. The effective version is resolved as project, then nearest scope, then workspace. This allows jobs (e.g., publish, build-info) to embed version information without running git themselves:

```json
{
  "version": {
    "base": "0.1.0",
    "full": "0.1.0-20260902173000-abc1234",
    "sha": "abc1234",
    "branch": "main",
    "tag": "canary",
    "suffix": "20260902173000-abc1234",
    "isDirty": false
  }
}
```

| Field | Description |
|-------|-------------|
| `base` | Effective project semver from project, scope, or workspace config (e.g., `"0.1.0"`) |
| `full` | Full version with suffix (e.g., `"0.1.0-20260902173000-abc1234"`) |
| `sha` | Short commit SHA (7 chars) |
| `branch` | Current git branch name |
| `tag` | Release channel: `"canary"` on main/master, `"dev"` on other branches |
| `suffix` | Version suffix — `sha` when clean, `sha-dirtyHash` when dirty |
| `isDirty` | Whether the working tree has uncommitted changes |

The `dirtyHash` is a 7-character SHA-256 hash of the `git diff HEAD` output, providing a stable identifier for the current dirty state. This means two developers with the same uncommitted changes get the same dirty hash.

### JSONL Event Protocol

Jobs communicate with the CLI via JSONL on stdout. Each line is a JSON object:

```json
{"v": 2, "type": "log", "time": "2025-03-02T14:15:30Z", "data": {"level": "info", "message": "compiling..."}}
{"v": 2, "type": "progress", "data": {"current": 1, "total": 3, "label": "Compiling..."}}
{"v": 2, "type": "phase", "data": {"name": "compile", "status": "start"}}
{"v": 2, "type": "phase", "data": {"name": "compile", "status": "end", "result": "success"}}
{"v": 2, "type": "metric", "data": {"name": "binary-size", "value": 4096, "unit": "bytes"}}
{"v": 2, "type": "diagnostic", "data": {"severity": "warn", "message": "unused import", "file": "main.go", "line": 5}}
{"v": 2, "type": "artifact", "data": {"id": "bin", "name": "app", "kind": "binary", "path": "dist/app"}}
{"v": 2, "type": "summary", "data": {"message": "Build completed in 1.2s"}}
{"v": 2, "type": "result", "data": {"status": "success", "data": {"binaryPath": "/out/app"}}}
```

The CLI parses these events in real-time and routes them to the active renderer and session recorder.

#### Protocol version negotiation

The event vocabulary is versioned (`protocols/runtime`): version 1 was the nine
types above, version 2 adds one — `ready`, the typed readiness signal a serve job
emits when its workload is actually listening. A stream may **not** mix
versions, so its version is chosen once, before its first line. The examples
above are stamped `"v": 2` because that is the only version a CLI speaking
contract 3 parses — see below.

The CLI advertises the highest version it accepts in the subprocess
environment, for every job:

```
PUTNAMI_RUNTIME_EVENTS=2
```

An extension's SDK resolves that advertisement (higher ⇒ clamped down) and
stamps the result on **every** line it writes. Since CLI contract 3 the CLI
accepts **exactly** the version it advertises; it carries
unknown-to-a-renderer types through untouched, as before.

Consequences worth knowing:

- **v1 extension streams are rejected.** The manifest loader is what makes that
  safe: an extension only reaches the parser if its manifest declares CLI
  contract 3, and contract 3 requires an SDK that reads this variable. A v1 line
  therefore means the stream contradicts the manifest, not that the extension is
  merely old — and interpreting it would be guessing which half is true. (Before
  contract 3 the CLI accepted both, because rejecting on version would have
  dropped a whole job's output — logs and result included — for an extension
  that predated the advertisement.)
- Shell extensions that emit JSONL by hand must stamp `"v":2`. The
  `bin/putnami-jsonl.sh` helpers of the shell-extension sample
  (`tooling/samples/shell-extension`) do.
- **The hook summary wire is a different protocol.** A `preBuild`/`onInstall`
  hook reports its exports and assets on a channel of its own, pinned at
  `"v": 1` (see [07-extensions.md](07-extensions.md#prebuild-hook)) and outside
  this ladder: hook binaries must not negotiate `PUTNAMI_RUNTIME_EVENTS` for it,
  and everything above about "exactly the version it advertises" describes job
  streams only.
- Precedence is last-wins over `os.Environ()`: the CLI's advertisement is
  appended after inherited values, context variables, caller-supplied
  `extraEnv`, and manifest `tasks.<name>.env`. It is therefore authoritative:
  an outer `putnami` run, an operator, a manifest, or a caller cannot override
  the runtime event version parsed by this CLI.

### Result Extraction

- The `result` event provides the canonical job outcome.
- If no `result` event is received, the subprocess exit code determines success/failure.
- Stderr is captured as a fallback error message.
- Status values are normalized: `"succeeded"` → `"success"`, `"failure"`/`"error"` → `"failed"`, `"skip"` → `"skipped"`.

### Template Variables

| Variable | Resolves To |
|----------|-------------|
| `{workspaceRoot}` | Absolute path to workspace root |
| `{projectRoot}` | Absolute path to project directory |
| `{extensionRoot}` | Absolute path to extension directory |
| `{outputRoot}` | Job output directory (`.putnami/out/{project}/{command}`) |
| `{cacheRoot}` | Per-workspace mutable scratch dir (`.putnami/cache/`), distinct from the machine-global content-addressed store |
| `{selectedProjects}` | Comma-separated selected project names for workspace-once jobs |
| `{selectedProjectIDs}` | Comma-separated selected project IDs for workspace-once jobs |
| `{selectedProjectPaths}` | Comma-separated selected project paths for workspace-once jobs |
| `{selectedProjectRoots}` | Comma-separated selected project roots for workspace-once jobs |
| `{extensionRuntime}` | Resolved extension runtime executable (`.exe` on Windows), for a task or a provider command (`cache-provider`, `runner-provider`, `session-reporter`) whose manifest declares `runtime`; prepared and verified before the task or provider starts |
| `{runtimeOutput}` | Prepared-runtime destination; available only to `runtime.prepare` |
| `{invocationArtifactRoot}` | Non-secret private artifact root; available only to the producer, listed consumers, and finalizer of one `finalizes` relation |

The invocation root is also carried as typed job-context
`invocation.artifactRoot`; manifest arguments use the token so task binaries
receive the location explicitly. It is not inferred from cwd and is not copied
into task results, runtime events, or cache documents.

## Invocation-Scoped Resources

A pipeline that provisions something for the duration of one run — a container,
a socket, a short-lived credential — declares it as an `invocation`-scoped
output and pairs it with a `runOn: finally` finalizer (see
`protocols/extension/doc/07-lifecycles.md`). The scheduler executes that
relation as follows.

**Private scratch.** Before the producing step starts, the CLI creates
`.putnami/invocations/<invocationId>/` at mode `0700` with an `artifacts/`
subdirectory, and reserves each declared `sensitive` output at mode `0600`. The
mode is re-asserted after the step runs, so a provider that writes through a
temporary file and renames still ends up with an owner-only artifact. A
sensitive output that was reserved and never written fails the step with
`sensitive.artifact_missing`.

**Confinement.** A sensitive artifact's path and its bytes are matched against
everything the relation's tasks emit — event messages and payloads, result data,
error messages. A match is redacted on the live stream and fails the task with
`sensitive.leak_detected`, so the value never reaches a cache entry, a session
record, or telemetry. Bytes are sampled under a bounded budget — 64 KiB per
artifact, shared across a sensitive artifact that is a directory, which is also
walked to a bounded entry count and depth — so the guard never costs I/O
proportional to the artifact. When a bound stops the guard from covering an
artifact completely it warns, naming the declared artifact and its provider (not
the private path), because bytes it did not read cannot be redacted.

**Cache identity.** A consumer's cache key folds in the producing action's
digest — derived from the producer's declared inputs, never from what it
produced. Negotiation runs before provisioning: when every consumer in the
frontier is already cached, the producer and its finalizer are both skipped.

**Exactly once.** Producer start arms the finalizer. It runs once when the
consumer frontier becomes terminal, and once at the end of the run otherwise —
consumer failure and cancellation included. After cancellation it receives an
independent, bounded cleanup context, so cleanup does not inherit the
cancellation that made it necessary. A failed setup blocks its consumers with
`sensitive.setup_failed` instead of the generic dependency-failure message,
including under `--continue-on-error`. A failed finalizer is reported with
`sensitive.finalizer_failed` and does not change the run's outcome.

**Crash recovery.** Each invocation writes a non-secret lease —
invocation id, pid, provider, action digest, creation time — that survives a
`SIGKILL`. Before provisioning, the next invocation reaps every scratch whose
lease has no live owner and reports each as `sensitive.lease_reaped`; there is
no GC interval to wait for. A lease carries only those five scalars, so it is
safe to stamp onto external resources as a label and to match orphans by.

## Cache Integration

Cache is checked during job execution (see [10-caching.md](10-caching.md) for full details):

1. **Hash computation** — Build a SHA-256 key from extension, task, project, params, file contents, env vars, and upstream hashes.
2. **Lookup** — Check if the hash exists in the store.
3. **Hit** — Restore the cached result and output files. Skip subprocess execution.
4. **Miss** — Execute the subprocess normally.
5. **Store** — On success, save the result and output directory to the cache.

Cache is disabled automatically in watch mode, or explicitly with `--no-cache`.
`--no-cache-projects <selector>` disables it for the named projects and their
planned dependents only, so a selection's dependency closure keeps its cache.

## Dry Run and Plan Modes

- **`--plan`** — Show the execution plan (projects, jobs, dependencies, cache status) and exit without running.
- **`--dry-run`** — Show the plan with a "dry-run mode" header and exit.

Both modes display a table grouped by project, followed by plan metrics:

```
  my-lib
    build~generate               MISS
    build~transpile              MISS  [deps: my-lib:build~generate]

  my-app
    build~generate               MISS
    build~transpile              MISS  [deps: my-app:build~generate, my-lib:build~transpile]

  4 jobs  ·  3 edges  ·  2 projects
  by command: build 4
```

The metrics line reports the DAG node count, dependency-edge count, and distinct
project count; `by command` breaks the node count down per root command. These
numbers make plan-shape changes (e.g. pruning deterministically-skipped nodes)
measurable across runs.

With explicit machine output, both modes preserve the same semantics without
printing this table. `--output=json` emits one successful result envelope with
`plan`; `--output=jsonl` emits one terminal `plan:end` record with the same
typed plan. Neither claims `run`, `session:end`, or a session artifact. A no-op
such as an empty `--impacted` selection is still one valid document with
`metrics.tasks: 0` and `tasks: []`, so automation can enforce non-empty work
without parsing “No jobs matched”.
