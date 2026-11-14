# Run a Pants repository with Putnami

`@putnami/pants` turns each package of a Pants repository into a Putnami
project and runs its lint, test and build through Pants. Putnami selects what
a change affects and gives the verdict; Pants keeps its own cache and remote
execution, so Putnami caches nothing for these tasks.

## Adopt it

`@putnami/pants` is not published yet: until its first release, `putnami init
--from pants` stops at the extension install, after it has written the
workspace files, and a workspace loads the extension from a source checkout.

At the root of the repository, where `pants.toml` lies:

```bash
putnami init --from pants
```

`init` writes a workspace that declares `@putnami/pants`, installs it, and
runs `putnami projects sync`. Pants must be on `PATH`, or the CI's export must
be at the root as `pants-graph.json` (`pants peek :: > pants-graph.json`).

A package is a directory whose `BUILD` or `BUILD.pants` file declares a
first-party target. Every other directory with a `BUILD` file, one that
declares only third-party targets such as `python_requirements` or only
`__defaults__`, and the repository root, is a bare member: a project named by
its directory, with no extension, that runs no task. The sync writes an edge
from each package to the third-party directories it depends on, so a change to
`3rdparty/python/requirements.txt` or to a resolve lockfile runs the packages
that use them.

When two `BUILD` directories share a directory name, such as `src/foo` and
`tests/foo`, `putnami projects sync` names each of them by its path.

Add `.putnami/` and `.gen/` to the repository's `.gitignore`: `init` writes one
only when the repository has none.

## What the sync writes

`putnami projects sync` runs the extension's sync task. It reads
`pants-graph.json` at the root, or runs `pants peek ::` when there is none, and
writes in each package's `putnami.json`:

| Member | Value |
|---|---|
| `extensions` | `@putnami/pants`, beside any other extension the file names |
| `dependencies` | The project ID of each package, and of each third-party directory, the export says it depends on, such as `/src/python/pricing` and `/3rdparty/python`; a dependency on a project with no `BUILD` file is kept |
| `options.@putnami/pants.filePatterns` | The resolve lockfiles and the files that declare the package's third-party targets, relative to the package, and the root `BUILD` file when it uses a target of the root. A change to one selects the package, wherever the file lies. A package with no third-party target has none |
| `disable.jobs` | `test` when none of the package's targets is a test target (a type that ends in `_test` or `_tests`, or a `go_package` with a `_test.go` source), so a package without tests runs no test task; the sync owns this entry and removes it from a package that has a test target |

A directory reached through a symbolic link is the directory it resolves to:
Pants lists a linked tree under both paths, and the sync writes its packages
once, under their own paths, with each edge to the link pointing at the
resolved directory.

It keeps every other member, and rewrites a file only when a member it owns
changes, with two-space indentation and sorted keys. A package that left the
export loses the extension and its Pants edges. Without the export and without
`pants`, or with an export that lists no package, the sync warns with
`pants.sync_no_export` and writes nothing.

Run the sync again after any change to the dependencies between packages, and
commit the result. An `import` is such a change: Pants infers the edge from it
with no change to a `BUILD` file. `putnami validate` compares each
`putnami.json` with the export and fails with `pants.sync_stale` when a sync
would change one. Keep it in the CI gate.

## How Putnami judges the edges

The probe reports each package's edges as the Pants graph's (`build-graph`):
those of the export, and each dependency of its `putnami.json` on a package or
another directory with a `BUILD` file, which is what the sync writes. Pants
builds with them, so the declared-edge check counts them as real and warns no
`workspace.declared_edge` for them. A dependency you add by hand on a
directory with no `BUILD` file is a declaration: the check reports it unless a
file input of the package reads inside that directory.

Visibility does not judge a Pants edge. A package's `putnami.json` makes it a
scope for the packages nested under it, and Pants's own graph, not Putnami's
scopes, says which package may depend on which.

## What each task runs

Each task first compares `pants --version` with `GLOBAL.pants_version` in
`pants.toml`, then runs one command at the workspace root, with no shell.
`<dir>:` names the targets of the package's directory and those they generate,
not those of the packages under it.

| Task | Command |
|---|---|
| `lint` | `pants lint <dir>:` |
| `test` | `pants --test-report --test-use-coverage --coverage-py-report=["xml"] test <dir>:` |
| `build` | `pants package <dir>:` |

The list form `["xml"]` replaces the `[coverage-py] report` list of
`pants.toml` rather than appending to it, so a repository that already lists
`xml` there still gets its one Cobertura report.

The test task then reads the JUnit reports under `dist/test/reports/` and the
Cobertura report `dist/coverage/python/coverage.xml` into the test and
coverage summary. Those reports are removed before the command runs, and the
packages of one repository run one task at a time, so a package never reads
another's report. A failed case, a command that exits non-zero, or a missing
report fails the task.

The shipped test command reads Python coverage. A repository without the
Python backend overrides the test steps and states `"coverage": []` in each
package's `putnami.json`:

```json
{
  "options": {
    "@putnami/pants": {
      "tasks": {
        "test": {
          "steps": [["pants", "--test-report", "test", "{component}:"]],
          "reports": { "junit": ["{root}/dist/test/reports/*.xml"], "coverage": [] }
        }
      }
    }
  }
}
```

## Pants keeps its cache

Every task sets `cache.enabled` to `false`: Putnami stores no entry and
replays none, and each task result records `delegatedTo: pants`. A task that
Pants has cached returns as fast as Pants does.

Impact selection is Putnami's: `putnami test --impacted` runs Pants on the
packages whose files changed and on the packages that depend on them, on the
edges the sync wrote. Each command runs a `graph` step before its task: the
`graph` step of a package depends on the `graph` step of each package it
depends on and runs nothing, and the task depends on its own package's `graph`
step alone. So a change to a package never runs Pants on the packages it
depends on.

## The version check

| Case | Result |
|---|---|
| `pants --version` prints the version `pants.toml` states | The task runs |
| It prints another one | The task fails with `stacks.toolchain_mismatch`; set `toolchain.check` to `warn` in the package's options to run anyway |
| `pants` is not on `PATH` | The task fails with `stacks.toolchain_missing` |
| `pants.toml` states no `pants_version` | The warning `stacks.toolchain_unpinned` |

The extension never installs Pants.
