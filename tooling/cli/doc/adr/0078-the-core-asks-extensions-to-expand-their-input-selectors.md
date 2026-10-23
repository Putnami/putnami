# ADR 0078 — The core asks extensions to expand their input selectors

- **Status**: accepted
- **Date**: 2026-10-23
- **Scope**: `@putnami/cli` (`internal/jobs/inputresolve`,
  `internal/jobs/cachekey`, `internal/jobs/scheduler`, `internal/store`,
  `internal/workspace`, `internal/engine`, `internal/runnerprovider`), CLI
  contract 14 in `protocols/cli`, the manifest rules of
  `protocols/extension`, and the Go extension's manifest

## Context

A task file pattern can name an input selector, `<scheme>:<name>`. The Go
extension's tasks name `go-embed:build` and `go-embed:test`. The core expanded
both with the SDK package `goembed` in four places: the cache key, change
impact, the check of whether a planned write can move a key, and manifest
validation. A new selector, or a change to how one expands, changed the core.

The workspace protocol lets an extension answer for the selectors it lists in
`runtime.inputSelectors`, through `__putnami input-resolve`. Each answer lists
the files the selector reads and the basis it computed them from, whose digest
`InputBasisDigest` computes
([workspace ADR 0004](../../../../protocols/workspace/doc/adr/0004-extensions-expand-their-input-selectors.md)).
The Go extension answers that call for its two selectors.

## Decision

1. **One resolver per run asks and keeps.** It keys an answer by extension,
   project, selector and the sorted, unique patterns of the input, and keeps
   it with the digest of its basis. A read recomputes the digest and asks
   again for that input alone when the digest moved. An error answer is never
   kept. The calls to one extension run one at a time, and a query that waits
   for a call reads its answer.
2. **One call per extension before any key.** The scheduler asks each
   extension once, in one request, for every input the plan's keys read,
   before its workers start. `--impacted` asks once for the inputs of the
   changed projects before it selects.
3. **Every failure fails the task.** A failed call, a timeout, a rejected
   answer, an error answer, a selector the extension does not list, and a
   project outside the workspace fail the input with `ErrInputSelector`. The
   task that reads it fails before cache restore or execution, with caching
   on or off.
4. **A job that names a selector needs its extension's runtime.** The runtime
   phase prepares it, so the call starts the prepared executable. A resolver
   that meets an unprepared extension prepares it on its first call.
5. **Every consumer reads one answer.** The cache key, portable admission and
   the batch guard read the files. Change impact reads the files and the
   paths the basis selects, and an input it cannot answer selects every path.
   A planned write that reaches a listed file or a path the basis selects
   reaches the selection; one that reaches the basis, through a content or a
   listing set, can change the answer, so the selection widens. Unkeyed
   validation reads the error.
6. **Only an extension task names a selector.** A pattern whose scheme is not
   `git` must be a selector the same manifest lists, as an include of a
   `from: "project"` input or of `cache.key.files`. Project `filePatterns` and
   `--filePatterns` cannot name one. A manifest that lists or names a selector
   requires contract 14, and a manifest stamped below 14 that names one is
   refused, with `putnami extensions update` as the remedy.
7. **The core names no selector.** It imports no `goembed`, and no core code
   spells `go-embed:`. The SDK keeps `goembed` for the Go extension and its
   module packager.

## Rejected alternatives

- **Keep the core's Go expansion for manifests below 14.** Two rules for one
  selector could key one tree two ways, and the core would still name Go.
- **Ask once per key, with no memo.** A run would start one extension process
  for each task input that names a selector.
- **Keep an answer for the whole run without its digest.** A task that writes
  a Go source during the run would leave the answer stale.

## Consequences

- A Go task's key is the key the core computed for the same tree. The store's
  `TestGoEmbedSelectorKeysArePinned` pins the key for listed files, and the Go
  extension's `TestAnswerKeepsTheCoreKeyFixtureFiles` pins the same lists
  against its answer.
- A new selector changes one extension's manifest and answer, not the core.
- The CLI and the Go extension ship together: the Go manifest requires
  contract 14, which only this core implements.
- An extension that does not answer costs its timeout for each input that
  reads it, because a failure is never kept.
- A batch member whose selection fails is not batched: it fails alone, before
  the shared process starts.
