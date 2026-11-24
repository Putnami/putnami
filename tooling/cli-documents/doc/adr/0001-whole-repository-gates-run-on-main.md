# ADR 0001 — Whole-repository gates run on main

- **Status**: accepted
- **Date**: 2026-11-24
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
   of those files changes.
2. **The whole-tree gates leave the per-change gate.** `@putnami/cli-documents`
   keeps the public cut, the release rehearsal and the `cli/release-rehearsal`
   feature, and keeps `git:**`. It and `@putnami/windows-vet` carry the
   `whole-repository` tag, which `disable.tags` names, so `--impacted` and
   `--all` never select them.
3. **Main runs them by name.** `.github/workflows/whole-repository-gates.yml`
   runs `lint,test,build,validate --projects /tooling/cli-documents,/tooling/windows-vet`
   on every push to `main`. A named project is a direct target, which
   `disable.tags` does not filter. Anyone can run the same command locally.
4. **The Windows compile gate stays whole.** No repository-local mechanism vets
   only the changed modules for Windows: a per-module vet would have to be a
   step of the Go extension's own jobs, which is out of this repository
   configuration's reach. Until one exists, a pull request does not run D-W11.

## Consequences

- A change that breaks the Windows build, the public-cut scan or the recorded
  release verdict fails on `main`, not on its pull request. Run the command of
  decision 3 before merging a change that touches platform-specific Go code or
  a release input.
- A recorded rehearsal verdict that drifts is caught at the next `main` run,
  in the same way: re-record it in the change that moves it.
- `putnami.ci.json` declares no per-branch command set, so Putnami CI's native
  runner does not run these gates either; the GitHub workflow is the `main`
  gate for them.
