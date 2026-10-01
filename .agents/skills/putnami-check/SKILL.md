---
name: putnami-check
description: Deprecated alias of check — run the ordinary gate and applicable local behavior proof for a Putnami workspace
---

# putnami-check (deprecated alias of `check`)

`putnami-check` is kept so existing invocations keep working. It is
deprecated: invoke `check` (`/check` on Claude Code, `$check` on Codex)
instead. The alias is removed in a later release of the contributor extension,
after the migration window its documentation announces.

Follow the `check` workflow in `.agents/skills/check/SKILL.md` exactly, with
the restriction this entry point always had: return the gate and behavior
evidence locally. Do not publish results or mutate any external system.

From the consumer workspace root, select the CLI the workflow uses:

```bash
PUTNAMI_CLI=putnami
if [ -x ./putnamiw ]; then
  PUTNAMI_CLI=./putnamiw
fi
```
