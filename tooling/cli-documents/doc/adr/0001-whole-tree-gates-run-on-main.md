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
`@putnami/cli-documents`
([ADR 0009 of the CLI](../../../cli/doc/adr/0009-release-rehearsal-recorded-verdict.md),
decision 5), every tracked `.go`, `go.mod`, `go.sum` and `putnami.json` for
`@putnami/windows-vet`. A declared input also selects its project under
`--impacted`, and a selected test task plans the prerequisite steps of the
projects it depends on. So a one-line change in a Dart sample planned 35
projects and 191 tasks, and a one-line change in a Go sample planned 87
projects and 503 tasks, of which two were these gates.

`@putnami/windows-vet` also depended on the 17 modules that generate a Go
client, since Git holds none and the vet type-checks their importers. A
dependency selects its dependent too: a change that reached one of the 17
planned the gate and the generation of all of them.

[ADR 0001 of `@putnami/cli-document-set`](../../../cli-document-set/doc/adr/0001-known-document-set-gates-key-on-their-documents.md)
moved the gates that read a known document set out, and left these.

## Decision

The whole-tree tests of both projects run only when `PUTNAMI_WHOLE_TREE_GATES`
is set, and skip otherwise. Both projects declare the variable under
`options.test.envInputs`, and declare no file input for the tree the gates
read. `@putnami/cli-documents` names the three documents its other tests read
outside the project; both name the workflow below, which their tests read.
`@putnami/windows-vet` declares no dependency.

[`.github/workflows/whole-tree-gates.yml`](../../../../.github/workflows/whole-tree-gates.yml)
sets the variable, generates the clients with `putnami client-generate --all`,
and runs both projects with `--no-cache`. It runs on every push to `main`, and
on a pull request that changes one of the two projects, the workflow, `go.work`,
or a file that records or repairs a verdict: the rehearsal report, the release
plan, the public-cut baseline and allowlist. A test in each project fails when
the manifest loses the variable or gains a workspace-wide pattern or a
dependency, or when the workflow loses its trigger, its variable, its client
generation or its cache switch.

This amends decision 5 of ADR 0009 of the CLI. Its rule stands, that a drift
check is never answered from a cache; the means changes from a whole-tree key
to a run without a cache.

Rejected:

- Keeping the whole-tree input and excluding the projects on pull requests.
  The per-change gate is one command for every trigger (`putnami.ci.json`), so
  an exclusion there also leaves `main` without the gates, and a plain
  `--impacted` run still plans them.
- A Windows compile on pull requests through the `platforms` option of the Go
  extension (`options.build.platforms: ["host", "windows/amd64"]`). It needs
  no engine change and runs per changed project: a library is compiled once
  more for Windows, an application is cross-compiled. It was read, not tried.
  It is left out because it changes the build, and the build key, of every Go
  project of the repository, which is wider than this change; because it
  compiles every changed Go project twice on every pull request; and because
  it checks less than the vet: no test file and no vet analyzer, so `main`
  would keep the vet anyway. Adopting it is a separate decision.
- A vet per changed module on pull requests. The gate is one task over the
  whole workspace; a task per project is a step of the Go extension.

## Consequences

- A change outside the two projects, the three documents and the workflow
  plans neither project nor its prerequisites.
- A pull request no longer proves that the public cut holds no new debt, that
  the recorded rehearsal verdict matches the tree, or that a Go change compiles
  for Windows. `main` proves each after the merge. The pull request that
  repairs a red run changes a path the workflow names, so it runs the gates
  before it merges.
- No declared input keys the tree these gates read, so a run of them is honest
  only without a cache. The documented commands and the workflow pass
  `--no-cache`.
- The hosted per-change gate does not run them on `main` either: only the
  workflow does.
- A new module that generates a Go client needs no declaration here:
  `client-generate --all` covers it.
