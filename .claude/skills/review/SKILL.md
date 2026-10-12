---
name: review
description: Independently challenge a material artifact or frozen candidate
model: claude-opus-5-5[1m]
allowed-tools: Bash, Read, Grep, Glob, Task, TodoWrite, Write, ToolSearch, mcp__putnami__putnami_search, mcp__putnami__putnami_context, mcp__putnami__describe_project, mcp__putnami__deps, mcp__putnami__impacted, mcp__putnami__why_impacted
argument-hint: <artifact-ref|candidate-ref>
---

# Review

Accept a material spec, design, prototype, ADR or frozen implementation
candidate. Work in a distinct context from its author. Read the mandate,
exact examined artifact and relevant source and context versions; a title or
summary alone is not review coverage. For candidate code review, use the
existing [code-review](../code-review/SKILL.md) rubric and v2 dossier contract.

Resolve the consumer CLI in each shell:

```bash
PUTNAMI_CLI=putnami
if [ -x ./putnamiw ]; then
  PUTNAMI_CLI=./putnamiw
fi
```

Start code discovery with Putnami and use its local graph fallback when
needed. Challenge outcomes and exclusions, native requirement coverage,
responsibility boundaries, invariants, experiment assumptions, compatibility,
proof entrypoints and likely failure scenarios. State what was actually
examined and what remains unavailable. Give each material finding a stable ID,
location, scenario, consequence and concrete correction. Write an original,
attributed report with exact artifact and relevant context references as
specified by the [workflow contract](../../../.agents/skills/build/references/workflow.md).
The author may append a correction and re-review, counterevidence or an
actual mandate-authorized deferral; preserve the original report. Changed
examined bytes reopen the opinion. Unrelated tree edits do not invalidate a
design opinion, while any covered edit invalidates whole-tree candidate proof.

Review is read-only on product and collaboration state. It never creates a
task, publishes a review or grants merge/deployment authority unless the
invocation separately and explicitly authorizes that external act. Return
the exact report reference, reviewed version, coverage, findings and missing
inputs. The author or build coordinator owns repair and progression.
