# ADR 0013 — A merge queue can stand for the hosted gate

- **Status**: accepted
- **Scope**: `@putnami/contributor` `change finalize`, the repository policy
  and the `build` skill
- **Extends**: [ADR 0012](0012-an-authorized-ready-publication-can-turn-auto-merge-on.md),
  on when a hosted ready publication turns auto-merge on, and
  [ADR 0002](0002-build-and-versioned-workflow-record.md), on what a hosted
  publication requires

## Context

`change finalize --gate ci` marks the proposal ready, waits until every check
`verification.ciGate.checks` names passes on the pushed head, and only then
sends the `publication.autoMerge` request. A base branch that merges through a
merge queue runs its checks on the queue's commit, not on the proposal head.
The head carries at most a gating check that stays pending until the proposal
enters the queue, because a completed check there would let the code host
merge the proposal outside the queue. Only auto-merge, or a person, enters the
proposal into the queue. No check on the head can pass before auto-merge is
on, and the run sends the request only after one passes: the run waits until
its timeout and never sends it.

## Decision

1. The repository policy takes `verification.ciGate.mergeQueue`, the list of
   base branches that merge through a merge queue, such as `["main"]`. A
   merge queue is a rule of one branch, so the member names branches rather
   than the repository. Absent, `null` or empty, it names none. Any other
   value, an empty name or a repeated one stops the run before any write.
2. When the list names the `--base` of the run, `change finalize --gate ci`
   takes that branch's queue as the hosted gate: it marks the proposal ready
   and waits for no check on the head. It reads the proposal back once,
   at the pushed head and open, and reports `merge queue of <base>` as its
   gate.
3. The run then goes on as after passing hosted checks: the verification
   comment, the task's delivered transition and, with
   `publication.autoMerge`, the auto-merge request. Each step reads the
   proposal again and requires the pushed head, open, but no check state.
4. The queue's checks never stand for a workflow unit's native proof, as the
   hosted checks never did. A queue gate promotes no unit and writes no
   hosted receipt. A complete delivery of a task still needs a workflow
   record that the native verifier finds complete once ready.
5. For a base the list does not name, `--gate ci` waits for the named checks
   on the head as before. A local dossier publication ignores the member.

## Consequences

- A workspace that uses both `publication.autoMerge` and a merge queue can
  finalize with the hosted gate: the run ends with auto-merge on and the
  queue gates the merge.
- The run proves less before ready than with head checks: a failing check is
  seen by the queue, which removes the proposal from it, not by the run,
  which keeps the proposal ready. The repository's queue rules decide whether
  the proposal lands.
- `verification.ciGate.checks` still names the checks, which the hosted
  workflow verifier requires; with a queue, they name what the queue runs.
- A partial delivery under a queue gate publishes with no ready unit, as a
  partial whose units are not eligible does today. A unit stays at
  `candidate` with its local proof; the merged proposal is its next record.
- A list that names a base with no queue makes the run report a gate that
  does not exist; the list is the repository's statement, kept next to the
  branch rules it describes.

## Alternatives rejected

- A repository-wide boolean: a queue is a rule of one branch, so a boolean
  would report a queue gate toward a base that has none, and could not opt
  in one release branch alone.
- Let the proposals provider report that the base merges through a queue:
  collaboration v1 has no member for it, a new optional operation or a v2
  contract is a larger change, and the policy already names the hosted gate.
- Wait for the head's gating check: it stays pending until the proposal is in
  the queue, so the deadlock stays.
- Send the auto-merge request before the hosted checks with no policy member:
  on a branch without a queue, the code host would merge before the run saw
  the checks, and draft-on-failure would no longer hold.

## Reversal

Drop `verification.ciGate.mergeQueue` from the policy and the queue branch
from `change finalize`; a workspace that set it then finalizes with a local
dossier again.
