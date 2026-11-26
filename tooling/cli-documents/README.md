# @putnami/cli-documents

The gates that read this repository's whole candidate tree: the public-cut scan,
and the release rehearsal with its release plan and recorded verdict. The gates
that read a known set of documents, such as the governance surface and the
neutral-contributor recipe, live in
[`@putnami/cli-document-set`](../cli-document-set/README.md), which keys on those
documents alone; the Windows compile gate lives in
[`@putnami/windows-vet`](../windows-vet/README.md).

## Where the whole-tree gates run

`TestPublicCutReleaseGate` and `TestReleaseRehearsalReportRecordsTheCurrentVerdict`
read every candidate file of the repository. They run only when
`PUTNAMI_WHOLE_TREE_GATES` is set, and skip otherwise:

- the [whole-tree gates workflow](../../.github/workflows/whole-tree-gates.yml)
  sets the variable on every push to `main`, and on a pull request that changes
  this project, `tooling/windows-vet` or the workflow itself;
- the per-change gate, `putnami lint,test,build,validate --impacted`, does not
  set it, so a change elsewhere in the repository plans neither this project
  nor the prerequisite steps of the projects it depends on.

Run them on any tree with:

```bash
PUTNAMI_WHOLE_TREE_GATES=1 ./putnamiw test --projects @putnami/cli-documents --no-cache
```

`--no-cache` is part of the command. Only a whole-tree input (`git:**`) keys
what these gates read, and such an input selects this project for every change,
which is the cost this arrangement removes. The project declares none, so a
stored verdict says nothing about another tree. The variable is declared under
`options.test.envInputs`: a run with it never restores the skip a run without
it stored.

A red run on `main` means the merged tree fails a gate its pull request did not
run: the public cut holds new debt, or the recorded rehearsal verdict is behind
the tree. Fix it on `main`.

The other tests exercise the scanner and the rehearsal on trees they build, and
read three documents of the CLI module: the release plan and the public-cut
baseline and allowlist. `options.test.filePatterns` names those three and the
workflow, so a change to one of them runs this suite on its pull request.
`TestReleaseRehearsalNeverReplaysACachedVerdict` keeps the declarations and the
workflow's trigger, variable and cache switch together.

## Why this is a separate project

A project declares cache-key inputs per **command**, never per task
(`options.test.filePatterns` folds into the key of every `test~*` task). While
these gates lived in `@putnami/cli`, `CONTRIBUTING.md` and the rest of the
document surface were declared inputs of the CLI's 5 000-test task, so a
documentation-only pull request re-ran the whole suite. Splitting the readers out
moved the declaration with them, and the same holds for the switch: the
variable keys this small suite, not the CLI's.

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
