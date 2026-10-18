# AGENTS.md

<!-- putnami:guidance v2 begin sha256:26b7c5e798329fe55b22ac2ae5279057e848824cc25371153b0fd4d9f7349cc1 -->
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
  - D-005 — with a support catalog, only a commit that touches a project the catalog lists as stable, or an unlisted project a stable one depends on, advances its version line past a patch (settled 2026-10-02, review-only, held by a human reviewer)
  - D-006 — the core CLI declares no development verb: build, test, lint, serve, run, format, package, publish, deploy, generate and validate come from @putnami/dev, which the language extensions depend on (settled 2026-10-14, review-only, held by a human reviewer)
  - D-007 — there is no kit or profile concept: installing an extension installs its extensionDependencies, and that graph decides what a workspace gets (settled 2026-10-14, review-only, held by a human reviewer)
  - D-008 — putnami init --extension <language> installs that language extension, and its dependencies bring the rest (settled 2026-10-14, review-only, held by a human reviewer)
  - D-009 — anonymous usage telemetry stays in the core CLI (settled 2026-10-14, review-only, held by a human reviewer)
  - D-010 — the core CLI computes a tree's version before any cache key; the version get|tag and changelog commands belong to @putnami/dev (settled 2026-10-14, review-only, held by a human reviewer)
  - D-011 — @putnami/contributor ships a compiled executable and owns change verify|finalize|language (settled 2026-10-18, review-only, held by a human reviewer)
  - D-012 — @putnami/dev does not depend on @putnami/contributor, which installs only on explicit request: a workspace receives the contributor skills and agents only when someone asks for @putnami/contributor (settled 2026-10-18, review-only, held by a human reviewer)
  - D-013 — putnami migrate agent-content is deleted in the next minor release, with no transition window (settled 2026-10-14, review-only, held by a human reviewer)
  - D-014 — an extension command calls the collaboration providers through an RPC call to the core, made with an SDK client (settled 2026-10-16, review-only, held by a human reviewer)
  - D-015 — the planner and the scheduler are packages, and import paths set the engine boundary: only internal/engine names planner.Plan and scheduler.RunPlan (settled 2026-10-16, review-only, held by a human reviewer)
  - D-016 — the change noun carries the change workflow: change verify, change finalize and change language replace tree verify, proposals finalize and contributor language (settled 2026-10-18, review-only, held by a human reviewer)
  - D-017 — sessions export and sessions summary stay in the core, because they read the host's session store (settled 2026-10-18, review-only, held by a human reviewer)
  - D-018 — @putnami/dev owns the version noun; version list|use, which list and switch the installed CLI binaries, become cli list|use (settled 2026-10-18, review-only, held by a human reviewer)
  - D-019 — contributor machine-load becomes doctor --machine, in the core (settled 2026-10-18, review-only, held by a human reviewer)
  - D-020 — the core's dev noun is deleted: dev extension validate becomes extensions validate <path>, and dev template <action> becomes templates validate|test|package <path> (settled 2026-10-18, review-only, held by a human reviewer)
  - D-021 — an extension manifest declares the kind of each command it adds, in an additive contract rung that ships with slice 5 of #256 (settled 2026-10-18, review-only, held by a human reviewer)
  - D-022 — a feature declaration may list the commands and MCP tools that carry the feature (settled 2026-10-18, review-only, held by a human reviewer)
  - D-023 — Cloud deletes its nine alias subcommands (tokens, distribution, oci, publish-archives, track, report, validate-config, publish-config, publish-migration) in a Cloud pull request outside #256 that ships as a breaking Cloud release (settled 2026-10-18, review-only, held by a human reviewer)
  - Project decisions, returned by `putnami.context` with their project: `tooling/cli/decisions.json` (12).
<!-- putnami:guidance v2 end -->
