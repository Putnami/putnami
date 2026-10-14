# ADR 0067 — @putnami/dev declares the development verbs

- **Status**: accepted
- **Scope**: `@putnami/cli` (`internal/cli`, `internal/commands/completion`,
  `internal/extension`), `@putnami/cli-model` (`extension`),
  `go.putnami.dev/protocol/extension` and `go.putnami.dev/protocol/cli`
  (contract rung 9), and the `@putnami/dev` extension
- **Decisions**: D-006, D-007, D-009, D-010 and D-012 of the root
  `decisions.json`

## Context

D-006 settles that the core CLI declares no development verb. `build`,
`test`, `lint`, `serve`, `run`, `format`, `package`, `publish`, `deploy`,
`generate` and `validate` come from `@putnami/dev`, which the language
extensions depend on (D-007,
[ADR 0066](0066-an-extension-installs-and-loads-its-extension-dependencies.md)).

Dispatch already works without the core: any first word that is not a core
command is planned through the jobs the loaded extensions declare under that
name. What the core still owns for these verbs is their presentation and
defaults:

- the order and descriptions that help lists;
- `putnami help <verb>` and its related commands;
- the one-letter aliases;
- completion of the verb list;
- the default traits of a job whose command declares none
  (`WellKnownVerbs`).

`@putnami/dev` runs none of these verbs: the language extensions do. A
manifest command needs at least one pipeline step, so `@putnami/dev` cannot
declare them as commands. Declaring them with a no-op step would add one job
per verb and project to every plan, and change every recorded session.

## Decision

### The verb vocabulary

1. **A manifest declares verbs without running them.** The manifest gains a
   `verbs` section: an array of verbs, each with a name, a description,
   aliases, default traits and related commands. The array order is the help
   order. A vocabulary adds no job, so plans, cache keys and session
   documents do not change, except through default traits (point 4).
2. **Vocabularies merge by extension name.** The CLI merges the
   vocabularies of the loaded extensions in extension-name order, each in
   its declared order. The first declaration of a verb wins whole. A second
   vocabulary that declares the same verb draws a discovery warning that
   names both extensions.
3. **The vocabulary comes first, the core falls back.**
   - Help lists the merged verbs first, then the core's job commands the
     vocabulary does not name. Inside a workspace it still lists a verb only
     when a loaded extension declares a command of that name. A declared
     command that neither the vocabulary nor the catalog names is not listed
     until the change that meets D2.
   - `putnami help <verb>` suggests the verb's related commands.
   - A command name resolves through the vocabulary's aliases, then the
     core's built-in aliases, then the workspace's. A workspace alias of the
     same name hides a vocabulary alias, with a warning, so an extension
     update never changes what a workspace alias runs. A workspace alias
     named like a built-in alias never applies, so it hides nothing and
     draws no warning. An alias that an
     earlier verb claimed, or that names a verb, a command or command group
     of a loaded extension, or a command of the core catalog, is dropped.
     `putnami impact-plan` resolves the same aliases. A provider process
     resolves no vocabulary alias.
   - Completion offers the merged verbs and their aliases.
   - Without a loaded vocabulary, every output is the one before this
     decision, byte for byte.
4. **A vocabulary gives default traits.** Once discovery has loaded every
   extension, a job whose command declares no traits takes its verb's
   traits. A verb that declares none leaves the `WellKnownVerbs` defaults in
   place. A command's own traits still replace both wholesale. The pass runs
   for the workspace and for the user scope. While `WellKnownVerbs` gives a
   verb side effects, a vocabulary keeps them: strict validation refuses
   default traits that change them (`invalid-verb-traits`), and discovery
   sets such traits of a loaded vocabulary aside with a warning. No
   vocabulary makes `publish` or `deploy` look harmless to the gates that
   read side effects.
