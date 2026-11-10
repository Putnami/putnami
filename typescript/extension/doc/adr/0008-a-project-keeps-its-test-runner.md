# ADR 0008 — A project keeps its test runner

- **Status**: accepted
- **Scope**: `@putnami/typescript` (`typescript/extension`), the `test-run` task;
  the import modes of `@putnami/nx` and `@putnami/turbo`

## Context

`putnami test` ran `bun test` and nothing else. A Node repository imported
from Nx or Turborepo tests with Vitest or Jest almost everywhere, so
`putnami init --from nx|turbo --mode import` left every one of those test
targets on the monorepo tool: the import moved the graph and the cache of
build and lint, and almost no test.

Moving such a suite to `bun test` is not an import. Bun's runner implements
most of Jest's API but not its module mocking, its environments or Vitest's
plugins, and a suite that passes under Vitest can fail, or pass for another
reason, under Bun. The import contract says no runner changes silently.

## Decision

A TypeScript project chooses its test runner: `bun`, `vitest` or `jest`.
The choice is the `runner` parameter of the test command, set with
`--runner` or in `options["@putnami/typescript:test"].runner` of its
`putnami.json`, else the runner its `package.json` `test` script runs, else
Bun. The issue that asked for the choice named
`options["@putnami/typescript"].test.runner`; that layer is projected flat
into the parameters of every command, where `test` is the `--test` name
filter, so the choice is a parameter of its own.

The task keeps one contract for every runner. It runs the bin entry the
runner's package declares, found in `node_modules` of the project or the
workspace, with the `node` on `PATH`, and fails with a message that names
`node` when there is none. A `node_modules/.bin` shim is a shell script or a
Windows wrapper, which `node` cannot run.

It writes `results.junit.xml` and `lcov.info` into its output: Vitest through
its own `junit` reporter and `lcov` coverage reporter, Jest through its
`--json` result, which the extension converts to JUnit, so a project needs no
reporter package, and its `lcov` coverage reporter. The summary, the case
records and `--enforce-coverage` read those two files whatever wrote them.
The runner parameter, the runner's configuration and setup files and
`__tests__` directories join the task's cache key.

The runner keeps reading its own configuration. Putnami passes the flags that
place its reports and select the work it was asked for, and nothing that a
`vitest.config.*` or `jest.config.*` already says.

## Consequences

- An Nx or Turborepo import maps a Vitest or Jest test target to
  `@putnami/typescript:test`: a script that runs `vitest` or `jest`, or the
  `@nx/vite:test` and `@nx/jest:jest` executors. The Nx sync records the
  runner in `options["@putnami/typescript:test"].runner`, because an executor
  leaves no script to read it from.
- Bun-only flags (`--timeout`, `--concurrent`, `--parallel`, `--todo`,
  `--only`, `--rerun-each`) do not reach Vitest or Jest: their configuration
  owns those settings.
- The `types` step checks a Vitest or Jest project with its own tsconfig's
  compiler options, not the types phase's Bun defaults, so an import does not
  change how its tests are type-checked.
- A project whose runner is Vitest or Jest needs `node` on the host. The rule
  that test needs no `node` holds for Bun projects only.
