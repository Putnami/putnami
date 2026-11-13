# Run a Turborepo repository with Putnami

`@putnami/turbo` turns each package of a Turborepo repository into a Putnami
project, with an edge to each workspace package it depends on. Putnami selects
what a change affects and gives the verdict. The mode decides who runs each
package and whose cache answers.

## Adopt it

`@putnami/turbo` is not published yet: until its first release, `putnami init
--from turbo` stops at the extension install, after it has written the
workspace files, and a workspace loads the extension from a source checkout.

At the root of the repository, where `turbo.json` lies:

```bash
putnami init --from turbo
```

In a terminal, `init` asks the mode, with Import preselected. Without a
terminal, in CI or from an agent, it imports. `--mode import` or
`--mode delegate` skips the question:

```bash
putnami init --from turbo --mode delegate
```

`init` writes a workspace that declares `@putnami/turbo`, records the mode in
`options["@putnami/turbo"].mode` of `putnami.workspace.json`, installs the
extension, and runs `putnami projects sync`. In import mode, the sync
declares `@putnami/typescript` only when a task moves to it, and `init` then
installs it. When no task moves, `init` installs nothing more, leaves the
lockfile as it was, and says that `--mode delegate` gives the same result
(ADR 0017 of `tooling/stacks`).

Run `init` again with `--mode` to change the mode; without it, a rerun keeps the recorded mode. It prints the change to the workspace
config and the diff of each `putnami.json`, and writes nothing. Add `--apply`
to write them.

## The two modes

| | Import | Delegate |
|---|---|---|
| A TypeScript package's task whose script runs only `bun build`, `bun test`, `vitest`, `jest` or `biome`, a bare `eslint` or `eslint .` lint, or a `build` script that runs `tsc` alone | Runs the `@putnami/typescript` task: Putnami's runner and cache | Runs `turbo run` on it |
| Any other task, such as a bundler build, another `tsc` script, a `prettier` script, an ESLint with arguments, a test whose arguments pick a configuration or a subset, or a lint chained with `prettier` | Runs `turbo run` on it | Runs `turbo run` on it |
| Cache | Putnami's for the tasks `@putnami/typescript` runs, Turborepo's for the others | Turborepo's, local and remote |

