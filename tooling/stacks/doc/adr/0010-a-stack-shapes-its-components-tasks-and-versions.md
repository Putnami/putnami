# ADR 0010 — A stack shapes its components' tasks, inputs and toolchain versions

- **Status**: accepted
- **Date**: 2026-11-09
- **Scope**: `tooling/stacks` (`@putnami/stacks`), `dotnet/extension`
  (`@putnami/dotnet`) and every stack extension on the core

## Context

The core of [ADR 0001](0001-one-extension-per-stack-on-a-shared-core.md), with
ADR 0002 to 0009, 0012 and 0013, runs every task of a profile on every
component of a variant, reads JUnit test reports, keys a task on the files
its input patterns select under the project, and matches the machine's
toolchain version with one rule: equal, or a stable release of a shorter version.
ADR 0007's `excludes`, ADR 0013's `!` patterns and ADR 0009's `!{root}/`
stack-root exclusions keep build outputs out of a task's files. .NET breaks
the rest on a typical repository:

- `dotnet test` runs only in a test project; in a library it exits 0 and
  writes no report, which the core reads as `stacks.report_missing`.
- `dotnet test` writes a TRX report with no extra package; a JUnit report
  needs a logger package in every test project.
- A folder that holds a solution and a project file makes `dotnet build`
  without an argument fail; the step must name the project file, and a
  workspace restore must name every project file of the stack root.
- `global.json` states an SDK version and a roll-forward policy: `8.0.400`
  accepts 8.0.404 by default, so the core's rule fails a correct machine.
- A test project measures the code of the projects it references, which run
  no test of their own; the core leaves another component's coverage file to
  that component, so the code would be counted by no task. Coverlet on Linux
  writes the source `/` and file names relative to it.

## Decision

The core gains five generic means, each optional, and the .NET profile uses
all of them, with ADR 0009's `!{root}/**/bin/**` and `!{root}/**/obj/**` so
the copies a build writes of `global.json` and `Directory.Build.props` change
no key:

1. **A detector names the tasks a component runs.** `Component.Tasks`
   reaches the probe metadata, and the task asks the detector again when it
   runs, so a file that decides it and that the task's key covers decides it
   fresh. A task outside the list passes with nothing run, not even the
   toolchain check. No list runs every task, as before; a component whose
   list is empty is no project.
2. **A test task reads TRX reports.** `reports.trx` names TRX patterns read
   by the evidence readers beside `reports.junit`; the two count in one test
   summary, and a test task whose JUnit and TRX patterns together match no
   report fails with `stacks.report_missing`.
3. **A step names the component's manifest.** The probe metadata carries the
   detector's manifest file, and `{manifest}` expands to its absolute path.
   A root step lists the manifests it runs for in `RootStep.Manifests`: the
   job's component, or every component at the stack root in a workspace job,
   so `dotnet restore` reaches each project of a root that holds no
   solution.
4. **A stack matches its own toolchain versions.** `Stack.MatchVersion`, when
   set, replaces the core's rule; the key still carries the expected version
   the files state, and the check compares the version the toolchain command
   prints, the one the build uses. A profile may name a fallback command,
   which lists the installed versions when the toolchain command fails: the
   check then fails with `stacks.toolchain_mismatch` and names them, and
   `toolchain.check: warn` relaxes it. The .NET stack reads `dotnet --version`,
   which fails outright when no SDK satisfies `global.json`, falls back to
   `dotnet --list-sdks`, and applies `global.json`'s `rollForward`.
5. **A stack counts the coverage its tests measure.**
   `Stack.CrossComponentCoverage` counts a coverage file of another component
   as the task's. Coverlet's file names, relative to the source `/`, are read
   under that source, as ADR 0009 settles for a relative path.

When the toolchain command fails and its fallback lists no satisfying
version, the toolchain on the machine is a mismatch: a stack that provides
its toolchain (ADR 0003, 0004) installs one the statement accepts. The core also empties
a task's output directory before its steps.

## Rejected alternatives

- **A test step per project kind in the profile.** A profile is one per
  stack; the detector already reads the project files that say which kind a
  project is.
- **A JUnit logger in the .NET profile.** Every test project would need the
  package before its first run.
- **Rollforward rules in the core.** They are .NET's; another stack's rule
  would be another branch in the core.

## Consequences

- The metadata of every stack whose detector names a manifest gains its path,
  so each of its keys changes once; its behavior does not.
- A component the detector misreads runs a task it should not, or skips one
  it should run: the detector's rule is in its stack's reference page, with
  the project-file property that settles it.
- A stack's version rule lives in its extension, so a team reads it in that
  extension's reference page.
