# ADR 0082 — Putnami serves no MCP server

- **Status**: accepted
- **Date**: 2026-10-28
- **Supersedes**: [ADR 0040](0040-register-the-mcp-server-and-reconcile-agent-workflows-at-session-start.md).
  The last sentence of its decision 3 stays in force: a skill declared
  explicit-only must declare it for both hosts. It concerns agent content, not
  the server.
- **Amends**: [ADR 0081](0081-the-core-writes-declared-host-hooks.md), the
  parts that name the MCP server (see Amends)
- **Scope**: `@putnami/cli` (`internal/mcp`, `internal/collaboration`,
  `internal/cli`, `internal/commands/agentctx`, `internal/commands/lifecycle`,
  `internal/launch`, `putnamiw`, the installer scripts), features
  `cli/context-discovery`, `cli/agent-workflow-lifecycle`,
  `cli/collaboration-providers`. Slice 1 of issue 649. The protocol vocabulary
  that names MCP leaves in slice 2.

## Context

The core CLI served a stdio MCP server (`putnami mcp`). It exposed workspace
tools, resources, the settled decisions and a `run_jobs` tool, and it
reconciled agent workflows when a host started it (ADR 0040).

Issue 649 measured how agents use it. The sample is 509 Claude Code sessions
on `putnami` and `putnami-cloud` over the 30 days to 27 October 2026, and the
607 pull requests they opened:

- **Use is low.** 1,944 MCP calls, against 65,711 `grep` and `rg` calls and
  about 17,000 `lint`, `test` and `build` runs through the shell. `run_jobs`
  is almost never called.
- **Outcomes do not improve.** At equal pull request size, sessions that call
  the MCP use 1.5 to 4 times more tokens. Their share of pull requests with a
  red CI commit is not lower: 23% against 40% under 500 lines, 31% against 40%
  from 500 to 2,000 lines, 43% against 45% from 2,000 to 8,000 lines. The
  numbers are a correlation, because MCP use mostly marks `/build` sessions
  with subagents.
- **The CLI answers the same questions.** `context map`, `context pack`,
  `projects describe`, `specs` and `features` print what the tools returned.
- **The hosted search adds little.** `putnami.search` fails on 34% of calls.
  When it answers, the agent runs `grep` within its next two calls 71% of the
  time.
- **It costs code.** `internal/mcp` holds 6,369 lines of production code and
  7,643 lines of tests, plus about 2,300 lines of glue elsewhere.

## Decision

1. **The core serves no MCP server.** `putnami mcp` and `putnami mcp install`
   are deleted, with `internal/mcp`: the tools, the resources, the guidance
   push, the context-change notice, `run_jobs` and the extension-tool host.
   The CLI is the agent surface. `putnamiw` drops its cold-start relay for the
   same reason, and `internal/launch` drops the agent-workspace entry that only
   the server used.
2. **Collaboration routing moves to `internal/collaboration`.** `putnami tasks`,
   `proposals`, `memory` and `missions`, and the extension host call
   `collaboration.call`, keep their routing, envelope and binding rules
   unchanged. The two entry paths are `putnami <contract> <operation>` and the
   host RPC `collaboration.call`. Tool listings, annotations and the rule that a
   host-only operation has no tool go with the server; a host-only operation is
   still reached by the command and the host channel.
3. **Three `projects` queries are ported, the rest are not.** The usage over
   the 30 days to 28 October, subagents and the `cloud` repository included,
   decides:

   | Tool | Calls / sessions | Result |
   |---|---|---|
   | `find_owner` | 260 / 41 | `putnami projects owner <path>` |
   | `deps` | 98 / 27 | `putnami projects deps <project> [--dependents] [--transitive]` |
   | `why_impacted` | 33 / 11 | `putnami projects why <from> <to>` |
   | `impacted` | 65 / 31 | not ported: `<verb> --impacted --dry-run` answers it (164 shell uses) |
   | `topo_sort` | 0 | not ported |
   | `go_docs`, `typescript_docs` | 17 / 11, mostly unavailable | not ported |

   The commands are read-only subcommands of the existing noun. Nothing new
   appears at the root. There is no agent-facing search; `putnami intelligence
   agent-readiness` stays.
4. **The `.mcp.json` registration is removed, and so is the entry earlier
   releases wrote.** `init`, `install` and `upgrade` write no `.mcp.json`. An
   explicit `install` or `upgrade` (not the first-use install, not `init`)
   deletes the `putnami` entry under `mcpServers` when it equals, by canonical
   JSON, the entry Putnami wrote: command `putnami` or `./putnamiw`, args
   `["mcp"]`, and env `PUTNAMI_AGENT_WORKSPACE` set to
   `${CLAUDE_PROJECT_DIR:-.}`. When only an empty `mcpServers` object remains,
   it deletes the file. Otherwise it rewrites the file without the key, with
   the serialization the writer used. A changed entry stays byte for byte with
   one warning on stderr that names the file and says to remove the entry by
   hand. An unparseable file stays untouched with a warning. A missing file, or
   a file with no `putnami` entry, is silent. The install summary records the
   removal as an action.
