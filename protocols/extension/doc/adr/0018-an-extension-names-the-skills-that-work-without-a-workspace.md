# ADR 0018 — An extension names the skills that work without a workspace

- **Status**: accepted
- **Date**: 2026-11-07
- **Scope**: `go.putnami.dev/protocol/extension` (`agentContent.userScope`),
  CLI contract 20 (`go.putnami.dev/protocol/cli`, `protocols/cli/contract.go`),
  and the SDK package step (`tooling/extension-sdk/agentartifact`)

## Context

A repository with no `putnami.workspace.json` can still run extension
commands from the user scope
([CLI ADR 0051](../../../../tooling/cli/doc/adr/0051-user-scope-extensions-run-without-a-workspace.md)):
`putnami observe` reads its components, impact and reports. Its agents get
none of an extension's skills, because only a workspace opts into agent
content ([ADR 0006](0006-agent-content-is-an-additive-contract.md)).

Some skills make sense there, such as one that writes a spec in the format the
repository already reads. Most do not: a skill that runs the workspace gate
cannot work without a workspace. Only the extension's author knows which is
which, and the person installing the extension cannot tell from the outside.

## Decision

### 1. `agentContent.userScope` lists the skills

The `agentContent` section gains an optional member, `userScope`: the names of
the contribution's skills that work in a repository without a workspace.

```json
{
  "name": "@acme/contributor",
  "agentContent": { "path": "content", "source": "src", "userScope": ["spec"] }
}
```

- Each entry is a skill name (`[a-z0-9][a-z0-9-]*`) and appears once
  (`invalid-agent-content-user-scope`). An empty list is the same as no
  member, and the package step writes none.
- The member belongs to both forms. The package step copies it unchanged and
  refuses a name the built content does not ship as a skill
  (`skills/<name>/SKILL.md`). A manifest alone cannot see the files, so the
  strict validator checks the shape and the package step checks the content.
- It names skills, not worker profiles: a worker profile serves a workflow
  that a workspace runs.

### 2. Contract 20 is additive

`protocols/cli` defines `UserScopeSkillsContract = 20` and raises
`LatestContract` to it. `RequiredLoadContract` returns 20 for a manifest that
declares a non-empty `userScope`. A CLI that predates the member refuses the
manifest ("requires a newer putnami") instead of installing the extension in
the user scope without its skills: the silent drop that ADR 0006 refuses for
agent content. Every other manifest keeps its stamp.

### 3. What the list means

Only `putnami extensions install --user` reads the list
([CLI ADR 0085](../../../../tooling/cli/doc/adr/0085-a-user-scope-install-writes-the-skills-its-extension-names.md)).
A workspace opt-in still installs the whole contribution and ignores it. A
listed skill therefore runs in two places: from a workspace that pins it, and
from the user's home in any repository. The author writes it to work in both.

## Rejected alternatives

- **A marker in each `SKILL.md`.** The frontmatter belongs to the agent hosts,
  and the CLI would read every skill before it knew whether to write any.
- **A second, content-only extension for the user scope.** The same skill
  would ship twice under two versions, which ADR 0006 exists to prevent.
- **Every skill in the user scope.** A skill that needs a workspace would run
  where it cannot work, and the person would learn it only from a failure.
