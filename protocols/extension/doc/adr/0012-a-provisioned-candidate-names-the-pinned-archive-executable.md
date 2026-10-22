# ADR 0012 — A provisioned candidate names the pinned archive's executable

- **Status**: accepted
- **Date**: 2026-10-22
- **Scope**: `go.putnami.dev/protocol/extension` (`protocols/extension`), the
  `provisioned` source of a runtime toolchain candidate, and CLI contract 13

## Context

A runtime toolchain lists the places its executable may be: `path`,
`environment`, `home` and `putnami-home`
([ADR 0002](0002-runtime-toolchains-are-lock-pinned-under-the-current-contract.md)).
Each one names a path, and the extension that runs the toolchain installs it
there. The Go, TypeScript and Python extensions each download their toolchain
into `toolchains/<name>/<name>-<version>` below the Putnami home, and each
names the executable inside that tree in its manifest.

From lock format version 5, a toolchain entry records each platform's archive:
its URL, its format, and the executable's path inside it
([workspace ADR 0003](../../../workspace/doc/adr/0003-extensions-answer-toolchain-pins.md)).
The extension that answers the toolchain-resolve call writes that entry. With
the archive in the lock, the core can download, verify and unpack it for any
toolchain, and the executable's path is already recorded. A candidate that
restates that path in the manifest could disagree with the lock.

## Decision

1. **A candidate may come `from` `provisioned`.** It resolves to
   `<PutnamiHome>/toolchains/<lock>/<lock>-<version>/<executable>`. `<lock>`
   is the toolchain's `lock` identity, `<version>` the version the workspace
   lock pins for it, and `<executable>` the `executable` of the host
   platform's entry in that pin's `archives`. The core unpacks the archive
   there before it resolves the candidate.
2. **The candidate takes no `path` and no `environment`.** The lock names the
   executable, so the manifest does not. `path` is omitted when empty, so a
   `provisioned` candidate marshals as `{"from": "provisioned"}`, and every
   other candidate, whose `path` is required, keeps its bytes and its
   toolchain identity.
3. **A toolchain lists at most one `provisioned` candidate.** A second one
   names the same file.
4. **The same manifest answers for the identity.** A toolchain with a
   `provisioned` candidate must have its `lock` identity listed in its
   manifest's `runtime.resolves`. Only a toolchain-resolve answer writes a
   version 5 entry with archives, so an extension that does not answer for the
   identity cannot rely on the entry it unpacks. Validation reports the
   omission as `unknown-reference` on the candidate and names
   `runtime.resolves` as the remedy.
5. **The source enters as additive contract 13, and the floor binds every
   origin.** A reader below 13 has no path to resolve, and it provisions
   nothing, so it must refuse the manifest instead of resolving the toolchain
   without its executable. A manifest member that a reader outside the
   lock-pinned set must not drop takes an additive contract (ADR 0002,
   decision 7). Only a manifest whose runtime toolchain declares the candidate
   is stamped 13.

## Rejected alternatives

- **Keep `putnami-home` and a path that names the archive's layout.** The
  manifest would restate what the lock records, and the two could disagree
  after an extension changes its answer.
- **Let the core infer the executable from the identity.** The core would name
  tools again, which ADR 0001 forbids.
- **Add no rung.** An older core would read `provisioned` as an unknown
  source, report every candidate missing, and fail the run without saying that
  a newer putnami is the remedy.

## Consequences

- An extension that answers for a toolchain can stop installing it: the core
  provisions every pinned archive the same way.
- A manifest with a `provisioned` candidate is stamped 13, so a CLI below 13
  refuses it with "requires a newer putnami".
- `runtime.resolves` itself still adds no rung. Only the candidate does.
