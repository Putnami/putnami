# ADR 0004 — The contributor extension ships a compiled executable

- **Status**: accepted
- **Scope**: `@putnami/contributor` (`tooling/contributor`): its manifest, its
  Go module and its packaging
- **Supersedes in part**: [ADR 0001](0001-one-contributor-extension.md) §1, on
  the extension running nothing

## Context

ADR 0001 §1 made the extension content-only: its manifest declared only
`agentContent`, and the native verifier, publication and policy helpers stayed
in the core CLI (`putnami tree verify`, `putnami proposals finalize`,
`putnami contributor language|machine-load`, `putnami sessions
export|summary`). Decision D-011 in the root `decisions.json` settles that
`@putnami/contributor` ships a compiled executable and owns those commands.
This record fixes what the extension ships before any of them moves.

## Decision

### 1. One Go runtime beside the agent content

`tooling/contributor` is the Go module `go.putnami.dev/tooling/contributor`.
Its command `cmd/putnami-contributor` is the extension's runtime, declared as
`runtime.executable: compiled/putnami-contributor`. Until a command moves, it
answers only the runtime handshake (`__putnami runtime-info`) and refuses
every other invocation.

The manifest keeps `agentContent` unchanged, so the generated `.agents/`,
`.claude/` and `.codex/` trees do not change. It claims `cliContract` 5, the
lowest contract that covers agent content, and names no extension dependency.

### 2. Only the locked Go release builds the runtime

For a path declaration, the host runs `bin/prepare`. Its `runtimeCompiler`
toolchain resolves a Go compiler whose `go env GOVERSION` equals the Go
version `putnami.lock.json` pins. The host passes that compiler as
`PUTNAMI_CONTRIBUTOR_GO` and the pinned version, `go{version}`, as
`PUTNAMI_CONTRIBUTOR_GO_VERSION`. `bin/prepare` builds with that compiler
only. It never falls back to a `go` on `PATH` or under `GOROOT`, and it fails
naming `putnami install` when the host resolved none. Before it builds, it
asks the compiler for its `GOVERSION` in the build environment and fails when
the answer differs from the pinned version. It builds outside any Go
workspace (`GOWORK=off`), with `GOTOOLCHAIN=local`, without cgo, a VCS stamp
or host paths, and ignores an ambient target platform.

The host prepares a runtime the first time a command, tool or hook runs it.
`putnami install` installs the agent content and builds no runtime that
nothing runs yet.

### 3. The Go packager packages the extension

`@putnami/go` packages the project into one archive per platform of the
distribution matrix. Each archive carries `content/` and
`compiled/putnami-contributor`. The package task stamps the version through
`options["@putnami/go"]["version-var"]` and checks that the binary embeds it.

The project does not name `@putnami/scaffold`. Both packagers own
`<command-output>/archives`, and
[scaffold ADR 0004](../../../scaffold/doc/adr/0004-one-packager-for-every-content-project.md)
§2 leaves an extension that runs something to its language extension.

## Invariants

- The extension's runtime is built only by the Go release the workspace lock
  pins, and `bin/prepare` checks that version itself before it builds.
- One packager, `@putnami/go`, owns the project's `<command-output>/archives`.
- Adding the runtime changes no generated agent-content file.
- The packaged manifest claims the contract the packager stamps for it: 5
  while it names no extension dependency.

## Rejected alternatives

- **Keep the commands in the core.** D-011 settles the move.
- **Build with the `go` on `PATH`.** A path-declared runtime would then depend
  on whichever compiler a machine or hosted runner happens to carry.
- **Keep `@putnami/scaffold` beside `@putnami/go`.** Two tasks would own the
  same archive directory, and the scaffold packager refuses an extension that
  runs something.

## Consequences

- A hosted run that loads this path-declared extension's runtime builds it
  from source with the locked Go release.
