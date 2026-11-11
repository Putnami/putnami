# ADR 0003 — The extension pins the SDK releases it installs

- **Status**: accepted
- **Date**: 2026-11-10
- **Scope**: `@putnami/dart`; amends [ADR 0001](0001-the-sdk-installs-from-the-publishers-archives.md)

## Context

ADR 0001 installed the newest stable Dart or Flutter release the pubspec
constraints admit, from the publisher's live release list, checked against the
SHA-256 the publisher states beside each archive. Two machines that run the
same commit a month apart installed different SDKs, and the checksum came from
the host that served the archive. The other stacks that install their
toolchain at task time, Rust, Java and Swift, pin the releases they install
and ship the digests (ADR 0004 and ADR 0006 of `tooling/stacks`).

The workspace lock pins a toolchain the CLI installs from a `tar.gz` or a
`zip`. A Flutter archive for Linux is a `.tar.xz`, so the lock cannot hold the
Flutter SDK.

## Decision

- The extension ships `sdkreleases.json`, which `scripts/sdk-releases.sh`
  writes: the newest patch of each stable Dart minor from 2.19, with the
  SHA-256 the dart-archive publishes for each platform's archive, and the
  newest stable Flutter release of each minor from 3.10 on each system, with
  the SHA-256 its release manifest states.
- A task installs the newest pinned release the constraints admit, checked
  against the pinned SHA-256. It reads no release list.
- A pin moves only when a release of the extension ships a regenerated table.

## Rejected alternatives

- **Pins in the workspace lock.** The lock cannot hold a `.tar.xz`, and a Dart
  SDK alone in the lock would give the Dart and Flutter variants two models.
- **Every stable patch.** The table would grow with each patch, for constraints
  that rarely pin one: a constraint admits the newest patch of its minor.

## Consequences

- The same extension release installs the same SDK on every machine.
- A constraint that admits only a patch the table does not pin, such as
  `sdk: 3.4.0`, fails with the remedy to put that SDK on `PATH`.
- A new SDK release reaches a workspace with the next extension release.
