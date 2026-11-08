# Design note: a shared core for stack extensions

- **Status**: accepted ([ADR 0001](adr/0001-one-extension-per-stack-on-a-shared-core.md))
- **Date**: 2026-11-07

## Problem

Putnami runs tasks through three full extensions: Go, TypeScript and Python. A
team on Rust, PHP, Java and Kotlin, Swift, Dart, .NET or Ruby cannot use
Putnami's runner, cache and verdict, and a full extension written from scratch
costs 20 to 40 agent-hours per stack.

Two parts of that work already exist:

- `putnami observe` finds each stack's components and the edges between them,
  without running the build: the detectors under
  `tooling/observe/internal/detect` (Cargo, Composer, Gradle, Maven, Swift,
  Dart, .NET, Ruby).
- `@putnami/evidence-readers` (`go.putnami.dev/evidence/readers`) reads the
  reports those builds write: JUnit, TRX, JaCoCo, Cobertura, Clover, LCOV.

## The shape

Each stack gets **its own extension**, in the language root the repository
layout gives it: `rust/extension` (`@putnami/rust`), then `php/extension`,
`java/extension`, `swift/extension`, `dart/extension`, `dotnet/extension` and
`ruby/extension`. Each one is thin: a manifest, a **stack
profile** and a ten-line `main`. Everything they share lives in one Go
library, the **stack core**, `tooling/stacks`
(`go.putnami.dev/tooling/stacks`).

```text
                     tooling/stacks  (the stack core, a Go library)
   ┌──────────────────────────────────────────────────────────────────┐
   │ profile + strict decoder            workspace probe from a detector│
   │ overrides from putnami.json         input selectors lint|test|build│
   │ toolchain check                     task runner + report reading   │
   │ manifest conformance test kit                                      │
   └──────────────────────────────────────────────────────────────────┘
        ▲ imports             ▲ imports                 ▲ imports
   rust/extension        php/extension     …     ruby/extension
   @putnami/rust         @putnami/php            @putnami/ruby
   manifest              manifest                manifest
   profile.json          profile.json            profile.json
   cmd: Entrypoint       cmd: Entrypoint         cmd: Entrypoint
        │
        ▼ reuses
   go.putnami.dev/tooling/observe/detect/cargo   (public, moved from internal)
   go.putnami.dev/evidence/readers               (JUnit, LCOV, …)
```

The core ships with `@putnami/rust`, the reference stack. Each other stack
then adds one extension on the same core and refines its profile, and the
Rust extension grows beyond what the reference proves.

A project names its stack's extension like any other:
`"extensions": ["/rust/extension"]`, or the probe assigns it. A stack whose
extension later needs more than a profile (a framework, a `serve`, a
`publish`) grows inside its own extension; no project changes.

### Projects come from the workspace probe

