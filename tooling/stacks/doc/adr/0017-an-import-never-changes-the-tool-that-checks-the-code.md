# ADR 0017 — An import never changes the tool that checks the code

- **Status**: accepted
- **Date**: 2026-11-11
- **Scope**: `tooling/stacks` (`@putnami/stacks`), the import mode of
  `@putnami/nx`, `@putnami/turbo`, `@putnami/rush` and `@putnami/moon`;
  `putnami init --from`

## Context

`putnami init --from <tool> --mode import` meant three things, one per tool:

- Nx and Turborepo moved a TypeScript project's target to
  `@putnami/typescript` only when the extension runs what the target runs:
  `bun build`, `bun test`, Vitest, Jest, Biome, a bare ESLint, or a `build`
  that runs `tsc` alone. Every other target kept running through the tool.
- Rush handed the whole project to `@putnami/typescript`, so a `test` script
  that runs Mocha became `bun test`, and a `build` that runs webpack became a
  Bun build, without notice.
- moon handed every project to the extension of its language, whatever its
  tasks ran.

A team that adopts Putnami reviews its code against the tools it chose. A
check that passes under another tool is not the check it reviewed.

`@putnami/typescript` now runs a project's Vitest or Jest suite (ADR 0008 of
`typescript/extension`), its ESLint configuration (ADR 0009) and its `tsc`
build (ADR 0010, D-069), each chosen from the project's `package.json`
scripts. D-066 rewrote the entry of every Rush project whose `main` names a
build output, so that its dependents build against its sources: it was
written when every imported build was a Bun build that writes nothing in the
project. A `tsc` build writes its `outDir`, which that `main` names.

## Decision

1. **One rule for every tool.** In import mode, a project whose language has
   a Putnami extension moves to that extension. Its `build`, `test` or `lint`
   target moves to Putnami only when the extension runs the same tool as the
   target. A target that runs another tool, or that no Putnami task takes
   over, keeps running as the repository defines it, through the tool, and
   the sync lists it as not mapped, with its reason. A project whose language
   has no extension stays on the tool. D-065 and D-066 apply to every import.
2. **The TypeScript rule is stated once.** `tooling/stacks` holds the rule
   that decides whether `@putnami/typescript` runs what a TypeScript script
   runs: `bun build` or a `tsc` alone for `build` (the `tscscript` package of
   the extension SDK), `bun test`, Vitest or Jest for `test`, Biome or a bare
   ESLint for `lint`, with no other command chained. The extension passes
   none of a script's arguments, so a test script moves only with arguments
   that choose no configuration and no subset of the suite (`vitest run`,
   `--coverage`, `--ci`, `--passWithNoTests` and the like), as an ESLint
   script moves only bare. A `bun build` script moves whatever its
   arguments: the Bun build reads no script argument; it transpiles the
   entries the `exports` of `package.json` name (the `.` export D-066 adds
   names `src/index.ts`), else `src/index.ts`, into Putnami's output, so a
   script that bundles another file builds another entry once moved. Turborepo, Rush and moon
   read it; Nx reads it beside its executors.
3. **A project runs both extensions.** A project that keeps one target on
   the tool names the tool's extension and the language extension in its
   `putnami.json`, and `disable.jobs` names the extension that does not run
   each task: `@putnami/<tool>:<task>` for a target that moved,
   `@putnami/<language>:<task>` for one that stays. The graph sync of
   `tooling/stacks` writes and owns those entries, as it owns a bare task the
   project has no target for.
4. **Each tool runs a kept target its own way.** Nx runs `nx run`, Turborepo
   `turbo run`, moon `moon run <project>:<task>`, and Rush `rushx <script>`
   in the project's directory: an import installs with the repository's
   package manager (D-065), so it runs the script itself rather than a Rush
   command over the projects upstream of it.
5. **moon reads what a task runs.** A moon task's `command` and `args`, or
   its `script`, decide whether it moves. A TypeScript task moves under the
   rule above, and its project records the test runner, `bun` included, and
   the ESLint linter, since `@putnami/typescript` would otherwise read them
   from `package.json`. A task of another language moves when it runs the
   program its extension runs, which runs it under the extension's own flags
   (race detection on the changed Go packages, nextest's `ci` profile, which
   the repository defines, the `src` and `tests` paths of phpstan and
   phpunit): `go build` or `go test`;
   `cargo build`, `cargo nextest` or `cargo llvm-cov nextest`; `pytest`;
   `rspec` or `rubocop`; `phpunit` or `phpstan`. Since the extension
   drops the task's own arguments, the task moves only with arguments that
   choose the whole project or only change the output, such as `./...`,
   `-v` or `--locked`; one that selects build tags, a release profile, a
   package or a subset of the suite, such as
   `go test -tags integration ./...`, `go test .`, `cargo build --release`
   or `pytest tests/unit`, keeps the task on moon. A Go, Rust or Python lint
   stays on moon, since the extension's lint checks more than the task did
   (staticcheck and the documentation links beside golangci-lint,
   `cargo fmt --check` and `-D warnings` beside clippy, `ruff format
   --check` beside `ruff check`), and so does a
   `cargo test`, since the Rust extension tests with nextest, which runs no
   doctest.
6. **D-066 follows the build.** An import rewrites the entry of a project
   the repository does not publish, whose `main` names a build output, only
   when its `build` moves to the Bun build: that build writes nothing in the
   project. A `tsc` build, or one that stays on the tool, writes the output
   the `main` names, so the entry stays. A project with no `tsconfig*.json`
   gets a `tsconfig.json` that extends the root's; one that holds another
   `tsconfig*.json` keeps its configuration, which its test runner or linter
   may read. Rush publishes a project with `shouldPublish` or a version
   policy; Nx, Turborepo and moon publish a package whose `package.json` is
   not `private`.
7. **validate checks both modes.** `putnami validate` fails with
   `<tool>.sync_stale` when a `putnami.json` no longer matches the tool's
   graph, in import mode as in delegate mode.
8. **Leaving import removes what it declared.** `putnami init --from <tool>
   --mode delegate --apply` in a workspace that imported, with no import
   list, removes from the workspace config each extension the import
   declared that no `putnami.json` names after the sync.

## Rejected alternatives

- **Moving a target whose tool the extension does not run, with a
  warning.** A warning scrolls by; the check changes anyway.
- **Rush runs a kept target with `rush <command> --to`.** It needs a Rush
  install of the whole repository and a `command-line.json` command for each
  task, and reruns the upstream projects Putnami already ran.
- **Writing a project `tsconfig.json` wherever it is missing.** ts-jest and
  typed ESLint read `tsconfig.json` by default, so the new file changes how
  the project's own tools check it.

## Consequences

- A Rush or moon import moves fewer targets than before, and lists each one
  it keeps, so the adoption how-to states one rule instead of a behavior per
  tool.
- A Rush import needs `rush` on `PATH` for a target it keeps; a moon import,
  `moon`.
- A rerun of `putnami projects sync` after an upgrade rewrites the
  `putnami.json` of Rush and moon projects whose targets stay on the tool:
  `putnami validate` fails with `<tool>.sync_stale` until it is committed.
