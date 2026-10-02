# AGENTS.md

<!-- putnami:guidance v2 begin sha256:6e34a49b1c1b361b447547b0db8b50742e735c729eadc3b322f89b38629b43ed -->
This is a Putnami workspace.
- Build, test, lint, install, add dependencies, and generate with `./putnamiw` when present, otherwise `putnami`. Never call npm, bun, go, pip, or cargo directly for those.
- While iterating, select the projects you changed with `--projects <a>,<b>`; unlike `--impacted`, it skips every project that depends on them. Before declaring work complete, run `putnami lint,test,build,validate --impacted --enforce-coverage` once, when feasible.
- Your first code-discovery step in any task is a Putnami MCP call, not grep, rg, find, or `git show`: `putnami.search` to locate code, `putnami.context` to orient on a project, `putnami.impact` for "what breaks if I change X". In Claude Code these are `mcp__putnami__putnami_search`, `mcp__putnami__putnami_context`, and `mcp__putnami__putnami_impact`; load them with ToolSearch `putnami` if they are deferred. Do this even when you already know a keyword, a commit, or a file name. Use grep only for literal text, or after the tool answers stale or unavailable. If the MCP root differs from your worktree, use the local CLI.
- Skills and agents that an extension's agent content installs under `.agents/`, `.claude/` and `.codex/` are managed by Putnami. To customize one, copy it under another name; edited managed files are preserved and reported, never overwritten.
- Read `.agents/constraints.md` when it exists.
- Settled decisions are recorded in `decisions.json` files: the root one binds every project, a project's own binds that project. Never re-decide one: to change a settled value, change the decision in its file in the same pull request.
  - D-001 — serverless workloads scale to zero when idle (settled 2026-09-03, `validate` enforces it)
  - D-002 — every pull request names what it deletes (settled 2026-09-03, review-only, held by a human reviewer)
  - D-003 — every GitHub artifact, code comment and document is written in English (settled 2026-09-11, review-only, held by a human reviewer)
  - D-004 — before 1.0, a feature that breaks nothing ships in a patch, and only a breaking change makes a minor (settled 2026-10-02, review-only, held by a human reviewer)
  - D-005 — with a support catalog, only a commit that touches a project the catalog lists as stable advances its version line past a patch; unlisted projects and unowned files promise nothing (settled 2026-10-02, review-only, held by a human reviewer)
  - Project decisions, returned by `putnami.context` with their project: `tooling/cli/decisions.json` (12).
<!-- putnami:guidance v2 end -->