Each stack extension declares a workspace adapter (see the extension
protocol's [lifecycles](../../../protocols/extension/doc/07-lifecycles.md))
with its profile's markers: `Cargo.toml` for Rust.

- `putnami projects sync` finds the component directories through those
  markers and adds them to `includes`. Membership stays Putnami's.
- The probe (`__putnami workspace-probe`), served by the core, runs the
  extension's detector and answers, for each candidate path that the detector
  knows as a component: the source name (the crate name), the dependencies as
  paths, the extension, and in `metadata` the component name, the stack root
  and the expected toolchain version. A component whose manifest names no
  package, such as a Composer application, keeps no source name, and its
  component name is its directory, the same on every checkout.

The detectors move from `tooling/observe/internal/detect/<stack>` to a public
package of the observe module, `go.putnami.dev/tooling/observe/detect/<stack>`,
so `putnami observe` and the stack extensions read a repository the same way,
and a detector fix reaches both. Their fixtures move with them and become the
extensions' fixtures.

### The stack profile

A profile is a JSON document each extension embeds in its runtime,
`profile.json` at the extension root, validated by the core's strict decoder: an unknown member, a placeholder the
core does not know, or a task that names no input set fails the extension's
own tests and, for an override, the project's load.
The Rust reference profile:

```json
{
  "id": "rust",
  "markers": ["Cargo.toml"],
  "toolchain": {
    "command": ["rustc", "--version"],
    "pattern": "^rustc (?<version>\\S+)",
    "files": [
      { "path": "rust-toolchain.toml", "key": "toolchain.channel" },
      { "path": "rust-toolchain" }
    ]
  },
  "inputs": {
    "sources": ["src/**", "build.rs"],
    "targets": ["tests/**", "benches/**", "examples/**"],
    "manifests": [
      "Cargo.toml", ".cargo/config.toml", ".cargo/config",
      "{root}/Cargo.toml", "{root}/.cargo/config.toml", "{root}/.cargo/config"
    ],
    "lintConfig": [
      "rustfmt.toml", ".rustfmt.toml", "clippy.toml", ".clippy.toml",
      "{root}/rustfmt.toml", "{root}/.rustfmt.toml", "{root}/clippy.toml", "{root}/.clippy.toml"
    ],
    "testConfig": ["{root}/.config/nextest.toml"],
    "lockfiles": ["Cargo.lock", "{root}/Cargo.lock"]
  },
  "tasks": {
    "lint": {
      "steps": [
        ["cargo", "fmt", "--package", "{component}", "--check"],
        ["cargo", "clippy", "--package", "{component}", "--all-targets", "--", "-D", "warnings"]
      ],
      "inputs": ["sources", "targets", "manifests", "lintConfig"]
    },
    "test": {
      "steps": [
        ["cargo", "llvm-cov", "nextest", "--package", "{component}", "--profile", "ci", "--lcov", "--output-path", "{output}/lcov.info"]
      ],
      "inputs": ["sources", "targets", "manifests", "testConfig", "lockfiles"],
      "reports": {
        "junit": ["{root}/target/nextest/ci/junit.xml"],
        "coverage": [{ "format": "lcov", "paths": ["{output}/lcov.info"] }]
      }
    },
    "build": {
      "steps": [
        ["cargo", "build", "--package", "{component}", "--locked"]
      ],
      "inputs": ["sources", "manifests", "lockfiles"],
      "artifacts": ["{root}/target/debug/{component}"]
    }
  }
}
```

| Member | Meaning |
|---|---|
| `id` | The stack id. |
| `markers` | The files that make a directory a candidate. |
| `toolchain.command`, `toolchain.pattern` | How to read the version on the machine: the command's output, standard output first, matched by the pattern's `version` group. |
| `toolchain.provisioned` | The environment variable that holds the toolchain the workspace lock pins, for a stack extension that pins one. |
| `tasks.<task>.withoutCoverage` | How the task runs on a toolchain with no coverage support: a `probe` that exits non-zero there, and the `steps` that replace the task's. Its coverage reports are not read, and the task warns with `stacks.coverage_unavailable`. |
| `toolchain.files` | The files that state the expected version or range, read in the project directory, then at the stack root. `key` reads one string value of a TOML table or a JSON object; without it, the file's first non-empty line. |
| `inputs.<set>` | Named sets of file patterns. A plain pattern is relative to the project directory; `{root}/` makes it relative to the stack root, the directory of the build that holds the component (the Cargo workspace, the Gradle settings). |
| `tasks.<task>.steps` | The commands the task runs in order, each an argument vector run in the project directory, with no shell. `{root}` is the stack root, `{component}` the detector's name for the component, `{output}` the task's output directory under the project. |
| `tasks.<task>.inputs` | The input sets the task reads. `lint` reads fewer than `test`: a lockfile change does not rerun it. |
| `tasks.<task>.reports` | The JUnit and coverage reports the task reads back, with the coverage format. The core copies each into `{output}` as soon as the task ends. |
| `tasks.<task>.artifacts` | The files the task copies into `{output}/artifacts` after its steps, so a cache hit restores them: patterns under the project or the stack root that may name `{component}`. The file a pattern with no wildcard names is removed before the steps, unless its path leads outside the project or the stack root or the inputs of one of the component's tasks select it, so only what the steps write is copied. A pattern with a wildcard is only copied, and copies whatever it matches, a file an earlier build left included. A component name that is no single path segment keeps no artifact. A pattern that matches nothing copies nothing. |

The toolchain files are inputs of every task, whatever its input sets.

Several components share one stack root: one Cargo workspace, one
`target/`, one `target/nextest/ci/junit.xml`. The core runs the tasks of one
stack root one at a time, under a lock it holds on that root, and copies the
reports out before it releases the lock, so two crates never read each
other's report. Cargo already serializes on its own build directory, so the
lock costs little.

### A project overrides any value

A project overrides its profile in its `putnami.json`, under its stack
extension's option namespace. An override replaces the value at its path
whole; it never merges into an array.

```json
{
  "options": {
    "@putnami/rust": {
      "tasks": {
        "test": { "steps": [["cargo", "llvm-cov", "nextest", "--package", "{component}", "--profile", "ci", "--features", "slow", "--lcov", "--output-path", "{output}/lcov.info"]] }
      },
      "inputs": { "sources": ["src/**", "fixtures/**"] }
    }
  }
}
```

The core reads the override from the project's own `putnami.json` only, at
the job and in the input selectors alike, so the two never disagree and the
selector's basis covers it. An override is validated by the same strict
decoder: an unknown member, a change to `id` or `markers`, or a stack-root
input the manifest does not declare fails the task with
`stacks.project_config_invalid`. Scope-level overrides are left for later.

### Inputs are input selectors

A task's inputs depend on its profile and on the project's overrides, so the
manifest cannot list them as static patterns. Each task names one selector,
`rust:lint`, `rust:test` or `rust:build`, in its `from: "project"` input. The
extension lists them in `runtime.inputSelectors`, and the core answers
`__putnami input-resolve` with the files the resolved profile selects for that
task, and the project's `putnami.json`. Its basis covers them, so an
override that changes the inputs changes the answer.

A selector answers files under the project only. The stack-root files
(`{root}/…`, and the toolchain files at the root) reach the key through a
second, static input of each task, `from: "workspace"`, which lists the
shipped profile's stack-root patterns of that task; the conformance kit holds
the two equal. So an override may narrow the stack-root inputs but not add
one.

### A nested stack root is an input-only project

A component whose stack root is neither the workspace root nor its own
directory, such as a crate of a Cargo workspace under `services/billing`,
reads the root's files through an edge
([ADR 0002](adr/0002-a-nested-stack-root-is-an-input-only-project.md)). The
detector answers that root as a component with no name when it holds none, a
virtual Cargo workspace: an **input-only project**, whose tasks run no step
and whose selector answers the stack-root files of each task in its own
directory. The probe gives each component of the root an edge to the project
at its root, so the same task's upstream key carries the root's files: a
lockfile change reruns the members' test and build, not their lint.

The probe leaves a component out with `stacks.stack_root_unsupported` when no
project of the stack sits at its root, or when its root reaches it over the
edges the probe answers, root edges included, since its own root edge would
close a cycle; it repeats that check without the components it left out until
no root reaches its member. The workspace root keeps
the static workspace input: a project there would own every file no deeper
project claims.

### What each extension holds

| File | Content |
|---|---|
| `putnami.extension.json` | The manifest: the runtime, the workspace adapter with the profile's markers, the `lint`, `test` and `build` commands and their tasks, the three input selectors. |
| `profile.json` | The stack profile, embedded in the runtime. |
| `cmd/putnami-<stack>/main.go` | `Stack.Entrypoint` of the stack the extension builds from its name, its profile and its detector: the observe detector that names the components and their edges. |
| `doc/`, `putnami.features.json`, `specs/` | The stack's own docs and requirements. |

The manifests repeat one shape. The core ships a test kit that every stack
extension runs: its manifest declares the three commands, their tasks and
selectors, and markers equal to its profile's; its profile passes the decoder.
A stack extension that drifts from the core fails its own gate.

## The cache key

A task's key is the core's key (see the CLI's
[caching](../../cli/doc/10-caching.md)) over what the task declares:

| Part of the core key | What it carries for a stack task |
|---|---|
| Implementation digest | The extension runtime, which embeds the core and the profile: editing either changes every key of the stack. |
| Task contract digest | The manifest's task. |
| Task params hash | The resolved `options["@putnami/<stack>"]` of the project, so every override. |
| Project metadata digest | The component name, the stack root and the expected toolchain version from the probe. |
| File content hash | The files the task's selector answered: its input sets, the stack-root files, the toolchain files. |
| Upstream hashes | The keys of the same task of the projects it depends on. |

A change in one component changes its file hash and the upstream hash of its
dependents, so only they rerun. A `lint` that reads `sources` and `manifests`
keeps its key when only `Cargo.lock` moves.

### The toolchain version

The key carries the **expected** version: the value the toolchain file
states, or `toolchain.version` when a project sets one. Before its first step,
the task reads the version on the machine with `toolchain.command`:

- the versions match, or nothing states an expected version: the task runs.
  An expected version with fewer components names the stable releases it
  prefixes: a `1.82` channel matches 1.82.0 and 1.82.3, not 1.82.0-nightly, and
  keys the minor release, not the patch;
- they differ: the task fails with `stacks.toolchain_mismatch`, which names
  both versions and the file that states the expected one. A project that
  accepts the difference sets
  `toolchain.check` to `warn`: the task runs and reports the diagnostic as a
  warning;
- the command is missing: the task fails with `stacks.toolchain_missing`.

An extension may pin its toolchain, as `@putnami/php` does
([ADR 0003](adr/0003-a-stack-extension-may-provide-its-toolchain.md)). The
profile's `toolchain.provisioned` names the environment variable that holds
the pinned toolchain. When the toolchain on `PATH` is missing, or fails the
check, the task puts the pinned one first on `PATH` and checks it. A toolchain
on `PATH` that passes always runs. `Stack.RootJob` gives an extension a job
that runs at the stack root with the same toolchain and under the same lock,
such as an install step.

A file may state a range instead, in Composer's constraint syntax: the PHP
profile reads `require.php` of `composer.json`, such as `>=8.4` or `^8.2`
([ADR 0002](adr/0002-a-stated-version-range-is-checked.md)). The task runs
when the machine's numeric release is in the range and fails with
`stacks.toolchain_mismatch` otherwise, unless `toolchain.check` is `warn`. The
key carries the range, so two machines in it share entries; a profile lists
an exact file first, such as `.php-version`, so an exact statement wins.

So a stored result was produced by the version, or a version of the range,
its key names, unless a project chose `warn`. A project that states no
expected version gets `stacks.toolchain_unpinned`, a warning: its key carries
no toolchain version, and two machines with two versions share entries. A
Rust channel name such as `stable` is neither a version nor a range, and gets
the same warning.

## Reports

After the last step, the task reads every report its profile names with the
evidence readers, and attributes each entry to a component:

- a JUnit case, through the stack's case resolver, which names the component
  from the case's class name, else its suite name: the Rust stack reads the
  package of a nextest binary id, with `-` and `_` read as one. A stack with no
  resolver reads every case as the task's;
- a coverage file, to the deepest component whose directory holds it: an
  absolute path, or one relative to the project, whose directories the stack's
  detector reads.

A test runner exits non-zero when a case fails, so a step that exits non-zero
stops the steps and the reports are still read. An entry of another component
is left out of the summary: that component's own task counts it. A case of no
component is counted, with the warning `stacks.test_unattributed`; a coverage
file of no component is left out, with the warning `stacks.coverage_unmapped`.
A JUnit failure, an error, or a step that exits non-zero fails the task; the
report's failed cases name the failure, with `stacks.test_failed`, and a failed
step that wrote no report is `stacks.step_failed`, not a missing report. The totals and
the coverage reach the task result. A JUnit or coverage pattern of the profile
that matches no report fails the task with `stacks.report_missing`: a command
that ran nothing is not a pass. The reports a task reads outside its own
output directory are removed before its steps, so a report another component
or an earlier run left is never read as this run's. A file the task's inputs
select is a source and is kept, and a report pattern outside the output
names one place: no `**` and no wildcard first directory. The copied reports are task outputs, so a cache hit
restores them and reads the same totals.

## What stays out

- **Publish and package.** A stack's release goes through its own tool
  (crates.io, Packagist, Maven Central); no profile declares a `publish`
  task. A stack extension may add one later, in its own issue.
- **Toolchain install for Rust.** cargo-nextest and cargo-llvm-cov are the
  project's tools, like `rustc`; `@putnami/rust` pins nothing.
- **Serve and qualify.** No profile declares a workload, so `putnami qualify`
  reports no reachable workload for a stack project.
- **Go, TypeScript and Python.** They keep their full extensions and do not
  move to the core.
- **Bazel and Pants.** They delegate: they keep their own cache.
- **The build's own task graph.** A dependency between Cargo targets outside
  a crate edge is not read: the edges are the detector's.

## Rejected alternatives

- **One shared `@putnami/stacks` extension for every stack**, the first
  version of this note. Configuration under one generic namespace, and a
  project that moves to a full extension later changes its options. One
  extension per stack names the stack where a team looks for it, and lets each
  one grow without touching the others.
- **Full extensions written from scratch.** Seven times the probe, the
  selectors, the runner and the report reading; the core writes them once.
- **Static input patterns in the manifest.** An override of an input would
  not reach the key.
- **The machine's toolchain version in the key.** The key is computed before
  the task runs, and the core keys only a lock-resolved toolchain, which would
  pin each stack in the workspace lock and install it: what this issue keeps
  out. Failing on a mismatch keeps the cache honest without it.
- **A copy of the detectors in each extension.** Two readers of one
  repository would drift.
- **The detectors as a module of their own.** One more module to version and
  publish, for code only observe and the core read.

## Proof

On a fixture Cargo workspace with two crates, `api` depending on `pricing`,
and the Rust profile:

- `putnami test` runs both, reads their JUnit and LCOV reports, and a second
  run is a cache hit for both;
- a change in `pricing` reruns `pricing` and `api`; a change in `api` reruns
  `api` alone;
- a `putnami.json` override of the test steps, and one of an input set,
  change the key and the command run.

The gate has no Rust toolchain. The tests of `@putnami/rust` run its probe,
selectors and jobs on such a fixture with a stub `cargo` and `rustc` first on
`PATH`: they print a version and write the reports cargo-nextest and
cargo-llvm-cov write. That double sits at a genuine external boundary, the
stack's own build tool, and the tests name it. The CLI scenario above runs on
a checkout of the repository whose root holds the fixture, with the same
stubs for `test`, and with the real `cargo`, rustfmt and clippy for `lint`
and `build`.

The extension's real-tool proof runs lint, test and build for every crate of
the observe Cargo detector's fixture repository with the real cargo,
cargo-nextest and cargo-llvm-cov, and the workflow
`.github/workflows/rust-stack.yml` installs those tools and runs it.

## Settled with the owner

1. The detectors become a public package of the observe module.
2. Rust is the reference stack.
3. A toolchain mismatch fails the task by default, `warn` by override.
4. One extension per stack, in its language root, on a shared core.
5. A stated range is checked, for the PHP profile's `require.php`.
