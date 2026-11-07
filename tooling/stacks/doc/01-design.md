# Design note: a shared core for stack extensions

- **Status**: proposed
- **Date**: 2026-11-07
- **Issue**: #933, in the epic #951

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
`ruby/extension` in #934 to #940. Each one is thin: a manifest, a **stack
profile** and a ten-line `main`. Everything they share lives in one Go
library, the **stack core**, `tooling/stacks`
(`go.putnami.dev/tooling/stacks`).

```text
                     tooling/stacks  (the stack core, a Go library)
   ┌──────────────────────────────────────────────────────────────────┐
   │ profile schema + strict decoder     workspace probe from a detector│
   │ overrides from putnami.json         input selectors lint|test|build│
   │ toolchain check                     task runner + report reading   │
   │ manifest conformance test kit                                      │
   └──────────────────────────────────────────────────────────────────┘
        ▲ imports             ▲ imports                 ▲ imports
   rust/extension        php/extension     …     ruby/extension
   @putnami/rust         @putnami/php            @putnami/ruby
   manifest              manifest                manifest
   profile.json          profile.json            profile.json
   cmd: stacks.Main      cmd: stacks.Main        cmd: stacks.Main
        │
        ▼ reuses
   go.putnami.dev/tooling/observe/detect/cargo   (public, moved from internal)
   go.putnami.dev/evidence/readers               (JUnit, LCOV, …)
```

This issue ships the core and `@putnami/rust`, the reference stack. Each of
#934 to #940 then adds one extension on the same core and refines its profile;
#939 completes Rust beyond what this issue proves.

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
  profile's detector and answers, for each candidate path that the detector
  knows as a component: the source name (the crate name), the dependencies as
  paths, the extension, and in `metadata` the component name, the stack root
  and the expected toolchain version.

The detectors move from `tooling/observe/internal/detect/<stack>` to a public
package of the observe module, `go.putnami.dev/tooling/observe/detect/<stack>`,
so `putnami observe` and the stack extensions read a repository the same way,
and a detector fix reaches both. Their fixtures move with them and become the
extensions' fixtures, as #951 plans.

### The stack profile

A profile is a JSON document each extension embeds in its runtime,
`profile.json` at the extension root, validated by the core's JSON Schema.
The Rust reference profile:

```json
{
  "id": "rust",
  "markers": ["Cargo.toml"],
  "detector": "cargo",
  "toolchain": {
    "command": ["rustc", "--version"],
    "pattern": "^rustc (?<version>\\S+)",
    "files": [
      { "path": "rust-toolchain.toml", "key": "toolchain.channel" },
      { "path": "rust-toolchain" }
    ]
  },
  "inputs": {
    "sources": ["src/**", "tests/**", "benches/**", "examples/**", "build.rs"],
    "manifests": ["Cargo.toml", "{root}/Cargo.toml", "{root}/.config/nextest.toml"],
    "lockfiles": ["{root}/Cargo.lock"]
  },
  "tasks": {
    "lint": {
      "steps": [
        ["cargo", "fmt", "--package", "{component}", "--check"],
        ["cargo", "clippy", "--package", "{component}", "--all-targets", "--", "-D", "warnings"]
      ],
      "inputs": ["sources", "manifests"]
    },
    "test": {
      "steps": [
        ["cargo", "llvm-cov", "nextest", "--package", "{component}", "--profile", "ci",
         "--lcov", "--output-path", "{output}/lcov.info"]
      ],
      "inputs": ["sources", "manifests", "lockfiles"],
      "reports": {
        "junit": ["{root}/target/nextest/ci/junit.xml"],
        "coverage": [{ "format": "lcov", "paths": ["{output}/lcov.info"] }]
      }
    },
    "build": {
      "steps": [["cargo", "build", "--package", "{component}", "--locked"]],
      "inputs": ["sources", "manifests", "lockfiles"]
    }
  }
}
```

