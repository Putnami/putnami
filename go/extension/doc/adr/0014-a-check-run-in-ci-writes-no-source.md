# ADR 0014 — A check run in CI writes no source

- **Status**: accepted
- **Date**: 2026-10-27
- **Scope**: `@putnami/go` (`go/extension`): the `fix` flag of `lint`, the
  `tidy` flag and the `tidy-check` step of `build`, and the
  `build-tidy-check` task

## Context

`lint` runs `golangci-lint --fix` and `build` runs `go mod tidy` by default.
Both rewrite sources. In CI that has two costs:

- A fix applied on the runner is discarded. The gate passes on a tree that
  differs from the committed one.
- `lint-golangci-fix` writes the project's sources, so the planner orders
  every reader of the project after it.

Locally, the automatic fix is what a developer or an agent wants: a fixable
issue gives a green run and a diff to commit.

## Decision

**With `CI` set, `lint` and `build` schedule no step that writes sources.**

1. `lint --fix` declares `ciDefault: false`
   ([protocol ADR 0016](../../../../protocols/extension/doc/adr/0016-a-flag-may-declare-a-ci-default.md)).
   In CI, lint runs the read-only golangci-lint step. A failure carries a hint
   that names `putnami lint --fix --projects <project>`.
2. `build --tidy` is a new flag, `true` by default and `false` with `CI` set.
   Off, build runs `build-tidy-check` instead of `build-tidy`: `go mod tidy
   -diff` reports the change tidy would make and writes nothing. A diff fails
   the task, carries the diff, and names
   `putnami build --tidy --projects <project>`.
3. With module downloads disabled, the check is the same no-op as tidy. A check
   that cannot resolve the module graph is a warning and an uncacheable skip,
   like a failed tidy.

## Consequences

- In CI, a fixable lint finding or an untidy `go.mod` fails the gate. The same
  command run locally fixes it and passes.
- `serve`, `run`, `package` and `config-extract` still tidy. They are not
  check runs.
- `--fix` and `--tidy` key only the tasks that declare them, so CI and local
  runs share the cache entries of every other lint and build task.
