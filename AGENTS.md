# AGENTS.md

<!-- putnami:guidance v2 begin sha256:9ea2620a4edd3faee3225e3f8f43b23f2874e5960f69c194f6019df1cfb37ae2 -->
This is a Putnami workspace.
- Build, test, lint, install, add dependencies, and generate with `./putnamiw` when present, otherwise `putnami`. Never call npm, bun, go, pip, or cargo directly for those.
- While iterating, select the projects you changed with `--projects <a>,<b>`; unlike `--impacted`, it skips every project that depends on them. Before declaring work complete, run `putnami lint,test,build,validate --impacted --enforce-coverage` once, when feasible.
- Your first code-discovery step in any task is a Putnami MCP call, not grep, rg, find, or `git show`: `putnami.search` to locate code, `putnami.context` to orient on a project, `putnami.impact` for "what breaks if I change X". In Claude Code these are `mcp__putnami__putnami_search`, `mcp__putnami__putnami_context`, and `mcp__putnami__putnami_impact`; load them with ToolSearch `putnami` if they are deferred. Do this even when you already know a keyword, a commit, or a file name. Use grep only for literal text, or after the tool answers stale or unavailable. If the MCP root differs from your worktree, use the local CLI.
- Skills and agents that an extension's agent content installs under `.agents/`, `.claude/` and `.codex/` are managed by Putnami. To customize one, copy it under another name; edited managed files are preserved and reported, never overwritten.
- Read `.agents/constraints.md` when it exists.
- Settled decisions are recorded in `decisions.json` files: the root one binds every project, a project's own binds that project. The Putnami MCP server lists the root ones. Read the file before you change a settled choice, and never re-decide one: to change a settled value, change the decision in its file in the same pull request.
<!-- putnami:guidance v2 end -->
