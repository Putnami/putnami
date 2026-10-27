---
name: build
description: Deliver an authorized outcome through one implementation, review and evidence loop
allowed-tools: Bash, Read, Grep, Glob, TodoWrite, Edit, Write, Agent, ToolSearch, mcp__putnami__putnami_search, mcp__putnami__putnami_symbol, mcp__putnami__putnami_context, mcp__putnami__putnami_impact, mcp__putnami__putnami_related, mcp__putnami__putnami_summary, mcp__putnami__list_projects, mcp__putnami__describe_project, mcp__putnami__deps, mcp__putnami__find_owner, mcp__putnami__why_impacted, mcp__putnami__topo_sort, mcp__putnami__impacted, mcp__putnami__get_diagnostics
argument-hint: [<intention|task|proposal>] [--resume <record>] [--tier <light|standard|heavy>] [--base <ref>] [--local] [--dry-run]
---

# Build

Own the requested outcome, whether it arrives as an intention, a specified
task, an existing proposal, or a coordinated epic. This is the one
implementation, review, repair and evidence loop. A request to build an
outcome authorizes the work toward it and the publication of its own proposal:
a draft once the plan is settled, the declarations as its first checkpoint when
the outcome changes one, a pushed checkpoint at each milestone, and ready for
review once the final local gate and dossier pass. It does not
authorize choosing backlog work, claiming a task, merging or deploying. A named
task may be claimed only when the mandate explicitly authorizes the claim.

Resolve the CLI from the consumer workspace root in every shell and worker:

```bash
PUTNAMI_CLI=putnami
if [ -x ./putnamiw ]; then
  PUTNAMI_CLI=./putnamiw
fi
```

Read repository instructions, accepted mandate, current task and proposal,
existing studies and [the execution loop](../../../.agents/skills/build/references/loop.md).
Use [records](../../../.agents/skills/build/references/records.md),
[scope contributions](../../../.agents/skills/build/references/scopes.md),
[collaboration contracts](../../../.agents/skills/build/references/collaboration.md)
and [repository policy](../../../.agents/skills/build/references/policy.md).
The [workflow contract](../../../.agents/skills/build/references/workflow.md)
describes the separate versioned local progress record and native artifacts.
Before relying on it, confirm `"$PUTNAMI_CLI" change verify --help` advertises
`--workflow`. An older CLI ignores unknown v2 dossier members, so a dossier
containing `workflow` is never accepted merely because an older `--record`
command exits zero. Report the missing capability and required CLI upgrade.

## Mandate and arguments

`<intention|task|proposal>` names the objective. `--resume <record>` continues
an existing run; `--tier light|standard|heavy` selects its worker tier;
`--base <ref>` selects an existing integration base; `--local` keeps the run
local, with no push and no proposal; `--dry-run` projects the work without
edits or external writes. A mandate that says to stay local has the effect of
`--local`. No objective means ask for one, unless an existing run with a
recorded objective was explicitly resumed. Preserve the invocation's local-only
or draft-only limit in the mandate reference. A workspace that binds no
proposals provider runs local and reports the missing binding. A task, review,
ADR or verified record grants no backlog, merge or deployment authority.

For a specified task, resolve an ID with policy `tasks.source` or use its
whole `{source,id}` reference, and read it through `tasks get`. Reconcile a
done or canceled task and any active proposal before work. When the mandate
explicitly authorizes a claim, the coordinator transitions the exact task to
policy `states.claimed` with its observed revision as `expectedRevision` where
supported, then assigns `tasks.claimAssignees` only if the provider offers
`assign`. Read the task back and report the actual state and assignees. A
conflict requires a fresh read and decision; an unresolved write requires
reconciliation before retry. Assignment is not an exclusive lease. This
specified-task path does not search, rank or claim backlog items.

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
Each unit's eight canonical stages name their nonempty `entryCondition`, exact
inputs, expected output and exit evidence, including pending or
non-applicable stages. `"$PUTNAMI_CLI" tree
verify --workflow <record>` validates and projects local progress; it does
not emit the candidate-ready stamp. Missing or unavailable local inputs block
their dependent stage and remain explicit in a handoff. Use repository or
provider references that another supported host can read when available.

## Implement and verify

When the outcome changes a feature declaration, spec, specs baseline,
architecture manifest, decisions file or ADR, commit those declarations alone
and push them to the draft before any implementation commit. Continue with
the code without waiting. Before each later checkpoint, read the reviews and
comments, handle each comment on a declaration and answer it on the proposal.
The loop's "Intent first" section gives the order.

Follow the single [loop](../../../.agents/skills/build/references/loop.md)
for ownership, tier, freeze, independent candidate review, repair, one
`--projects` iteration gate and final impacted gate, qualification, the v2
dossier and finalization. The workflow record is an optional exact reference
on that dossier; when present, the native verifier must establish complete
current requirements from source and producer-backed check observations.
Missing, duplicate, unknown, skipped, failed or stale expected checks cannot
be described as passed. Keep ordinary functional acceptance distinct from
smoke and native check reports.

On a partial unit proposal, pass `--partial-delivery` to
`"$PUTNAMI_CLI" change finalize`, including its draft checkpoint. That
retains a non-closing parent reference and leaves the parent task's delivered
transition untouched. Use normal finalization only when the mandated outcome
is complete and the required stage is supported by current evidence. Ready,
merged, published, deployed and adopted are separate receipts. Stop at the
mandate's authority boundary with the next exact action visible; by default
that boundary is the proposal ready for review.
Continue until the mandated outcome reaches its required stage, or preserve a
precise checkpoint at a demonstrated blocker, reserved decision, authority
boundary or exhausted host budget; a passing unit alone does not end the run.

Return demonstrated behavior, actual stage, exact records and candidate
binding, original unresolved findings and resolutions, scope reservations,
next result/action and any unavailable input. Progress comes from current
records and receipts, never a task-count percentage.
