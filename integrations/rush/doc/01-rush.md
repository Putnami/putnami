# Adopt a Rush repository

`@putnami/rush` turns each project of a Rush repository into a Putnami
project, in one of two modes:

| Mode | Who runs the tasks | Who installs | Whose cache |
|---|---|---|---|
| `import` | Putnami, through `@putnami/typescript` | Putnami, with Bun | Putnami's |
| `delegate` | Rush: `rush build`, `rush test` or `rush lint --to <package>` | Rush | Rush's build cache |

## Adopt it

`@putnami/rush` is not published yet: until its first release, `putnami init
--from rush` stops at the extension install, after it has written the
workspace files, and a workspace loads the extension from a source checkout.

At the root of the repository, where `rush.json` lies:

```bash
putnami init --from rush
```

In a terminal, `init` asks the mode, with `import` preselected. Without a
terminal, such as in CI or under an agent, it imports. `--mode import` or
`--mode delegate` skips the question. `init` records the mode under
`options["@putnami/rush"].mode` of `putnami.workspace.json`, declares the
extensions of the mode, installs them, and runs `putnami projects sync`. The
sync reads that mode, `import` when it is absent. A value other than `import` or `delegate` fails the sync and the probe with
`stacks.adoption_mode_invalid`. In delegate mode,
each edge the probe reports is attributed to Rush's project graph
(`build-graph`), as a delegated stack's are, so `workspace.declared_edge` does
not warn about them.

Run again in the adopted workspace, `putnami init --from rush` writes nothing:
it prints the diff of each file the sync would change. `--apply` writes them.
`putnami init --from rush --mode <mode> --apply` switches mode: the sync
replaces the other mode's extension in each project's `putnami.json`, with the
`test` and `lint` entries and the file patterns delegate mode wrote.

Add `.putnami/` and `.gen/` to the repository's `.gitignore`: `init` writes
one only when the repository has none.

## What the sync writes

`putnami projects sync` runs the extension's sync task. It reads `rush.json`
and each project's `package.json`, as `putnami observe` does: a project depends
on another when a dependency of its `package.json` names it with the
`workspace:` protocol or a range its version satisfies, unless
`decoupledLocalDependencies` lists it. Then it writes in each project's
`putnami.json`:

| Member | Delegate mode | Import mode |
|---|---|---|
| `extensions` | `@putnami/rush` | `@putnami/typescript` |
| `dependencies` | the project IDs of the Rush projects it depends on | the same |
| `options["@putnami/rush"].filePatterns` | `rush.json`, `command-line.json` and the Rush lockfile, so a lockfile change runs every project | none |
| `disable.jobs` | `test` and `lint` when `command-line.json` declares no bulk or phased command of that name | none |

It writes a file only when a member it owns changes, and keeps every other
member. In delegate mode it removes the extension and its edges from a project
that `rush.json` no longer lists. An import retracts nothing: Putnami owns the
projects from then on.

The sync writes nothing it cannot map, and lists it instead:

- each `command-line.json` command other than `build`, `test` and `lint`, and
  each global command: it keeps running through Rush;
- in import mode, each project script other than `build`, `test` and `lint`:
  run it with `rushx <script>`;
- in delegate mode, the projects whose test task names no report.

## Delegate mode

Each task checks that the Rush banner of `rush --help` reports the
`rushVersion` of `rush.json`, then runs at the workspace root, for one project:

| Command | Step |
|---|---|
| `putnami build` | `rush build --to <package>` |
| `putnami test` | `rush test --to <package>` |
| `putnami lint` | `rush lint --to <package>` |

Putnami's cache is off for every task, and its result records
`delegatedTo: rush`. Putnami selects what a change affects: a changed project
and the projects that depend on it run, and a project it depends on runs no
task of its own. `rush build --to` also builds the projects upstream of the
one it names; Rush's build cache makes that cheap. That cache is opt-in: when
`common/config/rush/build-cache.json` does not set `buildCacheEnabled` to
`true`, the sync warns that each delegated task reruns its upstream projects.

`putnami validate` fails with `rush.sync_stale` when a project's
`putnami.json` no longer matches `rush.json` and the project manifests, such as
after a dependency change that alters the edges. Run `putnami projects sync`
and commit the result.

### Test reports

Rush has no report convention, so the test task reads the reports a project
names under its `putnami.json`:

```json
{
  "options": {
    "@putnami/rush": {
      "tasks": {
        "test": {
          "reports": {
            "junit": ["temp/junit.xml"],
            "coverage": [{ "format": "lcov", "paths": ["temp/coverage/lcov.info"], "optional": true }]
          }
        }
      }
    }
  }
}
```

A failed case fails the task. Without reports, the task's result is the exit
code of `rush test` alone.

## Import mode

Each project becomes a `@putnami/typescript` project: `putnami build`, `test`
and `lint` run Putnami's TypeScript tasks, which replace the `build`, `test`
and `lint` scripts, and Putnami's cache stores their results. The TypeScript
extension's install writes the root `package.json` with the projects as
`workspaces` and installs them with Bun, since a Rush root declares no package
manager of its own: its lockfile lies under `common/config/rush`. Rush's
lockfile and `rush install` are no longer used. A root `package.json` that
declares `packageManager`, or a root lockfile, selects that manager instead.

An import also adopts Putnami's TypeScript conventions (D-066), so each
project builds and resolves its neighbours from their sources:

- A `package.json` that declares no `exports`, whose top-level `main` or
  `types` names a file outside `src/` such as Rush's `lib/index.js`, and whose
  project holds `src/index.ts`, gains `"exports": { ".": "./src/index.ts" }`
  after that member. Every other byte stays. A project `rush.json` publishes,
  with `shouldPublish` or a `versionPolicyName`, keeps its entry, since
  `rush publish` ships it to consumers that read the build output. Putnami's
  TypeScript build does not write that output, so the projects that depend on
  it cannot resolve it, and the sync warns. Add the `exports` by hand when no
  consumer outside the repository needs the build-output entry, or adopt the
  repository in delegate mode, where Rush builds it. So does a manifest it cannot edit line by line, such as one with CRLF
  line ends: add the `exports` by hand.
- A project without `tsconfig.json` gets one that extends the root's, which
  the TypeScript install writes.

The sync warns about each file it writes, and a rerun of `putnami init --from
rush` shows their diffs.

`rush.sync_stale` does not apply: the projects belong to Putnami.

## What it does not do

- Install or switch Rush: the version check compares and never installs.
- Rewrite `rush.json`, `command-line.json`, or a project's `package.json`
  beyond the `exports` an import adds.
- Map subspaces, version policies, `rush publish` or `rush version`: they keep
  running through Rush.
