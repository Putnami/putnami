# @putnami/windows-vet

The Windows compile gate (D-W11). `TestEveryGoWorkModuleVetsForWindows` runs
`GOOS=windows GOARCH=amd64 go vet work` from the repository root. The `work`
pattern matches every package of every `go.work` module, test files included, so
a Unix-only symbol anywhere in the workspace fails the contributor gate without a
Windows host. The test also fails when a `go.work` module has no package that
builds for Windows, since vet would then check nothing in it.

## Where the gate runs

The gate reads every Go source of every `go.work` module. It runs only when
`PUTNAMI_WHOLE_TREE_GATES` is set, and skips otherwise:

- the [whole-tree gates workflow](../../.github/workflows/whole-tree-gates.yml)
  sets the variable on every push to `main`, and on a pull request that changes
  this project, `tooling/cli-documents` or the workflow itself;
- the per-change gate, `putnami lint,test,build,validate --impacted`, does not
  set it, so a Go change plans neither this project nor the client generation
  it depends on.

Run it on any tree with:

```bash
PUTNAMI_WHOLE_TREE_GATES=1 ./putnamiw test --projects @putnami/windows-vet --no-cache
```

`--no-cache` is part of the command: the project declares no input for the
sources the gate reads, so a stored verdict says nothing about another tree.
The variable is declared under `options.test.envInputs`: a run with it never
restores the skip a run without it stored.
`TestTheGateRunsOnMainWithoutACache` keeps the declarations and the workflow's
trigger, variable and cache switch together.

A pull request therefore does not prove that its Go change compiles for
Windows; `main` does, after the merge. A red run there names the Unix-only
symbol: move it behind a `_unix.go` file or a `//go:build unix` constraint, next
to a Windows counterpart.

Running the gate on a pull request for the changed modules alone needs a vet
step of the Go extension, one task per project: this project is one task over
the whole workspace, and its dependencies, the client generation of every
module that generates one, are planned whenever it is selected.

## Why this is a separate project

A project declares cache-key inputs per **command**, never per task. The gate
used to live in `@putnami/cli-documents`, next to tests that key on documents.
A key over what `go vet` reads (the tracked `.go` and `.s` files, every `go.mod`
and `go.sum`, `go.work`, and the committed contracts the generated clients come
from) selects the project for every Go change, with the client generation of
about 80 modules before it. The project declares none of them and runs on
`main` instead.

The module requires no workspace module: it reads the other modules through the
go command, not through imports. Git holds no generated client, so the project
depends on every `go.work` module that generates one, and the planner runs each
`client-generate` before the gate. `TestTheGateDependsOnEveryGoModuleThatGeneratesAClient`
keeps that list equal to the modules whose `putnami.json` declares a Go
`generatedFrom` or the clientgen extension.