| Member | Meaning |
|---|---|
| `id` | The stack id. |
| `markers` | The files that make a directory a candidate. |
| `detector` | The observe detector that names the components and their edges. |
| `toolchain.command`, `toolchain.pattern` | How to read the version on the machine: the command's output, standard output first, matched by the pattern's `version` group. |
| `toolchain.files` | The files that state the expected version, nearest first: in the project directory, then up to the stack root. `key` reads one value of a TOML or JSON file; without it, the file's first line. |
| `inputs.<set>` | Named sets of file patterns. A plain pattern is relative to the project directory; `{root}/` makes it relative to the stack root, the directory of the build that holds the component (the Cargo workspace, the Gradle settings). |
| `tasks.<task>.steps` | The commands the task runs in order, each an argument vector run in the project directory, with no shell. `{root}` is the stack root, `{component}` the detector's name for the component, `{output}` the task's output directory under the project. |
| `tasks.<task>.inputs` | The input sets the task reads. `lint` reads fewer than `test`: a lockfile change does not rerun it. |
| `tasks.<task>.reports` | The JUnit and coverage reports the task reads back, with the coverage format. The core copies each into `{output}` as soon as the task ends. |
| `tasks.<task>.outputs` | The paths under the project the task writes, cached and restored like any task output. |

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
        "test": { "steps": [["cargo", "nextest", "run", "--package", "{component}", "--profile", "ci"]] }
      },
      "inputs": { "sources": ["src/**", "fixtures/**"] }
    }
  }
}
```

A workspace sets values for every project through a scope's `putnami.json`,
with the same inheritance as every other option. An override is validated
against the profile schema; an unknown member fails the load.

### Inputs are input selectors

A task's inputs depend on its profile and on the project's overrides, so the
manifest cannot list them as static patterns. Each task names one selector,
`rust:lint`, `rust:test` or `rust:build`, in its `from: "project"` input. The
extension lists them in `runtime.inputSelectors`, and the core answers
`__putnami input-resolve` with the files the resolved profile selects for that
task, including the stack-root files and the toolchain files. Its basis covers
the project's `putnami.json` and every scope `putnami.json` above it, so an
override that changes the inputs changes the answer.

### What each extension holds

| File | Content |
|---|---|
| `putnami.extension.json` | The manifest: the runtime, the workspace adapter with the profile's markers, the `lint`, `test` and `build` commands and their tasks, the three input selectors. |
| `profile.json` | The stack profile, embedded in the runtime. |
| `cmd/putnami-<stack>/main.go` | `stacks.Main(profile)`. |
| `doc/`, `putnami.features.json`, `specs/` | The stack's own docs and requirements. |

The manifests repeat one shape. The core ships a test kit that every stack
extension runs: its manifest declares the three commands, their tasks and
selectors, and markers equal to its profile's; its profile passes the schema.
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

- the versions match, or nothing states an expected version: the task runs;
- they differ: the task fails with `stacks.toolchain_mismatch`, which names
  both versions and the file that states the expected one. It never installs
  or switches a toolchain. A project that accepts the difference sets
  `toolchain.check` to `warn`: the task runs and reports the diagnostic as a
  warning;
- the command is missing: the task fails with `stacks.toolchain_missing`.

So a stored result was produced by the version its key names, unless a
project chose `warn`. A project that states no expected version gets
`stacks.toolchain_unpinned`, a warning: its key carries no toolchain version,
and two machines with two versions share entries. A Rust channel name such as
`stable` is no exact version, and gets the same warning.

## Reports

After the last step, the task reads every report its profile names with the
evidence readers. A JUnit failure, an error, or a step that exits non-zero
fails the task; the report's failed cases name the failure. The totals and
the coverage reach the task result. A test task whose profile names a JUnit
path and finds no report fails with `stacks.report_missing`: a command that
ran nothing is not a pass. The copied reports are task outputs, so a cache hit
restores them and reads the same totals.

## What stays out

- **Publish and package.** A stack's release goes through its own tool
  (crates.io, Packagist, Maven Central); no profile declares a `publish`
  task. A stack extension may add one later, in its own issue.
- **Toolchain install.** The check compares; it never downloads, pins in the
  workspace lock, or switches a version. cargo-nextest and cargo-llvm-cov are
  the project's tools, like `rustc`.
- **Serve and qualify.** No profile declares a workload, so `putnami qualify`
  reports no reachable workload for a stack project.
- **Go, TypeScript and Python.** They keep their full extensions and do not
  move to the core.
- **Bazel and Pants.** They delegate (#945, #946): they keep their own cache.
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

The gate has no Rust toolchain, so the fixture puts a stub `cargo` and
`rustc` first on `PATH`: they print a version and write the reports a real
run writes. That double sits at a genuine external boundary, the stack's own
build tool, and the test names it. A second test, skipped where `cargo`,
cargo-nextest and cargo-llvm-cov are missing, runs the same scenario with the
real tools.

## Settled with the owner

1. The detectors become a public package of the observe module.
2. Rust is the reference stack.
3. A toolchain mismatch fails the task by default, `warn` by override.
4. One extension per stack, in its language root, on a shared core.
