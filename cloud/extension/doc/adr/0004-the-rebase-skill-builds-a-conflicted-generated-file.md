# ADR 0004 — The rebase skill builds a conflicted generated file

- **Status**: accepted
- **Date**: 2026-11-26
- **Scope**: the `rebase` skill of `@putnami/cloud`
  (`agent-src/skills/rebase/SKILL.md`)

## Context

The rebase skill replays a pull request on its base and proposes the result.
It ran nothing from the repository: it ran git with hooks off, and the gate
built and tested the proposed series. A conflict was resolved by hand.

Some conflicted files are generated, such as a committed
`schema/http-routes.json`. Two pull requests that each change a route
conflict on its digest line. A hand merge produces bytes that neither side's
sources generate. The build then fails with `generated-output-drift`, and the
rebase cannot land.

## Decision

1. A conflicted generated file is never merged by hand. The skill first
   resolves and stages the conflicted source files of the replayed commit.
   Then it builds the file's project once with
   `putnami build --projects <project>` and stages the regenerated file.
2. A build that fails only with `generated-output-drift` counts as the
   rewrite. Any other failure is reported as `command_failed`.
3. The skill builds nothing else, and runs no test. The gate still checks
   the proposed series.

## Consequences

- The rebase skill now runs repository code: the workspace's CLI and the
  project's build, at the base plus the replayed commits of the pull request.
  The report token stays in reach of that code, as it already is of the
  repository files the skill reads. In a Putnami routine, `routine-build`
  already builds the same repository with the same tools. Git still runs
  with hooks off.
- A routine that cannot run the CLI, such as one set up for the rebase alone
  with no toolchain, reports `capability_missing` instead of resolving the
  file by hand.
- The putnami-routine skill's statement of what the rebase skill needs now
  names the build.
