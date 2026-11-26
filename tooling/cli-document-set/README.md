# @putnami/cli-document-set

The gates that run the CLI against a known set of this repository's documents:
the neutral-contributor recipe of `CONTRIBUTING.md`, which runs without
credentials and matches the gate `putnami.ci.json` declares; the workloads the
environments of `putnami.ci.json` follow; the cloudless release fixture; and the
guard that keeps the repository's documents out of the CLI's declared test
inputs.

## Why this is a separate project

A project declares cache-key inputs per **command**, never per task
(`options.test.filePatterns` folds into the key of every `test~*` task). The
checks that only read files as text, such as the governance record, the
first-use block and the moved-commands reference, live in
[`@putnami/repository-checks`](../repository-checks/README.md), which imports
nothing from this repository and keys on `git:**`. These gates need the CLI's
code, so they key on the files they read:

- `CONTRIBUTING.md`, `putnami.ci.json` and `putnami.workspace.json`;
- `git:../../**/putnami.json` and `git:../../**/schema/config.json`: the
  manifests whose patterns the cache-input guard reads, and the workloads the
  environment check follows.

A change to anything else, such as a Dart sample or a Markdown file, does not
select this project. A Go source of the CLI or of a module it imports still
does: the test binary compiles against them.

## How it reaches the CLI

The module path is `go.putnami.dev/tooling/cli/documents/documentset`, a
**child** of the CLI's module path. Go scopes `internal` visibility by import
path, so this module may import `go.putnami.dev/tooling/cli/internal/...`.
Nothing publishes this module.

## Coverage

The package carries no production code, so coverage is off here.
