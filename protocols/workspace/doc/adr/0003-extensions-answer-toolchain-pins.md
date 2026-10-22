# ADR 0003: Extensions answer toolchain pins

- **Status**: accepted
- **Date**: 2026-10-22
- **Scope**: `go.putnami.dev/protocol/workspace` (`protocols/workspace`): the
  `putnami.lock.json` toolchain entries and the toolchain-resolve exchange;
  `runtime.resolves` in `go.putnami.dev/protocol/extension`

## Context

The lock pins each toolchain to an exact version and a SHA-256 per platform.
Through lock version 4, an entry also names a `source` base URL, and its key is
one of four names: `go`, `node`, `bun`, `uv`. The lock does not record the
archive's file name, so a reader derives it from the name, the version, and
the platform.

`@putnami/cli` resolves each pin itself, with one resolver per name that knows
the vendor's release index, archive names, and digest files. Pinning another
toolchain changes the protocol's name list, the core, and every reader. The
extension that runs a toolchain is the one that knows its vendor.

## Decision

1. **The lock records the archive.** Lock version 5 keys `toolchains` by any
   lock identity. Each entry records, per platform, the archive's `url`,
   `format`, and `executable` next to its integrity, and carries no `source`.
   A reader installs any entry without knowing the toolchain. `integrities`
   is written after `archives`, because `putnamiw` takes the last platform
   line of the `go` entry as its digest.
2. **An extension answers for the identities it declares.** A manifest lists
   lock identities in `runtime.resolves`. The core asks for their pins through
   `<executable> __putnami toolchain-resolve`, over the probe's transport. The
   answer is the lock's next state for each identity: a pin, a problem, or no
   entry. Each pin follows the lock's version 5 entry rules, through the
   function `ValidateLock` uses, and covers the host platform.
3. **No contract rung and no implicit migration.** `runtime.resolves` is an
   optional member that an older core ignores, so it adds no CLI contract
   rung. A lock below version 5 that pins toolchains does not migrate:
   `MigrateLockToCurrent` reports `needs-resolution` for each entry, because
   only the answering extension can supply its archives.

## Consequences

- Pinning a new toolchain changes one extension's manifest and answer, not this
  protocol.
- The lock alone tells a reader what to download, how to verify it, and where
  the executable sits in it.
- A version 3 or 4 lock that pins toolchains is rewritten as version 5 only
  after a toolchain-resolve answer for each entry.
- `runtime.resolves` need not match the runtime's own toolchains, so an
  extension can answer for a toolchain another extension runs. This protocol
  does not choose between two extensions that answer for one identity.
