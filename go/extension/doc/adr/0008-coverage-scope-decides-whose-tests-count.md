# ADR 0008 — `coverage-scope` decides whose tests count toward a batched project's coverage

- **Status**: accepted
- **Scope**: `@putnami/go` (`go/extension`)

## Context

`test-exec` is batchable: ready Go projects under one `go.work` root with the
same effective flags and test environment share one `go test -json`
invocation ([Workspace batching](../test.md#workspace-batching)). With
coverage on (the default), that invocation instruments the union of every
member's packages and writes one profile, which `splitBatchCoverage` splits
into each member's `coverage.out`.

The union has two costs. Member `b`'s tests instrument member `a`, so the
lines of `a` that `b` reaches land in `a`'s profile: `a`'s cached
`coverage.out` then depends on batch mates absent from its key, and a
threshold can pass in one batch and fail alone. And every distinct member set
instruments a different package set, so the Go build cache keeps one
instrumented copy of shared packages per set.

A profile cannot be repaired afterwards: a block records a hit count, never
which test binary produced it.

## Decision

The `test` option `coverage-scope` takes `batch` (default) or `project`.

| Scope | `go test` processes per group | `-coverpkg` for member `a` | Whose tests count for `a` |
|---|---|---|---|
| `batch` | one, over every member | union of the members' owned packages | every member's |
| `project` | one per member, sequential | `a`'s owned packages only | `a`'s own |

The owned-package list comes from
[ADR 0009](0009-coverage-names-the-packages-a-module-owns.md). Under `project`
scope the group keeps one scheduler slot, one extension process, one scratch
and spec-fragment directory, and the Go build cache; only the `go test`
process splits. Each member's process runs from the `go.work` root and selects
its tests with `./a/...`.

- `planBatchInvocations` decides the processes. With coverage off, both scopes
  keep one union process.
- Per-member processes run one after another: the group holds one scheduler
  slot, and each `go test` already spreads its packages over the CPUs.
- A member's scratch profile is named `coverage-<digest of the project id>.out`,
  never after its position, so its arguments ignore batch mates and order.
- Output attribution, transcript emission and the unattributed-failure
  fallback run per process.
- `coverage-scope` is a `params` input of `test-exec`, so it keys the task, and
  the scheduler batch key hashes every resolved parameter, so two scopes never
  share a batch.
- Any other value fails every member with `GO_TEST_BATCH_PREPARE` before a
  `go` process starts. The solo path fails the same way.

## Invariants

- Under `project` scope, a member's `coverage.out` has the same bytes alone
  and in any batch
  (`TestRunBatchProjectScopeProfileIsIdenticalAloneAndInABatch`).
- Under `project` scope, a member's arguments are the same in every batch and
  position, except the scratch root
  (`TestRunBatchProjectScopeArgumentsIgnoreBatchMates`).
- Under `batch` scope, the union process and split are unchanged
  (`TestRunBatchDefaultScopeKeepsTheUnionSplit`).
- `coverage-scope` keys `test-exec` (`TestCoverageScopeKeysTheTestTask`), and
  an unknown value never starts `go test` (`TestUnknownCoverageScopeFailsClosed`).

## Consequences

- Under `project` scope a project counts its own tests only, as a solo run
  does; a threshold that relied on another project's tests measures lower.
- Under `project` scope a group pays package loading and linking once per
  member. Build-cache reuse of instrumented objects across batches is
  expected, not measured: measure it before making `project` the default,
  which is a change to this record.
- The TypeScript extension tests each batch member in its own process, so it
  needs no equivalent option.

## Rejected alternatives

- **Filter the union profile after the run.** A block does not name the test
  that hit it, and the objects stay instrumented for the union.
- **Drop `-coverpkg`.** A package reached from another package of the same
  project would count as uncovered.
- **Run per-member processes concurrently.** It exceeds the one scheduler slot.
- **Never batch tests that measure coverage.** Coverage is on by default, so
  batching would disappear.
