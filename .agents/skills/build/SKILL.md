---
name: build
description: Carry an authorized outcome through the contribution loop, from talk to merge, with current evidence
---

# Build

Own the requested outcome, whether it arrives as an intention, a task or an
existing proposal. A request to build an outcome authorizes the work toward it
and the publication of its own proposal: a draft once the plan is settled, the
declarations as its first checkpoint when the outcome changes one, a pushed
checkpoint at each slice, and ready for review once the final gate and dossier
pass. It does not authorize choosing backlog work, claiming a task, merging or
deploying. A named task is claimed only when the mandate says so.

Resolve the CLI from the workspace root in every shell:

```bash
PUTNAMI_CLI=putnami
if [ -x ./putnamiw ]; then
  PUTNAMI_CLI=./putnamiw
fi
```

Read the repository instructions and the
[repository policy](../../../.agents/skills/build/references/policy.md) once
per run.

## Arguments

`<intention|task|proposal>` names the objective; without one, ask, unless
`--resume <record>` continues a run that recorded it. `--profile <name>`
selects the workflow profile. `--base <ref>` selects an existing integration
base. `--local` keeps the run local, with no push and no proposal; a mandate
that says to stay local, and a workspace that binds no proposals provider, do
the same. `--dry-run` projects the work without edits or external writes.

## The loop

A change runs eight stages, in order. On entering a stage, run
`"$PUTNAMI_CLI" agent stage <stage>`, then read that stage's reference, and
only that one. A stage that does not apply passes with one line that says why.

| Stage | What it produces | Reference |
|---|---|---|
| talk | A solution the owner accepts | [talk](../../../.agents/skills/build/references/talk.md) |
| declare | The task, the plan and its estimate | [declare](../../../.agents/skills/build/references/declare.md) |
| show | The draft proposal, declarations first | [show](../../../.agents/skills/build/references/show.md) |
| code | The slices, in sequence, each pushed | [code](../../../.agents/skills/build/references/code.md) |
| check | The reviewed candidate and its final gate | [check](../../../.agents/skills/build/references/check.md) |
| prove | Local qualification and screenshots | [prove](../../../.agents/skills/build/references/prove.md) |
| validate | The owner's attention points and feedback | [validate](../../../.agents/skills/build/references/validate.md) |
| merge | The ready proposal; a merge only with authority | [merge](../../../.agents/skills/build/references/merge.md) |

When the outcome changes a declaration, the declarations go first and alone,
and the code follows without waiting: show's "Intent first" section gives the
order. Feedback from check or validate returns the change to code. A settled
small correction moves from declare to code with the current intent and
design.

## When code waits

At the end of declare, `"$PUTNAMI_CLI" change estimate --issue <id>` sizes the
plan S, M or L.

- An S plan: code starts, even with open design questions.
- Several slices and no important new decision: code starts, and the owner
  validates at the end.
- Several slices with important design or spec decisions, or a slice that
  touches a sensitive zone: the owner approves the study and a cost envelope
  (tokens, minutes, slices) before code.

A decision the owner settled in talk or in the task counts as approved. After
approval, stop only when the work exceeds the envelope, must change the
approved design, or contradicts an ADR. Slice reports inform; they do not ask.

## Who runs each stage

The main session runs every stage and codes the slices in sequence. A workflow
profile, from the policy's `profiles`, can move a stage to a separate agent:
`--profile` wins, then the task label that matches the policy's
`tasks.profileLabel`, then `default`. Delegate such a stage to the profile's
worker with its model and effort; a host that cannot set them uses the
worker's own metadata, and the report says so. By default only review runs in
a separate agent, the `reviewer` worker. A separate agent gets a brief, never
this skill: the slice contract, its files, its acceptance, its proof command
and the records it needs.

## Stop and report

Continue until the outcome reaches its mandated stage, by default the proposal
ready for review, or stop at a demonstrated blocker, a reserved decision, the
mandate's authority boundary or an exhausted host budget. A passing slice
alone does not end the run. Before a host limit interrupts the work, save a
checkpoint as the
[collaboration contracts](../../../.agents/skills/build/references/collaboration.md)
describe.

Progress, slice and end reports are short lists of the main steps: what was
done, the stage reached, the exact records, open findings, the next action and
any unavailable input. A question to the owner states what was being done,
what is at stake, the options and your pick, never a bare question.