5. **A path outside the catalog is not a built-in.** Help listed every
   command path the catalog does not know, as if the core provided it. Now
   help always lists a catalog command that is not a job command, and lists
   any other path only when a loaded extension declares it. Every path help
   reached before this decision is a catalog path, so its output does not
   change.

### Contract

6. **A vocabulary requires contract 9 from every origin.** A manifest that
   declares `verbs` requires CLI contract 9, `VerbVocabularyContract`. The
   rung is part of `RequiredLoadContract`, so the floor binds a manifest
   loaded from `node_modules` too. Its default traits change how other
   extensions' jobs are planned, and a reader below 9 would plan them with
   other traits. A vocabulary counts as contract surface, and a manifest
   that declares only a vocabulary is complete.
7. **The Go and npm packagers gate a vocabulary.** Their raw surface check
   counts a `verbs` section, so they validate a vocabulary strictly and
   stamp 9, a vocabulary-only manifest included. A defective vocabulary
   fails packaging, even beside a registry extension dependency, which
   otherwise takes the stamp-only path. The scaffold `package` command
   (`agentartifact.PackageExtension`) still requires agent content, so it
   does not package a vocabulary-only `@putnami/dev`.

### Slice decisions

This ADR states D1 to D10 as proposals that answer the open questions of
the `@putnami/dev` slice. Merging this ADR adopts them. The change that
implements D9 records it in the root `decisions.json` as an amendment of
D-009, and the change that implements D2 records it in
`tooling/cli/decisions.json`.

- **D1 — contributor skills.** A manifest field on `@putnami/dev` opts the
  workspaces of its dependents into the agent content of
  `@putnami/contributor` (D-012). It is designed with `@putnami/dev`.
- **D2 — what help lists.** Help lists the public commands that loaded
  extensions declare: vocabulary verbs first, in vocabulary order, then the
  other declared commands in name order. A workspace with only
  `@putnami/cloud` still lists the `build` that cloud declares, and a
  declared command such as `clientgen` is listed. This is a requirement on the
  change that removes the development verbs from the core catalog. This
  decision does not provide it (point 3).
- **D3 — development flags.** No global flag becomes a development flag in
  this slice. `--profile` and `--dry-run` stay core flags.
- **D4 — help outside a workspace.** Once the core declares no verb,
  `putnami --help` outside a workspace shows the "No extension of this
  workspace declares a job command" guidance. It does not read user-scope
  extensions.
- **D5 — the agent gate default.** The default gate tasks
  (`lint,test,build`) stay in `protocols/ci` as protocol data. Reading them
  is not the core declaring a verb.
- **D6 — `version get|tag`.** Where these commands move (D-010) is decided
  once the first tooling extension with an executable exists.
- **D7 — the rung-8 stamp.** The rung-8 stamp that a language extension
  takes for its dependency on `@putnami/dev` is not a breaking change, and
  its commit carries no breaking marker.
- **D8 — dependency constraints.** The language extensions name
  `@putnami/dev` with an empty constraint, which resolves on `latest`
  unless an explicit channel flows down (ADR 0066 point 5). `@putnami/dev`
  is published before any build that names it.
- **D9 — telemetry.** The telemetry vocabulary stays a closed wire
  allowlist (D-009). It widens to the public verbs that first-party
  `@putnami/*` manifests declare, and the CLI sends no other verb.
- **D10 — support status.** `@putnami/dev` takes the `preview` support
  status in `putnami.support.json`.

## Consequences

- An extension can change the help, aliases, completion and default traits
  of a verb another extension runs. The first vocabulary by extension name
  wins, so two vocabularies never interleave a verb.
- Removing the development verbs from the core catalog, aliases and
  `WellKnownVerbs` is a separate, breaking change, and it must meet D2. This
  decision removes nothing.
- The MCP `run_jobs` trait check reads the traits discovery gives each job,
  vocabulary defaults included. Only its fallback for a command no loaded
  extension declares reads `WellKnownVerbs`; it changes with the removal.
