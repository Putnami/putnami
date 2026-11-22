# AGENTS.md

<!-- putnami:guidance v2 begin sha256:e260218ee1219a5fbb6165b3e9b866a90d06cb14d07adf151dfc8a4ca1dadc72 -->
This is a Putnami workspace.
- Build, test, lint, install, add dependencies, and generate with `./putnamiw` when present, otherwise `putnami`. Never call npm, bun, go, pip, or cargo directly for those.
- While iterating, select the projects you changed with `--projects <a>,<b>`; unlike `--impacted`, it skips every project that depends on them. Before declaring work complete, run `putnami lint,test,build,validate --impacted --enforce-coverage` once, when feasible.
- Discover code through the CLI, then grep: `putnami projects list|describe|owner|deps|why` for the project graph, `putnami context map|pack` to orient on the workspace or a project, and `putnami <verb> --impacted --dry-run` for what a change affects. Use grep for literal text.
- To build an HTTP API with Postgres, follow https://putnami.dev/docs/how-to/build-a-postgres-api-in-go (Go) or https://putnami.dev/docs/how-to/build-a-postgres-api-in-typescript (TypeScript): the template, the framework docs, the database, migrations, tests and the Docker image.
- Skills and agents that an extension's agent content installs under `.agents/`, `.claude/` and `.codex/` are managed by Putnami. To customize one, copy it under another name; edited managed files are preserved and reported, never overwritten.
- Read `.agents/constraints.md` when it exists.
- Settled decisions are recorded one per file in `decisions/<id>.json`: the root `decisions/` binds every project, a project's own binds that project. Read the decision before you change a settled choice, and never re-decide one: to change a settled value, change its file in the same pull request. Record a new decision in a new file named by a slug id.
<!-- putnami:guidance v2 end -->

No commit, pull request, comment or document names an agent, a model or a session link.
