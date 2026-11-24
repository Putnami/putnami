# @putnami/cli-documents

The gates that read this repository's whole candidate tree: the public-cut scan,
and the release rehearsal with its release plan and recorded verdict. The gates
that read a known set of documents, such as the governance surface and the
neutral-contributor recipe, live in
[`@putnami/cli-document-set`](../cli-document-set/README.md); the Windows
compile gate lives in [`@putnami/windows-vet`](../windows-vet/README.md).

## Runs on main

The project carries the `whole-repository` tag, which `disable.tags` in
`putnami.workspace.json` names, so `--impacted` and `--all` never select it: its
`git:**` key would put it, and the prerequisite steps of every module it
compiles against, in the gate of every change. The
[whole-repository gates](../../.github/workflows/whole-repository-gates.yml)
workflow runs it by name on every push to `main`, and so can you:

```bash
./putnamiw lint,test,build,validate --enforce-coverage --projects /tooling/cli-documents,/tooling/windows-vet
```

[ADR 0001](doc/adr/0001-whole-repository-gates-run-on-main.md) records the
decision.

## Why this is a separate project

A project declares cache-key inputs per **command**, never per task
(`options.test.filePatterns` folds into the key of every `test~*` task). While
these gates lived in `@putnami/cli`, `CONTRIBUTING.md` and the rest of the
document surface were declared inputs of the CLI's 5 000-test task, so a
documentation-only pull request re-ran the whole suite. Splitting the readers out
moves the declaration with them: the CLI's `test~test` now keys on Go sources and
its own testdata, and a document edit runs this suite alone.

The public-cut and rehearsal gates read the whole candidate repository, so this
project declares `options.test.filePatterns: ["git:**"]`. The key covers tracked
files and non-ignored additions using the scanner's own Git enumeration, raw
bytes and symlink targets. A change in another project's document invalidates
its verdict. Ignored local state in
`.context`, worktrees and caches is outside this declaration. The previous fixed
document list could miss new inputs; a filesystem-wide glob would read ignored
state the scanner never reads.

## How it reaches the CLI

The module path is `go.putnami.dev/tooling/cli/documents`, a **child** of the
CLI's module path, in a **sibling** directory. Go scopes `internal` visibility by
import path, so this module may import `go.putnami.dev/tooling/cli/internal/...`
while `go.putnami.dev/cli/model` or any other sibling path may not; the sibling
directory is what keeps `go test ./...` inside `tooling/cli` from compiling these
gates again. Nothing publishes this module.

## Coverage

The package carries no production code — it is a test surface over the
repository — so coverage is off here. `@putnami/cli` keeps its 80 % threshold.
