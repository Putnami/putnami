# ADR 0001 — The JDK is pinned in the workspace lock

- **Status**: accepted
- **Date**: 2026-11-08
- **Scope**: `@putnami/java`, its toolchain check and its Maven steps

## Context

A Java or Kotlin build needs a JDK, and a Maven build needs Maven. A team that
adds `@putnami/java` on a machine with neither should run its tasks, as a PHP
team runs `@putnami/php` with no PHP. The Gradle wrapper downloads Gradle
itself, but needs a JDK to start.

## Decision

- `putnami install` pins one Eclipse Temurin JDK in `putnami.lock.json`, through
  the `toolchain-resolve` call of ADR 0003 of the stack core: 17.0.21, 21.0.13
  or 25.0.5, the release every `.java-version` of the workspace's builds
  accepts, 21 by default. Each archive is the Adoptium build of its platform,
  pinned by the SHA-256 the Adoptium API publishes.
- The toolchain check runs `javac -version`, which prints the bare version the
  lock's probe compares; a JRE alone is no JDK. A `javac` on `PATH` that
  satisfies `.java-version` runs. Otherwise the pinned JDK runs, first on
  `PATH` and as `JAVA_HOME`, which Gradle and Maven read before `PATH`.
- A Maven component whose `PATH` holds no `mvn` gets Apache Maven 3.9.11, which
  the extension installs under the Putnami home after checking its SHA-256: its
  version command prints several lines, which no lock probe compares.

## Rejected alternatives

- **Install the JDK at task time, as `@putnami/rust` installs Rust.** A JDK is
  a plain archive the CLI already installs and caches from a lock pin; the
  lock also makes the version reviewable.
- **One JDK per component.** The lock holds one pin per toolchain; a workspace
  whose builds state several majors runs the others from `PATH`.
- **Pin Maven in the lock too.** Its probe cannot compare an exact version.

## Consequences

- The archives come from GitHub releases of the Adoptium project; a pin moves
  only when an extension release ships new digests.
- A build that uses Gradle toolchains (`jvmToolchain(17)`) still finds or
  downloads its own JDK; the pinned one only starts Gradle.
