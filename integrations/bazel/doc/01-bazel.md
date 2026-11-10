# Run a Bazel workspace with Putnami

`@putnami/bazel` turns each package of a Bazel workspace into a Putnami
project and runs its tests and build through Bazel. Putnami selects what a
change affects and gives the verdict; Bazel keeps its own cache and remote
execution, so Putnami caches nothing for these tasks.

## Adopt it

`@putnami/bazel` is not published yet: until its first release, `putnami init
--from bazel` stops at the extension install, after it has written the
workspace files, and a workspace loads the extension from a source checkout.

At the root of the workspace, where `MODULE.bazel`, `WORKSPACE` or
`WORKSPACE.bazel` lies:

```bash
putnami init --from bazel
```

`init` writes a workspace that declares `@putnami/bazel`, installs it, and
runs `putnami projects sync`. Bazel must be on `PATH`, or the CI's export must
be at the root as `bazel-graph.json`
(`bazel query 'deps(//...)' --output=streamed_jsonproto > bazel-graph.json`).

A package is a directory whose `BUILD.bazel` or `BUILD` file declares a rule
the export lists. Without the export, the extension does not parse `BUILD`
files: a package is a directory whose `putnami.json` the sync wrote from
`bazel query`. Every other directory with a `BUILD` file, and the root
package, is a bare member: a project named by its directory, with no
extension, that runs no task. Directories under `bazel-*` links, hidden
directories and `node_modules` are never read.

Add `.putnami/`, `.gen/` and `/bazel-*` to the repository's `.gitignore`:
`init` writes one only when the repository has none.

## What the sync writes

`putnami projects sync` runs the extension's sync task. It reads
`bazel-graph.json` at the root, or runs
`bazel query //... --output=streamed_jsonproto` when there is none, and writes
in each package's `putnami.json`:

| Member | Value |
|---|---|
| `extensions` | `@putnami/bazel`, beside any other extension the file names |
| `dependencies` | The project ID of each package one of its rules reads a label of, such as `/pricing`; a label of an external repository is no edge, and a dependency on a project with no `BUILD` file is kept |
| `options.@putnami/bazel.filePatterns` | The files at the root that every package reads, relative to the package: `MODULE.bazel`, `MODULE.bazel.lock`, `WORKSPACE`, `WORKSPACE.bazel`, `.bazelrc` and `.bazelversion`, those that exist, and the root `BUILD` file when the package reads a target of the root package. A change to one selects the package |
| `disable.jobs` | `test` when the export lists no test rule of the package (a class that ends in `_test`, or a `test_suite`), so a package without tests runs no test task; the sync owns this entry and removes it from a package that has a test rule |

It keeps every other member, and rewrites a file only when a member it owns
changes, with two-space indentation and sorted keys. A package that left the
export loses the extension and its Bazel edges. Without the export and without
`bazel`, or with an export that lists no package, the sync warns with
`bazel.sync_no_export` and writes nothing.

Run the sync again after a `BUILD` change that alters the dependencies
between packages, and commit the result. `putnami validate` compares each
`putnami.json` with the export and fails with `bazel.sync_stale` when a sync
would change one. Keep it in the CI gate.

While `bazel-graph.json` lies at the root, the sync and `putnami validate`
read it and never run `bazel query`, so a stale export hides a new edge. Do
not commit it: write it in the CI job that runs the sync and the gate.

## How Putnami judges the edges

The probe reports each package's edges as the Bazel graph's (`build-graph`):
those of the export, and each dependency of its `putnami.json` on a package or
another directory with a `BUILD` file, which is what the sync writes. Bazel
builds with them, so the declared-edge check counts them as real and warns no
`workspace.declared_edge` for them. Visibility does not judge them: Bazel's own
`visibility` attribute, not Putnami's scopes, says which package may depend on
which.

## What each task runs

Each task first compares `bazel --version` with `.bazelversion`, then runs one
command at the workspace root, with no shell. `//<package>:all` names the
rules of the package, not those of the packages under it.

| Task | Command |
|---|---|
| `test` | `bazel coverage --build_event_json_file=<output>/build-events.json //<package>:all` |
| `build` | `bazel build //<package>:all` |

Bazel declares no lint convention, so a Bazel package has no lint task.

After the test command, whatever its outcome, the task copies into its
command output the `test.xml` and `coverage.dat` of each test run the
build-event file lists, from the file the event names or else from
`bazel-testlogs/<package>/<target>/`, and reads them into the test and
coverage summary. A source file that several test runs cover counts each
line once, covered when any run covered it. A log that a removed target left in `bazel-testlogs` is no
run of this one, and is not read. A failed case, a command that exits
non-zero, or a run with no JUnit report fails the task; a test rule without
coverage support fails nothing.

## Bazel keeps its cache

Every task sets `cache.enabled` to `false`: Putnami stores no entry and
replays none, and each task result records `delegatedTo: bazel`. The task
reads Bazel's outputs and never removes one, so a test Bazel has cached
returns as fast as Bazel does.

Impact selection is Putnami's: `putnami test --impacted` runs Bazel on the
packages whose files changed and on the packages that depend on them, on the
edges the sync wrote. Each command runs a `graph` step before its task: the
`graph` step of a package depends on the `graph` step of each package it
depends on and runs nothing, and the task depends on its own package's `graph`
step alone. So a change to a package never runs Bazel on the packages it
depends on.

## The version check

| Case | Result |
|---|---|
| `bazel --version` prints the version `.bazelversion` states | The task runs |
| It prints another one | The task fails with `stacks.toolchain_mismatch`; set `toolchain.check` to `warn` in the package's options to run anyway |
| `bazel` is not on `PATH` | The task fails with `stacks.toolchain_missing` |
| The root holds no `.bazelversion` | The warning `stacks.toolchain_unpinned` |
| `.bazelversion` names a Bazelisk alias, such as `latest` or `rolling` | The warning `stacks.toolchain_unpinned`, and the task runs |

The extension never installs Bazel; Bazelisk on `PATH` as `bazel` reads
`.bazelversion` itself.
