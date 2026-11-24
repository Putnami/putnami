# @putnami/cli-document-set

The gates that read a known set of this repository's documents: the governance
surface and the neutral-contributor recipe (`CONTRIBUTING.md`, `GOVERNANCE.md`,
`RELEASING.md`, `SECURITY.md`, `README.md`, `protocols/support/README.md`), the
first-use block, the published documentation page the CLI cites for moved
commands, the declared cache-key inputs of every project manifest, the shipped
manifests that must not depend on Putnami Cloud, and the workloads the
environments of `putnami.ci.json` follow.

## Why this is a separate project

A project declares cache-key inputs per **command**, never per task
(`options.test.filePatterns` folds into the key of every `test~*` task). The
whole-tree gates of [`@putnami/cli-documents`](../cli-documents/README.md), the public cut and
the release rehearsal, read every candidate file, so that project keys on
`git:**` and runs on `main` only. These gates read a set of files they can
name, so they key on those files and stay in the gate of every change that
touches one of them:

- `git:../../**/*.md`: the documents above and the documentation folders the
  site publishes, which the moved-commands check walks;
- `git:../../**/putnami.json` and `git:../../**/schema/config.json`: the
  manifests whose patterns the cache-input check reads, and the workloads the
  environment check follows;
- the shipped `go.mod` and `package.json` manifests the cloudless check reads;
- `putnami.ci.json`, `putnami.workspace.json`, the CLI's `go.mod` and the
  first-use smoke script.

A change to anything else, such as a Dart sample or a Go source file, does not
select this project.

## How it reaches the CLI

The module path is `go.putnami.dev/tooling/cli/documents/documentset`, a
**child** of the CLI's module path. Go scopes `internal` visibility by import
path, so this module may import `go.putnami.dev/tooling/cli/internal/...`.
Nothing publishes this module.

## Coverage

The package carries no production code, so coverage is off here.
