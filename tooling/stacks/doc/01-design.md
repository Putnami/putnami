# Design note: `@putnami/stacks`

- **Status**: proposed
- **Date**: 2026-11-07
- **Issue**: #933, in the epic #951

## Problem

Putnami runs tasks through three full extensions: Go, TypeScript and Python. A
team on Java and Kotlin, Swift, Dart, .NET, Ruby, Rust or PHP cannot use
Putnami's runner, cache and verdict without a full extension for its stack,
and each one costs 20 to 40 agent-hours.

Two parts of that work already exist:

- `putnami observe` finds each stack's components and the edges between them,
  without running the build: the detectors under
  `tooling/observe/internal/detect` (Gradle, Maven, Swift, Dart, .NET, Ruby,
  Cargo, Composer).
- `@putnami/evidence-readers` (`go.putnami.dev/evidence/readers`) reads the
  reports those builds write: JUnit, TRX, JaCoCo, Cobertura, Clover, LCOV.

`@putnami/stacks` joins them: one extension that turns each component into a
project and runs its `lint`, `test` and `build` from a **stack profile**.

## The shape

```text
             putnami.workspace.json + putnami.json (overrides)
                              │
  observe detectors ──► workspace probe ──► projects, edges, stack tag
  (public package)            │
                              ▼
  stack profile ──► resolved profile per project ──► input selectors
  (in the extension)          │                      stacks:lint|test|build
                              ▼
                lint / test / build task: run the command in the project
                directory, check the toolchain, read the reports
                              │
                              ▼
                 evidence readers (JUnit, coverage) ──► task result
```

### Projects come from the workspace probe

`@putnami/stacks` declares a workspace adapter (see the extension protocol's
[lifecycles](../../../protocols/extension/doc/07-lifecycles.md)), with the
union of every profile's markers: `settings.gradle.kts`, `settings.gradle`,
`build.gradle.kts`, `build.gradle`, `pom.xml`, `Package.swift`,
`pubspec.yaml`, `*.csproj`, `*.fsproj`, `Gemfile`, `Cargo.toml`,
`composer.json`.

- `putnami projects sync` finds the component directories through those
  markers and adds them to `includes`. Membership stays Putnami's.
- The probe (`__putnami workspace-probe`) answers, for each candidate path
  that a detector knows as a component: the source name (`:pricing`,
  `com.acme:pricing`, the crate or package name), the dependencies as paths,
  the tag `stack:<id>`, the extension `@putnami/stacks`, and in
  `metadata["@putnami/stacks"]` the profile id and the expected toolchain
  version.
- A directory whose stack a full extension owns (Go, TypeScript, Python) is
  no candidate of this one: those detectors are not wired in.

The detectors move from `tooling/observe/internal/detect/<stack>` to a public
package of the observe module, `go.putnami.dev/tooling/observe/detect/<stack>`,
so `putnami observe` and `@putnami/stacks` read a repository the same way,
and a detector fix reaches both. Their fixtures move with them and become the
profiles' fixtures, as #951 plans.

### The stack profile

A profile is a JSON document the extension ships, one per stack, under
`tooling/stacks/profiles/<id>.json`, validated by a JSON Schema in the same
directory. This issue ships the schema and one **reference profile**,
`gradle`. The other profiles follow in #934 to #940.

```json
{
  "id": "gradle",
  "markers": ["build.gradle.kts", "build.gradle"],
  "detector": "gradle",
  "toolchain": {
    "command": ["java", "-version"],
    "pattern": "version \"(?<version>[^\"]+)\"",
    "files": [".java-version", ".tool-versions"]
  },
  "inputs": {
    "sources": ["src/**"],
    "manifests": ["build.gradle.kts", "build.gradle", "{root}/settings.gradle.kts", "{root}/settings.gradle", "{root}/gradle/libs.versions.toml"],
    "lockfiles": ["gradle.lockfile", "{root}/gradle/wrapper/gradle-wrapper.properties"]
  },
  "tasks": {
    "lint": {
      "command": ["{root}/gradlew", "{component}:check", "-x", "test"],
      "inputs": ["sources", "manifests"]
    },
    "test": {
      "command": ["{root}/gradlew", "{component}:test", "{component}:jacocoTestReport"],
      "inputs": ["sources", "manifests", "lockfiles"],
      "reports": {
        "junit": ["build/test-results/test/*.xml"],
        "coverage": [{ "format": "jacoco", "paths": ["build/reports/jacoco/test/*.xml"] }]
      }
    },
    "build": {
      "command": ["{root}/gradlew", "{component}:assemble"],
      "inputs": ["sources", "manifests", "lockfiles"],
      "outputs": ["build/libs/**"]
    }
  }
}
```

