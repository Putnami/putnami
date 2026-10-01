# ADR 0002 — Generated contracts have exactly one committer, and the manifest relocates

- **Status**: accepted
- **Scope**: `@putnami/go` (`go/extension`)

## Context

A Go project's published contracts have two producers. The static AST pass
(`generate`) reads source without building, so it works for libraries and for
binaries that do not link. The runtime pass (`describe`) compiles a host
binary and runs it with `PUTNAMI_DESCRIBE`, so it sees streaming modifiers,
resolved config structures and the real route table, but only for a project
with an application entrypoint.

Both can produce `schema/openapi.json`. If both commit it, the committed
contract depends on scheduling; if neither does, it goes stale. The generation
manifest is also a cached artifact restored into other checkouts, so an
absolute path in it points nowhere after a restore.

## Decision

**One committer per artifact, chosen before either producer runs.** When
describe runs for a project, generate stages its output under `.gen/` and
defers the committed sidecar; describe merges and commits the converged
result ([ADR 0005](0005-describe-owns-the-migration-bundle-inside-gen.md)
records the staging hand-off). When describe does not run (a library, or a
binary with no `go.putnami.dev/app` dependency), generate is the sole
committer. For OpenAPI, the two documents merge deterministically by path and
method. Any other artifact with two producers fails instead of being
overwritten.

**Committed output is a diff.** Only files whose content changed are written.
`options.generate.schema=false` suppresses the tracked write, never the
`.gen` write, and the manifest's export points at the `.gen` copy. A project
with no describe phase may not set it (ADR 0005).

**Manifest paths are project-relative and slash-separated.** Producers
relativize at the boundary; consumers resolve against the project root, and an
absolute value resolves to itself.

**A generator cannot write outside the project root.** A traversal path is
rejected, not normalized.

## Rejected alternatives

- **Let describe win by running last.** Ordering is not a contract, and
  describe never runs for a library.
- **Commit only from `generate`.** It discards the fidelity describe exists
  for.
- **Never commit generated contracts.** Consumers compile against them and
  reviewers read them; regenerating on demand pushes a toolchain onto every
  consumer.
- **Absolutize manifest paths on read.** Every reader would need the
  producing checkout, which the cache entry does not carry.

## Consequences

- A new producer must declare which artifacts it commits and which it stages.
- Describe needs one entrypoint. A project with several `cmd/*/main.go`
  binaries must name one.
- The merge is defined for OpenAPI only. Another artifact kind with two
  producers needs its own merge rule first.
