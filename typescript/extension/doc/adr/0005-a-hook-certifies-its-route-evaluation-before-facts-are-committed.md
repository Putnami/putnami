# ADR 0005 — A hook certifies its route evaluation before facts are committed

- **Status**: accepted
- **Scope**: `@putnami/typescript` generation and `@putnami/application/http-routes`

## Context

The extension commits the aggregate of framework-declared route fragments to
`schema/http-routes.json`. A successful hook that emitted no fragment can mean
that it evaluated an empty contribution, or that it could not evaluate its
application. Retaining an older committed file while returning success lets
the scheduler cache those older facts for the current source. An absent
summary member from an independently upgraded hook is equally ambiguous.

The existing preBuild hook manifest declares execution, without identifying
which artifacts a hook may produce. The hook summary already permits custom
data. Route fact ownership remains with the framework producers; the extension
aggregates their fragments and commits the canonical result.

## Decision

Every discovered preBuild hook explicitly reports one `data.httpRoutes` object
in its summary: `{ "v": 1, "state": "complete" }`, or
`{ "v": 1, "state": "unavailable", "reason": "<text>" }`.
No other member is accepted in version 1. The reason is nonempty text and is
present only for unavailable evaluation. Missing, malformed, unsupported or
unavailable metadata fails generation and identifies the responsible hook.

Complete certifies that the hook evaluated its own framework-declared route
contribution for this invocation. It does not claim to observe arbitrary
runtime registrations or traffic. The helper belongs beside the fragment
writer in `@putnami/application/http-routes`. The generic hook runner adds no
default acknowledgement. The policy applies only to preBuild; configExtract
and other hook kinds retain their own result semantics.

The extension validates the returned evaluations before aggregating fragments,
advertising the aggregate, reconciling committed infra requirements, promoting
contracts or writing a successful generation result. Failure preserves the
committed route inventory and infra requirements. Scratch fragments may remain
after a failed producer, and are not current aggregate evidence. The next
invocation clears the scratch fragments through the normal lifecycle.

A readable project with zero discovered hooks is known absent. A dependency
without an extension manifest contributes no hook. Missing, unreadable or
malformed project metadata, and unreadable or malformed present extension
metadata, cannot establish absence. When every hook reports complete, no
fragment removes the stale committed route inventory; an explicit fragment
with `routes: []` commits a known-empty canonical inventory.

The scheduler's existing failed-result policy prevents successful cache
publication for unavailable evaluation. No new cache control or environment
input is added. Route declarations must be evaluable from their declared
inputs; deployment credentials are resolved and validated by runtime lifecycle
steps, separately from build evaluation.

## Consequences and migration

Hook providers and the TypeScript extension upgrade together. Older custom or
first-party preBuild hooks without the acknowledgement fail with a diagnostic
that names the missing member and the required provider upgrade or adaptation.
A newer generic hook SDK cannot certify a legacy producer automatically.
Application factories that require deployment secrets move credential
resolution and refusal to the native runtime lifecycle. Successful generation
still leaves generated drift visible for an intentional commit.

The HTTP routes protocol and extension manifest schema do not change. The
existing committed-route-inventory requirement in the
[toolchain spec](../../specs/typescript-project-toolchain.json) and its
[feature checks](../../putnami.features.json) protect the behavior. Solo and
batch diagnostics and failure isolation follow
[ADR 0001](0001-batched-phases-must-equal-solo-runs.md).

OAuth routes registered only during application warmup remain outside the
build producer's current contribution. This decision does not make that
inventory exhaustive for runtime registrations. That application boundary
must remain explicit in documentation and acceptance evidence.

## Alternatives

- Successful preservation with a warning admits stale facts into a current
  successful cache entry.
- Trusting statusless summaries permits older producers to remove or replace
  an inventory after an unevaluated factory.
- Adding defaults in the generic SDK falsely certifies legacy producers that
  use a newer SDK.
- A manifest declaration needs a published-schema change and still cannot
  identify an older undeclared producer. Version-name inference substitutes
  package release assumptions for evaluation evidence.

## Reversal conditions

Narrow the all-hook requirement only with a reviewed producer declaration and
a migration that remains closed to ambiguous legacy results. A reviewed native
cache/result contract may evolve failure semantics only if it preserves
per-source fact provenance and prevents partial aggregate publication.
