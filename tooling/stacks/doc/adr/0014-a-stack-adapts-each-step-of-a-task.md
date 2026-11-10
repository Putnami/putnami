# ADR 0014 — A stack adapts each step of a task

- **Status**: accepted
- **Date**: 2026-11-10
- **Scope**: `tooling/stacks` (`@putnami/stacks`), first `@putnami/java`

## Context

A profile states each step's program: the Java profile runs the root's
`{root}/gradlew` and the `mvn` on `PATH`. Some builds state the tool that runs
them elsewhere, or not at all. A Gradle build without a wrapper has no
`gradlew`; a Maven build's `.mvn/wrapper/maven-wrapper.properties` names the
Maven release it needs, whatever `mvn` the machine holds; and a Gradle build
whose toolchains ask for another JDK needs its location as an argument of
every Gradle step. `Stack.Tools` (ADR 0004, ADR 0006) appends directories to
`PATH`, after the machine's own tools, and cannot name a path program or add
an argument. `RunWith` adds arguments to the last step of one task only.

## Decision

- `Stack.Step` adapts each step of a component's task before it runs, with its
  placeholders expanded and the task's toolchain selected. It receives the
  task, the component's metadata, the stack root with the selected
  environment, and the step, and answers the step to run: another program,
  more arguments, or the same step.
- An offline run calls it too: the stack decides what it may use without the
  network.
- Its error fails the task with `stacks.toolchain_missing`.
- `RunWith` and `RunFiltered` read the adapted last step, so a test filter
  still recognizes the build tool the step runs.

## Consequences

- A stack that installs a tool a build states, or passes the location of an
  installed toolchain, does it in one place for every step, overrides
  included.
- A stack's own jobs, such as a workspace install or a `serve` build, run its
  adaptation themselves: `Step` adapts the steps of a task only.
- `Tools` stays for a tool any task's `PATH` needs whatever its steps name.
