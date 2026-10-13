---
name: adr
description: Record and challenge a durable architectural choice in the owning project's decision source
---

# ADR

Record a durable choice only when the change rests on one. First read the
owning project's existing decisions; a settled value stays settled unless
the same authorized change updates its decision source. An ADR cannot settle
a choice outside the accepted mandate or a decision reserved to another owner;
record that choice as pending with the responsible authority. A routine
correction needs no new ADR.

Resolve the consumer CLI anew from the workspace root:

```bash
PUTNAMI_CLI=putnami
if [ -x ./putnamiw ]; then
  PUTNAMI_CLI=./putnamiw
fi
```

Read repository instructions, native feature/spec, architecture contracts,
existing ADRs and the accepted mandate. Start discovery with Putnami. Follow
[build](../build/SKILL.md) and the
[workflow contract](../../../.agents/skills/build/references/workflow.md).
Planning alone is read-only; source edits require the existing mandate.

In the owning project's established ADR location, state the decision,
observable consequences, alternatives, affected contracts, migration and
reversal conditions. Link exact native feature/spec and context versions.
Keep exclusions and reservations visible. A material ADR receives an
independent [review](../review/SKILL.md) before adoption; retain original
findings and their evidence-backed resolutions. Return its exact source
reference, the current decision status, receiving scope positions and the
next implementation result. An accepted ADR does not prove implementation,
grant publication authority or waive a missing producer check.
