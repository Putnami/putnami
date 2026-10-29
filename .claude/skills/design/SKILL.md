---
name: design
description: Resolve a technical design against native scope and architecture contracts
allowed-tools: Bash, Read, Grep, Glob, TodoWrite, Edit, Write, Agent, ToolSearch
argument-hint: <intention|spec|task> [--dry-run]
---

# Design

Resolve a material implementation choice from the accepted outcome and spec.
Reuse a current design when the change is settled. A native architecture
declaration or ADR is its own authoritative source; do not mint a parallel
architecture registry.

Resolve the consumer CLI in each shell:

```bash
PUTNAMI_CLI=putnami
if [ -x ./putnamiw ]; then
  PUTNAMI_CLI=./putnamiw
fi
```

Read repository instructions, mandate, spec, settled decisions, existing
ADRs and design. Discover with the CLI first: `context map`, `context pack`,
`projects describe`, `specs` and `features`. Use native project, dependency
and scope selection to identify affected contracts and contributors; backlog
labels do not assign ownership. Follow [build](../build/SKILL.md) and the
[workflow contract](../../../.agents/skills/build/references/workflow.md).
Planning only stays read-only unless source editing is authorized.

## The design

Describe the behavior path, interfaces and data, invariants at risk,
alternatives considered, why the selected option fits current contracts, the
smallest executable vertical, dependencies, local environment and proof
entrypoint. State exclusions, unresolved decisions and a precise next cycle.
An architecture change carries a small diagram; a screen change, a mockup as
static HTML built with the design system's components. An independently
useful unit earns a separate delivery boundary through value and required
receipts, not project count or file ownership.

## A bounded prototype

When an uncertainty needs observation before the choice, answer that one
question first. Set an effort ceiling and a result another contributor can
inspect. Name the question, the exact source inputs, the runnable or
inspectable assets, the observations, the simulated behavior, the assumptions,
the conclusion and the remaining uncertainty. Simulation stays labeled and
never becomes production acceptance. Keep assets in an owned source path when
they must survive another checkout; an ignored local file is a limited handoff
and is reported unavailable when it cannot move. Avoid a new runtime or
dependency merely to show a result. Reused prototype code enters build as
ordinary source, subject to review, gate and local proof.

## A durable decision

Record an ADR only when the change rests on a durable choice; a routine
correction needs none. Read the owning project's existing decisions first: a
settled value stays settled unless the same authorized change updates its
decision source. An ADR cannot settle a choice outside the mandate or one
reserved to another owner: record it as pending, with the responsible
authority. In the owning project's ADR location, state the decision, its
observable consequences, the alternatives, the affected contracts, the
migration and the reversal. A machine-checkable rule goes in `decisions.json`,
pointing to its ADR.

## Challenge and return

Ask a distinct [review](../review/SKILL.md) to challenge a material design,
prototype conclusion or ADR against the exact versions it examined. Preserve
original objections and append evidence-backed resolutions. Return the design
source reference, the decision status, acceptance and proof path, scope
positions, open findings and the next result. No design, prototype or ADR
proves the implementation, grants publication, or waives a producer check.
