# ADR 0085 — A user-scope install writes the skills its extension names

- **Status**: accepted
- **Date**: 2026-11-07
- **Amends**: [ADR 0051](0051-user-scope-extensions-run-without-a-workspace.md)
  decision 2 ("no AI context is generated") and
  [ADR 0047](0047-extension-owned-agent-content.md) decision 1 (the
  workspace opt-in is the only way content is installed)
- **Scope**: `@putnami/cli` (`internal/commands/extensions`,
  `internal/userskills`, `internal/agentartifacts`)

## Context

An extension may name the skills of its agent content that work in a
repository without a workspace (`agentContent.userScope`,
[protocol ADR 0018](../../../../protocols/extension/doc/adr/0018-an-extension-names-the-skills-that-work-without-a-workspace.md)).
A person installs such an extension with
`putnami extensions install --user <name>`, the same command that makes
`putnami observe` work in a repository with no `putnami.workspace.json`.
Something must put those skills where the agent hosts read them, under rules
as strict as the workspace's, and without touching the repository.

## Decision

### 1. `install --user` writes the listed skills, and only them

After the pin is written, `extensions install --user <name>` reads the
extension's content through the user-scope pin, with the same chain as a
workspace: the lock binds the extension manifest, the manifest binds the
content manifest, and the content manifest binds every file
([ADR 0047](0047-extension-owned-agent-content.md) decision 3).

From that tree it keeps the files of each listed skill for the hosts that read
skills from the user's home:

| Host | Path under the user's home |
|---|---|
| Claude Code | `.claude/skills/<skill>/**` |
| Codex | `.agents/skills/<skill>/**` |

Worker profiles (`.claude/agents`, `.codex/agents`) and unlisted skills are
never written. An extension that lists no skill writes nothing and prints no
line. The home is `os.UserHomeDir()`.

The explicit user-scope install of a content extension is the opt-in: the
author decided which skills work without a workspace, and the person asked
for that extension in their own scope. Inside a workspace nothing changes:
only `agentArtifacts` opts into content there.

### 2. The workspace's ownership rules apply unchanged

`internal/agentartifacts` plans and applies the write, rooted at the user's
home, with the ownership record in the user scope
(`~/.putnami/user/.putnami/agent-artifacts/<name>.json`). The same table holds:
a file it did not write, or one the person edited, is preserved and reported
as a collision, and one collision writes nothing. The pin stays installed and
the command exits 1 with the collision report, so the person can move their
file and run the install again.

A re-run whose pin and record agree writes nothing, not even identical bytes.
A new release updates the files it wrote and removes the ones it dropped,
under the same rules.

### 3. `remove --user` removes what it wrote

`extensions remove --user <name>` removes the files the record names, keeps a
file the person edited and reports it, prunes the directories it emptied, and
deletes the record, before it drops the pin.

### 4. Inside a workspace, the workspace's copy wins

Claude Code prefers a personal skill to a project skill of the same name, so a
user-scope `spec` would hide the copy a workspace pins. ADR 0051 decision 5
says the user scope does not exist inside a workspace. The CLI cannot enforce
that in a host it does not run, so each listed skill does: its first step
looks for `putnami.workspace.json`, and a copy loaded from outside that
workspace follows the workspace's own copy.

## Enforceable invariants

- Nothing is written in the current directory.
- Only files of listed skills are written under the user's home, and only
  under `.claude/skills/` and `.agents/skills/`.
- A user-owned file is never overwritten or removed.

`internal/userskills/userskills_test.go` proves them.

## Known limits

- A person who points Claude Code at another configuration directory
  (`CLAUDE_CONFIG_DIR`) or Codex at another home does not get the skills
  there.
- The skills are written for every supported host; a person who uses one host
  gets the other's files too.

## Rejected alternatives

- **A separate flag** (`--agent-content`): the install of a content extension
  in the user scope has no other purpose, and a second step is one more way
  to end up with a pin and no skills.
- **Writing into the repository** (`.claude/skills` of the current
  directory): ADR 0051 forbids writes in the caller's directory, and the
  skills would land in the team's history unasked.
- **Distinct names for the user-scope copies**: nothing would be hidden, but a
  request for `build` would find no skill to say that it needs a workspace.
