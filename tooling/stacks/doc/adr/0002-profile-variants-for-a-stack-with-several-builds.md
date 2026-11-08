# ADR 0002 — Profile variants for a stack with several builds

- **Status**: accepted
- **Date**: 2026-11-08
- **Scope**: `tooling/stacks` (`@putnami/stacks`), `java/extension`
  (`@putnami/java`)

## Context

ADR 0001 gives each stack one extension and one profile. Java and Kotlin run
on two builds, Gradle and Maven, and a Gradle project is either a JVM project
or an Android one; its coverage report exists only when the build turns it
on. One set of steps cannot serve all of them, and a typical repository
should need no override. Two names of a component also differ: Gradle's
`:libs:core` and Maven's `com.acme:pricing` hold a colon, which the CLI's job
keys reserve, and the root Gradle project's task path is `:test`, not
`::test`.

## Decision

- A profile may declare `variants`: named parts of the profile, with its shape
  and every member optional, merged into it like an override. The detector
  names each component's variant; the probe records it in the metadata, and
  the job and the input selectors resolve the profile, then the variant, then
  the project's override. With variants, the profile may declare no task of
  its own; each variant, applied, must be a valid profile. An override cannot
  change `variants`.
- A stack-root input of a task is declared once in the manifest for every
  variant: the conformance kit and the override rule use the union of the
  variants' stack-root files of that task.
- A component has a reference apart from its name: `Component.Ref`, the
  build's own name, which steps receive as `{component}`; `Component.Name`
  stays the project's source name and may be empty, so the project keeps its
  directory's name. For Gradle the reference is the task path prefix, `:`
  for the root project and `:api:` for `:api`.
- A task's `reports.when` names project files: when none exists, the task
  expects no report, so a module without tests passes. A coverage report may
  be `optional`: missing, it is not read and fails nothing.
- A stated version with a single component, such as Java's `21`, names that
  major line, like Rust's `1.82` names its minor line.

## Rejected alternatives

- **Two extensions, `@putnami/gradle` and `@putnami/maven`.** A team looks for
  Java, and the toolchain check, the reports and the doc are the same; the
  issue names one extension.
- **One profile with every variant's steps behind a wrapper script.** The
  steps would leave the profile, and an override could no longer replace
  them.
- **Per-component placeholders.** A free map of values the steps read would
  move the profile's logic into each detector adapter.
- **An optional JUnit report.** A test command that ran nothing would pass;
  `when` keeps a missing report a failure wherever tests exist.

## Consequences

- A stack with one build declares no variant, and nothing changes for it.
- A detector adapter decides a variant from the build files it already
  reads; a wrong guess is corrected by an override of the project.
- The key carries the variant through the project metadata.
