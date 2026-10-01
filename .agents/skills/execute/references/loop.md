# Execution and recovery

## Begin from a mandate

Read the current task/intention, repository instructions and any existing plan,
branch, proposal and evidence. Resolve the base from the active workspace; do
not rename or switch an allocated checkout merely to fit a naming recipe.
Record acceptance, scope, reserved decisions and authorized external actions in
an ignored `.context/execute/<run>/` directory. Use an existing mission ID when
one exists; do not create a task to obtain one. When memory is bound, read the
mission and the affected projects' context first (collaboration.md).

`fix` selects/claims a task; `epic` supplies the dependency plan. They then
enter this loop instead of nesting separate finalization cycles. A task that
is done or canceled may have transferred scope: follow the explicit successor
and verify what remains before implementing. A completed worker or ready
proposal is an intermediate state when the mandate includes a usable deployed
capability.

Planning is proportional: identify the smallest executable vertical, mechanisms
to reuse, real dependencies and proof entrypoints. Involve affected scope owners
before fixing the decomposition (scopes.md). Separate design agreement from
realization review. Do not build a parallel architecture inventory.

When publication is authorized, open the proposal as a draft once the plan is
settled, before the first edit: run
`bash .agents/skills/fix/scripts/finalize-pr.sh --draft` with the planned
title, a body that states the intended change, and the task reference. The
helper adds an empty commit carrying the title when the branch has none; the
squash removes it. Push later checkpoints with `--draft` too, so the title and
body follow the branch. Readers see the work from its start, and a draft
starts no hosted run.

## One intent, gated phases

A proposal carries one intent. Its size is not a reason to split it: micro
proposals cost the reviewer more than they save, and a change that updates its
tests and docs is larger by design. The impacted gate prints the change size
by category (code, tests, docs, generated) as information; no line count fails
it, and no threshold exists to add.

Deliver work that spans several projects, or an epic, in phases:

1. Work on an integration branch that matches the workspace `epicBranches`.
2. Make one commit per phase.
3. Pass the gate on each phase before the next one starts.
4. Merge the branch as one unit that one revert undoes.

The gate warns when the authored code spans projects that no dependency
relates and the branch is not on an epic branch. The warning never fails the
run. Act on it: split the change by intent, or move it to an epic branch. Name
the decision in the proposal when you keep the spread.

## Worker quality and ownership

| Tier | When | Worker profile (policy default) | Effort |
|---|---|---|---|
| light | Prescribed mechanical change, no behavioral invariant at risk | `workers.light` (`fix-light`) | medium |
| standard | Settled multi-file design following existing patterns | `workers.standard` (`fix-standard`), or inline when the coordinator already runs at that quality | high |
| heavy | Open design, security, concurrency, caching or cross-contract invariants | `workers.heavy` (`fix-heavy`) | xhigh |

An explicit tier wins, then a task tier label (policy `tasks.tiers`), then the
risk classification; choose the higher tier for conflicting signals. Delegate
to the selected profile on both hosts, not a cheaper substitute: its host
metadata carries the model and effort the repository chose. A light worker that
discovers design work returns ESCALATE. Tier determines reasoning effort, not
permission to change contracts.

Give each worker owned files, scope mandate, acceptance and the other writers
it must accommodate. Workers never revert someone else's edits or mutate Git or
collaboration lifecycle state. The coordinator serializes integration.
Parallelize independent preparation; use separate worktrees for conflicting
tool outputs. No global serialization or always-on scope owner is required.
Admission is per Putnami run unless a host-wide capability has actually been
verified.

## Freeze, review and repair

Iterate with focused checks using Putnami `--projects`. Format and finish edits
before final proof. If publishing is authorized, make a meaningful checkpoint
commit before final review/proof: HEAD participates in the canonical fingerprint.
A subsequent commit, rebase or edit invalidates that binding, even if the working
file contents appear unchanged. Local-only work may be verified as a dirty tree;
that result is not automatically evidence for a future commit.

Each change, or each phase of phased work, runs its gates in this order, and
no other gate:

