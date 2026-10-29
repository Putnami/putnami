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
  projects that declare `featureAuthority.none` and author no feature. It
  pushes, marks the proposal ready, waits for the named checks on the pushed
  commit, and returns it to draft when one fails. It can wait up to
  `verification.ciGate.timeoutMinutes`: run it in the background where the
  host bounds a command's duration; invoking it again keeps waiting.
- One unit of a larger task: `--partial-delivery`, on the draft and the final
  call. It keeps a non-closing reference and leaves the task's state alone.

Neither mode runs a local gate. Once the outcome is complete and verified,
run the ready finalization in the same run; do not wait to be asked. Keep the
proposal a draft when the policy's `publication.draft` or the mandate says so.
A failed or `unresolved` external write leaves a resumable state, not a reason
to bypass a check or to repeat the write blindly. A local run ends with local
evidence and names the finalization it did not run.

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

## After ready

A merge or a deployment needs the mandate's authority; then go through the
normal required checks and provider operations, and re-evaluate after a head
or base change. A provider that offers no `merge` leaves it to the
repository's own controls: say so. Ready, merged, published, deployed and
adopted are separate receipts; upstream merged is not downstream adopted.

The end report gives the stage reached, the proposal, the gate and proof
records, the tokens spent against the estimate, every impacted run beyond the
final one and every `--projects` gate that followed no edit, with its cause.
Unavailable tokens or costs stay unavailable.
