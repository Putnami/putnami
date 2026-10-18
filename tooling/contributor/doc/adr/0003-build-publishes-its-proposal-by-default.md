# ADR 0003 — Build publishes its proposal by default

- **Status**: accepted
- **Scope**: `@putnami/contributor` `build` skill and its loop
- **Supersedes in part**: [ADR 0002](0002-build-and-versioned-workflow-record.md), on publication authority

## Context

ADR 0002 left every external action to an explicit mandate. A request such
as "build X" therefore ended at a local `Putnami verified` verdict: the gate
passed, the dossier was valid, and no pull request existed until the owner
asked again. The former `fix` entry published by default, so the
consolidation removed a step owners relied on, without a decision that said
so.

## Decision

A request to build an outcome authorizes the publication of the run's own
proposal through `putnami change finalize`:

1. Once the plan is settled, before the first edit, open it as a draft with
   `--draft`.
2. At each milestone (a delivery unit, a phase, or a repair round whose
   `--projects` gate passes), commit the finished edits and push them with
   `--draft`, rewriting the body when the net change moved.
3. Once the outcome is complete, the final local gate passes and the dossier
   is verified, mark it ready for review with `--verification-file` in the
   same run.

`--local`, a mandate that says to stay local, `--dry-run`, or a workspace
with no bound proposals provider keeps the run local. Policy
`publication.draft` still keeps the final proposal a draft. Backlog selection,
task claim, merge and deployment keep requiring explicit authority.

## Consequences

- A clean local gate no longer ends a default run: the run ends with a proposal
  ready for review and its read-back receipt.
- Reviewers see the work from the plan on, and Putnami CI holds runs on the
  draft, so milestone pushes cost no hosted run.
- Native finalization is unchanged: it still refuses a dirty tree for ready,
  runs no local gate, and checks proof, English and attribution.

## Alternatives rejected

- A policy option that grants publication: policy cannot grant authority
  ([repository policy](../../src/skills/build/references/policy.md)).
- Publishing only at the end: readers lose the draft from the plan on, and a
  host interruption leaves no pushed checkpoint.

## Reversal

Restore the ADR 0002 wording in `build` and its loop, and drop `--local`.
