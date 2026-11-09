# Adopt Putnami on an existing monorepo

You will put Putnami's runner, impact selection and verdict on a repository
that a monorepo tool already describes, without rewriting its build. One
command reads the tool's packages and their dependencies and turns each
package into a Putnami project; `putnami lint,test,build --impacted` then runs
what a change affects.

Putnami adopts a tool in one of two modes:

- **Delegate**: a build system with its own hermetic cache keeps it. Putnami
  runs the tool on each affected package and reads its reports, and caches
  nothing for those tasks.
- **Import**: a task runner's projects become Putnami projects, and Putnami's
  runner and cache take over its tasks.

| Tool | Mode | Command |
|---|---|---|
| Pants | Delegate | `putnami init --from pants` |
| Bazel | Delegate | `putnami init --from bazel` |

## Pants

Pants keeps its cache and its remote execution: each Putnami task runs
`pants` on one package directory.

### Before you start

- `@putnami/pants` is not published yet: until its first release,
  `putnami init --from pants` stops at the extension install.
- Pants is on `PATH`, at the version `pants.toml` states in
  `GLOBAL.pants_version`.
- `pants.toml` lies at the root of the repository, which becomes the
  workspace root.

### Turn the repository into a workspace

At the repository root:

```bash
putnami init --from pants
```

`init` declares `@putnami/pants`, installs it, and synchronizes the projects.
The sync runs `pants peek ::` and writes, in each package's `putnami.json`,
the extension and the packages it depends on:

```json
{
  "$schema": "https://putnami.dev/schemas/putnami-project.json",
  "extensions": ["@putnami/pants"],
  "dependencies": ["/3rdparty/python", "/src/python/pricing"]
}
```

A package is a directory whose `BUILD` file declares a first-party target.
`3rdparty/python`, which only declares requirements, joins the workspace as a
bare member that runs nothing; each package that uses its requirements depends
on it. A package with no test target also gets
`"disable": { "jobs": ["test"] }`. Add `.putnami/` and `.gen/` to
`.gitignore`, then commit the files: they are the package graph Putnami
selects on. Run `putnami projects sync` again after a change to the
dependencies between packages.

In a CI without Pants on the machine that syncs, write the export first and
the sync reads it instead:

```bash
pants peek :: > pants-graph.json
putnami projects sync
```

### Run what a change affects

```bash
putnami test --impacted
```

After a change to `src/python/pricing`, Putnami runs `pricing` and the
packages that depend on it, `api` here, each with one command at the root:

```text
pants --test-report --test-use-coverage --coverage-py-report=xml test src/python/pricing:
pants --test-report --test-use-coverage --coverage-py-report=xml test src/python/api:
```

A change to `api` alone runs `api` alone: Putnami never runs Pants on the
packages a changed package depends on.

Each task reads the JUnit reports under `dist/test/reports/` and the coverage
report `dist/coverage/python/coverage.xml` into its test and coverage summary,
and its result records `delegatedTo: pants`. `putnami lint` runs `pants lint`
and `putnami build` runs `pants package` the same way. Putnami stores no cache
entry for them: a package Pants has cached returns as fast as Pants does.

A change to third-party requirements or to a resolve lockfile runs the
packages that depend on them, wherever those files lie.

An `import` can change the edges with no change to a `BUILD` file, because
Pants infers them. `putnami validate` fails with `pants.sync_stale` when the
edges no longer match Pants: run `putnami projects sync` and commit the result.

### When the repository is not Python

The shipped test command reads Python coverage. Without the Python backend,
override the test task and its reports in each package's `putnami.json`:

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

## Bazel

Bazel keeps its cache and its remote execution: each Putnami task runs
`bazel` on one package.

### Before you start

- `@putnami/bazel` is not published yet: until its first release,
  `putnami init --from bazel` stops at the extension install.
- Bazel is on `PATH`, at the version `.bazelversion` states. Without
  `.bazelversion`, each task warns that the version is not pinned.
- `MODULE.bazel`, `WORKSPACE` or `WORKSPACE.bazel` lies at the root of the
  repository, which becomes the workspace root.

### Turn the repository into a workspace

At the repository root:

```bash
putnami init --from bazel
```

`init` declares `@putnami/bazel`, installs it, and synchronizes the projects.
The sync runs `bazel query //...` and writes, in each package's
`putnami.json`, the extension and the packages it depends on:

```json
{
  "$schema": "https://putnami.dev/schemas/putnami-project.json",
  "extensions": ["@putnami/bazel"],
  "dependencies": ["/pricing"]
}
```

A package is a directory whose `BUILD` or `BUILD.bazel` file declares a rule
the query lists. Any other directory with a `BUILD` file, the root included,
joins the workspace as a bare member that runs nothing. A package with no test rule also gets `"disable": { "jobs": ["test"] }`. Add
`.putnami/` and `.gen/` to `.gitignore`, then commit the files: they are the
package graph Putnami selects on. Run `putnami projects sync` again after a
change to the dependencies between packages.

In a CI without Bazel on the machine that syncs, write the export first and
the sync reads it instead:

```bash
bazel query 'deps(//...)' --output=streamed_jsonproto > bazel-graph.json
putnami projects sync
```

### Run what a change affects

```bash
putnami test --impacted
```

After a change to `pricing`, Putnami runs `pricing` and the packages that
depend on it, `api` here, each with one command at the root:

```text
bazel coverage --build_event_json_file=<output>/build-events.json //pricing:all
bazel coverage --build_event_json_file=<output>/build-events.json //api:all
```

A change to `api` alone runs `api` alone: Putnami never runs Bazel on the
packages a changed package depends on. A change to `MODULE.bazel`, its
lockfile, `.bazelrc` or `.bazelversion` runs every package.

Each task copies the `test.xml` and `coverage.dat` of each test run the
build-event file lists into its own output, and reads them into its test and
coverage summary. It never removes a Bazel output, so a test Bazel has cached
returns as fast as Bazel does. Its result records `delegatedTo: bazel`, and
Putnami stores no cache entry for it. `putnami build` runs `bazel build` the
same way. Bazel declares no lint convention, so a Bazel package has no lint
task.

`putnami validate` fails with `bazel.sync_stale` when the edges no longer
match Bazel: run `putnami projects sync` and commit the result.

## Next

- [Run a Pants repository with Putnami](https://github.com/Putnami/putnami/blob/main/tooling/pants/doc/01-pants.md):
  every command, the version check, what the sync owns.
- [Run a Bazel workspace with Putnami](https://github.com/Putnami/putnami/blob/main/tooling/bazel/doc/01-bazel.md):
  every command, the version check, what the sync owns.
- [Observe a change in your CI](/docs/how-to/observe-a-change-in-your-ci): the
  report on a pull request, before you adopt anything.
