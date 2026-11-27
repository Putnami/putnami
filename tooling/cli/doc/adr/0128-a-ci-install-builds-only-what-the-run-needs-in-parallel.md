# ADR 0128 — A CI install builds only what the run needs, in parallel

- **Status**: accepted
- **Date**: 2026-11-27
- **Scope**: `@putnami/cli` (`internal/commands/extensions` `extensions prepare`,
  `internal/jobs/scheduler` runtime comparison preparation)
- **Amends**: [ADR 0104](0104-a-hosted-run-compares-runtimes-prepared-before-it.md),
  decision 1 (which extensions `extensions prepare` digests and builds)

## Context

Before its gate, a Putnami CI run builds the CLI from the tree and runs
`putnami extensions prepare` (`./putnamiw --print-engine`). On a pull request,
that command digested the runtime inputs of every path extension at the tree
and at the merge base, wrote the base tree, and built the runtimes of each
changed extension one after another (ADR 0104). On a fresh clone with a warm
Go build cache, a pull request that changes two extensions spent 42 to 56 s
there, and one that changes a module every Go extension replaces, 192 to
227 s for 56 runtimes.

On 2026-11-27 the owner chose to make the install do the minimum, in
parallel, from the CLI, on the CI runner as locally.

## Decision

### 1. The comparison reads only the extensions a diff reaches

`extensions prepare` hands the comparison the paths the diff changes. An
extension is read only when a changed path lies under its directory or under
a directory of the local module replacements it reaches, or when the
workspace lock, which pins the prepare toolchains, changed. The base tree is
written only when an extension remains.

A runtime input digest reads only files under those directories, with no
link out of them, and the toolchains the lock pins. An extension the rule
skips can differ from the base only through a file Git lists on neither
side, such as an ignored one. Skipping is safe whatever it skips: the
preparation is best effort, and the hosted comparison computes both digests
itself and keeps the edge of an extension whose runtime the store lacks.

### 2. The comparison builds its runtimes in parallel

The runtimes of the tree and of the base are built at once, as many at a time
as a runtime synchronization stage runs, each on its own so one failure
leaves the others prepared, and each checked against the digest its lock
records.

### 3. Rejected and left to others

- **Fetching the CLI and the runtimes from the remote cache by digest** is
  rejected: the run would execute what another run wrote, and the install
  would need a read credential before the run, when `extensions prepare`
  holds none (ADR 0055) and `putnamiw` has no cache client.
- **Runner images with the tooling preinstalled** are left to Putnami Cloud,
  which builds them per workspace. The install keeps using a toolchain or a
  tool it finds installed at its pinned version.

## Consequences

- The two pull requests above now spend 17.6 to 19.5 s and 86 s in
  `extensions prepare` (the proposal holds the protocol).
- A pull request that changes no extension writes no base tree and builds
  nothing for the comparison.
- No runtime a hosted run finds, no digest and no edge a listed diff explains
  changes; an extension that differs from the base only through an ignored
  file keeps its edge.
- A lock change that moves an extension's pinned prepare toolchains still
  prepares that extension at both sides, though a hosted run keeps its edge
  without reading the store.
