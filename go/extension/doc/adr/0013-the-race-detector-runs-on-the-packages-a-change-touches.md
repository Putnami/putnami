# ADR 0013 — The race detector runs on the packages a change touches

- **Status**: accepted
- **Date**: 2026-10-26
- **Scope**: `@putnami/go` (`go/extension`): the `test-race` step of `test`,
  the `test-race-exec` task and the `race-changed` flag

## Context

A data race reaches `main` when no run before merge executes the racing code
under `-race`. `--race` runs every package of a project under the detector,
which multiplies the test cost of every run, so nobody passes it by default. A
maintained list of packages that need `-race` is wrong as soon as someone adds
concurrency elsewhere, and nobody updates it.

The CLI already measures the change of an impacted run: the changed files and
the projects that own them. The race detector needs exactly that change, not
the impacted closure: a dependent's tests exercise the changed code through its
API, and its own packages did not change. Contract 16 lets a step gate on and
bind those files
([ADR 0015 of the extension protocol](../../../../protocols/extension/doc/adr/0015-a-step-reads-the-changed-files-from-contract-16.md)).

## Decision

**`test` runs `go test -race` on the packages a change touched, in each project
that owns a changed file, in a step after the test step.**

1. **The step.** `test-race` runs the `test-race-exec` task. It gates on the
   changed `**/*.go` and `**/testdata/**` files outside `.gen`, and binds them
   as the `changed-files` input, which keys it. A project reached only through
   a dependency, and a run without a diff, plan no race step.
2. **The packages.** The job maps each file to a package directory: a Go
   file's directory, and for a file under `testdata` the directory that holds
   `testdata`. It keeps the directories `go list -race -e` resolves to a
   package and runs `go test -race -json` over their import paths.
3. **After test, not beside it.** The step depends on `test`, so it runs only
   once the plain run passed, and never shares the host with it. A failing
   test step blocks it, so a test failure is reported once.
4. **The flags are read at run time.** `--race` replaces the step: the test
   step runs every package under the detector. `--no-race-changed`
   (`race-changed`, default `true`) turns it off. The step reads both and
   skips itself. A plan-time `if` cannot express them: an `if` that excludes a
   step also excludes every prerequisite only it depends on, and `test` is
   one.
5. **Its own deadline.** The task's deadline is 600000 ms, set per project with
   `tasks["test~test-race"].timeoutMs`. `--timeout` does not apply: the job
   passes `go test -timeout` below the deadline the CLI exports
   (`PUTNAMI_TASK_DEADLINE_MS`), by a fifth of it and at most 30 s, so the go
   tool names the hung test before the scheduler stops the task.
6. **The verdict alone.** The step reports no test counts, test cases,
   coverage or feature verification report, and writes no file. The test step
   reports those for the same command, and the live view and run summary fold
   every step of a command into one row, where a second set of counts would
   replace or add to the first. The race counts are a log line, and a race is
   a failure diagnostic. The task is `noOutput`: it shares the command output
   directory with `test-exec`.
7. **The host check.** Off darwin the race runtime builds through cgo, so the
   job sets `CGO_ENABLED=1` and checks that the compiler `go env CC` names is
   on `PATH`, reading `CC` with the go tool's quoting rules. On darwin it keeps
   the host's cgo setting: the darwin race runtime needs no cgo. Three failures
   name `--no-race-changed` and mark the result `host-unsupported`, which the
   CLI never replays from its failure cache: a refusal of `go list -race`, a
   compiler check that fails, and a failed run in which no test ran and the
   detector reported no race.

## Invariants

- The race step is planned only for a project that owns a matching changed
  file, binds the files its gate selects, keys on them, and runs after test;
  --race and --no-race-changed keep test planned; the test step's key does
  not move with the change
  (`TestChangedFilesGate_PlansOnlyWhereAProjectOwnsAMatchingFile`,
  `TestChangedFilesBinding_KeysTheStepAndOnlyTheStep`,
  `TestGoManifestPlansTheRaceStepOnTheTouchedProject`,
  `TestRaceStepIsGatedBoundAndKeyedOnTheChangedFiles`).
- The job runs the touched packages only, under `-race`, without coverage
  flags or `--timeout`, and reports no test counts
  (`TestRunRace_RunsTheTouchedPackagesUnderTheDetector`,
  `TestRaceTestArgs_TimeoutSitsBelowTheStepDeadline`,
  `TestRunRace_TheDetectorRunsOnTheTouchedPackageOnly`, real toolchain).
- A host that cannot run the detector fails naming the opt-out, and its
  failure is never replayed
  (`TestRunRace_RefusesAHostThatCannotRunTheDetector`,
  `TestRunRace_ARunInWhichNoTestRanIsAHostFailure`,
  `TestRunRace_ADarwinHostKeepsItsCgoSetting`,
  `TestFailureReplay_AnExtensionReportedTimeoutRunsAgain`).

## Consequences

- An impacted run on a Go change pays one `go test -race` over the touched
  packages per owning project, after its test step. The race build cache is
  separate from the ordinary one, so the first race run on a machine compiles
  the dependencies once more.
- Serial order lengthens the critical path of a touched project by the race
  run. In exchange, the two runs never contend for the host's CPU, memory,
  ports or package directories, and a failing test step costs no race run.
  On a 10-core macOS host at load 15, four touched packages of `tooling/cli`
  took 184 s wall and 437 s CPU under the detector, after a 403 s test step,
  and two of `go/extension` took 76 s after 135 s. The race run of
  `tooling/cli` peaked at 1.4 GiB resident memory, four times its test step.
- A Linux or Windows host without a C compiler fails the step until it
  installs one or passes `--no-race-changed`. The next run tries again.
- Every flag of the `test` command keys the step, including the coverage and
  benchmark flags it never reads, because the CLI keys every step of a command
  on the command's flags. A run with `--enforce-coverage` and one without miss
  each other's race entries.
- The step depends on the test step, so its key folds the test step's key: an
  edit that reruns the test step reruns the race step.
- With `--race` or `--no-race-changed`, the step is planned and skips itself.
- A race that only a dependent's tests reach is not run under the detector,
  unless that dependent's own files changed.

## Rejected alternatives

- **A maintained package list.** It drifts from the code that needs it.
- **`-race` on every package of the impacted closure.** It is `--race` on every
  run, the cost this decision avoids.
- **`-race` inside the test step.** Coverage and the race detector would share
  one binary, Go would switch coverage to `atomic` mode, and the test step's
  key would move with every change.
- **The race step beside the test step.** Two `go test` processes would run the
  same packages at once: twice the peak CPU and memory, with `-race` adding
  more, and shared ports and package directories.
- **`--timeout` for both runs.** A value tuned for the plain run times out the
  slower race run.
- **Skip the step when no C compiler resolves.** A silent skip on a CI host
  would let races merge with a passing run.
