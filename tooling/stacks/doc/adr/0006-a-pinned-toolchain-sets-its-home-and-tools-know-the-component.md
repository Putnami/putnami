# ADR 0006 — A pinned toolchain sets its home, and tools know the component

- **Status**: accepted
- **Date**: 2026-11-08
- **Scope**: `tooling/stacks` (`@putnami/stacks`), first `@putnami/java`

## Context

ADR 0003 puts a pinned toolchain first on `PATH`. A build tool may read
another variable first: Gradle and Maven start the JDK `JAVA_HOME` names, so a
machine whose `JAVA_HOME` names another JDK would run that one. ADR 0004 lets a
stack install the tools of a task; a stack with several builds, such as Java on
Gradle and Maven (ADR 0005), must know which build the component runs, or it
installs Maven for a Gradle project.

## Decision

- `toolchain.home` names a variable that a task sets to the home of the pinned
  toolchain when it runs it: the parent of the directory that holds its
  command. It needs `toolchain.provisioned`.
- `Stack.Tools` receives the component's metadata, its variant included.

## Consequences

- A toolchain found on `PATH` leaves the variable as the machine set it.
- `@putnami/rust` ignores the metadata it now receives.
