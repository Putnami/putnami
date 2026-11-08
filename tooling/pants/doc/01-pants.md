# Run a Pants repository with Putnami

`@putnami/pants` turns each package of a Pants repository into a Putnami
project and runs its lint, test and build through Pants. Putnami selects what
a change affects and gives the verdict; Pants keeps its own cache and remote
execution, so Putnami caches nothing for these tasks.

## Adopt it

At the root of the repository, where `pants.toml` lies:

```bash
putnami init --from pants
```

`init` writes a workspace that declares `@putnami/pants`, installs it, and
runs `putnami projects sync`. Pants must be on `PATH`, or the CI's export must
be at the root as `pants-graph.json` (`pants peek :: > pants-graph.json`).

A package is a directory whose `BUILD` or `BUILD.pants` file declares a
first-party target. A directory that declares only third-party targets, such
as `python_requirements`, and the repository root are no project.

## What the sync writes

`putnami projects sync` runs the extension's sync task. It reads
`pants-graph.json` at the root, or runs `pants peek ::` when there is none, and
writes in each package's `putnami.json`:

| Member | Value |
|---|---|
| `extensions` | `@putnami/pants`, beside any other extension the file names |
| `dependencies` | The project ID of each package the export says it depends on, such as `/src/python/pricing`; a dependency on a project that is no Pants package is kept |
| `disable.jobs` | `test` when none of the package's targets is a test target (a type that ends in `_test` or `_tests`, or a `go_package` with a `_test.go` source), so a package without tests runs no test task |

It keeps every other member, writes a file only when its content changes, and
writes nothing when it has neither the export nor `pants`. Run the sync again
after a change to the dependencies between packages.

## What each task runs

Each task first compares `pants --version` with `GLOBAL.pants_version` in
`pants.toml`, then runs one command at the workspace root, with no shell.
`<dir>:` names the targets of the package's directory and those they generate,
not those of the packages under it.

| Task | Command |
|---|---|
| `lint` | `pants lint <dir>:` |
| `test` | `pants --test-report --test-use-coverage --coverage-py-report=xml test <dir>:` |
| `build` | `pants package <dir>:` |

The test task then reads the JUnit reports under `dist/test/reports/` and the
Cobertura report `dist/coverage/python/coverage.xml` into the test and
coverage summary. Those reports are removed before the command runs, and the
packages of one repository run one task at a time, so a package never reads
another's report. A failed case, a command that exits non-zero, or a missing
report fails the task.

The shipped test command reads Python coverage. A repository without the
Python backend overrides the test steps and reports in each package's
`putnami.json`:

```json
{
  "options": {
    "@putnami/pants": {
      "tasks": {
        "test": {
          "steps": [["pants", "--test-report", "test", "{component}:"]],
          "reports": { "junit": ["{root}/dist/test/reports/*.xml"] }
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

Impact selection is Putnami's: `putnami test --impacted` runs the packages
whose files changed and the packages that depend on them, on the edges the
sync wrote.

## The version check

| Case | Result |
|---|---|
| `pants --version` prints the version `pants.toml` states | The task runs |
| It prints another one | The task fails with `stacks.toolchain_mismatch`; set `toolchain.check` to `warn` in the package's options to run anyway |
| `pants` is not on `PATH` | The task fails with `stacks.toolchain_missing` |
| `pants.toml` states no `pants_version` | The warning `stacks.toolchain_unpinned` |

The extension never installs Pants.
