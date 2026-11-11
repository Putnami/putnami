# ADR 0002 — The extension pins the SDK and runtime releases it installs

- **Status**: accepted
- **Date**: 2026-11-10
- **Scope**: `@putnami/dotnet`; amends [ADR 0001](0001-the-sdk-comes-from-microsoft-and-an-image-copies-onto-the-official-runtime.md)

## Context

ADR 0001 installed the SDK the roll-forward policy picks from Microsoft's live
release metadata, and checked each archive against the SHA-512 that metadata
states. Two machines that run the same commit a month apart installed
different SDKs and runtimes, and the checksum came from the host that serves
the archives. Rust, Java and Swift pin the releases they install and ship the
digests (ADR 0004 and ADR 0006 of `tooling/stacks`).

The workspace lock binds a toolchain archive with a SHA-256, which Microsoft
does not publish, so the lock cannot pin the SDK without downloading every
archive to hash it.

## Decision

- The extension ships `sdkreleases.json`, which `scripts/sdk-releases.sh`
  writes from Microsoft's release metadata: for every channel from 8.0, the
  newest stable SDK of each feature band and the newest ASP.NET Core runtime,
  with the archive and the SHA-512 of each platform the extension installs on.
- A task picks the SDK as the .NET host would, among the pinned ones, installs
  it with the pinned runtimes of every channel from 8.0 up to its own, and
  checks each archive against the pinned SHA-512. It reads no release
  metadata.
- A pin moves only when a release of the extension ships a regenerated file.

## Rejected alternatives

- **Every SDK and runtime release.** The file would weigh over 1.5 MB and
  grow with each patch, while a policy other than `disable` admits the newest
  patch of a feature band.
- **Pins in the workspace lock.** The lock needs a SHA-256 per archive, and a
  .NET SDK archive weighs about 200 MB.

## Consequences

- The same extension release installs the same SDK and runtimes on every
  machine.
- `global.json` with `rollForward: disable` on an SDK that is not the newest of
  its feature band, or on a prerelease, fails: put that SDK on `PATH`.
- A new SDK or runtime reaches a workspace with the next extension release.
