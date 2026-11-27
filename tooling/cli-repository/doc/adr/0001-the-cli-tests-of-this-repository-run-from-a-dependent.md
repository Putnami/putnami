# ADR 0001 — The CLI's tests of this repository run from a dependent

- **Status**: accepted
- **Date**: 2026-11-26
- **Scope**: `@putnami/cli`, `@putnami/cli-repository`

## Context

A cache key is per project and per command, never per task. About 200 tests
of `@putnami/cli` load this repository's own files with the CLI's code: every
project and extension manifest, the decisions, the templates, the agent
content, the first-party extension sources and the API contracts. They kept
the CLI's test inputs at 119 patterns over the whole repository, so a one-line
`putnami.json` change re-ran its suite of about 5,000 tests.

Most of them check unexported code, so they cannot move to another module.
And a spec proof is reported only by the project whose tree declares it: the
SDK's spec report drops a fragment whose file is outside the reporting
project.

## Decision

- A test that reads this repository starts with `repotest.Require(t)`, which
  skips it unless `PUTNAMI_CLI_REPOSITORY_TESTS=1`. It skips before
  `spectest.Proves`, so the CLI's suite reports no proof for it.
- Every helper function of the CLI's tests that finds the repository root
  calls `repotest.Reads(t)` first, which fails the test when the variable is unset.
  An unmarked test that reaches the repository fails in the CLI's suite
  instead of passing on files its key does not hold. The consequences below
  name the two ways this does not guard.
- `@putnami/cli-repository` finds the marked tests by parsing the CLI's
  `_test.go` files that the host compiles, runs them with `go test` over their
  packages with the variable set, under the race detector when it runs under
  it, and reports each one as a subtest.
- The marked tests write their spec fragments to a directory of the driver's
  own. Each subtest declares the checks its marked test proved, and passes,
  fails or skips with it. A direct dependent is in the evidence scope of the
  CLI's features, so they keep their evidence. A check another test of the CLI
  also proves keeps that proof alone: a check has one proof.
- The project declares the repository files the marked tests read, and
  `git:../cli/**`. The CLI keeps its own tree and the two protocol corpora its
  conformance tests reproduce. `TestTheCLISuiteKeysOnItsOwnTree` refuses a CLI
  test input outside its tree.

## Consequences

- A change to one project's `putnami.json` runs the marked tests, about
  1.5 minutes, and not the CLI's suite.
- A change to the CLI's sources runs both suites; the project adds about ten
  tasks to such a plan.
- A test that reads the repository and is not marked fails in the CLI's suite
  once it reaches a helper function that finds the root. Two other ways are
  not guarded, and review keeps them marked: the `repoRoot` globals that a
  `TestMain` sets in the end-to-end packages, and a relative path from a
  package directory to a sibling project.
- The race detector reaches the marked tests only when this project's tests
  run under it: its own race step, which runs when its own Go files change,
  or a run with `--race`. A change to a CLI package no longer races the marked
  tests of that package, as the CLI's `race-changed` step did. Racing them on
  every run was rejected: in the impacted gate at 15b25be6
  (`lint,test,build,validate --impacted --enforce-coverage`, a 4 vCPU
  container), this project's `test~test` took 87 s and its `test~test-race`
  393 s. Racing only the marked tests of the changed CLI packages stays a
  follow-up: the run would have to read the change, which its key does not
  hold.
