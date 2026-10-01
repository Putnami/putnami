---
name: fix
description: Select a task from the bound tasks provider and carry it through the shared execute implementation, review and verification loop
allowed-tools: Bash, Read, Grep, Glob, TodoWrite, Edit, Write, Agent, ToolSearch, mcp__putnami__putnami_search, mcp__putnami__putnami_symbol, mcp__putnami__putnami_context, mcp__putnami__putnami_impact, mcp__putnami__putnami_related, mcp__putnami__putnami_summary, mcp__putnami__list_projects, mcp__putnami__describe_project, mcp__putnami__deps, mcp__putnami__find_owner, mcp__putnami__why_impacted, mcp__putnami__topo_sort, mcp__putnami__impacted, mcp__putnami__get_diagnostics
argument-hint: [<task-id> | <task-reference>] [--from-audit] [--label <label>] [--<filter> <value>] [--tier <light|standard|heavy>] [--base <branch>] [--dry-run]
---

# Fix

Compatibility entrypoint for task selection and claiming. Then enter
[execute](../execute/SKILL.md) with that task and the existing mandate; there
is one implementation/review/repair loop, not a nested workflow. Read its
[loop](../../../.agents/skills/execute/references/loop.md), [records](../../../.agents/skills/execute/references/records.md),
the [collaboration contracts](../../../.agents/skills/execute/references/collaboration.md)
and the [repository policy](../../../.agents/skills/execute/references/policy.md).
The worker profiles and helper scripts below remain available.

Resolve the CLI again in each consumer shell or delegated worker:

```bash
PUTNAMI_CLI=putnami
if [ -x ./putnamiw ]; then
  PUTNAMI_CLI=./putnamiw
fi
```

## Compatible arguments

| Argument | Meaning |
|---|---|
| `<id>` | Select that task: its reference is `{"source": <policy tasks.source>, "id": "<id>"}` |
| `<reference>` | Select the task a whole `{"source","id"}` reference names |
| `--from-audit` or no argument | Highest-ranked task of the policy backlog; with an explicit `<id>` or `<reference>`, that task wins and nothing is selected from the backlog |
| `--label <label>` | Backlog label filter; repeatable, all must match |
| `--<filter> <value>` | A named filter the policy's `tasks.filters` defines |
| `--tier light|standard|heavy` | Explicit worker tier |
| `--base <branch>` | Existing proposal/integration base, otherwise repository trunk |
| `--dry-run` | Select, triage and report without edits or remote mutations |

Read an explicit task with `"$PUTNAMI_CLI" tasks get`. For backlog selection,
page through `"$PUTNAMI_CLI" tasks find` with the policy's `tasks.backlog`
states and labels plus the requested filters, until `page.next` is absent.
Rank by the policy's `tasks.rank`, then by age. Resolve collaboration scopes
from native projects and dependencies; read existing architecture contracts
where relevant. Backlog labels do not establish scope ownership. Exclude work
with an active run or open proposal (`proposals find` on its branch) unless
resuming it. A `done` or `canceled` task needs its scope/transfer reconciled;
do not blindly fix it again.

Reuse the task's plan. A task keeps one intent in one proposal. When its
change spans several projects, deliver it in gated phases on an epic branch
(execute loop, "One intent, gated phases") instead of splitting it by size.
Tier precedence is explicit flag, then the tier label
the policy maps (`tasks.tiers`, higher on conflict), then execute's risk
classification. Report the chosen tier and reason. `--dry-run` stops here with
target, scope and tier.

## Claim and continue

An explicit `fix` invocation retains its task and proposal lifecycle authority.
Reuse the allocated worktree/branch and existing proposal; do not rename
host-managed branches. If a new branch is needed, use the accepted base after
checking current state, without destroying unrelated work. Only the coordinator
mutates Git and collaboration state.

Claim the task: `tasks transition` to the policy's `states.claimed` (default
`in_progress`) with the revision just read as `expectedRevision` when the
provider enforces preconditions; then, when the provider offers `assign` and
the policy lists `tasks.claimAssignees`, `tasks assign` them. Read the task
back and report its state and assignees. An assignee is not an exclusive
lease: when another run may hold the task, say so. On `conflict`, re-read and
decide again; on `unresolved`, reconcile before anything else. Do not pretend
an unverified claim succeeded.

When publication is in scope, open the draft proposal right after the claim,
before the first edit: run
`bash .agents/skills/fix/scripts/finalize-pr.sh --draft` with the planned
title, the body stating the intended change, and the task reference (execute
loop, "Begin from a mandate").

Pass the task reference, accepted scope, base, tier, publication authority and
existing record into execute. Preserve independently attributed review, scope
positions, producer evidence and unresolved findings across retries. Continue
corrections without requiring another user invocation. A finding on this
unpublished work stays in this change unless explicitly deferred.

## Existing helpers

The helpers live under the workspace-root `.agents/skills/fix/scripts/` on both
hosts. Run them from the workspace root through `bash`: the installer does not
keep an executable bit.

- `tree-fingerprint.sh` forwards to Putnami's canonical tree producer.
- `bash .agents/skills/fix/scripts/finalize-pr.sh --help` documents the
  publication helper. `--draft` opens or refreshes the draft proposal. Without
  it, the helper commits enumerated files and pushes with Git, publishes the
  proposal ready for review through the proposals contract, posts the
  verification record as a proposal comment, and moves the task given by
  `--task-source`/`--task-id` to the policy's delivered state. Execute supplies
  `--verification-file` on a clean checkpointed candidate to reuse validated
  current evidence. Without it, the helper reuses a green impacted gate that
  already ran on the same tree; otherwise it gates locally, or leaves the gate
  to the policy's hosted checks when the machine is loaded. Caller proof prose
  alone does not mint Putnami verified.
- `machine-load.sh` prints the one-minute load average divided by the CPU
  count; the finalizer and the coordinator compare it with the policy's
  `verification.ciGate.load`.
- `session-cap.sh` is an optional Claude hook that counts the orchestrator's
  own tool calls (a subagent's calls carry `agent_id` and are not counted) and
  warns once, at 250; it never blocks. At its warning, save the execute
  checkpoint so a compaction or a relaunch has something to consume. The
  context a long session replays is bounded by the harness's auto-compaction
  (`CLAUDE_CODE_AUTO_COMPACT_WINDOW`; 300000 is the measured starting point),
  not by this hook. Codex has neither; its guidance is not host enforcement.
  Do not claim a universal token-cost multiplier from a tool-call count.

A ready fix has applicable evidence and the helper's verified proposal
reference (and its URL when the provider has one) when publication is in
scope. That is not a merge or deployment receipt. Follow any already authorized
remaining delivery steps; otherwise return the ready checkpoint and reserved
action. Failures preserve work, state and the exact condition for resumption.
Never blanket-restore files or turn missing proof into success. Report every
gate record and retry. A fix runs the worker's `--projects` gates and one
final impacted gate after the review, which the finalizer reuses; name the
cause of any impacted run beyond that one.
