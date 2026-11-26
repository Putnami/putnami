# ADR 0001 — Whole-tree gates run on main, without a cache

- **Status**: accepted
- **Date**: 2026-11-26
- **Scope**: `@putnami/cli-documents`, `@putnami/windows-vet`

## Context

Three gates read more than any change touches. The public-cut scan and the
release rehearsal's recorded verdict read every candidate file of the
repository. The Windows compile gate (D-W11) reads every Go source of every
`go.work` module.

Each keyed its tests on what it reads: `git:**` for
`@putnami/cli-documents`, every tracked `.go`, `go.mod`, `go.sum` and
`putnami.json` for `@putnami/windows-vet`. A declared input also selects its
project under `--impacted`, and a selected test task plans the prerequisite
steps of the projects it depends on. So a one-line change in a Dart sample
planned 35 projects and 191 tasks, and a one-line change in a Go sample planned
87 projects and 503 tasks, of which two were these gates.

[ADR 0001 of `@putnami/cli-document-set`](../../../cli-document-set/doc/adr/0001-known-document-set-gates-key-on-their-documents.md)
moved the gates that read a known document set out, and left these.

## Decision

The whole-tree tests of both projects run only when `PUTNAMI_WHOLE_TREE_GATES`
is set, and skip otherwise. Both projects declare the variable under
`options.test.envInputs`, and declare no file input for the tree the gates
read. `@putnami/cli-documents` names the three documents its other tests read
outside the project; both name the workflow below, which their tests read.

[`.github/workflows/whole-tree-gates.yml`](../../../../.github/workflows/whole-tree-gates.yml)
sets the variable and runs both projects with `--no-cache` on every push to
`main`, and on a pull request that changes one of the two projects or the
workflow. A test in each project fails when the manifest loses the variable or
gains a workspace-wide pattern, or when the workflow loses its trigger, its
variable or its cache switch.

Rejected:

- Keeping the whole-tree input and excluding the projects on pull requests.
  The per-change gate is one command for every trigger (`putnami.ci.json`), so
  an exclusion there also leaves `main` without the gates, and a plain
  `--impacted` run still plans them.
- Running the Windows gate on a pull request for the changed modules alone.
  The project is one task over the whole workspace, and its dependencies, the
  client generation of every module that generates one, are planned whenever
  it is selected. A task per changed module is a step of the Go extension,
  which is an engine change.

## Consequences

- A change outside the two projects, the three documents and the workflow
  plans neither project nor its prerequisites.
- A pull request no longer proves that the public cut holds no new debt, that
  the recorded rehearsal verdict matches the tree, or that a Go change compiles
  for Windows. `main` proves each after the merge, and a red run there is fixed
  on `main`.
- No declared input keys the tree these gates read, so a run of them is honest
  only without a cache. The documented command and the workflow both pass
  `--no-cache`.
- The hosted per-change gate does not run them on `main` either: only the
  workflow does.
