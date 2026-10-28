---
name: prototype
description: Run one bounded inspectable experiment
allowed-tools: Bash, Read, Grep, Glob, TodoWrite, Edit, Write, Agent, ToolSearch
argument-hint: <question|spec|design> [--dry-run]
---

# Prototype

Answer one material question before a costly design or implementation choice.
Set an effort ceiling and a result that another contributor can inspect. A
mockup may demonstrate an interaction; simulation and assumptions stay labeled
and cannot become production acceptance.

Resolve the CLI from the consumer root for every shell:

```bash
PUTNAMI_CLI=putnami
if [ -x ./putnamiw ]; then
  PUTNAMI_CLI=./putnamiw
fi
```

Read the accepted mandate, native spec, current design, repository rules and
[build workflow contract](../../../.agents/skills/build/references/workflow.md).
Start discovery with Putnami. Name the question, effort ceiling, exact source
inputs, runnable or inspectable assets, instructions, observations, simulated
behavior, assumptions, conclusion and remaining uncertainty. Keep assets in
an appropriate owned source path when they must survive another checkout;
an ignored local file is a limited handoff and must be reported unavailable
when it cannot be transferred. Avoid a new runtime or dependency merely to
display a result.

Challenge a material conclusion independently with
[review](../review/SKILL.md) using the exact experiment and context refs.
Carry supported conclusions into the native spec/design/ADR as appropriate.
Reused prototype code enters [build](../build/SKILL.md) as ordinary source
subject to review, gate and real local proof. A prototype grants no task,
publication or delivery authority.
