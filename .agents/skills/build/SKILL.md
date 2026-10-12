---
name: build
description: Carry an authorized outcome through proportionate planning, implementation, challenge, review, current proof and authorized delivery
---

# Build

Own the requested outcome, whether it arrives as an intention, a specified
task, an existing proposal, or a coordinated epic. This is the one
implementation, review, repair and evidence loop. `fix` may select a backlog
task before entering it; invoking `build` alone never selects backlog work.
`execute` and `epic` remain compatible entries into this loop.

Resolve the CLI from the consumer workspace root in every shell and worker:

```bash
PUTNAMI_CLI=putnami
if [ -x ./putnamiw ]; then
  PUTNAMI_CLI=./putnamiw
fi
```

Read repository instructions, accepted mandate, current task and proposal,
existing studies and [the execution loop](../../../.agents/skills/execute/references/loop.md).
Use [records](../../../.agents/skills/execute/references/records.md),
[scope contributions](../../../.agents/skills/execute/references/scopes.md),
[collaboration contracts](../../../.agents/skills/execute/references/collaboration.md)
and [repository policy](../../../.agents/skills/execute/references/policy.md).
The [workflow contract](../../../.agents/skills/build/references/workflow.md)
describes the separate versioned local progress record and native artifacts.
Before relying on it, confirm `"$PUTNAMI_CLI" tree verify --help` advertises
`--workflow`. An older CLI ignores unknown v2 dossier members, so a dossier
containing `workflow` is never accepted merely because an older `--record`
command exits zero. Report the missing capability and required CLI upgrade.

## Mandate and arguments

`<intention|task|proposal>` names the objective. `--resume <record>` continues
an existing run; `--tier light|standard|heavy` selects its worker tier;
`--base <ref>` selects an existing integration base; `--dry-run` projects the
work without edits or external writes. No objective means ask for one, unless
an existing run with a recorded objective was explicitly resumed. Preserve the
invocation's local-only or ready-only limit in the mandate reference. A task,
review, ADR or verified record grants no additional publication, backlog,
merge or deployment authority.

## Shape the next result

Start from observable outcome and acceptance. Reuse current native feature
declarations, specs, decisions, architecture, scope contracts and exact task
revisions. Load the full selected native requirement inventory, including
expected checks declared by features; do not author a second expected-check
list or a manual passing verdict. Partition the inventory into current, later
and excluded work, with authority for an outcome-changing exclusion. A smaller
unit or a finished task cannot silently replace the original outcome.

Before choosing implementation primitives or a nearby example, read the
[canonical recipe index](../../../.agents/skills/build/references/recipes.md)
when present. If an intention matches, cite its intention, sample and
primitives in the plan and avoid its named anti-patterns. State "no recipe
matches" when none does. Reuse a still-valid existing plan on resume after
checking the current recipe and native contracts; do not create a new plan
merely because this entry was invoked.

Name useful delivery units by their demonstrated behavior and boundary. A
unit may span projects; a milestone groups progress and is not a delivery
receipt. Give the next cycle precise tasks, inputs, local environment, proof
entrypoint and expected inspectable result. Keep later cycles proportionate.
Use native project dependencies for build order; a unit dependency expresses
delivery order only. For a settled small correction, reuse current intent and
design and move directly to implementation. Use `spec` or `design` where the
question warrants it; use `prototype` only for a bounded material uncertainty.
An ADR records a durable choice. Material proposed artifacts receive a
distinct `review` challenge before adoption. No document count is required.

Record the run as a projection of authoritative native sources, attributed
artifact opinions, original findings, producer records and provider receipts.
Each stage names its entry condition, exact inputs, expected output and exit
evidence, including pending or non-applicable stages. `"$PUTNAMI_CLI" tree
verify --workflow <record>` validates and projects local progress; it does
not emit the candidate-ready stamp. Missing or unavailable local inputs block
their dependent stage and remain explicit in a handoff. Use repository or
provider references that another supported host can read when available.

## Implement and verify

Follow the single [loop](../../../.agents/skills/execute/references/loop.md)
for ownership, tier, freeze, independent candidate review, repair, one
`--projects` iteration gate and final impacted gate, qualification, the v2
dossier and finalization. The workflow record is an optional exact reference
on that dossier; when present, the native verifier must establish complete
current requirements from source and producer-backed check observations.
Missing, duplicate, unknown, skipped, failed or stale expected checks cannot
be described as passed. Keep ordinary functional acceptance distinct from
smoke and native check reports.

On a partial unit proposal, pass `--partial-delivery` to the existing
`fix/scripts/finalize-pr.sh` helper, including its draft checkpoint. That
retains a non-closing parent reference and leaves the parent task's delivered
transition untouched. Use normal finalization only when the mandated outcome
is complete and the required stage is supported by current evidence. Ready,
merged, published, deployed and adopted are separate receipts. Stop at the
mandate's authority boundary with the next exact action visible.

Return demonstrated behavior, actual stage, exact records and candidate
binding, original unresolved findings and resolutions, scope reservations,
next result/action and any unavailable input. Progress comes from current
records and receipts, never a task-count percentage.
