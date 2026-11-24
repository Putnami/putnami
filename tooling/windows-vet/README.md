# @putnami/windows-vet

The Windows compile gate (D-W11). `TestEveryGoWorkModuleVetsForWindows` runs
`GOOS=windows GOARCH=amd64 go vet work` from the repository root. The `work`
pattern matches every package of every `go.work` module, test files included, so
a Unix-only symbol anywhere in the workspace fails the gate on `main` without a
Windows host. The test also fails when a `go.work` module has no package that
builds for Windows, since vet would then check nothing in it.

## Runs on main

The project carries the `whole-repository` tag, which `disable.tags` in
`putnami.workspace.json` names, so `--impacted` and `--all` never select it: in
the per-change gate, every Go change ran it and the prerequisite steps of every
module it reads. The
[whole-repository gates](../../.github/workflows/whole-repository-gates.yml)
workflow runs it by name on every push to `main`, and on a pull request that
changes this project or a platform-specific Go file (`*_windows.go`,
`*_unix.go`, `*_linux.go`, `*_darwin.go`, `*_bsd.go`). A Unix-only symbol can
also sit in an unsuffixed file; before merging such a change, run it yourself:

```bash
./putnamiw test --projects /tooling/windows-vet
```

No repository-local mechanism vets only the changed modules: that would be a
step of the Go extension's own jobs, so the path filter is the per-change
approximation
([ADR 0001 of `@putnami/cli-documents`](../cli-documents/doc/adr/0001-whole-repository-gates-run-on-main.md)).

## Why this is a separate project

A project declares cache-key inputs per **command**, never per task. The gate
used to live in `@putnami/cli-documents`, whose test key is the whole tracked
tree, so a change to a document re-ran a `go vet` of every module.

Here the key holds what `go vet` reads: the tracked `.go` and `.s` files, every
`go.mod` and `go.sum`, `go.work`, and the committed contracts and `putnami.json`
files the generated clients come from (`options.test.filePatterns`). A change that
touches none of them reuses the verdict. A file that a `//go:embed` directive
names is not an input: the build of the project that embeds it fails on the same
change.

The module requires no workspace module: it reads the other modules through the
go command, not through imports. Git holds no generated client, so the project
depends on every `go.work` module that generates one, and the planner runs each
`client-generate` before the gate. `TestTheGateDependsOnEveryGoModuleThatGeneratesAClient`
keeps that list equal to the modules whose `putnami.json` declares a Go
`generatedFrom` or the clientgen extension.
