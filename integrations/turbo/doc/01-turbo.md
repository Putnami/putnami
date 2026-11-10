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

`init` writes a workspace that declares `@putnami/turbo`, and
`@putnami/typescript` in import mode, records the mode in
`options["@putnami/turbo"].mode` of `putnami.workspace.json`, installs the
extensions, and runs `putnami projects sync`.

Run `init` again with `--mode` to change the mode; without it, a rerun keeps the recorded mode. It prints the change to the workspace
config and the diff of each `putnami.json`, and writes nothing. Add `--apply`
to write them.

## The two modes

| | Import | Delegate |
|---|---|---|
| A TypeScript package's task whose script runs `bun build`, `bun test` or `biome` | Runs the `@putnami/typescript` task: Putnami's runner and cache | Runs `turbo run` on it |
| Any other task, such as a `tsc`, `vitest`, `jest` or `eslint` script | Runs `turbo run` on it | Runs `turbo run` on it |
| Cache | Putnami's for the tasks `@putnami/typescript` runs, Turborepo's for the others | Turborepo's, local and remote |

Import mode moves a task to `@putnami/typescript` only when its script
already runs what that extension's task runs: moving a
`tsc`, `vitest`, `jest` or `eslint` task would change the tool that checks the
code. Each task that stays on Turborepo is listed with the reason. One
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

adds the entry, declares `@putnami/typescript`, and synchronizes; without
`--apply` it previews. The sync job's `import` parameter outranks the option,
as `mode` does, so a preview reads the list it would record.

### What import mode installs

Import mode declares `@putnami/typescript` in the workspace, and its own
workspace setup then runs: it adds its devDependencies to the root
`package.json`, writes there the `workspaces` list of the members, and writes
a root `tsconfig.json`, `biome.json` and `bun.lock` when they are missing.
Delegate mode declares only `@putnami/turbo` and changes none of them.

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

It keeps every other member, and rewrites a file only when a member it owns
changes, with two-space indentation and sorted keys. A package that left the
workspace loses the extension, the tag, its edges to packages and the
disabled tasks.

It then lists, with the reason, each `turbo.json` task other than `build`,
`lint` and `test`, such as `dev` or `//#format`: no Putnami task maps it, and it
keeps running through Turborepo. It also lists each delegated package whose
test task names no JUnit report.

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
