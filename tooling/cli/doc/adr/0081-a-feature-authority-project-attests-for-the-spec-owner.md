# ADR 0081 — A feature-authority project attests for the spec owner

- **Status**: accepted
- **Date**: 2026-10-27
- **Scope**: `@putnami/cli` (`internal/specgate`), feature
  `cli/job-planning-execution`, requirement `spec-gate-evidence-scope`
- **Amends**: §2 of [ADR 0026](0026-spec-gate-evidence-scope.md). The
  dependency closure and the direct dependents stay candidates.

## Context

The CLI's install and smoke script tests take 245 s and ran inside the CLI's
`test~test`, so every change to the CLI ran them. They read the scripts and the
install documentation, not the CLI's code. They prove 38 checks of 4 CLI
features.

A project declares cache-key inputs per command, never per task, so the tests
move to a project of their own whose key holds the files they read. That project
must not depend on the CLI: a dependency edge folds the CLI's sources into its
key, and the tests would run on every CLI change again.

Under a narrowed run, the spec gate recovers a missing proof only from the
owner's dependency closure and its direct dependents (ADR 0026 §2). A project
with no edge to the CLI is neither, so the CLI's 38 checks would read
check-not-observed.

## Decision

A project whose `featureAuthority.owner` names a feature that the spec owner's
criteria projection declares is a third candidate class. Its captured report is
read from its own `test~test` cache entry under the same rules as the two other
classes: lazy lookup, provenance containment, and a planned report producer
leaves the set.

A declaration that names a feature the owner does not declare admits nothing.

## Consequences

- The set is bounded by explicit declarations, not by the graph.
- `featureAuthority.owner` already has to name an authored feature (D-025), so
  the declaration that admits the project is the one the coverage gate reviews.
- Rejected: a dependency edge from the new project to the CLI. The tests would
  still run on every CLI change.
- Rejected: moving the 38 checks into a feature the new project owns. It splits
  4 CLI features for a test-layout reason.
- Proof: `TestRecoveredObservationResolvesAFeatureAuthorityAttester` and
  `TestAFeatureAuthorityOfAnotherFeatureStaysOutOfScope` in
  `tooling/cli/internal/specgate/recover_test.go`.
