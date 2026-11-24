# ADR 0019 — A delegated verb runs as one tool command

- **Status**: accepted
- **Date**: 2026-11-24
- **Scope**: `tooling/stacks` (`@putnami/stacks`), `@putnami/cli` (the
  scheduler's batching), `integrations/moon` (`@putnami/moon`),
  `integrations/nx` (`@putnami/nx`); `@putnami/turbo`, `@putnami/rush`,
  `@putnami/pants` and `@putnami/bazel` follow in their own change
- **Amends**: [ADR 0008](0008-a-delegated-stack-keeps-its-tool-cache.md),
  which ran the tool once per component

## Context

A delegated task starts its own tool process on its own project (ADR 0008,
`delegate-runs-one-process-per-task`), and the tasks of one stack root hold
the root's lock, so they also run one after the other. On a warm cache, the
tool alone runs the same work several times faster. Measured on 2026-11-11
against the canary of that day, on the second run of an unchanged tree:

| Repository | Tool alone, one call | Putnami delegate, run 2 |
| --- | --- | --- |
| moonrepo/moon, 10 tasks | `moon run`: 0.2 s | 4.8 s |
| ngrx, 6 tasks | `nx run-many`: 1.5 s | 9.4 s, tasks end 1.4 s apart |
| TanStack/query, 15 builds | `nx affected -t build`: 2.0 s | 25.8 s |
| rushstack, 4 tasks | `rush test --impacted-by`: 0.85 s | 9.3 s |
| st2, 28 tasks | `pants … lint package`: 2.3 s | 182.6 s |

The scheduler already runs a group of tasks as one process under the batch
contract, one result per project id, but only for cached tasks, and a
delegated task has its cache off.

## Decision

- A task of a delegated profile may state `batch.steps`: the steps that run
  the task once over several components. In a batch step, an argument that
  holds `{component}` is repeated once per component, in the order of the
  selection, and `{components}` is the components joined with commas. moon
  states `moon run {component}:<task>`, which runs `moon run a:lint b:lint`;
  Nx states `nx run-many -t <task> -p {components}`.
- The manifest declares such a task `batchable`, with the stack id as its
  tool, and keeps its cache off. The conformance kit holds the manifest to the
  profile: a task is batchable exactly when its profile task states
  `batch.steps`.
- The scheduler batches a batchable task whose cache is off. It groups the
  ready tasks that share a batch key, with no project cap unless the policy
  states one, and a group waits for its peers: while a task of the same
  extension, task and tool is not ready yet, and depends on no task of that
  verb, the ready ones wait, as long as other work is in flight. The graph
  steps of a delegated stack end at different times, so without the wait one
  verb would run one command per wave of graph steps. When no work is in
  flight, the ready tasks run at once.
- The shared invocation runs the toolchain check once, takes the stack root's
  lock once, removes each component's reports, runs the batch steps at the
  root, then answers one result per selected project id. The stack extension
  reads the tool's report:
  - moon: the run report moon writes in `.moon/cache/runReport.json` during
    the command. A target moon reports `passed` or `cached` passes; a target
    it reports failed fails with moon's error and the end of the output moon
    kept for that task.
  - Nx: the static output of `nx run-many`. Each target's section starts at
    `> nx run <project>:<target>`, and the closing summary lists the failed
    targets. A failed target fails with its section; a target with a section
    and not listed passes.
  - A target the report does not name, when the command fails, fails as not
    run, naming the targets that failed. A command that fails and whose
    report names no target fails every project with the step's message. A
    command that passes passes every project.
- Each project then reads its own JUnit and coverage reports into its own
  output directory, as one process per task reads them, and its result
  records `delegatedTo`.
- A verb stops as a whole. Within one command, the tool decides whether it
  runs the targets left after a failure. Putnami's fail-fast acts between
  commands: a failed project fails the verb's command, and the run stops the
  other commands it would have started.
- A task that runs alone, such as a retry or the only task of its verb, runs
  its own steps, as before.

## Rejected alternatives

- **Batch only the peers that are ready together, as cached tasks do.** Each
  delegated task waits for its own project's graph step, which waits for the
  graph steps upstream; the tasks become ready in waves, and one verb would
  start one tool command per wave.
- **A fixed coalescing window.** It idles the run by a fixed delay even when
  every peer is ready, and still splits a verb whose graph steps take longer.
- **`nx affected` or `moon run --affected`.** The selection stays Putnami's:
  impact, `--projects` and the import list choose the projects.
- **One command for every verb of a run, such as `moon run a:lint a:test`.**
  The order between verbs, and the fail-fast between them, are Putnami's.
- **Read each project's result from its exit code.** One command has one exit
  code; only the tool's report says which project failed.

## Consequences

- A warm delegated `putnami lint` on moon or Nx starts one tool process per
  verb, plus one per later wave of a verb whose projects depend on each other
  through a `^command` entry ([ADR 0097 of tooling/cli](../../../cli/doc/adr/0097-a-project-names-what-a-command-waits-for.md)).
- Within one command, a failing project stops the other targets at the
  tool's own rule, and a target the tool does not run fails as not run.
- A run report or an output the tool changes its format of fails every
  project of a failing command with the step's message, never passes one.
- turbo, rush, pants and bazel keep one process per task until their profiles
  state `batch.steps`.
