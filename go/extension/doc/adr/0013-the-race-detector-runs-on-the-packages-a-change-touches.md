# ADR 0013 — The race detector runs on the packages a change touches

- **Status**: accepted
- **Scope**: `@putnami/go` (`go/extension`), with the CLI planner fact it reads

## Context

A data race reaches `main` when no run before merge executes the racing code
under `-race`. `--race` runs every package of a project under the detector,
which multiplies the test cost of every run, so nobody passes it by default. A
maintained list of packages that need `-race` is wrong as soon as someone adds
concurrency elsewhere, and nobody updates it.

The CLI already measures the change of an impacted run: the changed files and
the projects that own them (impact seeds). The race detector needs exactly
that change, not the impacted closure: a dependent's tests exercise the
changed code through its API, and its own packages did not change.

## Decision

**`test` runs `go test -race` on the packages a change touched, in each project
that owns a changed file, in a step beside the test run.**

- The step is `test-race`, running the `test-race-exec` task. It depends on
  `describe` and `test-env`, like `test`, so excluding it never drops `test`,
  and the test environment's teardown waits for both.
- It is planned only when the run measured a diff and the project owns a
  changed `**/*.go` or `**/testdata/**` file (the `changedFiles` activation
  gate). A project reached only through a dependency owns no changed file. A
  run without a diff (`--all`, `--projects`, the specification gate) plans no
  race step.
- The CLI binds the project's changed files, project-relative, sorted and
  unique, as the `changed-files` input (`from: context`,
  `path: selection.changedFiles`). The bound files key the step, so a stored
  verdict answers the same change only.
- The job maps the files to package directories: a Go file's directory, and
  for a file under `testdata` the directory that holds `testdata`. It keeps
  the ones `go list -race -e` resolves to a package and runs
  `go test -race -json` over their import paths, with `CGO_ENABLED=1`.
- The run measures no coverage, runs no benchmark, writes no file and
  publishes no feature verification report. The task is `noOutput`: it shares
  the command output directory with `test-exec`, and an entry that captured
  that directory would restore test's coverage files.
- `--race` replaces the step (`if: params.race-changed && !params.race`).
  `--no-race-changed` (`race-changed`, default `true`) turns it off.
- A host that cannot run the detector fails the step with a diagnostic that
  names `--no-race-changed`: `go list -race` refuses an unsupported platform,
  and off darwin the job checks that the compiler `go env CC` names is on
  `PATH`. Since Go 1.20, the darwin race runtime needs no cgo.
- The manifest declares `cliContract` 16, the rung of the changed-files gate
  and binding. An older CLI refuses the manifest instead of planning the step
  on every run without its files.

## Invariants

- The race step is planned only for a project that owns a matching changed
  file, binds that project's files, and keys on them, while the test step's
  key does not move with the change
  (`TestChangedFilesGate_PlansOnlyWhereAProjectOwnsAMatchingFile`,
  `TestChangedFilesBinding_KeysTheStepAndOnlyTheStep`,
  `TestGoManifestPlansTheRaceStepOnTheTouchedProject`).
- The job runs the touched packages only, under `-race` and without coverage
  flags (`TestRunRace_RunsTheTouchedPackagesUnderTheDetector`,
  `TestRunRace_TheDetectorRunsOnTheTouchedPackageOnly`, real toolchain).
- A host that cannot run the detector fails before `go test` starts, naming
  the opt-out (`TestRunRace_RefusesAHostThatCannotRunTheDetector`).

## Consequences

- An impacted run on a Go change pays one `go test -race` over the touched
  packages per owning project. The race build cache is separate from the
  ordinary one, so the first race run on a machine compiles the dependencies
  once more.
- A Linux or Windows host without a C compiler fails the step until it installs
  one or passes `--no-race-changed`.
- `--no-race-changed` moves the test step's cache key too: the CLI keys every
  step of a command on the command's flags.
- A race that only a dependent's tests reach is not run under the detector,
  unless that dependent's own files changed.

## Rejected alternatives

- **A maintained package list.** It drifts from the code that needs it.
- **`-race` on every package of the impacted closure.** It is `--race` on every
  run, the cost this decision avoids.
- **`-race` inside the test step.** Coverage and the race detector would share
  one binary, Go would switch coverage to `atomic` mode, and the test step's
  key would move with every change.
- **Skip the step when no C compiler resolves.** A silent skip on a CI host
  would let races merge with a passing run.
