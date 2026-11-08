# ADR 0007 — Nested markers, excluded directories and runtime commands

- **Status**: accepted
- **Date**: 2026-11-09
- **Scope**: `tooling/stacks` (`@putnami/stacks`), first `@putnami/swift`
  (`swift/extension`)

## Context

[ADR 0005](0005-profile-variants-for-a-stack-with-several-builds.md) gives a
stack with several builds one variant per build. Swift has two: SwiftPM
packages and Xcode projects. Three things still do not fit the core:

- An Xcode project's directory holds no marker file of its own: its manifest is
  `<name>.xcodeproj/project.pbxproj`, one directory down.
- An Xcode project's sources have no fixed layout, so its inputs are every file
  of its directory; a `**` walk would then read `.build`, `DerivedData` or
  `Pods`, and the key would move with each build.
- The LCOV export of `llvm-cov` and the conversion of an `.xcresult` bundle
  write to standard output or need logic, such as finding the test binary or
  picking a simulator, that a shell-less argument vector cannot hold.

## Decision

- **Nested markers.** A marker may name a file one directory down, with a
  wildcard directory: `*.xcodeproj/project.pbxproj`. The workspace adapter
  already reads markers as path patterns. A wildcard marker is no watched file,
  and the attribution of a report reads the marker alone as a pattern, never
  the directory above it.
- **Excluded directories.** A profile may name `excludes`: directory names that
  an input pattern with `**` never walks into. The basis of an input answer
  leaves them out too, with `.git` and `.putnami`, so a build writing there
  neither moves the digest nor races its read; a directory a pattern names
  before its first wildcard stays in.
- **Runtime commands.** A step may run `{runtime}`, the extension's own
  executable, with a command the extension registers in `Stack.Commands`. The
  entrypoint dispatches such a call before the jobs. The command is the
  extension's code, in the runtime the key already carries.
- **One component per directory.** When the detector names two components at
  one path, the first is the project and the probe reports the other with
  `stacks.component_conflict`; the input selectors read the same one.

## Rejected alternatives

- **A step that writes its standard output to a file.** It solves the
  redirection only: finding a test binary or a simulator would still need a
  shell or a third-party converter on the machine.
- **Third-party converters** (`xcresultparser`, `xcbeautify`). Each one is a
  tool the team installs and the toolchain check does not see.
- **Listing an Xcode project's file types.** A missing type, such as
  `.xcconfig` or a Core Data model, would leave a change out of the key.

## Consequences

- A runtime command is code the extension tests like any other; its external
  tools (`llvm-cov`, `xcodebuild`, `xcrun`) are the project's own, or the
  toolchain the extension provides under ADR 0003 and ADR 0004.
- The Rust, PHP and Java profiles name no nested marker, no exclude and no
  runtime command: their probe answers and selector files do not change. The
  basis of their `**` patterns now leaves `.git` and `.putnami` out, which
  moves their selector keys once.
