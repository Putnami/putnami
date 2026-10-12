---
name: putnami-change
description: Deprecated alias of execute — implement an explicitly requested change in a Putnami workspace and verify it locally
allowed-tools: Bash, Read, Grep, Glob, ToolSearch, mcp__putnami__*
---

# putnami-change (deprecated alias of `execute`)

`putnami-change` is kept so existing invocations keep working. It is
deprecated: invoke `execute` (`/execute` on Claude Code, `$execute` on Codex)
instead. It remains through the current pre-1.0 line; removal needs a
separately announced breaking change.

Follow the `execute` workflow in `.agents/skills/execute/SKILL.md` exactly,
with the restriction this entry point always had: the mandate is local. Do not
create or move branches, commit, push, publish a proposal, or change a task or
any other external system unless the user separately and explicitly requests
that action. Planning, the gate, local proof and the independent review stay
proportionate to the change.

From the consumer workspace root, select the CLI the workflow uses:

```bash
PUTNAMI_CLI=putnami
if [ -x ./putnamiw ]; then
  PUTNAMI_CLI=./putnamiw
fi
```