| Member | Meaning |
|---|---|
| `id` | The profile id, the value of `stack:<id>` and of the metadata. |
| `markers` | The files that make a directory a candidate. |
| `detector` | The observe detector that names the components and their edges. |
| `toolchain.command`, `toolchain.pattern` | How to read the version on the machine: the command's output, standard output first, matched by the pattern's `version` group. |
| `toolchain.files` | The files that state the expected version, nearest first: in the project directory, then up to the stack's root. |
| `inputs.<set>` | Named sets of file patterns. A plain pattern is relative to the project directory; `{root}/` makes it relative to the stack's root, the directory of the build that holds the component (the Gradle settings, the Cargo workspace). |
| `tasks.<task>.command` | The argument vector the task runs, in the project directory, with no shell. `{root}` is the stack's root, `{component}` the detector's name for the component (`:pricing`), `{project}` the project directory. |
| `tasks.<task>.inputs` | The input sets the task reads. `lint` reads fewer than `test`: a lockfile change does not rerun it. |
| `tasks.<task>.reports` | The JUnit and coverage paths the task reads back, relative to the project directory, with the coverage format. |
| `tasks.<task>.outputs` | The paths the task writes, cached and restored like any task output. |

The toolchain files are inputs of every task, whatever its input sets.

### A project overrides any value

A project overrides the profile in its `putnami.json`, under the extension's
option namespace. An override replaces the value at its path whole, the way
the policy's members do; it never merges into an array.

```json
{
  "options": {
    "@putnami/stacks": {
      "profile": "gradle",
      "tasks": {
        "test": { "command": ["{root}/gradlew", "{component}:integrationTest"] }
      },
      "inputs": { "sources": ["src/**", "fixtures/**"] }
    }
  }
}
```

`profile` picks another shipped profile for one project. A workspace sets
values for every project through a scope's `putnami.json`, with the same
inheritance as every other option. An override is validated against the
profile schema; an unknown member fails the load, as a strict decoder does.

### Inputs are input selectors

A task's inputs depend on its profile and on the project's overrides, so the
manifest cannot list them as static patterns. Each task names one selector,
`stacks:lint`, `stacks:test` or `stacks:build`, in its `from: "project"`
input. The extension lists them in `runtime.inputSelectors`, and answers
`__putnami input-resolve` with the files the resolved profile selects for that
task, including the stack-root files (`{root}/…`) and the toolchain files.
Its basis covers the project's `putnami.json` and every scope `putnami.json`
above it, so an override that changes the inputs changes the answer.

## The cache key

A task's key is the core's key (see the CLI's
[caching](../../cli/doc/10-caching.md)) over what the task declares:

| Part of the core key | What it carries for a stack task |
|---|---|
| Implementation digest | The extension runtime, which embeds the shipped profiles: editing a profile changes every key of its stack. |
| Task contract digest | The manifest's task. |
| Task params hash | The resolved `options["@putnami/stacks"]` of the project, so every override. |
| Project metadata digest | The profile id and the expected toolchain version from the probe. |
| File content hash | The files the task's selector answered: its input sets, the stack-root files, the toolchain files. |
| Upstream hashes | The keys of the same task of the projects it depends on. |

A change in one component changes its file hash and the upstream hash of its
dependents, so only they rerun. A `lint` that reads `sources` and `manifests`
keeps its key when only a lockfile moves.

### The toolchain version

