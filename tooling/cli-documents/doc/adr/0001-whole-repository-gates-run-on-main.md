# ADR 0001 — Whole-repository gates run on main

- **Status**: accepted
- **Date**: 2026-11-24
- **Amends**: decision D-W11 (`tooling/cli/decisions/D-W11.json`), where the
  Windows vet runs
- **Scope**: `@putnami/cli-documents`, `@putnami/cli-document-set`,
  `@putnami/windows-vet`, `disable.tags` in `putnami.workspace.json`,
  `.github/workflows/whole-repository-gates.yml`
- **Decision record**: [`whole-repository-gates-run-on-main`](../../../../decisions/whole-repository-gates-run-on-main.json)

## Context

A cache key is per project and per command, never per task. Two projects of
this repository key their tests on every file of a kind in the tree:

- `@putnami/cli-documents` declared `options.test.filePatterns: ["git:**"]`,
  because the public-cut scan and the release rehearsal read every candidate
  file. Any change selected it under `--impacted`, with the prerequisite steps
  of the ~33 Go modules it compiles against: a one-line change to a Dart sample
  planned 34 projects and 191 tasks.
- `@putnami/windows-vet` runs `GOOS=windows go vet work` over every `go.work`
  module and keys on every tracked `.go`, `.s`, `go.mod`, `go.sum`, OpenAPI
  contract and `putnami.json`. Any Go change selected it, with the prerequisite
  steps of ~80 modules: a one-line change to a Go sample planned 86 projects
  and 503 tasks, of which two were real test runs.

The other gates of `@putnami/cli-documents` read a set of files they can name:
the governance surface, the neutral-contributor recipe, the first-use block,
the documentation page the CLI cites, the project manifests, the shipped
manifests and `putnami.ci.json`.

## Decision

1. **The gates that read a known set move out.** They form
   `@putnami/cli-document-set` (`tooling/cli-document-set`), which keys its
   tests on the files they read, so a pull request runs them exactly when one
   of those files changes. Its Markdown key names the documents it reads and
   the documentation folders the site publishes, not every Markdown file:
   `TestPublishedDocumentationIsADeclaredInput` fails when the site publishes a
   folder the key does not name. The project is a sibling, not a child, of
   `tooling/cli-documents`: a nested project is in that project's scope, and
   the scope boundary forbids its import of `/tooling/cli`.
2. **The whole-tree gates leave the per-change gate.** `@putnami/cli-documents`
   keeps the public cut, the release rehearsal and the `cli/release-rehearsal`
   feature, and keeps `git:**`. It and `@putnami/windows-vet` carry the
   `whole-repository` tag, which `disable.tags` names, so `--impacted` and
   `--all` never select them.
3. **Main runs them by name.** `.github/workflows/whole-repository-gates.yml`
   runs `lint,test,build,validate --projects /tooling/cli-documents,/tooling/windows-vet`
   on every push to `main`. A named project is a direct target, which
   `disable.tags` does not filter. Anyone can run the same command locally.
4. **A pull request runs them when its paths say so.** The same workflow runs
   on a pull request that changes the two projects, the release plan, the
   recorded report, a public-cut `.tsv`, `go.work`, or a platform-specific Go
   file (`*_windows.go`, `*_unix.go`, `*_linux.go`, `*_darwin.go`,
   `*_bsd.go`). That filter lives in the workflow, not in the planner: the
   gate the planner builds for a change never holds these projects.
5. **The Windows compile gate stays whole.** No repository-local mechanism vets
   only the changed modules for Windows: a per-module vet would have to be a
   step of the Go extension's own jobs, which is out of this repository
   configuration's reach. The path filter of decision 4 is the per-change
   approximation; D-W11 records it.

## Consequences

- A change outside the paths of decision 4 that breaks the Windows build, the
  public-cut scan or the recorded release verdict fails on `main`, not on its
  pull request: a Unix-only symbol in an unsuffixed file, a new private name in
  a document, a renamed internal symbol of `tooling/cli` that these gates
  import. Each push to `main` keeps its own run, so the red run names the
  commit. Run the command of decision 3 before merging such a change.
- A recorded rehearsal verdict that drifts is caught at the next `main` run,
  in the same way: re-record it in the change that moves it.
- `putnami.ci.json` declares no per-branch command set, so Putnami CI's native
  runner does not run these gates either; the GitHub workflow is the `main`
  gate for them.
