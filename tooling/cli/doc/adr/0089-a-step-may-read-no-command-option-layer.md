# ADR 0089 — A step may read no command option layer

- **Status**: accepted.
- **Scope**: `protocols/extension` (`PipelineStep.commandLayerInputs`),
  `protocols/cli` (`CommandLayerInputsContract`, contract 23),
  `@putnami/cli-model` (`JobDefinition.NoCommandLayerInputs`),
  `@putnami/cli` (`internal/jobs/cachekey` option layers,
  `internal/workspace` task index, `TasksReachedIn`), `@putnami/cli-model`
  (`TaskImpactIndex`, the impact walk), the TypeScript and Go extensions
- **Date**: 2026-11-13

## Context

A project declares extra inputs in its option layers: `filePatterns` and
`envInputs` under `options.<command>`, `options.<extension>` and
`options.<extension>:<command>`. The cache key folds a layer into every step
of the job's command, and the impact index attributes a changed file the
layer selects to the same steps ([ADR 0044](0044-selection-is-task-level.md)).

Most commands run steps that are not theirs. `test` in the TypeScript and Go
extensions first generates and compiles the project, with the same tasks as
`build`, and a dependent's `test~generate` waits on the dependency's through
`^generate`. A file only one project's tests read, such as a fixture under
`options.test.filePatterns`, therefore seeds that project's `test~generate`,
and the `^` edge carries it to every project above. One newline in a
`putnami.features.json` that `/protocols/features` tests read selected 141
projects and 1065 tasks.

Two more causes carried the same file to the same 141 projects, both in
selection alone:

- The index attributed a command layer to every task scope of that command,
  whichever extension declares it. `/protocols/features` runs Go and the SDD
  extension, yet its `test` layer seeded the monorepo integrations'
  `test~graph`, which dependents reach through `^graph`. A task's own ports
  are read only where its command may be planned (ADR 0044); its layers were
  not.
- Task scopes merge across extensions, and the `^` relation was read without
  them. Dart, .NET, Java, Rust and Swift declare `test` steps that depend on
  `^test`, so a seeded `test~test` reached every dependent's `test~test`,
  Go projects included.

The rejected alternatives:

- **Drop the command layer from every step a dependent names through `^`.**
  No manifest change, but wrong: a Dart `test` step depends on `^test` and
  would lose its own layer, and a `build` step that dependents read through
  `^build` does consume `options.build.filePatterns`.
- **Narrow the impact index and keep the cache key.** ADR 0044 requires
  selection and the key to read the same declarations. A task the index drops
  for a file its key still reads would be skipped while its key moves.

## Decision

**1. A pipeline step may declare `commandLayerInputs: false`.** Such a step
reads no `filePatterns` and no `envInputs` from the project's command layers:
`options.<command>`, `options.<extension>:<command>` and
`options.<extension path>:<command>`. The extension layer still reaches it.
Absent or `true` keeps every layer, so a manifest that does not name the field
keys and selects exactly as before. The field governs input declarations
only: the flag values a command layer sets still reach the step's parameters.

**2. The cache key and the impact index read the same rule.** The key's
option layers leave the command layers out for such a step. The index
attributes a command layer to a task scope only when one of the steps that
declare the scope reads it. A scope no step reads the layer for is reached
only through the intra-project closure and the `^` edges, like any other
task.

**3. The field raises the CLI contract to 23.** A reader below 23 does not
know the field and drops it, so it would key and select the step with every
layer. A manifest that declares it requires `CommandLayerInputsContract`, so
an older reader refuses the stamp with the ladder's message instead of
reading the step differently.

**4. The TypeScript and Go `test` commands mark the steps that prepare the
project.** TypeScript `test~generate`, `test~tsc` and `test~tsc-auto`, and Go
`test~config-merge`, `test~generate` and `test~describe` declare
`commandLayerInputs: false`. The steps that run and finalize the tests keep
the layer. Both extensions stamp contract 23.

**5. An option layer claims only the tasks the project may run.** A layer's
task scopes are kept where a command that declares the scope may be planned
in the project, the rule a task's own ports follow.

**6. A `^` reference reaches a dependent only when its command may run
there.** The index records which commands declare each `^` step reference and
each `^command` dependency, and the impact walk asks it per dependent
(`TaskImpactIndex.TasksReachedIn`) on dependency and contract edges. The
prerequisite of a `^command` dependency runs across the whole dependency
closure, so it travels on through every dependent, whatever that dependent
plans. A dependent the edge carries nothing into is not selected through it;
one a `^command` prerequisite travels through is, for that prerequisite.

With the six decisions, the newline in `integrations/nx/putnami.features.json`
selects 5 projects and 238 tasks (`impact-plan lint,test,build,validate`)
instead of 141 and 1065.

## Consequences

- Those six steps take a new cache key once, in every project that declares a
  `test` layer; afterwards an edit of a `test`-layer file no longer moves
  them, and a dependent's tests no longer run for it.
- A command layer declared for a generation step must move to the extension
  layer, which still reaches it.
- Other commands that run `build` steps (`serve`, `run`, `package`,
  `config-extract`) keep every layer: no project here declares one of their
  layers. They may adopt the field when one does.
