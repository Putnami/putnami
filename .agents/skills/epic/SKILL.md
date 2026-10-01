---
name: epic
description: Coordinate an epic's scope and dependencies through the shared execute loop on coherent integration branches
---

# Epic

Own the epic's outcome and dependency ordering; use
[execute](../execute/SKILL.md) for its implementation, independent review,
corrections, evidence, and authorized delivery. Read its
[loop](../../../.agents/skills/execute/references/loop.md),
[records](../../../.agents/skills/execute/references/records.md),
[collaboration contracts](../../../.agents/skills/execute/references/collaboration.md) and
[repository policy](../../../.agents/skills/execute/references/policy.md). A
completed worker or a published proposal is a checkpoint, not completion of
the epic.

Prefer one coherent integration branch and draft-to-ready proposal, with
meaningful commits for useful verticals. This is the default for any change
that spans several projects, not only for a declared epic: name the branch so
it matches the workspace `epicBranches`, make one commit per phase, pass the
gate on each phase before the next one starts, and merge the branch as one
unit that one revert undoes (execute loop, "One intent, gated phases").
Scope ownership does not require one proposal or a permanent agent per scope.
Separate tasks or proposals must earn their coordination cost through
independent value, contract, ownership, release, or rollback.

## Portable host and CLI

Treat `/name` as the host-native skill invocation (`$name` on Codex). This
workflow is explicit-only and mutates Git and collaboration state within the
existing mandate. It grants no new merge or deployment authority. The
coordinator owns integration, branches, commits, pushes, proposal metadata,
and task transitions; workers retain their bounded file ownership.

```bash
PUTNAMI_CLI=putnami
if [ -x ./putnamiw ]; then
  PUTNAMI_CLI=./putnamiw
fi
```

Resolve this selector from the consumer root again in each new shell or worker.

## Arguments

| Flag | Purpose |
|------|---------|
| `<task>` | Epic task to execute, as an id (with the policy's `tasks.source`) or a whole reference (required) |
| `--skip-refine` | Continue with settled or already delegated defaults; does not resolve reserved decisions |
| `--refine-only` | Record the plan and prepare the integration lane, then stop |
| `--resume` | Reconstruct and continue an existing integration run or legacy sub-proposal run |
| `--sub-prs` | Use separate sub-tasks/proposals only for independently useful verticals |
| `--max <n>` | Legacy sub-proposal checkpoint bound; never a reason to split work |
| `--tier <t>` | Force a supported light, standard, or heavy worker tier |
| `--no-analyst` | Perform analysis inline instead of delegating to the analyst profile |
| `--dry-run` | Return the plan without writes |

## Worker profiles

| Responsibility | Profile |
|----------------|---------|
| Orchestration | The session the user started, at the repository's orchestration quality |
| Epic analysis when needed | `workers.analyst` (default `epic-analyst`), read-only |
| Implementation | The tier's worker profile (`workers.light`, `workers.standard`, `workers.heavy`) |
| Independent review | `code-review` in a fresh context |

Each profile's host metadata carries the model and effort the repository
chose; do not substitute a cheaper one. An analyst contributes a distinct
planning or integration question, not an obligatory extra final pass. On
analyst failure or `--no-analyst`, perform that analysis inline once at the
orchestration quality, disable later analyst calls, and disclose the fallback.
This does not replace the independent review.

## Plan with affected scopes

Load the epic task (`tasks get`), its sub-tasks where the provider reports a
hierarchy, the existing branch/proposal (`proposals find`), decisions, memory
context when bound, and the local run record. Reuse current discovery and
completed work. A `done` or `canceled` task requires reconciling its actual
acceptance state before reopening or claiming new work; do not equate its
state with delivery.

Follow [plan](../plan/SKILL.md) for unanswered questions. Resolve native scope/project coverage and dependencies using
[scope contributions](../../../.agents/skills/execute/references/scopes.md); read
existing architecture contracts where relevant, separately from backlog labels.
Invite only materially affected scope owners to shape scope and contracts
before imposing the decomposition. Record their
positions under the execute records contract. Keep objections visible until
resolved; reassignment cannot erase them.

Build a dependency-ordered plan with acceptance behavior, owning scopes,
projects/files, dependencies, local environment, existing proof entrypoint,
tier, and justified separate proposals. Start service work with its smallest
useful running baseline. Escalate only decisions outside the mandate, with
alternatives and consequences; continue independent ready work while those are
unresolved.

`--dry-run` stops without writes. Otherwise record the plan on the epic task
under the existing authority (`tasks update` with the revision just read), and
reuse the workspace branch without renaming a host-managed branch. Prepare a
fresh branch only when required and safe. Publish one draft proposal once the
plan is settled, with `bash .agents/skills/fix/scripts/finalize-pr.sh --draft`
(execute loop, "Begin from a mandate"); its empty commit carries the title and
disappears at the squash.
`--refine-only` stops here, even if no proposal yet exists.

## Execute and integrate

Pass the epic mandate, settled plan, scope positions, acceptance obligations,
and existing run state to execute. Preserve one continuous review/correction
loop instead of invoking independent finalization ceremonies for each vertical.
Parallelize implementation only with independent file ownership and compatible
contracts; integrate serially and freeze the combined version before evidence.
A scope owner can contribute an internal transformation, not merely approve code.

Reuse valid producer evidence. After integration or a correction, determine
which positions, reviews, gates, qualification, and acceptance evidence became
invalid. Whole-tree evidence still binds the whole tree. Obtain the canonical
impacted gate and applicable local qualification for the combined revision;
worker-local proof alone cannot establish combined acceptance. Do not repeat
an already applicable gate merely because another vertical or review completed.

Resolve integration failures before expanding dependent scope. Use an analyst
for a genuinely difficult cross-vertical diagnosis or specialized integration
review when justified; the ordinary independent review remains bounded and
records all relevant coverage. Fix findings in the same run and proposal by
default.

For legacy `--sub-prs`, reuse existing sub-tasks/proposals without forcing new
work into that topology. Invoke execute through the compatible `/fix
<sub-task> --base <integration-branch>` entry. Integrate sub-proposals only
under already granted merge authority, normal checks and a provider that offers
`merge`; otherwise leave an explicit ready checkpoint. Verify the integrated
content, reconcile task state, and invalidate affected proof. `--max` bounds
this legacy progress without changing acceptance criteria.

## Resume and finish

Resume from exact recorded revisions and producer references, reconciling them
with actual branch, proposal, and task state. Preserve findings and objections;
skip completed work only while its evidence remains valid. Use execute's
bounded-progress and suspension rules for blockers, decisions, or budgets, and
save a checkpoint before stopping.

Readiness requires the shared computed verification result, applicable gate,
qualification, acceptance evidence, and resolved review/scope obligations.
Missing or blocked requirements leave the proposal draft. Report ready, merged,
deployed, and accepted separately. Continue merge and deployment only when the
existing explicit mandate covers them; never use an administrative bypass.
Without that authority, report the concrete ready result and next required
permission. A delivery objective remains incomplete until its acceptance proof.

Return the epic/proposal, run record, covered revision, vertical status,
verification, exact gate and local proof references, review resolutions,
remaining blockers, and analyst fallback if any. The common report projects
recorded facts rather than introducing a second manually maintained plan.