Import mode moves a task to `@putnami/typescript` only when its script
runs what that extension's task runs and nothing else: moving a `prettier`
task would change the tool that checks the code. A test script that runs
`vitest` or `jest` moves, because the test task runs the project's own runner
with its own configuration, as
[the test command](https://github.com/Putnami/putnami/blob/main/typescript/extension/doc/03-test.md#test-runner)
describes. A bare `eslint` lint script moves: `@putnami/typescript` lints the
package with ESLint because its lint script runs it, from the package's own
configuration. An ESLint script with arguments, such as `--max-warnings 0`,
stays: the TypeScript lint passes ESLint no argument, so move them to the
ESLint configuration first. A `build` script that runs `tsc` alone (`tsc`,
`tsc -b` or `tsc -p` with one config) moves with that `tsc`, which Putnami
runs into the package's `outDir`, as
[A library built with tsc](https://github.com/Putnami/putnami/blob/main/typescript/extension/doc/02-build.md#a-library-built-with-tsc)
describes. Each task that stays on Turborepo is listed with the reason. One
extension runs each task, so no Putnami cache sits over Turborepo's.

A package is TypeScript when it holds a `tsconfig.json`, or a TypeScript
source (`.ts`, `.tsx`, `.mts` or `.cts`, not a `.d.ts`) outside `node_modules`,
`dist`, `coverage` and hidden directories.

When a package's `build` moves to `@putnami/typescript`, a package that
depends on it and still builds through Turborepo builds it again through
`^build`: `@putnami/typescript` builds it once for Putnami, and Turborepo once
for the delegated package.

### Import some packages of a delegated workspace

In delegate mode, `options["@putnami/turbo"].import` lists package paths and
globs, such as `/packages/pricing` or `/packages/*`: a leading `/` is
ignored, and `*`, `?` and `[...]` match within one path segment. The sync
writes each package an entry selects as import mode writes it, and every
other package as delegate mode writes it. Import mode reads no list.

```bash
putnami init --from turbo --import /packages/pricing --apply
```

adds the entry and synchronizes; the sync declares `@putnami/typescript` when
a task of an imported package moves to it. Without `--apply` it previews. The sync job's `import` parameter outranks the option,
as `mode` does, so a preview reads the list it would record.

### What import mode installs

Once a task moves, the sync declares `@putnami/typescript` in the workspace,
and its own workspace setup then runs. It adds to the root `package.json`, in sorted
order, only the devDependencies a moved task needs: `typescript` and
`@types/bun`, which every type check reads, for a build, package, validate or
test, and `@biomejs/biome` for a Biome lint. It adds to
the member list where the repository's package manager reads it
(`pnpm-workspace.yaml` for pnpm, the root `package.json` `workspaces`
otherwise) only a member no glob covers, and keeps every glob and exclusion.
A repository that keeps no list, a `pnpm-workspace.yaml` without `packages`,
no `pnpm-workspace.yaml` or a root `package.json` without `workspaces`, gains
none. It writes a root
`tsconfig.json` and `biome.json` when they are missing, and installs with that
manager, which keeps its lockfile: no `bun.lock` is written, and a repository
whose tasks need no new package keeps its lockfile unchanged. A failed install
fails `init` with the manager's output. Delegate mode declares only `@putnami/turbo` and changes
none of them.

An import also adopts Putnami's TypeScript conventions (D-066) on each
TypeScript project it hands to `@putnami/typescript`, so the projects that
depend on it build against its sources:

- A private `package.json` whose build moves to the Bun build, that declares
  no `exports`, whose top-level `main` or `types` names a file outside `src/`,
  and whose project holds `src/index.ts`, gains
  `"exports": { ".": "./src/index.ts" }`: the Bun build writes nothing in the
  project. A `tsc` build, or one that stays on Turborepo, writes the output
  its `main` names, so its entry stays. A `bun build` whose entry the sync
  does not rewrite stays on Turborepo, and the sync lists it with its reason:
  a package that is not private, one whose `exports` name a build output,
  and one with no `src/index.ts`.
- A test moves with `types: false` in
  `options["@putnami/typescript:test"]`, since its script type-checked
  nothing. When a lint moves, `putnami lint --impacted` lints the projects
  a change touches, not their dependents, and the sync says so.
- A lint whose script passes no write flag (`--write`, `--fix`, `--apply`)
  moves with `fix: false` in `options["@putnami/typescript:lint"]`: it
  checks and rewrites no file. When a Biome lint moves and the repository
  root holds no `biome.json` or `biome.jsonc`, the sync writes a root
  `biome.json` that sets nothing, so the lint keeps Biome's defaults, and
  lists it.
- A project with no `tsconfig*.json` gets a `tsconfig.json` that extends the
  root's. A project that holds another one, such as `tsconfig.lib.json`,
  keeps its configuration, which Jest or ESLint may read.

The sync lists each file it writes, and a dry run shows its diff.

## What the sync writes

`putnami projects sync` runs the extension's sync task. It reads the members
of the workspace (`pnpm-workspace.yaml`, or `workspaces` in the root
`package.json`), their `package.json`, and `turbo.json` at the root and in each
package, and writes in each package's `putnami.json`:

| Member | Value |
|---|---|
| `extensions` | `@putnami/turbo`, and `@putnami/typescript` for a TypeScript package in import mode or on the import list, beside any other extension the file names |
| `dependencies` | The project ID of each workspace package it depends on, such as `/packages/pricing`; a dependency on a project that is no package is kept |
| `tags` | `turbo`, which marks the packages the sync owns |
| `disable.jobs` | Each of `build`, `lint` and `test` the package has no task for: no script of that name in its `package.json`, or no task of that name, or `<package>#<task>`, in `turbo.json`. In import mode, or for a TypeScript package on the import list, for each task the job of the extension that does not run it: `@putnami/turbo:<task>` when `@putnami/typescript` runs it, `@putnami/typescript:<task>` otherwise. The sync owns these entries, bare and qualified |
| `options.@putnami/typescript.runtime` | `node` for a TypeScript package in import mode or on the import list whose `package.json` `start` script runs `node`, so `putnami serve`, `run` and `package --docker` run it on Node. The sync writes it when the package declares no runtime and never changes one it declares |

It keeps every other member, and rewrites a file only when a member it owns
changes, with two-space indentation and sorted keys. A package that left the
workspace loses the extension, the tag, its edges to packages and the
disabled tasks.

It then lists, with the reason, each `turbo.json` task other than `build`,
`lint` and `test`, such as `dev` or `//#format`: no Putnami task maps it, and it
keeps running through Turborepo. It also lists each delegated package whose
test task names no JUnit report.

Each edge the sync writes is Turborepo's package graph's (`build-graph`), in
both modes: the probe attributes it so, and `workspace.declared_edge` does not
warn about it and `putnami deps prune` does not remove it, even when no source
file imports the package, such as a shared `tsconfig` or ESLint configuration.

Run the sync again after any change to the dependencies between packages, and
commit the result. `putnami validate` compares each `putnami.json` with what
the sync would write and fails with `turbo.sync_stale` when a sync would change
one. Keep it in the CI gate.

## What each delegated task runs

Each task first runs `turbo --version`, and compares it with the range of
`devDependencies.turbo` in the root `package.json` when it states one. Then it
runs one command at the workspace root, with no shell:

| Task | Command |
|---|---|
| `lint` | `turbo run lint --filter=<package>` |
| `test` | `turbo run test --filter=<package>` |
| `build` | `turbo run build --filter=<package>` |

`turbo` must be on `PATH`: a global install defers to the repository's own
version. Turborepo's `dependsOn` runs the upstream tasks, such as `^build`, and
its cache answers them.

Every task sets `cache.enabled` to `false`: Putnami stores no entry and replays
none, and each task result records `delegatedTo: turbo`. Impact selection is
Putnami's, on the edges the sync wrote: `putnami test --impacted` runs
Turborepo on the packages a change touches and on the packages that depend on
them, never on a package they depend on. A change to a root file Turborepo hashes
into every package, a lockfile, `pnpm-workspace.yaml` or `.npmrc`, runs every
package.

## Test reports

Without more, a delegated test task's result is the exit code of
`turbo run test`. To read the test and coverage summary, name the reports the
package's test script writes in its `putnami.json`:

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

The reports are removed before the command runs. A failed case fails the task
with `stacks.test_failed`, and a named report the run did not write with
`stacks.report_missing`.

## Diagnostics

| Code | Meaning |
|---|---|
| `turbo.sync_failed` | The sync could not read the workspace or a manifest, and wrote nothing |
| `turbo.sync_stale` | A package's `putnami.json` no longer matches what the sync writes |
| `turbo.task_unmapped` | A `turbo.json` task no Putnami task maps; it keeps running through Turborepo |
| `turbo.reports_unnamed` | A delegated package's test task names no JUnit report |
| `turbo.import_unmatched` | An entry of `options["@putnami/turbo"].import` selects no package; a warning |
