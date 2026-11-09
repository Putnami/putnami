# ADR 0008 — Task targets, input exclusions and pub ranges

- **Status**: accepted
- **Date**: 2026-11-08
- **Scope**: `tooling/stacks` (`@putnami/stacks`), `dart/extension`
  (`@putnami/dart`) and the stack extensions that follow it

## Context

A Dart repository mixes pure Dart packages, which run `dart test`, and
Flutter packages, which run `flutter test` and check the Flutter version.
[ADR 0005](0005-profile-variants-for-a-stack-with-several-builds.md) gives
each its own profile variant. A Flutter app also builds one artifact per
platform directory it holds, and a package builds none. ADR 0002 checks a range in Composer's syntax; a Dart or
Flutter SDK constraint sits in `pubspec.yaml`, which the core cannot read, and
pub reads some constraints differently from Composer.

## Decision

- **Variants.** The Dart profile follows ADR 0005: its detector names the
  `dart`, `flutter` or `dart-in-flutter` variant of every component. The
  `dart` variant changes nothing: it is the profile itself.
- **Targets.** A task may declare `targets`: a target is named, or derived
  from the files a pattern matches (`each`), and may require paths (`when`)
  and host systems (`hosts`). The core lists them in the project directory
  when the task runs; a step that names `{target}` runs once per target, and
  not at all when there is none. The paths a target needs must be task inputs,
  so the key follows them, and a task whose targets name `hosts` declares the
  `hostPlatform` runtime input; an override cannot add `hosts` to any other
  task.
- **Exclusions.** An input pattern under the project that starts with `!`
  leaves its files out, so a file the build tool writes under a directory the
  task reads, such as a Flutter plugin registrant, is no input. It never
  leaves out `putnami.json` or a toolchain file.
- **Pub ranges.** A toolchain file may be YAML, read at a dotted key.
  `toolchain.ranges` reads the range a file states as pub reads an SDK
  constraint, for a package below language version 3.9: `pub-sdk` reads a
  null-safe upper bound of `3.0.0` as `4.0.0` and a `^0.0.x` caret up to
  `0.1.0`; `pub-flutter` reads the constraint as a lower bound, its upper bound
  ignored and an exact version a minimum. `toolchain.version` reads plain.
- **Further checks.** `toolchain.also` adds checks every task runs, such as
  the Dart SDK a Flutter SDK bundles, with the selected toolchain. A check that
  states no version warns only when no check does. A further check that fails
  calls `Stack.Provide` as a failed first check does. A pub-flutter constraint
  in a pubspec whose `environment.sdk` starts at 3.9 or later reads plain, as
  pub reads it there.
- **Report steps.** A task's `reportSteps` run after its steps whatever their
  outcome, so a converter writes the report a failing test runner leaves to
  it, such as `tojunit` after `dart test`, and the failed cases name the
  failure.

## Rejected alternatives

- **A component with no variant runs the profile itself.** ADR 0005 has
  every component of a profile with variants name one; an empty `dart`
  variant keeps that rule.
- **Targets in the probe metadata.** The probe is not rerun when a platform
  directory appears, so the targets would go stale; the task's inputs already
  cover the files a target needs.
- **Composer's reading of a pubspec constraint.** A null-safe package's
  `<3.0.0` would refuse every Dart 3 SDK that pub accepts.

## Consequences

- A pub range keys every version it admits, as a Composer range does under
  ADR 0002.
- A task whose targets are empty runs only its steps that name no target.
- `Stack.Provide` (ADR 0004) receives the component's resolved toolchain and
  the version each of its checks expects, so a stack with variants installs
  the toolchain the component runs. A workspace job of a stack that provides
  its toolchain counts it present when the run may install it.
- A step's program is the pinned toolchain or a program beside it, then a
  program beside the toolchain on the task's `PATH`, then the first of that
  name on the task's `PATH`: a Flutter package's `dart` is its Flutter SDK's.