5. **The CLI installer removes its user-scope registrations.** `install.sh`
   and `install.ps1` stop registering a server in Claude Code and Codex. When a
   registration equals the definition the installer wrote, by the detectors it
   already has, the installer removes it (`claude mcp remove --scope user
   putnami`, `codex mcp remove putnami`). It keeps a customized registration
   with a warning. `PUTNAMI_NO_AGENT_HOSTS` and `--no-agent-hosts` still skip
   the whole step.
6. **The removal code is deleted in the minor release that follows the one
   shipping this change.** Decisions 4 and 5 exist so that no host logs a
   failed server at each session start in the meantime. A code comment in each
   place states the deletion.
7. **There is no session-start reconcile.** The reconcile of agent workflows
   (ADR 0040, decision 2) had no moment other than the server start. `install`
   and `upgrade` are the reconcile moments, with the same plan, collision check,
   apply and record sequence (ADR 0004, ADR 0047). A worktree that moved to
   another lock gets the matching workflows at its next `putnami install`.
8. **The settled decisions are not pushed.** The generated `AGENTS.md` block
   keeps its line that names the `decisions.json` files. The line is enough, and
   nothing else lists the decisions.
9. **Protocol vocabulary leaves in slice 2.** Slice 1 deletes the core
   producers only: the `mcpResult` document producers in
   `internal/machine/documents.go` and the wire goldens that pin them. These
   stay until slice 2 removes them with the extension tool contract:
   `protocols/cli` `mcpResult`, the `MCPTool*` names and `ReportOriginMCP`;
   `hostHooks[].mcpTools`; the feature `surface` tool list; the extension
   manifest `tools` section; `tooling/extension-sdk/mcp`; the `mcptool.go`
   files; the `mcp-tool` bridge of `tooling/sdd-extension`; and D-022. The
   engine keeps `EphemeralSession` and `origin: "mcp"` for `cache verify`.

## Amends

- **ADR 0081, decision 2.** The explicit lifecycle commands write the host
  hooks next to the agent content, not next to a server registration. They no
  longer write a registration. The first-use install, the first-run pass and
  `context generate` still never write the hooks; the server start is gone from
  that list.
- **ADR 0081, decision 10.** A hook starts with the launcher the workspace
  needs. The rule that chose it for the server registration stays the rule: a
  source workspace (its lock records `cli.source: "workspace"` and its root
  holds a `putnamiw` file) uses `./putnamiw`, and any other uses `putnami`.
  The code keeps that choice under a launcher name, with no server behind it.
- **ADR 0081, decision 11.** Putnami writes no `.mcp.json`. The hooks no
  longer assume a `putnami` server in Claude Code. A matcher rendered from
  `mcpTools` names a tool only a host-registered server named `putnami` can
  provide, which Putnami does not install. The field stays in the manifest
  until slice 2 removes it.

## Consequences

- This is a breaking change, shipped in a minor release (D-004) with a release
  note. A workspace that `init` or `install` registered still names
  `putnami mcp` in its `.mcp.json` until an explicit `install` or `upgrade`
  runs. Until then `putnami mcp` is an unknown command, and the host reports a
  failed server.
- A user who customized the `putnami` entry or a host registration must remove
  it by hand. Putnami warns once and keeps the bytes.
- The generated `AGENTS.md` block and `.agents/constraints.md` name no MCP
  tool. Discovery runs through `context map`, `context pack`,
  `projects describe|owner|deps|why`, and `grep` for literal text.
- The `cli/context-mcp-discovery` feature becomes `cli/context-discovery`.
  Its server requirements are deleted, and three requirements are added:
  `mcp-registration-removal`, `host-registration-removal` and
  `project-graph-queries`. `cold-mcp-start`, `session-start-reconcile` and the
  MCP-only requirements of `cli/collaboration-providers`,
  `cli/diagnostics-results` and `cli/workspace-initialization` are deleted.
- `putnamiw` no longer answers an MCP client while it builds the CLI. A first
  run of a cold wrapper prints its progress on stderr and nothing else.
- ADR 0040, ADR 0028 (the wrapper relay rationale) and ADR 0055 (the `.mcp.json`
  rule) keep their text as history. This ADR replaces them where they conflict.