1. A `--projects` gate over the touched projects, run by whoever edited last,
   again only after a new edit.
2. One final impacted gate, on the candidate the review accepted.
3. The finalizer reuses that gate, through the dossier or on the same tree,
   instead of running its own.

The coordinator does not replay a gate someone else ran: it reads that
session record and compares its `tree.fingerprint` with the live tree, before
the checkpoint commit. The checkpoint changes HEAD, which the fingerprint
includes, so no earlier record matches after it; the final impacted gate is
the first gate bound to the new HEAD, and the checkpoint is not a reason to
re-run a `--projects` gate. An impacted gate that runs before the review ends
is thrown away by the first finding, so it waits for the review.

Capture the candidate with
`"$PUTNAMI_CLI" tree verify --snapshot --base origin/<base>`: the remote base the
PR targets, because a local base branch can be stale. Read the exact `binding.baseSHA` from it.

Launch one independent `code-review` context on the frozen candidate, with the
objective, authoritative instructions, base/head/fingerprint, full diff, scope,
scope opinions and the workers' gate records. Do not provide the desired verdict or
private implementation reasoning. The reviewer writes its own output directly
to the shared ignored run directory and returns its reference. Nobody edits
inputs while it reviews. Add a specialized pass only for a named risk.

Classify findings, repair in the same change, and return the changed surface and
interactions to the reviewer. Reopen affected design positions when contracts
or responsibilities move. Keep original reports and objections; append new
responses rather than rewriting the previous author. A repair moves the
candidate: take a new snapshot for the reviewer. A whole-tree proof is
stale after any binding change; cache reuse is the producer's decision. The
coordinator builds a new dossier projection from the latest attributed notes.
Use records.md for the verifier inputs. This projection is not a second plan.

Once no finding needs an edit, derive blocking command roots and flags
from `putnami.ci.json` and repository constraints. Measure the machine first
with `bash .agents/skills/fix/scripts/machine-load.sh`. When the ratio exceeds
the policy's `verification.ciGate.load` (default 0.7) and the policy names
`verification.ciGate.checks`, run no local impacted gate: commit the
checkpoint and call the finalizer with `--gate ci`. It pushes, marks the
proposal ready, which starts the hosted checks, waits for every named check on
the pushed commit, and returns the proposal to draft when one fails. It can
wait up to `verification.ciGate.timeoutMinutes`, so run it in the background
where the host bounds a command's duration; invoking it again keeps waiting.
Otherwise run the final gate with
`--impacted --baseline <baseSHA> --enforce-coverage`, or the broader all-project
selection. `validate` includes its manifest-declared `validate-workspace`
companion; spelling both does not add coverage. Do not infer missing checks
from the shorter command list. Unsupported custom required policy needs an
explicit implementation before the local verifier can claim it.

Save the exact producer session and v2 report paths returned by that invocation;
never find a record by newest timestamp. The finalizer's own scan, newest
first, is the exception: it accepts a record only once the verifier's
`--gate` check passes it.
Reuse already valid records with the same tree, selection, baseline, policy
and configuration. The session proves
its tree and result; its report proves coverage enforcement. A gate that changes
inputs requires a fresh snapshot and affected proof. Do not use `--no-cache`
unconditionally. A concrete transient failure may use `--retry-failed` once;
a second red run is a blocker to preserve, not a run to repeat.

Resolve workloads from project ownership and `runsWith`, then run existing
`qualify <project> --target local --output=json` where applicable, on the same
tree as the final gate. Save the producer output unchanged, with passed state, clean cleanup and the same full
tree binding. An unsupported CLI/worker/image qualifier is a missing adapter,
not success: use an existing artifact-specific execution harness if repository
policy allows it, and record that functional evidence separately. Do not
silently waive a mandatory qualification. Generated-client changes require the
provider-to-generated-client-to-real-consumer journey. HTTP smoke, ordinary
gate and functional acceptance are separate; a response below 500 is not proof
of authorization or business behavior.

## Ready and authorized delivery

