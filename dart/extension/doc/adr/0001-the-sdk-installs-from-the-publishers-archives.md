# ADR 0001 — The SDK installs from the publisher's archives

- **Status**: accepted
- **Date**: 2026-11-08
- **Scope**: `@putnami/dart`, the toolchain check of every task and the test
  tools

## Context

A Dart or Flutter repository states the SDK it runs on as a pub constraint in
`pubspec.yaml`, such as `sdk: ^3.7.0` or `flutter: ">=3.29.0"`, rarely as one
version. `@putnami/rust` installs its toolchain when a task needs it (ADR 0004
of the stack core), and `@putnami/php` pins one PHP in the workspace lock. The
Dart team publishes every stable SDK on the dart-archive with a SHA-256 beside
it, and the Flutter team publishes a release manifest per host system that
names each stable archive, its SHA-256 and the Dart SDK it bundles. A Flutter
archive weighs 1.5 to 2.5 GB.

## Decision

When the `dart` or `flutter` on `PATH` is missing, or outside the constraint,
a task that is not offline installs the newest stable release the constraint
admits, as pub reads it:

- Dart: the release list of the dart-archive, then the archive and its
  `.sha256sum`;
- Flutter: the release manifest of the host system, whose release must also
  bundle a Dart in `environment.sdk`.

The archive is checked against that SHA-256 before it is unpacked, under
`tools/dart` of the Putnami home, and is reused by every later task. An SDK on
`PATH` that satisfies the constraint runs, and nothing is installed. The
checksum comes over HTTPS from the host that serves the archive: it detects a
truncated or altered download, not a compromised publisher.

The test task needs `tojunit` and `format_coverage`. For each one its `PATH`
lacks, it activates `junitreport` 2.0.2 or `coverage` 1.x, which pub resolves
for the selected SDK, into a pub cache of that SDK version under the Putnami
home, and runs them from there. The machine's pub cache is untouched.

## Consequences

- A repository runs on a machine with no SDK, after one download per release.
- The release a range installs moves when the publisher ships a newer one; the
  key carries the constraint, as for any range (ADR 0002 of the stack core).
- Flutter on Linux arm64 has no archive: such a machine puts `flutter` on
  `PATH`.
- A Flutter archive on Linux is a `.tar.xz`, which the extension SDK does not
  unpack: the host's `tar` unpacks every Flutter archive after the check,
  without keeping the archive's owner, so Linux needs `xz`. Flutter itself
  needs `git`.

## Alternatives rejected

- **A table of pinned releases in the extension**, as `@putnami/php` does for
  a third-party PHP build. The publisher's checksums already pin each archive,
  and a table would refuse every release it does not list.
- **A git clone of Flutter.** The tool then downloads its engine and Dart SDK
  on first run, with no checksum the extension checks.
- **The machine's pub cache for the test tools.** An activation there changes
  what the team's own `dart pub global` commands run.
