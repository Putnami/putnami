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

## Pants

Pants keeps its cache and its remote execution: each Putnami task runs
`pants` on one package directory.

### Before you start

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
  "dependencies": ["/src/python/pricing"]
}
```

A package is a directory whose `BUILD` file declares a first-party target;
`3rdparty/python`, which only declares requirements, is none. A package with
no test target also gets `"disable": { "jobs": ["test"] }`. Commit the files:
they are the package graph Putnami selects on. Run `putnami projects sync`
again after a change to the dependencies between packages.

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

Each task reads the JUnit reports under `dist/test/reports/` and the coverage
report `dist/coverage/python/coverage.xml` into its test and coverage summary,
and its result records `delegatedTo: pants`. `putnami lint` runs `pants lint`
and `putnami build` runs `pants package` the same way. Putnami stores no cache
entry for them: a package Pants has cached returns as fast as Pants does.

### When the repository is not Python

The shipped test command reads Python coverage. Without the Python backend,
override the test task in each package's `putnami.json`:

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

## Next

- [Run a Pants repository with Putnami](https://github.com/Putnami/putnami/blob/main/tooling/pants/doc/01-pants.md):
  every command, the version check, what the sync owns.
- [Observe a change in your CI](/docs/how-to/observe-a-change-in-your-ci): the
  report on a pull request, before you adopt anything.