Run `"$PUTNAMI_CLI" tree verify --record <dossier.json>` on
the final tree. Only exit 0 produces
`Putnami verified` at stage `ready`, with local declared-scope assurance. Keep
missing/blocked states explicit. Do not hide non-applicability behind empty
arrays: the reviewer must assess the scope and required-workload rationale.

Publish with the fix `finalize-pr.sh` helper and
**`--verification-file <dossier.json>`**. It requires the clean checkpointed
tree, rechecks the dossier and reuses its gate instead of running another.
Without a dossier, it reuses a green impacted gate that ran on the same tree
once the verifier's `--gate` check accepts it, and gates itself only when
none exists. It commits and pushes with Git, publishes the proposal with
`proposals upsert`, reads it back with `proposals status`, and, when
`--task-source`/`--task-id` name a task, moves that task to the policy's
delivered state. An intention without a task uses the same helper without the
task arguments; do not invent a task to use it. Without a dossier, the caller's
`--proof` prose cannot mint the local verified verdict. See the helper's
`--help` for arguments; do not duplicate its lifecycle in a new script.

The proposal title and body are the squash commit: the repository merges
with them, its merge button included. The body states the net change of the
branch against its base, as it stands now, never the history of commits or
review rounds: why, then what changes for a reader of the history, in plain
paragraphs or a short list, without headings, file lists, proof or checklists.
Rewrite it whole whenever a push changes what the proposal does; a body that
describes an earlier state of the branch is a review finding. The finalizer
adds the task reference line, joins the lines of each paragraph because the
host wraps the squash message itself, refuses a heading or a body over the
policy's `publication.bodyMaxBytes`, and posts the verification record (files,
gate, proof) as a proposal comment instead.

No commit message, proposal, comment, code comment or document names an agent:
no `Co-Authored-By` trailer for a model, no "Generated with" line, no model
name or version. Disable the host's own attribution in its settings. The
models that worked on a mission belong in its memory checkpoint
(collaboration.md), never in the repository or its proposals.

When the repository titles changes with Conventional Commits, take the type
from what the diff changes, never from the skill, the task label or the word
"fix" in the finding: a change that only touches tests is `test`, one whose
point is speed, memory or allocations is `perf`, one that keeps behavior is
`refactor`, and documentation, build, CI and upkeep are `docs`, `build`, `ci`
and `chore`. `fix` is a user-visible defect corrected; `feat` is new
behavior. The finalizer refuses `fix` and `feat` on a change whose every path
is a test.

The proposal stays a draft until the finalizer marks it ready for review; keep
it a draft at finalization when requested (`publication.draft` or the
mandate). Draft backup with missing proof is never marked verified. A failed or `unresolved`
external operation leaves a resumable state, not permission to bypass checks or
to repeat the write blindly. Publishing is not required for a mandate that
explicitly stays local.

If merge/deployment is already authorized, continue through the normal required
checks and provider operations; re-evaluate after head/base changes. A provider
that offers no `merge`, or reports checks `unsupported`, leaves that step to
the repository's own controls: say so instead of reporting it done. Record
actual receipts for merged, published, pinned, deployed and accepted
separately. For cross-repository work, upstream merged is not downstream
adopted. Do not open new approval rounds for already authorized ordinary work.
When authority ends at ready, report ready and the reserved action without
claiming the whole delivery completed.

## Resume and measure

Before host limits interrupt work, save a short checkpoint: objective, accepted
scope/decisions, current worktree/branch, owned uncommitted edits, exact evidence,
open findings/objections, pending external operations and next executable step.
Save it with `memory checkpoint` when memory is bound, following
collaboration.md, with evidence entries pointing at the actual records;
otherwise write it to the ignored run directory and state that it does not
survive workspace deletion. A host that supports automatic resumption may
continue from this checkpoint; otherwise expose that missing capability.
Never require the owner to reconstruct the handoff from several agent messages.

Capture observed timestamps, tool/usage counters when supplied by the host,
gate session CPU and reuse metrics, repeated controls and human interventions
with causes. Unavailable tokens/costs stay unavailable. Report each impacted
run beyond the final one, and each `--projects` gate that did not follow an
edit, with its cause.
