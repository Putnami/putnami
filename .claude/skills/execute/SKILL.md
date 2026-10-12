---
name: execute
description: Compatible build entry for an authorized implementation, independent review and current evidence
allowed-tools: Bash, Read, Grep, Glob, TodoWrite, Edit, Write, Agent, ToolSearch, mcp__putnami__putnami_search, mcp__putnami__putnami_symbol, mcp__putnami__putnami_context, mcp__putnami__putnami_impact, mcp__putnami__putnami_related, mcp__putnami__putnami_summary, mcp__putnami__list_projects, mcp__putnami__describe_project, mcp__putnami__deps, mcp__putnami__find_owner, mcp__putnami__why_impacted, mcp__putnami__topo_sort, mcp__putnami__impacted, mcp__putnami__get_diagnostics
argument-hint: [<intention|task|proposal>] [--resume <dossier>] [--tier <light|standard|heavy>] [--base <ref>] [--dry-run]
---

# Execute

This entry preserves existing `execute` invocations. Enter the canonical
[build](../build/SKILL.md) workflow with exactly the objective and authority
supplied to `execute`. It uses one implementation, review, repair and evidence
loop, at the existing
[loop](../../../.agents/skills/execute/references/loop.md), with the existing
[records](../../../.agents/skills/execute/references/records.md),
[scope contributions](../../../.agents/skills/execute/references/scopes.md),
[collaboration contracts](../../../.agents/skills/execute/references/collaboration.md)
and [repository policy](../../../.agents/skills/execute/references/policy.md).
Those shared source paths remain stable for consumers of this entry.

Resolve the consumer CLI anew in every shell and worker:

```bash
PUTNAMI_CLI=putnami
if [ -x ./putnamiw ]; then
  PUTNAMI_CLI=./putnamiw
fi
```

`<intention|task|proposal>` names the objective. `--resume <dossier>` resumes
its existing local run; `--tier light|standard|heavy` chooses the worker tier;
`--base <ref>` selects the existing integration base; `--dry-run` plans without
writes. With no objective or explicit resume, ask for one: this entry never
selects backlog work. Preserve a local-only invocation as local-only. A
natural-language implementation request does not grant task mutation, Git
publication, merge or deployment. A previous mandate's granted authority
persists only for that same run.

Use build's outcome-first units, proportionate spec/design/prototype/ADR work,
independent material challenge and precise next cycle. Before relying on the
optional versioned workflow reference, confirm the consumer CLI advertises
`tree verify --workflow`; an older verifier can ignore an unknown dossier
member. The existing v2 dossier still supplies candidate review, gate,
qualification and acceptance proof. A workflow progress projection does not
mint the ready verdict. Follow build's partial-delivery rule so an intermediate
unit cannot close its parent task.

Report demonstrated acceptance, exact producer and provider references,
original unresolved findings, current delivery stage and next action. A
ready proposal is a checkpoint when the requested outcome needs later
publication, deployment or adoption; do not infer those receipts.
