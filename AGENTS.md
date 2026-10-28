# AGENTS.md

<!-- putnami:guidance v2 begin sha256:6a0271f98bb8ee54791c977b6a06d39fd8bdec16ca0da90429105cc119612314 -->
This is a Putnami workspace.
- Build, test, lint, install, add dependencies, and generate with `./putnamiw` when present, otherwise `putnami`. Never call npm, bun, go, pip, or cargo directly for those.
- While iterating, select the projects you changed with `--projects <a>,<b>`; unlike `--impacted`, it skips every project that depends on them. Before declaring work complete, run `putnami lint,test,build,validate --impacted --enforce-coverage` once, when feasible.
- Discover code through the CLI, then grep: `putnami projects list|describe|owner|deps|why` for the project graph, `putnami context map|pack` to orient on the workspace or a project, and `putnami <verb> --impacted --dry-run` for what a change affects. Use grep for literal text.
- Skills and agents that an extension's agent content installs under `.agents/`, `.claude/` and `.codex/` are managed by Putnami. To customize one, copy it under another name; edited managed files are preserved and reported, never overwritten.
- Read `.agents/constraints.md` when it exists.
- Settled decisions are recorded in `decisions.json` files: the root one binds every project, a project's own binds that project. Read the file before you change a settled choice, and never re-decide one: to change a settled value, change the decision in its file in the same pull request.
<!-- putnami:guidance v2 end -->
