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
`publish`) grows inside its own extension; no project changes. `@putnami/rust`
adds `run`, `serve`, `package` and `publish` this way
([ADR 0011](adr/0011-rust-runs-on-the-core-helpers-and-owns-its-packaging.md)).

### Projects come from the workspace probe

Each stack extension declares a workspace adapter (see the extension
protocol's [lifecycles](../../../protocols/extension/doc/07-lifecycles.md))
with its profile's markers: `Cargo.toml` for Rust.

- `putnami projects sync` finds the component directories through those
  markers and adds them to `includes`. Membership stays Putnami's.
- The probe (`__putnami workspace-probe`), served by the core, runs the
  extension's detector and answers, for each candidate path that the detector
  knows as a component: the source name (the crate name), the dependencies as
  paths, the extension, and in `metadata` the build's name of the component,
  its variant, the stack root and the expected toolchain versions. A component
  whose manifest names no package, such as a Composer application, keeps no
  source name, and its component name is its directory, the same on every
  checkout.

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
| `toolchain.fallback` | A command that lists the installed versions, run when `toolchain.command` fails: when none of them satisfies the expected version, the check is a mismatch that names them, so a provided toolchain may take its place. |
| `toolchain.provisioned` | The environment variable that holds the toolchain the workspace lock pins, for a stack extension that pins one. |
| `toolchain.path` | Directories, relative to the stack root, put first on `PATH`, in order, for the toolchain check and every step, offline too: the tools the repository installs for itself, such as `node_modules/.bin`. A directory that does not exist is left out; a pinned toolchain still comes before them ([ADR 0018](adr/0018-a-profile-puts-the-repository-tools-first-on-path.md)). |
| `tasks.<task>.withoutCoverage` | How the task runs on a toolchain with no coverage support: a `probe` that exits non-zero there, and the `steps` that replace the task's. Its coverage reports are not read, and the task warns with `stacks.coverage_unavailable`. |
| `toolchain.files` | The files that state the expected version or range, read in the project directory, then at the stack root. `key` reads one string value of a TOML table, a JSON object or, in a `.yaml` or `.yml` file, a YAML block or flow mapping; `pattern`, a regular expression matched against the file's content, reads its `version` group, such as `^(?:ruby-)?(?<version>\S+)` for `ruby-3.3.6`; without either, the file's first non-empty line. |
| `toolchain.ranges` | How a range a file states reads: Composer's syntax, or `pub-sdk` and `pub-flutter`, as pub reads a Dart or Flutter SDK constraint ([ADR 0013](adr/0013-task-targets-exclusions-and-pub-ranges.md)). `toolchain.version` reads plain. |
| `toolchain.also` | Further checks with the same members but `also` and `path`, such as the Dart SDK a Flutter SDK bundles or the Bundler beside Ruby, run with the selected toolchain. An entry without `check` takes the first check's. |
| `inputs.<set>` | Named sets of file patterns. A plain pattern is relative to the project directory; `{root}/` makes it relative to the stack root, the directory of the build that holds the component (the Cargo workspace, the Gradle settings); a pattern that starts with `!` leaves its files out of the task's inputs, such as a file the build tool writes under a directory the task reads or `!vendor/**` beside `**/*.rb`, never `putnami.json` or a toolchain file. A stack-root exclusion, such as `!{root}/**/obj/**`, reaches the manifest's workspace input with the other stack-root patterns. |
| `tasks.<task>.steps` | The commands the task runs in order, each an argument vector run in the directory `dir` names, with no shell. `{root}` is the stack root, `{component}` the build's name for the component (`Component.Ref`, or its name), `{manifest}` the absolute path of the manifest the detector read it from, `{output}` the task's output directory under the project, `{target}` one of the task's targets. |
| `tasks.<task>.inputs` | The input sets the task reads. `lint` reads fewer than `test`: a lockfile change does not rerun it. |
| `tasks.<task>.reportSteps` | Steps that run after the task's steps whatever their outcome, such as a converter that writes the JUnit report a failing test runner leaves to it, so the failed cases name the failure. They name no `{target}`, and may name `{runtime}`. |
| `tasks.<task>.reports` | The JUnit (`junit`) and TRX (`trx`) test reports and the coverage reports the task reads back, with the coverage format. The core copies each into `{output}` as soon as the task ends. `when`, `optional` and `warnWhenMissing` say when a report is expected. |
| `tasks.<task>.targets` | What a step that names `{target}` runs on, listed in the project directory when the task runs: a `name`, or one target per file an `each` pattern matches, named by its base name without extension; `when` lists paths that must all exist, `hosts` the systems (`linux`, `darwin`, `windows`) that build it. A step that names `{target}` runs once per target, and not at all when there is none; it needs the task to declare targets, and each target's paths are in the task's inputs. |
| `tasks.<task>.artifacts` | The files the task copies into `{output}/artifacts` after its steps, so a cache hit restores them: patterns under the project or the stack root that may name `{component}`. The file a pattern with no wildcard names is removed before the steps, unless its path leads outside the project or the stack root or the inputs of one of the component's tasks select it, so only what the steps write is copied. A pattern with a wildcard is only copied, and copies whatever it matches, a file an earlier build left included. A component name that is no single path segment keeps no artifact. A pattern that matches nothing copies nothing. |
| `variants.<name>` | A part of the profile one build runs, applied before the project's override. |
| `tasks.<task>.dir` | Where the steps run: `project`, the default, or `root`, the stack root, for a tool that resolves its addresses from there. |
| `delegated` | `true` for a stack whose build tool keeps its own cache; see [Delegated stacks](#delegated-stacks). An override cannot change it. |

The toolchain files are inputs of every task, whatever its input sets. Every
task names at least one input set.

### A profile adapts to each component

A Ruby component tests with RSpec, with Minitest, or not at all: the Ruby
detector names its variant, `rspec`, `minitest` or `untested`, as Java's
names a build. In a variant or an override, `null` removes a member, such as
a task, which then passes with nothing to run
([ADR 0009](adr/0009-a-profile-adapts-to-each-component.md)).

A task with `dir` set to `root` runs its steps at the stack root: a packwerk
pack's specs load the application's `.rspec` and `spec_helper`, and run from
the directory of the `Gemfile`, with the pack's directory as an argument.
One coverage report at that root then holds every file the run loaded, and
the attribution counts only the component's own: it reads a relative path
under a source root the report states, else under the directory the steps
ran in, the project directory when only the project holds the file. A
stack whose every directory holding a marker is a component, such as Ruby,
sets `Stack.IsComponent`, so the attribution asks it instead of running the
detector, which may read the whole repository, for each such directory a
report reaches.

### Targets

A Flutter app builds one artifact per platform directory it holds, and a Dart
package one executable per `bin/*.dart`. A task lists its targets when it
runs, from the project directory and the host system, rather than from the
probe, which is not rerun when a directory appears. The files a target needs
are task inputs, so a target that appears or disappears changes the key. A
task whose targets name `hosts` declares the `hostPlatform` runtime input, so
the host system reaches its key; the conformance kit holds it, and an
override cannot add `hosts` to another task.
([ADR 0013](adr/0013-task-targets-exclusions-and-pub-ranges.md)).

### What a stack shapes beyond its profile

A stack whose components differ in kind, or whose toolchain has its own
version rule, says so in its extension's Go code
([ADR 0010](adr/0010-a-stack-shapes-its-components-tasks-and-versions.md)):

- the detector names the tasks each component runs (`Component.Tasks`): a
  task outside the list passes with no step and no toolchain check. A .NET
  library runs no `dotnet test`; without a list, every task runs. The probe
  records the list, and the task asks the detector again when it runs, so a
  file that decides it and that the key covers decides it fresh;
- the detector names the other files it read for a component
  (`Component.Watched`), such as each `Directory.Build.props` above a .NET
  project; the probe reports them, with the component's manifest, among its
  watched files;
- `Stack.MatchVersion` replaces the core's rule for an exact expected
  version: the .NET stack applies `global.json`'s `rollForward`;
- a root step lists the absolute manifests it runs for
  (`RootStep.Manifests`): the job's component, or, in a workspace job, every
  component of the workspace at that stack root;
- a root step names its component (`RootStep.Component`), empty at a stack
  root that holds none, and the version its first toolchain check expects
  (`RootStep.Expected`), so a job of the extension installs the toolchain the
  tasks select;
- `Stack.SelectorSets` exposes input sets as selectors of their own, see
  [Inputs are input selectors](#inputs-are-input-selectors).

Several components share one stack root: one Cargo workspace, one
`target/`, one `target/nextest/ci/junit.xml`. The core runs the tasks of one
stack root one at a time, under a lock it holds on that root, and copies the
reports out before it releases the lock, so two crates never read each
other's report. Cargo already serializes on its own build directory, so the
lock costs little.

### Variants: a stack with several builds

A stack whose components run on different builds, such as Java on Gradle,
Android and Maven, declares `variants` in its profile: named parts of the
profile, with its shape and every member optional, merged into it like an
override ([ADR 0005](adr/0005-profile-variants-for-a-stack-with-several-builds.md)).
The detector names each component's variant; the probe records it in the
metadata, and the job and the selectors resolve the profile, then the
variant, then the project's override. With variants, the profile may declare
no task of its own. A task's stack-root input in the manifest is the union of
its variants' stack-root files.

A component also has a reference apart from its name: `Component.Ref`, the
build's own name, which steps receive as `{component}`, such as Gradle's task
path prefix `:api:`. `Component.Name` stays the project's source name, and
may be empty so the project keeps its directory's name.

A task's `reports.when` names project files: when none exists, the task
expects no report, so a module without tests passes. A coverage report marked
`optional` is read when it exists and fails nothing when it does not, and the
summary carries no coverage. One that only the project's own setup produces,
such as Coverlet's or SimpleCov's, also declares `warnWhenMissing`, and its
absence warns with `stacks.coverage_unavailable`
([ADR 0015](adr/0015-the-stack-extensions-share-one-command-surface.md)).

### Nested markers, excluded directories and runtime commands

[ADR 0007](adr/0007-nested-markers-excludes-and-runtime-commands.md) adds
three members a stack needs when its components have no fixed layout or its
tools write to standard output:

| Member | Meaning |
|---|---|
| `markers` | A marker may name a file one directory down with a wildcard directory: `*.xcodeproj/project.pbxproj`. |
| `excludes` | Directory names an input pattern with `**` never walks into: `.build`, `DerivedData`. |
| `{runtime}` | In a step: the extension's own executable, which runs a command the extension registers in `Stack.Commands`. |

When the detector names two components at one path, the first is the project,
and the probe reports the other with `stacks.component_conflict`.

### A project overrides any value

A project overrides its profile in its `putnami.json`, under its stack
extension's option namespace. An object merges, `null` removes the value at
its path, and any other value replaces it whole; an override never merges
into an array.

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
decoder: an unknown member, a change to `id`, `markers` or `variants`, a
stack-root input the manifest does not declare, a target whose paths are in
none of the task's inputs, a step that names `{target}` in a task without
targets, or a target with `hosts` in a task whose manifest declares no
`hostPlatform` input fails the task with `stacks.project_config_invalid`. Scope-level overrides are left for later.

### Inputs are input selectors

A task's inputs depend on its profile and on the project's overrides, so the
manifest cannot list them as static patterns. Each task names one selector,
`rust:lint`, `rust:test` or `rust:build`, in its `from: "project"` input. The
extension lists them in `runtime.inputSelectors`, and the core answers
`__putnami input-resolve` with the files the resolved profile selects for that
task, and the project's `putnami.json`. Its basis covers them, so an
override that changes the inputs changes the answer.

A stack may also expose an input set as a selector of its own, through
`Stack.SelectorSets`, for a job the extension runs itself. `@putnami/rust`
does this for `rust:package`, the whole crate less its `target/` directory and
the `.gen/version.json` stamp, because a released binary may embed any file of
the crate. A walk does not enter a directory an exclusion of the form
`!<dir>/**` leaves out, so the build directory is never read.

A selector answers files under the project only, never under a `.git` or
`.putnami` directory. A delegated stack's selector also leaves out every
`.gen` directory, at any depth and whatever its patterns name: a delegated
task feeds no Putnami cache and `.gen` is never an impact seed, so a file
there is state the run writes, and the build stamp a finished task writes in
`.gen/version.json` would otherwise move the selector's basis
([ADR 0008](adr/0008-a-delegated-stack-keeps-its-tool-cache.md)). A language
stack keeps `.gen`, since a generated client there is an input. The
stack-root files (`{root}/…`, and the toolchain files at the root) reach the
key through a second, static input of each task, `from: "workspace"`, which
lists the shipped profile's stack-root patterns of that task; the conformance
kit holds the two equal. So an override may narrow the stack-root inputs but not add
one.

### A nested stack root is an input-only project

A component whose stack root is neither the workspace root nor its own
directory, such as a crate of a Cargo workspace under `services/billing`,
reads the root's files through an edge
([ADR 0012](adr/0012-a-nested-stack-root-is-an-input-only-project.md)). The
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
| `putnami.extension.json` | The manifest: the runtime, the workspace adapter with the profile's markers, a command, a task and an input selector for each of `lint`, `test` and `build` the profile declares, and the jobs the extension adds in its own code, such as `serve`, `run`, `package` and `publish`. |
| `profile.json` | The stack profile, embedded in the runtime. |
| `cmd/putnami-<stack>/main.go` | `Stack.Entrypoint` of the stack the extension builds from its name, its profile and its detector: the observe detector that names the components and their edges. |
| `doc/`, `putnami.features.json`, `specs/` | The stack's own docs and requirements. |

The manifests repeat one shape. The core ships a test kit that every stack
extension runs: its manifest declares a command, a task and a selector for
each of `lint`, `test` and `build` its profile declares, and no command for
one the profile does not declare, as `build` for Ruby and PHP; its markers
equal its profile's; its profile passes the decoder. A stack extension that
drifts from the core fails its own gate.

## The cache key

A task's key is the core's key (see the CLI's
[caching](../../cli/doc/10-caching.md)) over what the task declares:

| Part of the core key | What it carries for a stack task |
|---|---|
| Implementation digest | The extension runtime, which embeds the core and the profile: editing either changes every key of the stack. |
| Task contract digest | The manifest's task. |
| Task params hash | The resolved `options["@putnami/<stack>"]` of the project, so every override. |
| Project metadata digest | The build's name of the component, its variant, the stack root, the manifest, the tasks it runs and the expected toolchain versions from the probe. |
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
  warning. The same member in the extension's block of
  `putnami.workspace.json` options sets it for every project, and a project's
  own `putnami.json` outranks it;
- the command is missing: the task fails with `stacks.toolchain_missing`.

An extension may pin its toolchain, as `@putnami/php` and `@putnami/ruby` do
([ADR 0003](adr/0003-a-stack-extension-may-provide-its-toolchain.md)). The
profile's `toolchain.provisioned` names the environment variable that holds
the pinned toolchain. When the toolchain on `PATH` is missing, or fails the
check, the task puts the pinned one first on `PATH` and checks it. A toolchain
on `PATH` that passes always runs. `Stack.RootJob` gives an extension a job
that runs at the stack root with the same toolchain and under the same lock,
such as an install step.

An extension may instead install its toolchain when a task needs it, as
`@putnami/rust` and `@putnami/swift` do
([ADR 0004](adr/0004-a-stack-extension-may-install-its-toolchain-at-task-time.md)).
`Stack.Provide` runs when the toolchain on `PATH` is missing or fails the check,
for a resolved profile that names `toolchain.provisioned`: a variant that names
none, such as Swift's Xcode project, keeps its check only, and a workspace job
leaves its component out when its toolchain is missing. `Stack.Tools` returns directories appended to the task's `PATH`. An offline
run calls neither. `Provide` receives the component's resolved toolchain and
the version each of its checks expects, so a stack with variants, such as Dart
and Flutter, installs the one the component runs; a further check that fails
calls it too, and the task then runs the installed toolchain. A workspace job
counts a toolchain the stack installs as present when the run is not offline,
and runs at each stack root with a component of that root, before its
input-only project, so the root's tasks and its workspace job select the same
toolchain.
A step's program is the pinned toolchain or a program beside it, then a
program beside the toolchain on the task's `PATH`, such as a Flutter SDK's
`dart`, then the first of that name on the task's `PATH`, where the
directories of `Tools` are. `Stack.Step` adapts each step of a task before it
runs, with its placeholders expanded and the toolchain selected, offline too:
it may name another program, such as the release of a build tool the build
states, and add arguments; its error fails the task with
`stacks.toolchain_missing`, and `RunWith` and `RunFiltered` read the adapted
last step
([ADR 0014](adr/0014-a-stack-adapts-each-step-of-a-task.md)). `Stack.Env`
answers `NAME=VALUE` entries for the steps of a task and of the extension's
jobs that select the component's toolchain, offline too, such as the home of
an SDK the build reads from the environment. It reads an `EnvRequest`: the
task, the workspace root, the component's metadata, the task's environment,
the extension's block of the workspace's options and whether the run is
offline. Its error, or an entry that names no variable, fails the task with
`stacks.toolchain_missing`
([ADR 0016](adr/0016-a-stack-sets-the-environment-of-a-task.md)). `Stack.Selected`,
`RunForeground`, `AnnounceReady` and `FreePort` serve a stack's `run` and
`serve` commands. `ImageName`, `ImageMember` and `ImagePort` serve a stack
that packages an image: the image is the project's name as one lowercase path segment, the
name the shared Docker publisher expects, so a project named `userService`
packages `userservice`; `ImageMember` declares that image as a release-set
member when the project's `putnami.json` publishes it; `ImagePort` reads the
port its image listens on, 8080 by default. Its `imageload`
package loads the image into the local Docker daemon under each of its tags;
only the stacks that package an image import it.
`LockedToolsHome`, `ExecutableSuffix`, `CompareVersions`, `Accepted` and
`Choose` serve a stack that installs its toolchain, and
`ServeToolchainResolve` one that pins it in the workspace lock. The test kit
`stackstest` gives every stack extension `CheckManifest`, `CheckSurface`, which
holds a language stack's manifest to the command surface the stacks share
([ADR 0015](adr/0015-the-stack-extensions-share-one-command-surface.md)), with
a named exemption for each stated reason, and `WriteFile`, and
its `imagetest` package `Registry`, an in-memory registry that serves a
two-platform base image. A test binary that calls `Registry` first calls
`dockertest.IsolateConfig` of the extension SDK in its `TestMain`; `Registry`
fails the test otherwise.

A file may state a range instead, in Composer's constraint syntax: the PHP
profile reads `require.php` of `composer.json`, such as `>=8.4` or `^8.2`
([ADR 0002](adr/0002-a-stated-version-range-is-checked.md)). The task runs
when the machine's numeric release is in the range and fails with
`stacks.toolchain_mismatch` otherwise, unless `toolchain.check` is `warn`. The
key carries the range, so two machines in it share entries; a profile lists
an exact file first, such as `.php-version`, so an exact statement wins. A
Dart or Flutter SDK constraint in `pubspec.yaml` reads as pub reads it, under
`toolchain.ranges`, and `toolchain.also` adds a further check, such as the
Dart SDK a Flutter SDK bundles
([ADR 0013](adr/0013-task-targets-exclusions-and-pub-ranges.md)).

So a stored result was produced by the versions, or versions of the ranges,
its key names, unless a project chose `warn`. A project that states no
expected version for any check gets `stacks.toolchain_unpinned`, a warning: its key carries
no toolchain version, and two machines with two versions share entries. A
Rust channel name such as `stable` is neither a version nor a range, and gets
the same warning.

A further check, such as the Bundler beside Ruby, is read with the selected
toolchain: a pinned Ruby's own `bundle`. The probe records its expected
version under `toolchains` in the project metadata, in the order of
`toolchain.also`, so it reaches the key too.

## Reports

The task's output directory is emptied before its first step. After the
last step, the task reads every report its profile names with the evidence
readers, JUnit and TRX alike, and attributes each entry to a component:

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
A source file that several coverage reports name is merged: each line
counts once, covered when any report covered it, and a file a report states
by totals alone keeps the largest totals.
A JUnit failure, an error, or a step that exits non-zero fails the task; the
report's failed cases name the failure, with `stacks.test_failed`, and a failed
step that wrote no report is `stacks.step_failed`, not a missing report. The totals and
the coverage reach the task result. A JUnit or coverage pattern of the profile
that matches no report fails the task with `stacks.report_missing`; JUnit
and TRX patterns count together: a command
that ran nothing is not a pass. A test filter an extension passes through
`RunFiltered` is the one exception: the filter also reaches the components the
selected ones depend on, where it may select no test, and a runner writes no
report for such a run, so a filtered run whose steps pass and that finds no
JUnit or TRX report reads zero tests and no coverage. The reports a task reads outside its own
output directory are removed before its steps, so a report another component
or an earlier run left is never read as this run's. A file the task's inputs
select is a source and is kept, and a report pattern outside the output
names one place: no `**` and no wildcard first directory. The copied reports are task outputs, so a cache hit
restores them and reads the same totals.

## Delegated stacks

A build system with its own hermetic cache, Pants or Bazel, is delegated to:
its profile sets `"delegated": true`, and Putnami runs the tool on each
affected component instead of importing its targets
([ADR 0008](adr/0008-a-delegated-stack-keeps-its-tool-cache.md)).

- Every task of the extension's manifest sets `cache.enabled` to `false`, and
  the conformance kit holds the manifest to it: Putnami stores no entry and
  replays none, and the tool's cache decides what reruns.
- Each task result records `delegatedTo`, the stack id, beside its test and
  coverage summary.
- A task whose `dir` is `root` runs its steps in the stack root, where the
  tool resolves its addresses.
- Each command runs a `graph` step, then its task. The `graph` step depends
  on the `graph` step upstream and runs nothing; the task depends on its own
  `graph` step alone. Impact selection stays Putnami's: a change runs the
  tool on its component and the components that depend on it, never on the
  components it depends on.
- A detector may answer a candidate directory as bare: a member named by its
  directory, with no extension, that runs no task.
- The package graph is the tool's: `GraphSync` writes each package's
  `putnami.json` from the tool's export (the extension, the edges as project
  IDs, the files that select it as `filePatterns`, and `test` in
  `disable.jobs` without a test target), and retracts a former package.
  `GraphJobs` runs it as the workspace sync task, and as a validate task that
  fails with `<tool>.sync_stale` when a sync would change a file.
- A report step may name `{runtime}`: a tool whose outputs it caches, such as
  Bazel's test logs, keeps them, and the extension's runtime copies the run's
  reports into `{output}`, which the core never reads from another run.

A delegated stack is an integration with a third-party tool, so it lives in
`integrations/<tool>`, not in a language root (D-063).
`@putnami/pants` and `@putnami/bazel` delegate alone. `@putnami/nx`,
`@putnami/moon`, `@putnami/turbo` and `@putnami/rush` delegate too, and also
offer import mode: each has a sync of its own that reads the adoption mode
and, in import mode, moves to a Putnami language extension what that
extension can run. Nx and Turborepo move a TypeScript project's task whose
script already runs `bun build`, `bun test` or Biome to `@putnami/typescript`;
moon moves each project whose language has a Putnami extension to it; Rush
moves every project to `@putnami/typescript`. What stays delegated runs the
tool, so no Putnami cache sits over the tool's.

## What stays out

- **Serve, run, package and publish in the profile.** No profile declares
  these tasks or a workload: each stack starts its programs and ships its
  artifacts its own way, so each extension adds them as jobs in its own code.
  All seven stacks serve and run their applications on helpers the core
  shares (`RunForeground`, `AnnounceReady`, `FreePort`). `serve` reports the server ready once its port accepts
  connections, so `putnami qualify` and compose can wait on it. Rust, Java,
  Swift, .NET, Ruby and PHP package an application's image and publish it
  through the shared Docker publisher, and Rust also packages release
  archives and publishes its crates
  ([ADR 0011](adr/0011-rust-runs-on-the-core-helpers-and-owns-its-packaging.md)).
  Dart packages nothing, and a library of the other stacks reaches its
  registry through its own tool, such as Packagist or Maven Central.
- **Go, TypeScript and Python.** They keep their full extensions and do not
  move to the core.
- **Bazel and Pants as cached stacks.** They delegate: they keep their own
  cache, see [Delegated stacks](#delegated-stacks).
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
