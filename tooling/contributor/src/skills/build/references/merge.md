# Merge

Merge turns the checked and proven candidate into a proposal ready for review.
The merge itself is the owner's, unless the mandate authorizes it.

## The dossier

Build the v2 dossier from the gate session and report, the review reports and
the qualification outputs, as the [records contract](records.md) describes,
then run `"$PUTNAMI_CLI" change verify --record <dossier.json>` on the final
tree. Only exit 0 produces `Putnami verified` at stage `ready`. Keep a
missing or blocked state explicit; never hide non-applicability behind an
empty array.

A workflow record is optional on a local dossier, and required to name a task
with the hosted gate ([workflow contract](workflow.md)). Before relying on it,
confirm `"$PUTNAMI_CLI" change verify --help` advertises `--workflow`: an
older verifier ignores unknown dossier members, so its exit 0 alone validates
no workflow.

## Finalize

Run `"$PUTNAMI_CLI" change finalize` with `--base`, `--branch`, `--title`,
`--body-file`, and the task pair `--task-source` and `--task-id` when a task
exists; never invent a task to use it.

- Local ready: `--verification-file <dossier.json>`. It needs a clean
  committed tree and rechecks the dossier and its gate.
- Hosted ready, when check selected it: `--gate ci`, plus `--workflow-file`
  when it names a task, unless a partial delivery changes only paths of
  projects that declare `featureAuthority.none` and author no feature. The
  command pushes, marks the proposal ready, waits for the named checks on the
  pushed commit, and returns it to draft when one fails. It can wait up to
  `verification.ciGate.timeoutMinutes`: run it in the background where the
  host bounds a command's duration; invoking it again keeps waiting. Toward a base
  the policy's `verification.ciGate.mergeQueue` lists, it waits for no check:
  the merge queue is the gate, and the end report names it. The queue
  promotes no unit: each unit stays at `candidate` with its local proof, and
  the merged proposal is its next record.
- Hosted ready that completes a task: the hosted checks never stand for the
  workflow's native proof. After the last commit, run one `--projects` gate on
  the committed tree, over the projects of the current requirements' specs.
  Its `spec-verification.json` must show each current requirement
  `verified`. Set the workflow's `proof` to that session,
  `.putnami/sessions/<id>/session.json`, and its sibling
  `spec-verification.json`, and set each current unit to `candidate`. The end
  report names this gate as one that followed no edit. Without that proof,
  publish the unit with `--partial-delivery`. A change that carries no
  feature requirement, such as an ADR with a `decisions/<id>.json` file, needs no such
  gate: its workflow names no source, its unit lists no requirement and sits
  at `candidate`, and the hosted checks complete it and close the task.
- One unit of a larger task: `--partial-delivery`, on the draft and the final
  call. It keeps a non-closing reference and leaves the task's state alone.

Neither mode runs a local gate. Once the outcome is complete and verified,
run the ready finalization in the same run; do not wait to be asked. Keep the
proposal a draft when the policy's `publication.draft` or the mandate says so.

With the policy's `publication.autoMerge` set to `"squash"`, the ready
finalization of a complete outcome also asks the proposals provider to turn
auto-merge on. Do not turn it on another way. A draft, a partial delivery and
a mandate that keeps the proposal a draft send no request. The command prints
`AUTO_MERGE=squash` or `AUTO_MERGE=off` with the provider's reason; a refusal
does not fail the delivery. Report which one in the end report.
A failed or `unresolved` external write leaves a resumable state, not a reason
to bypass a check or to repeat the write blindly. A local run ends with local
evidence and names the finalization it did not run.

## Push-only

A push-only run runs no `change finalize`, ready or draft. Its final gate is
the local `--projects` gate of its last edit, whatever the machine's load:
the hosted gate runs once the epic session publishes. Once check and prove
pass, it pushes the final head and ends. Its end report gives what the
epic session needs to publish: the branch, the pushed head, the base, the
title, the body as it stands, the task reference, the projects the gate
covered and whether the delivery is partial. The epic session reviews and gates
that head again in a local worktree before it publishes; the run's own
records stay where they are.

## Title and body

The title and body are the squash commit. When the repository uses
Conventional Commits, take the type from what the diff changes: `test` when
only tests change, `perf`, `refactor`, `docs`, `build`, `ci` or `chore` as
fits, `fix` for a user-visible defect corrected, `feat` for new behavior.

The body states the net change of the branch against its base, as it stands
now: why, then what changes for a reader of the history, in plain paragraphs,
without headings, file lists, proof or checklists. It names what the change
deletes and the P3 findings left unfixed. Rewrite it whole whenever a push
changes what the proposal does.

No commit, proposal, comment, code comment or document names an agent: no
co-author trailer for a model, no generated-with line, no model name. The
models that worked on a mission belong in its memory checkpoint.

A host that appends an attribution footer to the proposal body after each
write, such as a Claude Code cloud session, fails the readback of
`change finalize`, and the error names the footer. Do not edit the body again
from that host: it appends the footer to each write. Publish from a local
session.

## After ready

A merge or a deployment needs the mandate's authority; then go through the
normal required checks and provider operations, and re-evaluate after a head
or base change. Auto-merge that `publication.autoMerge` turned on is not a
merge by the run: the repository's rules decide when it lands. A provider that offers no `merge` leaves it to the
repository's own controls: say so. Ready, merged, published, deployed and
adopted are separate receipts; upstream merged is not downstream adopted.

The end report gives the stage reached, the proposal, the gate and proof
records, the tokens spent against the estimate, every impacted run beyond the
final one and every `--projects` gate that followed no edit, with its cause.
Unavailable tokens or costs stay unavailable.
