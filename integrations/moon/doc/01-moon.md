# Run a moon workspace with Putnami

`@putnami/moon` turns each project of a moon workspace into a Putnami project,
with an edge to each project moon says it depends on. Putnami selects what a
change affects and gives the verdict; what runs each task depends on the mode
the workspace adopted moon in.

## Adopt it

`@putnami/moon` is not published yet: until its first release, `putnami init
--from moon` stops at the extension install, after it has written the
workspace files, and a workspace loads the extension from a source checkout.

At the root of the repository, where `.moon/workspace.yml` lies:

```bash
putnami init --from moon
```

In a terminal, `init` asks the mode, with import preselected. Without a
terminal, in CI or from an agent, it imports unless `--mode delegate` is
given; `--mode import` or `--mode delegate` skips the question. `init` writes
a workspace that declares `@putnami/moon` with the mode under
`options["@putnami/moon"].mode`, installs it, runs `putnami projects sync`,
then installs the language extensions the sync declared. A TypeScript project
then installs with the package manager the repository declares, npm, pnpm,
Yarn or Bun, which keeps its lockfile.

Rerun in a workspace, `init --from moon` prints the diff of what the sync
would write and changes nothing; `--apply` writes it.

## The two modes

| | Import | Delegate |
|---|---|---|
| A task of a TypeScript, Go, Python, PHP, Ruby or Rust project that runs the tool its extension runs | runs `@putnami/<language>` | runs `moon run` |
| Any other task of such a project | runs `moon run` | runs `moon run` |
| Any other project | runs `moon run` | runs `moon run` |
| Cache | Putnami's for the tasks that moved, moon's for the others | moon's, local and remote |

An import never changes the tool that checks the code (D-070): a task moves
only when the language extension runs its tool, read from its `script`, else
its `command` and `args`:

| Language | Tools the extension runs |
|---|---|
| TypeScript | `bun build`, or `tsc` alone when it is the `build` script of `package.json`; `bun test`, Vitest or Jest without an argument that picks a configuration or a subset; Biome or a bare `eslint` |
| Go | `go build`, `go test` |
| Rust | `cargo build`; `cargo nextest` or `cargo llvm-cov nextest` |
| Python | `pytest`, `ruff` |
| Ruby | `rspec`, `rubocop` |
| PHP | `phpunit`, `phpstan` |

A Go or Rust lint stays on moon, since the extension's lint checks more than
the task did (staticcheck beside golangci-lint, `cargo fmt --check` beside
clippy), and so does a `cargo test`, since the Rust extension tests with
nextest, which runs no doctest. A TypeScript test records its runner, `bun`
included. The extension runs the program under its own arguments and drops the
task's, so a task moves only with arguments that choose the whole project or
only change the output, such as `./...`, `-v`, `--locked` or `ruff check .`.
A task that selects build tags, a release profile, a package or a subset of
the suite, such as `go test -tags integration ./...`, `cargo build --release`
or `pytest tests/unit`, stays on moon, and the sync names the arguments.
A runner prefix such as `bundle exec`, `uv run` or `npx`, or a path such as
`vendor/bin/`, does not change the tool. A task chained with another command
stays on moon. A TypeScript task also stays when the project's `package.json`
scripts would lead `@putnami/typescript` to another tool: a Bun build beside a
`tsc` build script, a Biome lint beside an ESLint lint script.

No task runs both: no Putnami cache sits over moon's. Switching mode is a
change to `options["@putnami/moon"].mode` and a `putnami projects sync`;
`putnami init --from moon --mode delegate --apply` also removes from the
workspace each language extension no `putnami.json` names any more.
A workspace that records no mode imports, as `init` does without a terminal;
a value other than `import` or `delegate` fails the sync and the probe with
`stacks.adoption_mode_invalid`.

For each project moon runs, the sync also writes, as `filePatterns`, the root
files every moon task reads: `.moon/workspace.yml`, `.moon/tasks.yml`,
`.moon/tasks/**`, `.moon/toolchain.yml`, `.prototools`, and the root lockfile
of each language moon runs (npm, pnpm, Yarn, Bun, Cargo, Go workspaces, uv,
Poetry, Composer, Bundler) with `.npmrc` and `pnpm-workspace.yaml`, those the
root holds. A change to one of them
selects every such project, as moon's own hashes do. Each edge of a project
moon runs is attributed to moon's project graph (`build-graph`), so
`workspace.declared_edge` does not warn about it. A project imported into a
language extension takes its edges' provenance from that extension: an edge
moon states that its manifests do not, such as a TypeScript project's edge to
a Rust one, is still reported as a declaration.