The key carries the **expected** version: the content of the toolchain file,
or the profile's `toolchain.version` when a project sets one. Before it runs
the command, the task reads the version on the machine with
`toolchain.command`:

- the versions match, or nothing states an expected version: the task runs;
- they differ: the task fails with `stacks.toolchain_mismatch`, which names
  both versions and the file that states the expected one. It never installs
  or switches a toolchain. A project that accepts the difference sets
  `toolchain.check` to `warn`: the task runs and reports the diagnostic as a
  warning.
- the command is missing: the task fails with `stacks.toolchain_missing`.

So a stored result was produced by the version its key names, unless a
project chose `warn`. A project that states no expected version gets
`stacks.toolchain_unpinned`, a warning: its key carries no toolchain version,
and two machines with two versions share entries.

## Reports

After the command, the task reads every report its profile names with the
evidence readers. A JUnit failure, an error, or a command that exits non-zero
fails the task; the report's failed cases name the failure. The totals and the
coverage reach the task result. A test task whose profile names a JUnit path
and finds no report fails with `stacks.report_missing`: a command that ran
nothing is not a pass. The reports are task outputs, so a cache hit restores
them and reads the same totals.

## Switching to a full extension

A stack can later get a full extension (`@putnami/java`). It reads the same
option block shape, and claims the stack's markers. A project switches by
replacing `@putnami/stacks` with the new name in `extensions` and in its
`options` key, and nothing else. Until it does, the probe of the full
extension and the probe of `@putnami/stacks` both answer for its directory,
and the explicit `extensions` of the project decides which one runs.

## What stays out

- **Publish and package.** A stack's release goes through its own tool (Maven
  Central, NuGet, crates.io); a profile declares no `publish` task.
- **Toolchain install.** The check compares; it never downloads, pins in the
  workspace lock, or switches a version.
- **Serve and qualify.** A profile declares no workload, so `putnami qualify`
  reports no reachable workload for a stack project.
- **The stacks with a full extension.** Go, TypeScript and Python keep theirs.
- **Bazel and Pants.** They delegate (#945, #946): they keep their own cache.
- **Reading the build's own task graph.** A Gradle task dependency outside a
  project edge is not read: the edges are the detector's.

## Rejected alternatives

- **One full extension per stack.** 20 to 40 agent-hours each, and seven
  stacks; the profiles cost a fraction, and a full extension stays possible.
- **Profiles as user files only.** The workspace would write each stack's
  commands from scratch. Shipped profiles with overrides keep the common case
  at zero configuration.
- **Static input patterns in the manifest.** One pattern set for every stack
  would key each task on files it does not read, and an override of an input
  would not reach the key.
- **The machine's toolchain version in the key.** The key is computed before
  the task runs, and the core keys only a lock-resolved toolchain, which would
  pin each stack in the workspace lock and install it: what this issue keeps
  out. Failing on a mismatch keeps the cache honest without it.
- **A copy of the detectors in the new extension.** Two readers of one
  repository would drift; the issue asks to reuse them.

## Proof

On a fixture repository with two Gradle components, `:pricing` depending on
`:catalog`, and the reference profile:

- `putnami test` runs both, reads their JUnit and JaCoCo reports, and a
  second run is a cache hit for both;
- a change in `catalog` reruns `catalog` and `pricing`; a change in `pricing`
  reruns `pricing` alone;
- a `putnami.json` override of the test command, and one of an input set,
  change the key and the command run.

The fixture's `gradlew` is a stub that writes the reports, so the gate needs
no JDK; a JVM in the toolchain check is replaced the same way. That double
sits at a genuine external boundary, the stack's own build tool, and the test
names it.

## Decisions for the owner

1. The detectors become a public package of the observe module, not a module
   of their own.
2. The reference profile is `gradle`, because JUnit and JaCoCo are its native
   reports; #934 completes Java and Kotlin around it.
3. A toolchain mismatch fails the task by default, `warn` by override.
4. Overrides live under `options["@putnami/stacks"]`, and a switch to a full
   extension renames that key with the extension.
