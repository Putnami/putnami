# ADR 0020 — A stack tool takes the CPU budget unless the project sets its width

- **Status**: accepted
- **Date**: 2026-11-25
- **Scope**: `tooling/stacks` (`@putnami/stacks`), the stack extensions and
  the delegated integrations that run on it, and the SDK's `cpubudget`
  package

## Context

The scheduler grants each job a CPU ceiling and exports it as
`PUTNAMI_CPU_BUDGET` ([ADR 0100 of tooling/cli](../../../cli/doc/adr/0100-admission-charges-the-cpu-it-grants.md)).
The tools a stack's steps start ignored it: cargo, Gradle, dotnet, dart test
and the delegated tools each started as many threads or processes as the host
has cores. ADR 0100 measured the no-cache gate of this repository on 4 cores at
a one-minute load of 13 to 15 on average, and names tools that exceed their
grant as the cause that remains once admission charges what it grants.

Each tool has its own control: a variable, such as `CARGO_BUILD_JOBS`, or a
flag, such as Gradle's `--max-workers`. Most of these controls rank above the
project's own configuration, so a control set without care replaces a width
the project chose, such as `concurrency: 1` in a `dart_test.yaml` that keeps a
suite serial for correctness.

## Decision

A profile declares how its tools take the budget, and the core binds it to a
task's steps:

- `cpuEnv` names the variables that reach every step set to the budget, unless
  the environment already sets them: the operator's value wins.
- `{cpus}` in a step's argument, with its flag (`--jobs={cpus}`), expands to
  the budget, or to `GOMAXPROCS` in a run without one.
- `cpuConfig` names the files, with a pattern, in which a component sets its
  tools' width itself. When one holds its setting, the budget binds nothing
  for that task: no variable is set, and each argument that names `{cpus}` is
  left out. A batch binds nothing when one of its members configures its
  width.

The budget reaches the steps of a task, of a root job and of a workspace job,
which run the stack's tools, such as `bundle install`, a package's compile or
`dotnet publish`. It never reaches a selected toolchain (`Stack.Selected`),
whose environment starts the application a serve or run job built, nor a
locked step (`Stack.Locked`), which may hand its environment to the
application it starts, as Java's run does: a served application must not run
with a tool's thread cap. Neither the variables nor `{cpus}` reach the cache key.

A control that would turn parallelism on, rather than bound it, is not bound:
Maven's `-T`, Rails' `PARALLEL_WORKERS`. A control that also bounds remote
work, Bazel's `--jobs`, is not bound either; `--local_cpu_resources` is.

## Rejected alternatives

- **Bind the budget to every environment the toolchain selection builds.**
  The first form of this change did so; a served .NET application then ran
  with `DOTNET_PROCESSOR_COUNT` set to a tool's grant for its whole life.
- **Read each tool's configuration and pass the smaller width.** Every tool
  has its own files, includes and per-profile sections; a pattern that only
  detects the setting keeps the project's choice whole with a few lines of
  profile.
- **Pass `-p` and `-parallel` to `go test`.** The scheduler already exports
  `GOMAXPROCS`, which sets both defaults; the flags would add the budget to
  Go's own test-result cache key and replace an operator's `GOMAXPROCS`.

## Consequences

- Each stack's test shows what its tools receive from a budget of 2, and the
  stacks design note lists the tools that take no budget.
- A project that sets a width in a file the profile does not name, such as
  the user's `~/.gradle/gradle.properties`, still sees the budget replace it.
- One matching `cpuConfig` file turns off every control of the profile for
  that task, not only the one it sets.
