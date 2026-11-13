# ADR 0088 — A cycle among delegated projects loads

- **Status**: accepted.
- **Scope**: `@putnami/cli-model` (`workspace.DependencyGraph` components,
  `Workspace.CycleError`), `@putnami/cli` (`internal/workspace` load,
  `internal/jobs/planner` dependency resolution)
- **Date**: 2026-11-13

## Context

The Pants and Bazel syncs make one project per BUILD directory. A package and
its subpackages import each other, so the directory graph closes cycles the
tool's target graph does not: st2 holds 83, buildbuddy 73. The load refused
every cycle, so no command ran on these repositories, `projects list`
included.

Merging each cycle into one project was rejected: it coarsens projects, and a
change to one directory would select the whole merged project.

## Decision

**1. A project is delegated when every extension it names delegates.** An
extension delegates when its manifest declares `workspace.buildGraphEdges`:
every task it runs is a build tool's, and that tool states and orders the
edges between its own targets. A project that names no extension is not
delegated.

**2. A cycle loads when every project of its strongly connected component is
delegated.** A strongly connected component is a group of projects that all
reach each other over dependency edges. Any other cycle fails the load, and the
message names the cycle and each project that keeps it from loading. A recorded
provider view follows the same rule, so `projects sync` records a delegated
cycle. The load reads the extension manifests only when the graph holds a
cycle.

**3. Selection follows every edge.** Impact and dependency closures already
walk a cycle without looping, so a change to one member selects the whole
component and what depends on it.

**4. The scheduler orders components.** A `^step` or `^command` dependency
binds the projects outside the job's component that any member depends on,
never a member. The build tool orders the members' targets inside a component.

**5. The plan refuses a task no build tool orders in a cycle.** A task can
reach a project from an extension the project does not name, through
activation files. The planner fails when it plans such a task in a project of
a cycle.

## Consequences

- A delegated workspace loads, validates and selects what its tool selects,
  with no hand edit.
- A workspace with no cycle reads no extension manifest at load.
- A cycle in a workspace Putnami orders itself still fails as before.
