# ADR 0040 — Install registers the MCP server, and a session start reconciles agent workflows

- **Status**: accepted
- **Scope**: `@putnami/cli` (`internal/commands/agentctx`,
  `internal/commands/lifecycle`, `internal/cli`), the extension SDK
  agent-content builder, public onboarding docs

## Context

Agents in a Putnami workspace rarely call the Putnami MCP tools. In a measured
week, 2 of 183 Claude Code sessions made any Putnami MCP call, against 7,278
grep, rg, and find calls. In headless runs of one discovery question with the
server connected, guidance that said "MCP first" produced a Putnami MCP call
first in 3 of 3 runs; the guidance block alone did in 0 of 2.

Three gaps explain the rest. A workspace that never ran `putnami mcp install`
gives the host no project-scoped server. Skills were not allowed to call the
tools. And an agent reads its skills when the session starts, before any
command runs the first-run ensure pass, so a worktree that moved to another
lock serves the workflows of the lock it was installed with.

## Decision

### 1. `.mcp.json` registration

`putnami mcp install` writes the `putnami` entry to `.mcp.json` at the
workspace root. It creates a missing file, keeps other servers and unknown
top-level keys, rewrites a diverged `putnami` entry (the user asked for this
exact write, so it repairs a hand-edited registration), and fails on a file it
cannot parse, leaving it untouched.

`putnami init`, an explicit `putnami install`, and `putnami upgrade` (except
its dry run) add the entry with the same merge when no `putnami` entry exists.
They differ from the explicit command where the user did not ask for this
write:

- A `putnami` entry that differs from the canonical one is kept byte for byte.
  Someone wrote it on purpose.
- A file that cannot be read or parsed is left untouched and reported as a
  warning, and the command carries on.

Two paths never register:

- The first-use bootstrap that runs before an unrelated command
  ([ADR 0024](0024-lazy-workspace-initialization.md)): it must not change a
  committed file as a side effect.
- `putnami context generate`: its output is compared byte for byte against the
  committed files.

No command deletes or rewrites a `.mcp.json` for any other reason. The
installer's host-level registration through the Claude Code and Codex CLIs is
separate and stays the zero-configuration path; `--no-agent-hosts` or
`PUTNAMI_NO_AGENT_HOSTS` skips it.

### 2. The MCP server start reconciles agent content

`putnami mcp`, the stdio server, reconciles before it answers its first
request. A host starts that server when an agent session starts, the one
moment that always precedes the agent reading a skill. A git `post-checkout`
hook is rejected: it lives in a directory Putnami does not own, runs on
checkouts no agent follows, and misses worktrees created without it.

For each `extension:` opt-in whose extension the committed lock pins, or whose
npm package is installed ([ADR 0047](0047-extension-owned-agent-content.md)):

1. When the local ownership record names exactly the pinned version, identity
   digest, and content manifest digest, the pass ends for that contribution.
   No file is hashed and nothing is written.
2. Otherwise, the content is read from the extension release this worktree
   already installed and materialized through the ordinary plan, collision
   check, apply, and record sequence ([ADR 0004](0004-agent-artifact-ownership.md)).
   Nothing is downloaded: a session start must not wait on a registry.
3. Every failure is a warning on stderr (never stdout, which carries the
   protocol), and the server starts regardless. A collision writes nothing,
   and the warning names each preserved path, its reason, and
   `putnami install`.

The pass never resolves a version, never writes the lock, never builds a local
extension's content, and never retires an owner. It runs under a 5-second
deadline. Two sessions starting in one worktree converge, because each
publishes with per-file atomic renames, writes the record last, and writes the
same bytes for the same pin.

### 3. A skill can call the tools its guidance names

The SDK agent-content builder writes
`allowed-tools: Bash, Read, Grep, Glob, ToolSearch, mcp__putnami__*` for a
skill that declares no Claude metadata of its own. Skill bodies stay provider
neutral: they name the MCP tools (`putnami.search`, `putnami.context`,
`putnami.impact`) and tell the agent to load deferred tools by searching for
`putnami`, so Codex gets the same discovery step. A skill declared
explicit-only (`disable-model-invocation` for Claude,
`allow_implicit_invocation: false` for Codex) must declare it for both hosts,
or the build fails.

## Consequences

- A fresh `putnami init` writes three files for assistants: `AGENTS.md`,
  `CLAUDE.md`, and `.mcp.json`. A new Claude Code session finds the server
  after the host's one-time project approval.
- A repository that commits a `.mcp.json` without a `putnami` entry sees the
  file change after its next explicit `putnami install`.
- A worktree that moved to another lock gets the matching workflows at its
  next agent session. When the pinned release is not installed, the session
  starts with the old files and a warning naming `putnami install`.
- A file the user edited inside managed content blocks the reconcile, with the
  same warning at every session start, until the user reverts or moves it.
