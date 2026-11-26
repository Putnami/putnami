# @putnami/cli-documents

The proof that the whole-tree checks of
[`@putnami/repository-checks`](../repository-checks/README.md) key exactly the
files their public-cut scanner reads.

`TestPublicCutCandidateInputsInvalidateTheRecordedVerdict` builds a Git
fixture, computes the cache key of that project's declared
`options.test.filePatterns` with the CLI's own collector (`store.CacheKey`,
`store.CollectKeyFiles`, `store.SelectsPath`), and checks that ignored local
state leaves the key unchanged, that a new document in an unrelated project
changes it and fails the scan, and that the keyed file set equals the
scanner's candidate cut. `TestPublicCutTrackedFilesIncludeNonIgnoredCandidatesAndHandleUnusualNames`
checks the scanner's enumeration on unusual names, deleted tracked files and
ignored files. The fixtures run Git through `gittest`, and their
foreign-machine twins prove them independent of the machine's Git
configuration.

## Why this is a separate project

The checks themselves import nothing from this repository, so their test task
depends on no other project. This proof needs the CLI's store package, so it
lives in a project that imports the CLI. Its declared test input is the one
manifest it reads, `tooling/repository-checks/putnami.json`; its Go imports
select it for a change to the code it compiles.

## How it reaches the CLI

The module path is `go.putnami.dev/tooling/cli/documents`, a **child** of the
CLI's module path, in a **sibling** directory. Go scopes `internal` visibility by
import path, so this module may import `go.putnami.dev/tooling/cli/internal/...`
while `go.putnami.dev/cli/model` or any other sibling path may not; the sibling
directory is what keeps `go test ./...` inside `tooling/cli` from compiling these
tests again. Nothing publishes this module.

## Coverage

The package carries no production code — it is a test surface — so coverage is
off here. `@putnami/cli` keeps its 80 % threshold.
