# ADR 0084 — A first generation warns outside CI

- **Status**: accepted
- **Date**: 2026-11-04
- **Amends**: [ADR 0034](0034-a-declared-output-polices-its-own-drift.md),
  [`protocols/extension` ADR 0004](../../../../protocols/extension/doc/adr/0004-a-declared-output-may-police-its-own-drift.md)
- **Scope**: `@putnami/cli` (`internal/jobs/scheduler`, `internal/engine`),
  requirement `generated-output-drift` of feature
  `cli/job-planning-execution`

## Context

Under ADR 0034, a task whose `drift: "fail"` output differs from the bytes
present before the run fails. An output absent before the run differs too. So
`putnami init --project webapp --extension ts` followed by `putnami build
webapp` exits 1 on `webapp/schema/http-routes.json`, and a second run exits 0.
`typescript-server` behaves the same. A template cannot ship the file: its
`digest` covers routes that hold the project name.

The diagnostic alone cannot tell the two cases apart. An output that held no
bytes is either a new project's first generation or an output a contributor
forgot to commit. The first must not fail. The second must, or a pull request
can merge without its generated output.

## Decision

An output whose reference is empty and that holds bytes after the task is a
**first generation**. Under `fail`:

- Outside CI, the verdict is a warning that says to commit the output. The task
  keeps its status.
- In CI or a hosted run, the verdict is the error ADR 0034 defines. That
  checkout is what the repository holds, so the output is one nobody committed.

The rule holds for an executed task and for a restored hit. `warn` is unchanged.
An output that held any file before the run is not a first generation: a file
added beside committed ones stays drift.

The engine computes `cachekey.InCI() || runcredential.Hosted()` once and passes
it as `SchedulerConfig.FirstGenerationFails`. `InCI` reads a non-blank `CI`
environment variable. `Hosted` reports a run that holds the hosted credential,
so the rule does not depend on what a runner exports.

## Consequences

- A new project builds and serves on the first run, on a developer's machine.
- A generated output missing from a pull request still fails its CI run.
- The default renderers print a drift warning at the end of the session, so a
  `warn` verdict is visible too. In a terminal, `putnami serve` prints it when
  the session ends; until then its row shows the warning count.
- A CI job that scaffolds a project and builds it without committing fails on
  that build. It has no committed bytes to compare with, and the verdict says to
  commit them.
- The variable changes the verdict, never the reference: neither reaches a cache
  key, an entry or the failure cache, as in ADR 0034. A local hit and a CI hit
  of one entry reach their own verdict.

## Alternatives considered

- **Treat every absent output as a success.** Rejected: a pull request that
  forgets its generated output would pass CI.
- **Run the generators inside `init`.** Rejected: it needs the install and the
  extension runtime, and it still leaves `build` failing for a project created
  by hand.
- **Read `CI` in the scheduler.** Rejected: tests would mutate the process
  environment, and the other engine inputs arrive through its configuration.
