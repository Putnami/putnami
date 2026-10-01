# ADR 0002: Runtime evidence needs an application carrier

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/architecture`, `go.putnami.dev/app/darc`

## Context

A `domainAccess` capability row is evidence that a running component was
configured with a declared import. A capability manifest is written by an
application describing itself: in Go, `Application.Describe` runs when an
`app.Application` process starts under `PUTNAMI_DESCRIBE`, and the Go
extension's `build~describe` job refuses a project that has no `main` or does not
import `go.putnami.dev/app`. In TypeScript, the capability producer emits it and
the opt-in `options.generate.capabilities` promotes it into the tracked tree.

The repository's tooling binaries (`putnami`, `putnami-sdd`, the language
extensions) are CLIs, not applications. They dispatch a subcommand and exit, so
they have no carrier.

## Decision

**A DARC evidence row is produced by an application describing itself. A
project that is not a Putnami application emits no row, and that silence is
honest.**

- `architecture.declared_without_evidence` fires only inside a domain that
  already emits evidence, for an active import nothing in it implements. A
  domain with zero rows passes.
- Evidence is domain-atomic: the first row a domain emits obliges a row for
  every active import it declares.
- Nobody writes a capability row by hand.
- The DARC runtime primitives live in `go.putnami.dev/protocol/architecture/darc`
  and import only the architecture and diagnostic protocols.
  `go.putnami.dev/app/darc` re-exports them through type aliases and owns only
  what is application-specific: the describe `Plugin` and the `Evidence`
  projection onto a capability row.
- Tooling does not depend on `go.putnami.dev/app` to obtain evidence, and there
  is no tooling-side emission carrier. Tooling imports stay enforced at their
  code paths and verified by their own tests.

## Rejected alternatives

- **Hand-write the rows.** A row with no runtime guard behind it turns the
  gate's join into a tautology.
- **Mark the tooling imports `planned`.** They are implemented and enforced;
  calling shipped behavior a target is a lie in the other direction.
- **Make tooling depend on `go.putnami.dev/app`.** It links a DI container,
  config loader, logger and migration engine into build tools, adds
  `cli`/`sdd`/`extension-providers` → `go-framework` domain pairs, and makes the
  CLI bootstrap build depend on the framework tree.
- **A tooling-side carrier** (an SDK writer plus a describe branch reading a
  declared Go symbol). Its only consumers would be this repository's own
  binaries; it spends a design decision on an evidence count. Revisit only when
  a consumer outside this repository needs evidence from a non-application
  binary.

## Consequences

- Domains whose projects are all tooling emit no evidence and pass; the gate
  reports `coverage.domainAccess: framework-evidence` because other domains
  emit.
- The repository ratchet asserts which projects own evidence rows, so a row
  that appears without a runtime behind it fails a test.
- A component built through either import path is the same type.
