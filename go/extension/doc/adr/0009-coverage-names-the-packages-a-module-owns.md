# ADR 0009 — Coverage names the packages a module owns, never a directory pattern

- **Status**: accepted
- **Scope**: `@putnami/go` (`go/extension`)

## Context

`go test` matches `-coverpkg` by prefix against the test binary's build list,
not by module. A module nested under the project directory, with its own
`go.mod` in `go.work`, matches `-coverpkg=./...` as soon as a project test
imports it. Its blocks then land in the project's profile. A module-path
pattern (`example.com/a/...`) matches the same way. A project that imports its
own generated client under `clients/go` reports the client's coverage mixed
into its own.

## Decision

**A project's coverage instruments the packages its module owns, listed
explicitly, and every attribution uses that list.**

- Before `go test`, the job runs
  `go list -e -f '{{if not .Error}}{{.ImportPath}}{{end}}' ./...` in the
  project directory with the `go test` environment, and passes the sorted
  result as `-coverpkg`. The enumeration stops at the module boundary. `-e`
  keeps an empty pattern or an ignored directory from failing the listing; a
  package with a syntax error is still listed and `go test` reports it.
- The test-selection pattern (`./...` solo, `./a/...` in a batch) is a
  separate argument and is unchanged.
- In a batch, each member lists its own packages in its own directory with its
  own environment. `-coverpkg` is the sorted union under `batch` scope and the
  member's list under `project` scope
  ([ADR 0008](0008-coverage-scope-decides-whose-tests-count.md)). A listing
  failure fails that member alone with `GO_TEST_BATCH_PREPARE` before any
  `go test` starts. The listing runs coverage on or off, because output
  attribution needs it.
- Attribution is exact membership: a profile block, a test event and its
  transcript context belong to the member that listed the package. An unknown
  package fails the profile split closed and leaves the output unattributed.
  No longest-prefix rule exists, so a nested module is never charged to its
  parent.
- `runGoCommand` is the one subprocess seam for solo `go test`, batch
  `go test` and every `go list`. `go list` stdout is data; its stderr returns
  only with a failure and never reads as a package.
- An empty list keeps `-cover` without `-coverpkg`.

## Invariants

- On the solo path and in every batch shape, a project's profile holds no line
  of a nested module, and the nested module's profile holds no line of the
  parent (`TestRunNestedModuleStaysOutOfTheProviderProfile`, real toolchain).
- `go list` runs with the arguments above, in the module directory, with the
  caller's environment; its result is sorted with blank lines dropped
  (`TestListModulePackagesSortsWhatGoListOwns`); a failure carries what go said
  (`TestListModulePackagesFailureCarriesWhatGoSaid`).

## Consequences

- Every test run pays one `go list ./...` per project.
- A threshold or workspace floor lowered to absorb nested-module dilution is
  restored only from newly measured evidence.
- The `-coverpkg` value changes when packages are added or removed;
  `test-exec` already keys on sources, so no cache-key input is added.

## Rejected alternatives

- **A module-path pattern.** `-coverpkg` patterns are prefix matches whatever
  their form.
- **Name-based exclusions (`clients/go`, `*.gen.go`).** They decide by name,
  not ownership.
- **`GOWORK=off` for the test process.** Workspace siblings stop resolving.
- **Filter the profile after the run.** The nested packages stay instrumented,
  and the percentage `go test` prints stays diluted.
- **Honour a nested project's `coverage: false`.** One project's measurement
  would depend on another's configuration.
