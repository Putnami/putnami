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
- **Import**: Putnami's runner and cache take over the tasks a Putnami
  extension can map, and every other task keeps running through the tool.
  What an import maps depends on the tool: every Rush project, each moon
  project whose language has a Putnami extension, and in Nx and Turborepo
  only a TypeScript project's targets whose script already runs `bun build`,
  `bun test` or Biome. Each tool's section below gives the exact scope.

| Tool | Mode | Command |
|---|---|---|
| Pants | Delegate | `putnami init --from pants` |
| Bazel | Delegate | `putnami init --from bazel` |
| Nx | Import or delegate, asked | `putnami init --from nx` |
| moon | Import or delegate, asked | `putnami init --from moon` |
| Turborepo | Import or delegate, asked | `putnami init --from turbo` |
| Rush | Import or delegate, asked | `putnami init --from rush` |

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
joins the workspace as a bare member that runs nothing. A package with no
test rule also gets `"disable": { "jobs": ["test"] }`. Add `.putnami/`,
`.gen/` and `/bazel-*` to `.gitignore`, then commit the files: they are the package graph
Putnami selects on. Run `putnami projects sync` again after a
change to the dependencies between packages.

In a CI without Bazel on the machine that syncs, write the export first and
the sync reads it instead:

```bash
bazel query 'deps(//...)' --output=streamed_jsonproto > bazel-graph.json
putnami projects sync
```

Do not commit `bazel-graph.json`: while it lies at the root, the sync and
`putnami validate` read it and never ask Bazel, so a stale file hides a new
edge. Write it in the same CI job, before the sync and the gate.

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

## Nx

Nx adopts in the mode you pick. In a terminal, `putnami init --from nx` asks,
with **Import** preselected; in CI or from an agent it imports unless you pass
`--mode delegate`.

- **Import**: a TypeScript project's `build`, `test` and `lint` whose script
  already runs `bun build`, `bun test` or Biome run as `@putnami/typescript`
  tasks, under Putnami's runner and cache. Every other target, a `jest` test
  or an `@nx/webpack:webpack` build for example, keeps running through Nx, so
  no target changes the tool that checks it.
- **Delegate**: you keep Nx and its cache, local or Nx Cloud. Each task runs
  `nx run <project>:<target>` on one affected project, and Putnami caches
  nothing for it.

### Before you start

- `@putnami/nx` is not published yet: until its first release,
  `putnami init --from nx` stops at the extension install.
- `nx.json` lies at the root of the repository, which becomes the workspace
  root, and Nx is installed in `node_modules`: each task runs
  `npx --no nx`, which never downloads one.

### Turn the repository into a workspace

At the repository root:

```bash
putnami init --from nx --mode delegate
```

`init` declares `@putnami/nx`, and `@putnami/typescript` in import mode,
records the mode in `putnami.workspace.json`, installs the extensions and
synchronizes the projects. The sync reads `graph.json` when the root holds
one (`nx graph --file=graph.json`), else `nx.json`, each `project.json` and
each workspace `package.json`, and writes in each project's `putnami.json`
the extension, the projects it depends on, and the jobs it disables:

```json
{
  "$schema": "https://putnami.dev/schemas/putnami-project.json",
  "extensions": ["@putnami/nx"],
  "dependencies": ["/packages/pricing"],
  "disable": { "jobs": ["lint"] }
}
```

A project with no `lint` target has `lint` disabled. In import mode a
project's `disable.jobs` also names the extension that does not run each
target: `@putnami/nx:test` when a `bun test` script moves to
`@putnami/typescript`, `@putnami/typescript:build` when a
`@nx/webpack:webpack` build keeps running through Nx. The sync owns these
entries. The output lists each target Putnami does not run itself, such as
`serve`, `e2e` or a `jest` test, with the reason.

In import mode, `@putnami/typescript`'s workspace install writes the root
`package.json` with the projects as `workspaces` and installs them with Bun,
recorded in `bun.lock`: the repository's npm, pnpm or yarn lockfile and its
install are no longer used.

### Switch mode, or see what a sync would change

Rerun `init` in the workspace. It shows the diff of each `putnami.json` and
the mode change, and writes nothing:

```bash
putnami init --from nx --mode import
```

