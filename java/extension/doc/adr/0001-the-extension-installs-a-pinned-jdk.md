# ADR 0001 — The extension installs a pinned JDK

- **Status**: accepted
- **Date**: 2026-11-08
- **Scope**: `@putnami/java`, its toolchain check and its Maven steps

## Context

A Java or Kotlin build needs a JDK, and a Maven build needs Maven. A team that
adds `@putnami/java` on a machine with neither should run its tasks, as a PHP
team runs `@putnami/php` with no PHP. The Gradle wrapper downloads Gradle
itself, but needs a JDK to start.

The CLI installs a toolchain the workspace lock pins (ADR 0003 of the stack
core), with the extension SDK's `pinnedarchive`, which refuses an archive that
holds a link. The Linux Temurin JDK archives link license files to one another
under `legal/`.

## Decision

- The toolchain check runs `javac -version`; a JRE alone is no JDK. A `javac`
  on `PATH` that satisfies `.java-version` runs.
- Otherwise the task installs the Eclipse Temurin JDK `.java-version`
  accepts, through the stack core's `Provide` (ADR 0004 of the stack core):
  17.0.21, 21.0.13 or 25.0.5, 21 first, for Linux and macOS on x64 and
  arm64 and Windows on x64. Each archive is pinned by the SHA-256 the
  Adoptium API publishes, installed under `toolchains/java` of the Putnami
  home and reused by every later task. It runs first on `PATH` and as
  `JAVA_HOME`, which Gradle and Maven read before `PATH` (ADR 0006 of the
  stack core).
- `pinnedarchive` gains `SkipLinks`: a link entry is skipped and nothing is
  created for it. `bin/javac` and `bin/java` read no link.
- A Maven component whose `PATH` holds no `mvn` gets Apache Maven 3.9.11, from
  the Apache archive, pinned by SHA-256, the same way.

## Rejected alternatives

- **Pin the JDK in the workspace lock, as PHP is pinned.** The CLI's installer
  refuses the links, and accepting links there would widen what every lock
  archive may create on the machine.
- **Accept the links inside the archive.** A link is the one entry whose
  target extraction does not control; skipping it keeps the rule that an
  install creates no link.
- **A JDK distribution with no link.** None of the major distributions ships
  one, and Temurin is the one the official images use.

## Consequences

- The archives come from GitHub releases of the Adoptium project; a pin moves
  only when an extension release ships new digests.
- An installed JDK has no license links under `legal/`; its license files
  remain under `java.base`.
- Offline, a task installs nothing and fails when the machine has no JDK.
- A build that uses Gradle toolchains (`jvmToolchain(17)`) still finds or
  downloads its own JDK; the installed one only starts Gradle.
