---
name: putnami-plan
description: Deprecated alias of plan — inspect a Putnami workspace and return a local implementation plan
allowed-tools: Bash, Read, Grep, Glob, ToolSearch, mcp__putnami__*
---

# putnami-plan (deprecated alias of `plan`)

`putnami-plan` is kept so existing invocations keep working. It is deprecated:
invoke `plan` (`/plan` on Claude Code, `$plan` on Codex) instead. The alias is
removed in a later release of the contributor extension, after the migration
window its documentation announces.

Follow the `plan` workflow in `.agents/skills/plan/SKILL.md` exactly, with the
restriction this entry point always had: the result is a local plan. Do not
create or update a task, a branch or a proposal, and do not change files.

From the consumer workspace root, select the CLI the workflow uses:

```bash
PUTNAMI_CLI=putnami
if [ -x ./putnamiw ]; then
  PUTNAMI_CLI=./putnamiw
fi
```
