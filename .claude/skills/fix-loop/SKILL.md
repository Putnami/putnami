---
name: fix-loop
description: Repeatedly invoke the fix workflow until no matching backlog task remains
allowed-tools: Bash, Read, Grep, Glob, TodoWrite, Edit, Write, Skill, ToolSearch, mcp__putnami__putnami_search, mcp__putnami__putnami_symbol, mcp__putnami__putnami_context, mcp__putnami__putnami_impact, mcp__putnami__putnami_related, mcp__putnami__putnami_summary, mcp__putnami__list_projects, mcp__putnami__describe_project, mcp__putnami__deps, mcp__putnami__find_owner, mcp__putnami__why_impacted, mcp__putnami__topo_sort, mcp__putnami__impacted, mcp__putnami__get_diagnostics
argument-hint: [--label <label>] [--<filter> <value>] [--tier <light|standard|heavy>] [--base <branch>] [--max <n>] [--dry-run]
disable-model-invocation: true
---

# Fix Loop

Select successive backlog tasks and invoke the compatible `/fix` entry to
[execute](../execute/SKILL.md). Each task uses the same continuous
implementation, review, correction, and evidence loop. Read execute's
[loop](../../../.agents/skills/execute/references/loop.md),
[records](../../../.agents/skills/execute/references/records.md),
[collaboration contracts](../../../.agents/skills/execute/references/collaboration.md) and
[repository policy](../../../.agents/skills/execute/references/policy.md). Do not start a second gate at the
backlog-loop level: each task runs execute's gates in execute's order.

## Portable host and CLI

Treat `/name` as the host-native skill invocation (`$name` on Codex). This is
explicit-only on both hosts; Codex enforces invocation through
`agents/openai.yaml`. Preserve the worker profiles the policy selects per tier;
tier changes instructions and effort within that mapping.

```bash
PUTNAMI_CLI=putnami
if [ -x ./putnamiw ]; then
  PUTNAMI_CLI=./putnamiw
fi
```

Resolve this selector from the consumer root again in each shell or worker.

## Arguments

Forward all compatible fix filters and options. The established loop options
remain:

| Flag | Meaning | Default |
|------|---------|---------|
| `--label <label>` | Backlog label filter; repeatable, all must match | none |
| `--<filter> <value>` | A named filter the policy's `tasks.filters` defines | none |
| `--tier <t>` | light, standard, or heavy | per-task triage |
| `--base <branch>` | Integration base passed to fix | repository trunk |
| `--max <n>` | Stop after N completed task runs | no task-count limit |
| `--dry-run` | Show selection and intended actions, then stop without writes | false |

No arguments selects the policy backlog. The lack of a task-count limit does
not permit unbounded retries of a stuck task; each run obeys execute's
progress/budget rules.

## Select and continue

1. Page through `"$PUTNAMI_CLI" tasks find` with the policy's `tasks.backlog`
   states and labels plus the requested filters, until `page.next` is absent,
   and deduplicate by reference. Treating the first page as the whole backlog
   is an error. Report an empty selection accurately.
2. Reconcile each candidate with its existing run and proposal
   (`proposals find` on its branch). Resume unfinished work rather than
   publishing a duplicate. A ready proposal awaiting authorized integration is a
   recorded checkpoint, not a reason to select the task again merely because
   it remains open.
3. For each selected task invoke `/fix <reference>` with its tier, base, and
   existing run context. The reference is the task this loop reconciled; pass
   no backlog-selection flag, so fix selects nothing else. Preserve the
   supplied workspace and other workers' edits; do not blindly switch the shared
   checkout to the base branch between invocations. Use isolated lanes where
   required.
4. Read the returned run state and exact result references. Count requested
   outcomes actually completed separately from ready checkpoints, blocked
   runs, and failed attempts. A proposal URL alone does not establish
   completion. A blocked task may be set aside while an independent ready task
   advances, with its blocker and resume condition retained.
5. Re-query between tasks and deduplicate against the visited run/proposal
   index. Stop at the requested bound, exhausted selection, budget, or three
   consecutive attempts without meaningful progress. Do not revert owned or
   pre-existing work automatically, invent success, or keep retrying unchanged
   failures. Report which state change would permit resumption.

The loop grants no additional merge/deployment authority. If already granted,
execute continues under normal checks; otherwise preserve the ready checkpoint.
Independent tasks need not become one combined proposal, and one finding
within a task must not become a new task automatically.

## Report

Return completed, ready, blocked, and failed runs with task/proposal and report
references, remaining selection, elapsed time and tokens only when measured,
repeated checks, and human decisions still required. Backlog summaries describe
task state, not deployed or accepted behavior.