`--apply` records the mode, installs `@putnami/typescript` when import needs
it, and synchronizes:

```bash
putnami init --from nx --mode import --apply
```

### Run what a change affects

```bash
putnami test --impacted
```

In delegate mode, after a change to `packages/pricing`, Putnami runs
`pricing` and the projects that depend on it, `api` here, at the root:

```text
npx --no nx run @demo/pricing:test --outputStyle=static
npx --no nx run @demo/api:test --outputStyle=static
```

Nx's `dependsOn` runs the upstream targets, and Nx's cache answers them. A
change to `api` alone runs `api` alone. The test task reads the JUnit and
Cobertura or LCOV files the project's jest or vitest configuration names,
`jest-junit`'s `outputDirectory` and `coverageDirectory` for example; without
them its result is the exit code alone. Each result records `delegatedTo: nx`.

Nx infers edges from imports, so `putnami validate` fails with
`nx.sync_stale` when a project's `putnami.json` no longer matches Nx's graph:
run `putnami projects sync` and commit the result.

## moon

moon offers both modes. Delegate keeps moon and its cache, local and remote:
each Putnami task runs `moon run` on one project. Import runs each project
whose language has a Putnami extension (TypeScript, Go, Python, PHP, Ruby or
Rust) through that extension, under Putnami's cache, and keeps every other
project delegated to moon, so no Putnami cache sits over moon's.

### Before you start

- `@putnami/moon` is not published yet: until its first release,
  `putnami init --from moon` stops at the extension install.
- `.moon/workspace.yml` lies at the root of the repository, which becomes the
  workspace root.
- In delegate mode, moon is on `PATH`, at the version `.prototools` pins.
  Without a pin, each task warns that the version is not pinned.

### Turn the repository into a workspace

At the repository root:

```bash
putnami init --from moon --mode delegate
```

`init` declares `@putnami/moon` with the mode under
`options["@putnami/moon"].mode`, installs it, and synchronizes the projects.
The sync reads `moon-graph.json` at the root when the CI wrote it, else
`.moon/workspace.yml` and each `moon.yml`, with the tasks each project
inherits from `.moon/tasks.yml` and `.moon/tasks/*.yml`. It writes, in each
project's `putnami.json`, the tag `moon`, the extension, and the projects it
depends on:

```json
{
  "$schema": "https://putnami.dev/schemas/putnami-project.json",
  "tags": ["moon"],
  "extensions": ["@putnami/moon"],
  "dependencies": ["/packages/pricing"]
}
```

`putnami lint`, `putnami test` and `putnami build` run the moon task named
`lint`, `test` or `build`, else the first one tagged that way. A project with
no such task has that command in `disable.jobs`. The sync lists every other
task, such as `api:dev`: it keeps running through moon.

In import mode, `putnami init --from moon` writes `@putnami/typescript` in
the `putnami.json` of a TypeScript project, declares the extension in the
workspace and installs it; a project in a language with no Putnami extension
stays on `@putnami/moon`. `@putnami/typescript`'s workspace install then
writes the root `package.json` with the projects as `workspaces` and installs
them with Bun, recorded in `bun.lock`: the repository's npm, pnpm or yarn
lockfile and its install are no longer used.

Commit the files: they are the project graph Putnami selects on. Run
`init --from moon` again later to see what the sync would change, and
`--apply` to write it:

```bash
putnami init --from moon           # prints the diff, writes nothing
putnami init --from moon --apply   # writes it
```

### Run what a change affects

```bash
putnami test --impacted
```

In delegate mode, after a change to `pricing`, Putnami runs `pricing` and the
projects that depend on it, `api` here, each with one command at the root:

```text
moon run pricing:unit
moon run api:test
```

A change to `api` alone runs `api` alone. Each result records
`delegatedTo: moon`, and Putnami stores no cache entry for it: moon's cache
decides what reruns.

A delegated test reads the JUnit and coverage reports the project names
under `options["@putnami/moon"].tasks.test.reports` in its `putnami.json`;
without them its result is moon's exit code alone, and `init` lists the
project.

moon infers edges from manifests and task inputs, so an edit can change them:
`putnami validate` fails with `moon.sync_stale` when a `putnami.json` no
longer matches moon's project graph. Run `putnami projects sync` and commit
the result.