## Projects and tasks

The extension reads `moon-graph.json` at the root when the CI wrote it
(`moon project-graph --json > moon-graph.json`), else `.moon/workspace.yml`
and each project's `moon.yml`. A project's tasks are the export's, or its
`moon.yml` tasks over the ones it inherits from `.moon/tasks.yml` and
`.moon/tasks/*.yml`: by file name (`node`, `<language>`, `<stack>`,
`<language>-<layer>`, `tag-<tag>`) or by `inheritedBy`, after its
`workspace.inheritedTasks` include, exclude and rename.

The moon projects are the ones the workspace's `projects` setting or the
export names. `putnami projects sync` asks the extension about the
directories that hold a `moon.yml` or a language manifest moon reads
(`package.json`, `tsconfig.json`, `deno.json`, `go.mod`, `Cargo.toml`,
`pyproject.toml`, `requirements.txt`, `composer.json`, `Gemfile`), and about
those that hold a `putnami.json`, so a moon project with none of those files
joins once the sync has written its `putnami.json`.

`putnami lint`, `putnami test` and `putnami build` run the moon task named
`lint`, `test` or `build`, else the first one, by name, tagged that way. A
project with no such task has that command in `disable.jobs`. Every other
task is listed by the sync and keeps running through moon.

## What the sync writes

`putnami projects sync` runs the extension's sync task. In each project's
`putnami.json` it writes:

- the tag `moon`, for selection;
- `options["@putnami/moon"].graphSync`, which marks the project as the
  sync's, whatever extension it runs: a project the team tags `moon` is not
  the sync's;
- the extension the mode maps it to, and in import mode `@putnami/moon`
  beside its language extension when a task stays on moon;
- its `dependencies` on the other moon projects, as project IDs;
- `build`, `lint` or `test` in `disable.jobs` when the project has no task for
  it, and for a project that runs both extensions, the command qualified by
  the one that does not run it, such as `@putnami/moon:test` for a test that
  moved and `@putnami/go:lint` for a lint that stays;
- for a task moon runs that is tagged rather than named, the step under
  `options["@putnami/moon"].tasks.<command>.steps`;
- for a TypeScript project, the runner of a test that moved, `bun`, `vitest`
  or `jest`,
  under `options["@putnami/typescript:test"].runner`, and `eslint` under
  `options["@putnami/typescript"].lint.linter` for an ESLint lint that moved;
- for a TypeScript project, the conventions of D-066: a `tsconfig.json` that
  extends the root's when it holds no `tsconfig*.json`, and
  `"exports": { ".": "./src/index.ts" }` when its build moves to the Bun
  build, its `package.json` is private and its `main` names a build output.

It keeps every other member, the project's own steps included: it writes a
step only where none is set or where it wrote the one there. It retracts a
project moon no longer lists, keeping the language extension the project
runs, and in import mode adds the language extensions to the workspace's
`extensions`, in its list or object form.

Import sets each project's extension from moon's language: a JavaScript
project runs `@putnami/moon` even when its `putnami.json` named
`@putnami/typescript`, and a TypeScript project whose tasks all moved loses
`@putnami/moon`. The sync lists each task it keeps on moon with its reason.

`putnami validate` fails with `moon.sync_stale` when a sync would change a
`putnami.json`, in both modes: moon infers edges from manifests and task
inputs, and a task's command decides where it runs, so an edit can change
them.

## Reports

A delegated `test` task reads the JUnit and coverage reports the project
names in its own options, as the stack core reads any report:

```json
{
  "options": {
    "@putnami/moon": {
      "tasks": {
        "test": {
          "reports": {
            "junit": ["{root}/apps/api/reports/junit.xml"],
            "coverage": [{ "format": "lcov", "paths": ["{root}/apps/api/coverage/lcov.info"] }]
          }
        }
      }
    }
  }
}
```

Without them the result is moon's exit code alone, and the sync lists the
project.

## Toolchain

Each task checks `moon --version` against the `moon` version `.prototools`
pins; without one it warns and runs. The extension never installs moon.
