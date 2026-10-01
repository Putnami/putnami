# ADR 0004 — An unavailable binding is no source claim, not an error

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/features` (`protocols/features`)

## Context

`source-v1` computes a binding from the files Git tracks and the untracked
files Git does not ignore. A workspace root with no Git program on `PATH`, or
outside every Git repository, has no such input set. The scheduler stamps that
state as `sourceBindingUnavailable: true` with an empty `sourceBinding`.

Both describe-time producers, `go.putnami.dev/app` and `@putnami/application`,
refuse that stamp. Every build, test and serve of a workload then fails in a
directory that is not a repository, although none of them publishes a source
claim. The Capability Manifest carries no binding at all: only the feature
evidence document does (ADR 0003, decision 6).

## Decision

An unavailable binding means "this build makes no source claim".

1. A root Git does not manage has no `source-v1` binding. The scheduler stamps
   every capability package of that root with an empty `sourceBinding`, the
   marker, and its `sourceRoot`. It sets the marker for that reason only: a
   binding that fails inside a repository is stamped empty with the marker
   unset, and stays an error.
2. A producer that reads the marker emits the same Capability Manifest bytes
   as for a bound root, writes no feature evidence document, and removes a
   stale one from its scratch output.
3. The shape stays strict. A marked package that carries a binding is
   malformed. A package with neither a valid binding nor the marker is
   malformed. A stamp that mixes marked and bound packages is malformed. A
   missing `sourceRoot` is malformed in both states.
4. Go and TypeScript apply the same rule. One pair of stamps and one manifest
   under `protocols/capabilities/fixtures/v2/equivalence/` pins both.
5. A reader with no evidence document reports the evidence as unavailable. A
   publication or deployment refuses the root and names Git.
6. A task cache key carries the source state of the root, so an output built
   with no source claim never serves a repository, and the reverse. A key
   computed inside a repository is unchanged.

## Rejected alternatives

- **Compute a binding without Git** by walking the file system. Without Git
  the walk cannot apply ignore rules, so the same tree would bind differently
  with and without Git, and a binding would claim a source state no reader can
  recompute.
- **Keep failing the build and require Git.** The build publishes nothing
  that needs a binding, so the requirement buys no safety and blocks the first
  run of a new workspace.
- **Stamp a placeholder binding** such as an all-zero digest. A reader would
  compare it as a real value and report stale evidence instead of unavailable
  evidence.
- **Mark every failed binding unavailable.** A conflicted index or an
  unreadable repository would then build silently with no evidence. Those stay
  errors.

## Consequences

- Feature evidence mappings are not validated in a build with no source
  claim: the producer returns before it resolves them. The same project fails
  on a wrong mapping as soon as it builds inside a repository.
- A project with committed evidence keeps that committed file when it builds
  outside a repository: a producer removes only its scratch output. A reader
  cannot recompute the binding there, so it reports that evidence as stale
  with `features.source_binding_unavailable`.
- Task outputs are cached per source state. A workspace that gains a
  repository rebuilds once.
- Two producers and the scheduler now share one more state. A change to the
  stamp shape must update both producers and the shared fixtures together.