## Turborepo

Turborepo adopts in either mode:

- **Import**: a TypeScript package's task whose script already runs
  `bun build`, `bun test` or `biome` runs the `@putnami/typescript` task,
  with Putnami's cache. Every other task, such as a `tsc`, `vitest`, `jest` or
  `eslint` script, keeps running through Turborepo: moving it would change the
  tool that checks the code. Import mode also declares `@putnami/typescript`,
  whose setup adds its devDependencies and a `workspaces` list to the root
  `package.json`, and writes a root `tsconfig.json`, `biome.json` and
  `bun.lock` when they are missing. When a package's `build` moves to
  `@putnami/typescript`, a package that depends on it and still builds through
  Turborepo builds it again through `^build`.
- **Delegate**: every package runs through Turborepo, which keeps its cache,
  local and remote.

### Before you start

- `@putnami/turbo` is not published yet: until its first release,
  `putnami init --from turbo` stops at the extension install.
- `turbo.json` lies at the root of the repository, which becomes the
  workspace root, beside the npm, pnpm or yarn workspace that lists the
  packages.
- In delegate mode, `turbo` is on `PATH`: a global install defers to the
  repository's own version.

### Turn the repository into a workspace

At the repository root:

```bash
putnami init --from turbo
```

In a terminal, `init` asks the mode:

```text
  How should Putnami adopt this turbo repository?
    1) import    Putnami's runner and cache take over the targets it can map (default)
    2) delegate  turbo keeps running each target, with its cache
  Mode [1]:
```

`init` declares `@putnami/turbo`, and `@putnami/typescript` in import mode,
records the mode in `putnami.workspace.json`, installs the extensions and
synchronizes the projects. The sync reads the package manifests and
`turbo.json`, and writes in each package's `putnami.json` the extension that
runs it and the packages it depends on:

```json
{
  "$schema": "https://putnami.dev/schemas/putnami-project.json",
  "extensions": ["@putnami/turbo"],
  "dependencies": ["/packages/pricing"],
  "tags": ["turbo"],
  "disable": { "jobs": ["lint"] }
}
```

A package with no `build`, `lint` or `test` task, that is no script of that
name or no task of that name in `turbo.json`, has it disabled. The sync then
lists what it does not map, which keeps running through Turborepo:

```text
Not mapped, kept on Turborepo:
  @demo/api#test: @putnami/typescript runs bun test, not "vitest run": it keeps running through Turborepo, with turbo run test
  dev: no Putnami task maps it: it keeps running through Turborepo, with turbo run dev
Test result from the exit code alone, no JUnit report named: apps/api, packages/pricing
```

Add `.putnami/` and `.gen/` to `.gitignore`, then commit the files. Run
`putnami projects sync` again after a change to the dependencies between
packages: `putnami validate` fails with `turbo.sync_stale` until the files
match the package manifests.

### Run what a change affects in delegate mode

```bash
putnami test --impacted
```

After a change to `packages/pricing`, Putnami runs `pricing` and the packages
that depend on it, each with one command at the root:

```text
turbo run test --filter=@demo/pricing
turbo run test --filter=@demo/api
```

Turborepo's `dependsOn` runs the upstream tasks, such as `^build`, and its
cache answers them. Putnami stores no cache entry for these tasks, and each
result records `delegatedTo: turbo`. A change to `api` alone runs `api`
alone. A change to the root lockfile, `pnpm-workspace.yaml` or `.npmrc`
runs every package, since Turborepo hashes them into each one.

A delegated test task's result is the exit code of `turbo run test`. To read
the test and coverage summary, name the reports the package writes in its
`putnami.json`:

```json
{
  "options": {
    "@putnami/turbo": {
      "tasks": {
        "test": {
          "reports": {
            "junit": ["reports/junit.xml"],
            "coverage": [{ "format": "lcov", "paths": ["reports/coverage/lcov.info"] }]
          }
        }
      }
    }
  }
}
```

### Change the mode

Run `init` again with the other mode; without `--mode`, a rerun keeps the recorded one. It prints the change to the workspace
config and the diff of each `putnami.json`, and writes nothing:

```bash
putnami init --from turbo --mode delegate
```

Add `--apply` to write it, install an extension the mode needs, and
synchronize the projects.

