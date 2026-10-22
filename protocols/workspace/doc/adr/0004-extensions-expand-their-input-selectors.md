# ADR 0004: Extensions expand their input selectors

- **Status**: accepted
- **Date**: 2026-10-22
- **Scope**: `go.putnami.dev/protocol/workspace` (`protocols/workspace`): the
  input-resolve exchange and `InputBasisDigest`; `runtime.inputSelectors` in
  `go.putnami.dev/protocol/extension`

## Context

A task file pattern can name an input selector instead of a glob. The Go
extension's tasks name `go-embed:build` and `go-embed:test`, which select the
files that the `//go:embed` directives of a package's build or test sources
name. The extension manifest protocol knows these two names, and a manifest
that names one requires CLI contract 6.

`@putnami/cli` expands both selectors itself, with the Go SDK package
`goembed`, in four places: the cache key, change impact, the check of whether a
key computed before any task runs can still change, and manifest validation.
Adding a selector, or changing how one expands, changes the core and this
protocol. The extension that declares the task is the one that knows its
language.

## Decision

1. **An extension expands the selectors it declares.** A manifest lists the
   selectors its extension expands in `runtime.inputSelectors`. A selector name
   is `<scheme>:<name>`, at most 128 bytes, and the `git` scheme stays reserved
   for the core's `git:` file pattern. `IsInputSelectorName` states the
   grammar; the extension protocol applies the same rule to the manifest list.
2. **The core asks through a control call.** `<executable> __putnami
   input-resolve` runs over the probe's transport: one JSON document each way,
   the workspace root as working directory, and the probe's timeout. The
   request lists inputs. Each names a project directory, a selector, and the
   other patterns of the task input that names it, because an expansion can
   depend on what the input's ordinary patterns select.
3. **An answer states its files and its basis.** For each input, in request
   order, the answer gives either an error or the files the selector reads now
   together with a basis. The basis lists pattern sets in the file-pattern
   grammar. `content` sets name the files whose bytes the answer read;
   `listing` sets name the paths whose existence, kind or link target it read.
   Each set is matched on its own, so one set's exclusions never remove another
   set's paths. `ValidateInputResolveResult` holds a result to the grammar,
   including that a basis set selects each file, and
   `ValidateInputResolveAnswer` holds it to its request.
4. **The protocol computes the basis digest.** `InputBasisDigest` hashes the
   tree as a basis selects it, never following a link, with framed and sorted
   records. An answer stays exact while its digest is unchanged, so a consumer
   keeps it until a write changes what its basis selects. Because the protocol
   computes the digest, an extension can prove in its own tests that its basis
   covers what it reads.
5. **`runtime.inputSelectors` is optional.** A core that does not read it
   ignores it and expands `go-embed:build` and `go-embed:test` itself. The core
   that asks extensions instead ships with the next CLI contract rung, which a
   manifest that names a selector must declare.

## Consequences

- A new selector changes one extension's manifest and answer, not this protocol
  or the core.
- Every consumer of a selection reads one answer: the files, the basis sets that
  bound it, or the error.
- A consumer keeps an answer across a run and asks again only for an input
  whose basis digest changed.
- The digest records only what the basis selects: a basis that misses a path
  the answer read makes the answer stale without a digest change. The
  extension's tests own that coverage.
- This protocol does not choose between two extensions that list one selector.
