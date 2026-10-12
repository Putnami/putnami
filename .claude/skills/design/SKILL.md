---
name: design
description: Resolve a technical design against native scope and architecture contracts
allowed-tools: Bash, Read, Grep, Glob, TodoWrite, Edit, Write, Agent, ToolSearch, mcp__putnami__putnami_search, mcp__putnami__putnami_context, mcp__putnami__describe_project, mcp__putnami__deps, mcp__putnami__impacted, mcp__putnami__why_impacted
argument-hint: <intention|spec|task> [--dry-run]
---

# Design

Resolve a material implementation choice from the accepted outcome and spec.
Reuse a current design when the change is settled. Use a project-local Markdown
note when needed; a native architecture declaration or ADR remains its own
authoritative source. Do not mint a parallel architecture registry.

Resolve the consumer CLI in each shell:

```bash
PUTNAMI_CLI=putnami
if [ -x ./putnamiw ]; then
  PUTNAMI_CLI=./putnamiw
fi
```

Read repository instructions, mandate, spec, settled decisions and existing
design. Discover with Putnami first. Use native project/dependency and scope
selection to identify affected contracts and actual contributors; backlog
labels do not assign ownership. Follow [build](../build/SKILL.md) and the
[workflow contract](../../../.agents/skills/build/references/workflow.md).
Planning only stays read-only unless source editing is authorized.

Describe the behavior path, interfaces and data, invariants at risk,
alternatives considered, why the selected option fits current contracts,
the smallest executable vertical, dependencies, local environment and proof
entrypoint. State exclusions, unresolved decisions and a precise next cycle.
An independently useful unit earns a separate delivery boundary through
value and actual required receipts; it does not arise from project count or
file ownership. If an uncertainty needs observation, invoke
[prototype](../prototype/SKILL.md) with an effort ceiling before fixing the
choice. A durable choice belongs in [adr](../adr/SKILL.md).

Ask a distinct [review](../review/SKILL.md) to challenge a material proposal
against the exact design, spec, source and decision versions it examined.
Preserve original objections and append evidence-backed resolutions. Return
the chosen design source reference, acceptance and proof path, receiving scope
positions, open findings and next result. No design opinion grants permission
to publish or bypass the build gate.
