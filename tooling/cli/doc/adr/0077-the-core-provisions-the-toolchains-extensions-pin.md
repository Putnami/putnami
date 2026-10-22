# ADR 0077 — The core provisions the toolchains extensions pin

- **Status**: accepted
- **Date**: 2026-10-22
- **Scope**: `@putnami/cli` (`internal/jobs/toolchainpins`,
  `internal/jobs/toolchain`, `internal/lockfile`, `internal/commands/lifecycle`,
  `internal/workspace`) and the Go, TypeScript and Python extensions' manifests
  and `workspace-install`
- **Supersedes in part**: [ADR 0019](0019-toolchain-lock-follows-provisioned-runtimes.md),
  decisions 1, 2 and 4, the toolchain steps of decision 5, and the statement
  of decision 3 that `workspace-install` installs a toolchain

## Context

Under ADR 0019, the core derives the Go, Bun and uv pins from `go.work`,
`package.json#packageManager` and the root `pyproject.toml`, and reads each
vendor's release index, archive names and digest files. Each extension then
downloads the pinned archive in its `workspace-install`, unpacks it under the
Putnami home, and names the executable inside it in its manifest. The core
therefore names three languages and three vendors, and three extensions repeat
one download, verification and unpacking.

The workspace protocol lets an extension answer for the toolchains it declares
in `runtime.resolves`, through `__putnami toolchain-resolve`, and lock format 5
records each platform's archive beside its digest
([workspace ADR 0003](../../../../protocols/workspace/doc/adr/0003-extensions-answer-toolchain-pins.md)).
A runtime toolchain candidate may come from `provisioned`, under CLI contract
13 ([extension ADR 0012](../../../../protocols/extension/doc/adr/0012-a-provisioned-candidate-names-the-pinned-archive-executable.md)).

## Decision

1. **The extensions answer, before any installer.** `putnami install`,
   `putnami init`, `putnami projects create` and the implicit install run one
   phase before any `workspace-install`. It sends each installed extension
   whose manifest lists `runtime.resolves` one toolchain-resolve request, with
   the workspace probe's transport and timeout. The request carries the lock's
   committed entries for those identities, the host platform, and whether the
   run is offline. The core names no language, release index or declaration
   file.
2. **The answers become the lock's toolchains.** An explicit command writes
   exactly the answered pins: an identity the answer omits, or that no
   installed extension answers for, is removed. The implicit install keeps a
   committed format 5 pin, takes the answer for every other identity, and
   removes nothing. A hosted run never writes the lock and uses the answers in
   memory. A lock whose toolchains change is written at format 5.
3. **A disagreement stops the command.** A problem, a failed, timed-out or
   invalid answer, or two extensions that answer one identity with different
   pins fail the command before any installer runs. The error names the
   extension and the problem's remedy.
4. **The core installs each pin the machine lacks.** When no other candidate
   of the toolchain that locks the identity answers the pinned version, the
   core downloads the pin's archive for the host platform, verifies its
   SHA-256 digest, and unpacks it, `tar.gz` or `zip`, into
   `<PutnamiHome>/toolchains/<identity>/<identity>-<version>/` with
   `pinnedarchive`'s limits. The folder is complete when the probe of the
   answering extension's runtime toolchain that locks the identity answers the
   pinned version from the archive's executable, or, without such a toolchain,
   when that executable is a regular file. A complete folder is reused without
   network access. An incomplete one keeps each entry the archive does not
   provide, so a released extension's install in it, such as Bun's package
   cache, survives. Go's folder is the one `putnamiw` installs into. A hosted
   run provisions nothing: it runs the toolchain the runner holds.
5. **An extension installs no toolchain.** Each extension lists a
   `provisioned` candidate for the toolchain it answers for, in place of its
   `putnami-home` one. A missing or incomplete folder fails a job that needs
   it, with `putnami install` as the remedy. A toolchain that locks an
   identity another extension answers for keeps its `putnami-home` candidate
   when that path is the provisioned executable, as the `go` runtime compiler
   of every other extension does: Go's archive holds `go/bin/go`. Otherwise it
   lists the `provisioned` candidate, as the Bun of `@putnami/clientgen`'s
   TypeScript emitter does.
6. **Each extension keeps its declaration and its default.** It reads its own
   declaration (`go.work`, `packageManager`, `[tool.uv]`). The TypeScript and
   Python extensions pin their default release when the workspace declares
   none; the Go extension keeps a committed pin and otherwise pins nothing
   until `go.work` declares a release. Each `workspace-install` still writes
   a missing declaration, as ADR 0019 decision 4 states.

ADR 0019 decisions 6 and 7 stand. Decision 3 still resolves an alias that only
`workspace-install` needs as optional, and still reports it after the run.

## Rejected alternatives

- **Each extension installs the archive it pins.** Three extensions would keep
  three copies of one download, verification and unpacking, and the folder
  layout would differ by extension.
- **The core keeps its resolvers and the extensions describe only the
  archive.** The core would still name every vendor's release index.

## Consequences

- Pinning a new toolchain changes one extension, not the core.
- The CLI and the Go, TypeScript, Python and clientgen extensions ship
  together: their manifests require contract 13, which only this core
  implements.
- The CLI unpacks `zip` toolchain archives, because Bun publishes only zip
  archives and Go and uv publish zip archives for Windows. CLI decision D-W2
  states which archives stay `tar.gz`.
- A lock below format 5 that pins toolchains is rewritten only by
  `putnami install`.
