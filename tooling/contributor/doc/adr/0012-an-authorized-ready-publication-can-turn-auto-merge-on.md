# ADR 0012 — An authorized ready publication can turn auto-merge on

- **Status**: accepted
- **Scope**: `@putnami/contributor` `change finalize`, the repository policy
  and the `build` skill
- **Extends**: [ADR 0003](0003-build-publishes-its-proposal-by-default.md),
  on what a ready publication does

## Context

ADR 0003 ends a build run with a proposal ready for review, and keeps merge
behind explicit authority. A repository whose rules already decide when a
proposal merges (required reviews, required checks, a merge queue) then needs
one more step from a person: turning auto-merge on. Agents worked around that
step by calling the code host's own client after `change finalize`, outside
the providers, with no readback and no check that the head was the one the
gate verified.

## Decision

1. The repository policy takes `publication.autoMerge`: `"squash"` or
   `false`, `false` by default. Any other value stops the run before any
   write.
2. With `"squash"`, a ready `change finalize` of a complete outcome asks the
   bound proposals provider to turn auto-merge on, once, after the ready
   upsert, the gate, the verification comment and the task's delivered
   transition. The request names the published head.
3. `--draft`, a policy `publication.draft: true` and `--partial-delivery` send
   no request.
4. The request goes through a new optional operation of the proposals
   contract, `proposals.automerge`. It joins collaboration v1 as an optional
   capability, as ADR 0006 of the collaboration protocol allows, and
   `@putnami/github-collaboration` implements it.
5. A refusal, or a provider that does not offer the operation, is reported in
   the result and on stderr. It never fails the run: the proposal stays ready
   and the command exits 0.

## Consequences

- The run still merges nothing. Auto-merge hands the merge to the
  repository's own rules, which decide when, and whether, the proposal lands.
- The setting grants no authority: it acts only inside a publication the
  mandate already authorized, so the ADR 0003 rule that policy cannot grant
  authority stands.
- A head that moved after the gate is refused by the provider, so auto-merge
  never lands commits the gate did not see.
- A provider without the operation keeps working: the run reports that it
  sent no request.

## Alternatives rejected

- A `--auto-merge` flag on `change finalize`: the choice belongs to the
  repository, and a flag lets each run decide differently.
- Calling the code host's client from the skill: it bypasses the provider,
  its readback and its reconcile rules, and works on one code host only.
- Failing the run on a refusal: the delivery is complete and published; a
  missing auto-merge is one person's click, not a failed delivery.
- Adding a member to `proposals.merge`: v1 is frozen, and a new member needs
  v2; an optional operation does not.

## Reversal

Drop `publication.autoMerge` from the policy and the request from
`change finalize`. The `proposals.automerge` operation can stay: it is
optional, and no caller then sends it.
