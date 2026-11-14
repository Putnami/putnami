# ADR 0015 — Without a declaration, the Go pin is the machine's Go

- **Status**: accepted
- **Date**: 2026-11-14
- **Scope**: `@putnami/go` (`go/extension`): the answer to
  `__putnami toolchain-resolve` for the lock identity `go`
- **Amends**: [CLI ADR 0077](../../../../tooling/cli/doc/adr/0077-the-core-provisions-the-toolchains-extensions-pin.md),
  decision 6

## Context

The extension's `runtimeCompiler` toolchain locks the identity `go` and is a
required run toolchain: every task of the extension, and the preparation of
its runtime, needs a Go pin in the workspace lock. Under ADR 0077 decision 6,
the extension keeps a committed pin and otherwise pins nothing until `go.work`
declares a release.

A repository that keeps its Go modules without a `go.work`, such as one that a
monorepo tool (moon, Bazel, Nx) drives, therefore never gets a pin. `putnami
install` exits 0 with no `go` entry, and every later command fails with
"workspace lock has no exact "go" pin: run `putnami install`", a remedy that
changes nothing. The TypeScript and Python extensions do not loop this way:
each pins its default release when the workspace declares none.

## Decision

**With no `go.work` declaration and no committed pin, in a workspace that
holds a Go module, the extension pins the release of the Go the machine
runs.**

1. The workspace holds a Go module when a `go.mod` lies at its root or below
   it, outside hidden, `node_modules`, `vendor` and `testdata` directories. A
   workspace with none, such as one whose first Go project a template is
   about to render, pins no Go: that project writes the `go.work` its release
   comes from, and `putnami init` and `putnami projects create` pin it in one
   pass, as CLI ADR 0019 states.
2. The go command asked is the one the `runtimeCompiler` candidates find
   first: `GOROOT/bin/go` when `GOROOT` names an executable one, else the
   first `go` in an absolute PATH directory. It is asked as the manifest's
   probe asks it, `go env GOVERSION` with `GOROOT` unset and
   `GOTOOLCHAIN=local`, so the answer is the release that go command runs, not
   one it would switch to.
3. That release resolves as a declared release does: from the machine's pin
   record, else from the go.dev release index, which the answer then records.
4. It pins no Go, as before, when no go command answers, when the go command
   reports no Go release (a development build), when the request is offline
   and no pin record holds that release, or when the lookup fails. Nothing
   declares that release, so an install that cannot pin it goes on as one
   with no Go: a hosted run reads the committed lock, and a run that plans no
   Go job still goes on without a pin (CLI ADR 0019, decision 7).
5. A `go.work` declaration and a committed pin keep their precedence. Once
   pinned, the release is the committed pin, and a teammate whose go command
   runs another release gets the pinned one provisioned by `putnami install`.

## Rejected alternatives

- **Read the monorepo tool's own pin**, such as `.moon/toolchains.yml`. The Go
  extension would name every tool that can pin Go; the go command that tool
  puts on PATH already reports the release.
- **Pin the root `go.mod` directive.** It states a minimum, not a release the
  machine runs, and a repository with no root module has none.
- **Answer a problem when nothing declares Go, or when the lookup fails.** A
  workspace that installs the extension before its first Go project, that
  plans no Go job, or whose machine cannot reach go.dev, would fail
  `putnami install` where it passed before.

## Consequences

- `putnami install` on a repository with Go on PATH and no `go.work` records
  an exact `go` pin, and the commands that need it run.
- The pin depends on the machine that first installs; committing the lock
  makes it the workspace's choice, and `go.work` overrides it. The first-use
  install of a checkout whose committed lock pins no Go also records the
  machine's release, as it records the default Bun of the TypeScript
  extension.
- A repository whose machine cannot reach go.dev and holds no pin record still
  gets no pin; declaring the release in `go.work` and installing with network
  access pins it.
