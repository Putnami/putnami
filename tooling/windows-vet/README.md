# @putnami/windows-vet

The Windows compile gate (D-W11). `TestEveryGoWorkModuleVetsForWindows` runs
`GOOS=windows GOARCH=amd64 go vet work` from the repository root. The `work`
pattern matches every package of every `go.work` module, test files included, so
a Unix-only symbol anywhere in the workspace fails the gate without a Windows
host. The test also fails when a `go.work` module has no package that builds
for Windows, since vet would then check nothing in it.

## Where the gate runs

The gate reads every Go source of every `go.work` module. It runs only when
`PUTNAMI_WHOLE_TREE_GATES` is set, and skips otherwise:

- the [whole-tree gates workflow](../../.github/workflows/whole-tree-gates.yml)
  sets the variable on every push to `main`, and on a pull request that changes
  this project, `tooling/cli-documents`, `go.work` or the workflow itself;
- the per-change gate, `putnami lint,test,build,validate --impacted`, does not
  set it, and no change outside this project and the workflow plans it.

Run it on any tree with:

```bash
./putnamiw client-generate --all
./putnamiw client-generate --projects /go/samples/service-to-service
PUTNAMI_WHOLE_TREE_GATES=1 ./putnamiw test --projects @putnami/windows-vet --no-cache
```

Git holds no generated client, and `go vet work` type-checks the modules that
import one, so the first two commands generate them: `--all` skips a project
that carries a tag the workspace disables, and the second command names those.
`TestTheWorkflowGeneratesEveryGoClient` fails when one is missing. `--no-cache`
is part of the last command: the project declares no input for the sources the gate reads, so a
stored verdict says nothing about another tree. The variable is declared under
`options.test.envInputs`: a run with it never restores the skip a run without
it stored. `TestTheGateRunsOnMainWithoutACache` keeps the declarations and the
workflow's trigger, variable, client generation and cache switch together.

A pull request therefore does not prove that its Go change compiles for
Windows; `main` does, after the merge. A red run there names the Unix-only
symbol: move it behind a `_unix.go` file or a `//go:build unix` constraint, next
to a Windows counterpart.

## Why this is a separate project, with no input and no dependency

A project declares cache-key inputs per **command**, never per task. The gate
used to live in `@putnami/cli-documents`, next to tests that key on documents.

A declared input also selects its project under `--impacted`, and so does a
dependency. A key over what `go vet` reads (the tracked `.go` and `.s` files,
every `go.mod` and `go.sum`, `go.work`, and the committed contracts the
generated clients come from) selected the project for every Go change. A
dependency on the 17 modules that generate a client planned the gate, and the
generation of all 17, for every change that reached one of them. The project
declares neither: the workflow generates the clients, and the gate runs on
`main`.

The module requires no workspace module: it reads the other modules through the
go command, not through imports.

## A Windows check on pull requests

None runs today. Two mechanisms exist, and
[ADR 0001 of `@putnami/cli-documents`](../cli-documents/doc/adr/0001-whole-tree-gates-run-on-main.md)
records why neither is used here:

- the `platforms` option of the Go extension (`options.build.platforms`)
  compiles a project once more for `windows/amd64`. It runs per changed
  project, but it changes the build of every Go project of the repository and
  checks less than the vet: no test file, no vet analyzer;
- a vet step of the Go extension, one task per project, would check what this
  gate checks for the changed modules alone. It is a change to the extension.
