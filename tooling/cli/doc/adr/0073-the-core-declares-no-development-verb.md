# ADR 0073 — The core declares no development verb

- **Status**: proposed, amended by
  [ADR 0079](0079-compose-and-qualify-move-to-putnami-dev.md): `compose` and
  `qualify` moved to `@putnami/dev`, so no core literal names them, and the
  portable, MCP `run_jobs` and `--no-watch` checks read their traits
- **Scope**: `@putnami/cli` (`internal/commandmeta`, `internal/cli`,
  `internal/commands/completion`, `internal/engine`, `internal/extension`,
  `internal/mcp`), `@putnami/cli-model` (`extension`) and `@putnami/dev`
- **Amends**: [ADR 0067](0067-putnami-dev-declares-the-development-verbs.md)
  decisions 3, 4 and 5, D2, D4 and its consequences;
  [ADR 0070](0070-a-task-scope-keeps-a-job-by-its-declared-side-effects.md)
  decision 1 and its consequence on the four readers
- **Decisions**: D-006 of the root `decisions.json`

## Context

ADR 0067 let `@putnami/dev` declare the development verbs as a vocabulary,
and kept the core's job commands, built-in aliases and `WellKnownVerbs`
defaults as a fallback. ADR 0070 kept the default side effects of `publish`
and `deploy` as a floor that no manifest removes. Both left the removal to
one breaking change that meets D2. This is that change.

## Decision

1. **The catalog declares no job command and no alias.** The eleven job
   entries (`build`, `test`, `lint`, `serve`, `run`, `format`, `package`,
   `publish`, `deploy`, `generate`, `validate`) and the nine built-in
   aliases (`b`, `t`, `l`, `s`, `f`, `p`, `P`, `d`, `D`) leave the catalog.
   Help, completion, the command surface and MCP `run_jobs` read the
   vocabulary and commands of the loaded extensions: the vocabulary's verbs
   first, in vocabulary order, then the other public commands by name (D2),
   except a name the catalog resolves to a core command, which a user cannot
   run as a job.
   Outside a workspace, help lists no job command, alias or provider-bound
   flag, says that no extension is loaded and how to add one, and reads no
   user-scope extension (D4). There, the help of a job command says that no
   workspace was found, as a run of it does. A command name resolves through the
   vocabulary's aliases, then the workspace's. A workspace alias of the same
   name hides a vocabulary alias with a warning, one named like a former
   built-in alias included.
2. **One refusal in the engine covers every entry point.** After the run's
   one extension discovery, and before flags, policy or selection, the
   engine refuses the commands that no loaded extension declares as a job.
   The usage error exits 2 and reads: `no extension of this workspace
   declares <commands>; name a language extension in
   putnami.workspace.json, then run: putnami extensions install`. A terminal
   run, a comma list, an alias, `impact-plan`, `cache verify`, `compose` and
   MCP `run_jobs` all reach it through `engine.Run`; `putnami help
   <command>` and `<command> --help` return the same error. The refusal
   steps aside for a workspace lifecycle run, and when a configured registry
   extension or a dependency is missing, so the missing-extension guidance
   comes first. When another extension did not load, it may be the one that
   declares the command: the refusal names each such extension and why it
   did not load, and exits 1. `cache verify` runs the agent gate tasks
   (`agentctx.GateTasks`). The engine's `@putnami/sdd` hint is deleted.
3. **The core keeps no side-effect floor.** A command's side effects are
   the traits its loaded jobs declare. The git requirement, the cache trust
   floor, the task-scope narrowing and the MCP `run_jobs` refusal read
   `extension.CommandSideEffects` with no `WellKnownVerbs` fallback. A job
   that declares no traits takes its vocabulary verb's defaults, or none.
   Every first-party `publish` declares `sideEffects: "registry"`, so
   first-party plans keep what they kept. The CLI resolves the cache trust
   before the first-use bootstrap, from the extensions installed then, so
   the run raises a trust of `any` to `ci` when its own discovery finds a
   command with registry side effects.

## What this amends

- ADR 0067 decision 3: the core falls back to no job command and no
  built-in alias, so an output without a loaded vocabulary changes.
- ADR 0067 decision 4: a verb without default traits leaves none, and a
  command's own traits no longer keep a well-known side effect.
- ADR 0067 decision 5: every catalog command is a structured command.
- ADR 0067 D2 and D4: decision 1 meets both. ADR 0067 consequences: this
  is the removal, and `run_jobs` fails closed when extensions cannot load.
- ADR 0070 decision 1 and the consequence on the four readers: no reader
  counts a well-known default; each reads the declared traits.

## Consequences

- A workspace without a development extension shows no development verb
  and refuses each with exit 2. Installing `@putnami/dev` through a
  language extension restores the verbs, aliases and help it had.
- A `publish` or `deploy` whose traits name no side effect is planned, kept
  and gated like any command: it narrows by task scope, needs no git
  history and keeps the requested cache trust.
- The first-use bootstrap and the workspace before hooks run before the
  refusal, which needs the run's discovery: in a fresh checkout, a mistyped
  command installs the workspace, then exits 2.
- `protocols/` keeps only the five lifecycle rows of `WellKnownVerbs` and no
  `invalid-verb-traits` floor: a vocabulary gives a verb any valid side
  effect, or none.
- The workload and source-rewriting traits, not the names `serve`, `run`
  and `format`, decide watch mode, the hosted and `run_jobs` refusals,
  composition, exit-code forwarding and the portable refusal. The `compose`
  and `qualify` literals name core commands and stay.
- Publication reads no name:
  [ADR 0074](0074-deployment-follows-the-declared-release-set-trait.md)
  replaces `runner.PublicationCommands` with the registry and cloud side
  effects that commands and tasks declare, in the ancestry snapshot, the
  portable and publication paths and the publication outbox, and `--dry-run`
  synthesizes its parameter for a single command that declares the registry
  side effect.
