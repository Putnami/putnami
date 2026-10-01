---
name: execute
description: Carry an authorized change through scope collaboration, implementation, independent review, repair and verified evidence
---

# Execute

Own the requested outcome through the whole loop. Accept an intention, task,
existing branch/proposal, or epic vertical; a task is optional. Reuse the
current worktree, plan, decisions and evidence. `fix` is the compatible
task-selection entrypoint and `epic` supplies scope/dependencies to this same
loop.

Resolve the consumer CLI in each shell and worker:

```bash
PUTNAMI_CLI=putnami
if [ -x ./putnamiw ]; then
  PUTNAMI_CLI=./putnamiw
fi
```

Read repository instructions and start discovery with Putnami MCP; use the
local graph/source fallback on unavailable or stale results. Preserve existing
work. Read repository conventions from the
[policy](../../../.agents/skills/execute/references/policy.md), and reach
tasks, proposals and memory only through the
[collaboration contracts](../../../.agents/skills/execute/references/collaboration.md).

## Mandate and entries

`<intention|task|proposal>` selects the objective. `--resume <dossier>` resumes
a local record; `--tier light|standard|heavy` selects the worker tier;
`--base <ref>` selects an existing integration base; `--dry-run` plans without
writes. An absent objective needs clarification; it does not select backlog
work implicitly (the `fix` entry retains that behavior).

Record accepted scope, acceptance, delegated decisions and reserved decisions.
Continue ordinary authorized execution without asking again. Task, branch and
proposal mutations require the invocation's existing authority; a
natural-language local change request does not authorize publishing. Merge and
deployment require explicit authority in the current mandate and normal
repository controls. Never bypass them. Continue independent work while a
reserved decision waits.

Shared resources live at workspace-root `.agents/skills/execute/` on both
hosts; Claude materializes only the entrypoint in its own skill directory.

## One loop

Read [loop.md](../../../.agents/skills/execute/references/loop.md) when executing or resuming; it owns the
phase ordering, tier table, evidence reuse, finalization and recovery rules.

1. Reuse the plan; involve only affected scope owners while shaping it.
   Resolve scope coverage from native scopes/projects and dependencies; use
   existing contracts to identify the responsible contributor. Read
   [scopes.md](../../../.agents/skills/execute/references/scopes.md) when a boundary or strategic capability
   is affected. A single internal change needs proportionate ownership, not a
   standing committee or a proposal per scope. A small local change stays
   small: one focused vertical, one gate, one bounded review. One proposal
   carries one intent; work that spans several projects lands in gated phases
   on an epic branch (loop.md, "One intent, gated phases"), never in micro
   proposals cut by size.
2. Deliver coherent verticals through the worker profiles the policy selects
   for each tier. Integrate serially in the coordinator checkout; parallelize
   distinct ownership only when safe.
3. Freeze the candidate and run one independent review on that exact
   version. Return reviewer output directly to the coordinator; the owner need
   not copy it between tabs.
4. Repair in the same change. Preserve findings and objections; record their
   resolutions. Reopen affected agreements/review and any invalidated producer
   evidence. Never infer file-level reuse from a whole-tree fingerprint. Once
   no finding needs an edit, run the one final impacted gate and the missing
   controls on that version; the finalizer reuses that gate (loop.md, "Freeze,
   review and repair").
5. Check the local dossier with
   `"$PUTNAMI_CLI" tree verify --record <path>` following
   [records.md](../../../.agents/skills/execute/references/records.md). Only successful output may be called
   **Putnami verified (local pilot, declared scope)**. Missing required proof
   or coverage leaves the run not verified. A proposal URL alone is not completion.
6. Continue authorized publication, merge, deployment and acceptance; record
   each separately. The local `ready` verdict cannot claim a deployed result.

Use [pilot.md](../../../.agents/skills/execute/references/pilot.md) for the initial behavioral evaluation and
measures. No new service, state branch, hosted review activation or permanent
agent pool is required. Existing hosted review reports remain unchanged
producer artifacts; local coverage/decision notes supplement them when
available.

## Limits and stopping

The local verifier checks consistency, binding and required record presence.
It does not authenticate authors, prove scope completeness, or replace the
producer's full schema validator. Agents sharing write access can alter both
notes and policy: owner attribution is a convention in this runtime, not isolation.
Do not mint hosted signatures or pretend a hosted review service was used.

Stop only on a demonstrated blocker, reserved decision, exhausted budget or
requested checkpoint. Preserve the work and exact resume condition first, and
save the checkpoint described in the loop's resume section.
Never turn an incomplete repair into success or retry without progress forever.
Report changed behavior, deleted/reused mechanisms, evidence, limits and next
state. Missing cost metrics stay unavailable, not zero.