## Rush

Rush offers both modes. Import hands the projects to Putnami's TypeScript
extension; delegate keeps Rush and its build cache.

### Before you start

- `@putnami/rush` is not published yet: until its first release,
  `putnami init --from rush` stops at the extension install.
- `rush.json` lies at the root of the repository, which becomes the workspace
  root.
- For delegate mode, `rush` is on `PATH`, at the `rushVersion` of `rush.json`.

### Turn the repository into a workspace

At the repository root:

```bash
putnami init --from rush --mode import
```

or

```bash
putnami init --from rush --mode delegate
```

`init` records the mode under `options["@putnami/rush"].mode` of
`putnami.workspace.json`, declares the extensions of the mode, installs them,
and synchronizes the projects. The sync reads `rush.json` and each project's
`package.json`, and writes each project's `putnami.json` with the projects it
depends on:

```json
{
  "$schema": "https://putnami.dev/schemas/putnami-project.json",
  "extensions": ["@putnami/rush"],
  "dependencies": ["/libs/pricing"]
}
```

In import mode the extension is `@putnami/typescript`, and the TypeScript
install writes the root `package.json` with the projects as `workspaces` and
installs them with Bun: Rush's lockfile and `rush install` are no longer used.
A project whose `main` names Rush's build output, such as `lib/index.js`,
gains `"exports": { ".": "./src/index.ts" }` unless `rush.json` publishes it,
and a project without `tsconfig.json` gets one that extends the root's, so
Putnami builds each project from its sources. A project `rush.json`
publishes keeps its build-output entry, so the projects that depend on it
cannot resolve it in import mode: the sync warns, and delegate mode suits such
a repository better.

The sync writes nothing it cannot map. It warns, with the reason, about each
`command-line.json` command other than `build`, `test` and `lint`, which keeps
running through Rush, and in import mode about each project script other than
those three, which `rushx` still runs.

Commit the files. Run `putnami init --from rush` again later to see what a
sync would change, and `putnami init --from rush --apply` to write it.

### Run what a change affects in import mode

```bash
putnami lint,test,build --impacted
```

Putnami's TypeScript tasks replace the `build`, `test` and `lint` scripts, and
Putnami's cache stores their results.

### Run what a change affects in delegate mode

```bash
putnami build --impacted
```

After a change to `libs/pricing`, Putnami runs `@demo/pricing` and the projects
that depend on it, `@demo/api` here, each with one command at the root:

```text
rush build --to @demo/pricing
rush build --to @demo/api
```

A project it depends on runs no task of its own. Each result records
`delegatedTo: rush`, and Putnami stores no cache entry. `rush build --to`
also builds the projects upstream of the one it names, so turn Rush's build
cache on in `common/config/rush/build-cache.json`: without it, the sync warns
that each task reruns them.

`putnami test` and `putnami lint` run `rush test` and `rush lint --to
<package>` when `command-line.json` declares a bulk or phased command of that
name; otherwise the sync disables them. Rush has no report convention, so the
test task reads the JUnit and coverage files a project names under
`options["@putnami/rush"].tasks.test.reports` of its `putnami.json`; without
them its result is the exit code, and the sync names those projects.

`putnami validate` fails with `rush.sync_stale` when the edges no longer match
`rush.json` and the project manifests: run `putnami projects sync` and commit
the result.

## Next

- [Run a Pants repository with Putnami](/docs/monorepo-tools/pants):
  every command, the version check, what the sync owns.
- [Run a Bazel workspace with Putnami](/docs/monorepo-tools/bazel):
  every command, the version check, what the sync owns.
- [Run a Nx repository with Putnami](/docs/monorepo-tools/nx):
  both modes, what the sync owns, which targets import maps.
- [Run a moon workspace with Putnami](/docs/monorepo-tools/moon):
  the two modes, task matching, reports, what the sync owns.
- [Run a Turborepo repository with Putnami](/docs/monorepo-tools/turbo):
  the two modes, what the sync owns, the test reports.
- [Adopt a Rush repository](/docs/monorepo-tools/rush):
  both modes, what the sync writes, the test reports.
- [Observe a change in your CI](/docs/how-to/observe-a-change-in-your-ci): the
  report on a pull request, before you adopt anything.
