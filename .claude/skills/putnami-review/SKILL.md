---
name: putnami-review
description: Deprecated alias of code-review — review a local Putnami diff and return findings locally
allowed-tools: Bash, Read, Grep, Glob, ToolSearch, mcp__putnami__*
---

# putnami-review (deprecated alias of `code-review`)

`putnami-review` is kept so existing invocations keep working. It is
deprecated: invoke `code-review` (`/code-review` on Claude Code,
`$code-review` on Codex) instead. The alias is removed in a later release of
the contributor extension, after the migration window its documentation
announces.

Follow the `code-review` workflow in `.agents/skills/code-review/SKILL.md`
exactly, with the restriction this entry point always had: review the local
working tree or the requested revision range and return the findings in the
conversation. Do not publish the review or change external state.

From the consumer workspace root, select the CLI the workflow uses:

```bash
PUTNAMI_CLI=putnami
if [ -x ./putnamiw ]; then
  PUTNAMI_CLI=./putnamiw
fi
```
