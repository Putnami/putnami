# @putnami/cli-documents

The gates that read this repository's own documents: the governance surface, the
neutral-contributor recipe, the release plan and its recorded verdict, the
public-cut candidate, and the shipped manifests. The Windows compile gate lives
in [`@putnami/windows-vet`](../windows-vet/README.md).

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
bytes and symlink targets. A change in another project's document selects this
project under `--impacted` and invalidates its verdict. Ignored local state in
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
