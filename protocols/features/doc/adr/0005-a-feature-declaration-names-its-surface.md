# ADR 0005 — A feature declaration names the surface its own project serves

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/features` (`protocols/features`)

## Context

A feature entry says what a product feature is for, never how a user or an
agent reaches it. An agent that asks for a feature learns its requirements and
evidence, then has to search the code for the commands and MCP tools that carry
it. Repository decisions settled that a feature declaration may list those
commands and tools. They did not settle whose commands and tools it may list.

The manifest wire has two versions, 1 and 2, and no rung above them. A reader
that predates a member refuses it as an unknown field, as a pre-criterion
reader refuses `verification`.

## Decision

1. A feature entry of `putnami.features.json` may carry an optional `surface`
   object with two optional lists, `commands` and `tools`. At least one list is
   present. A present list is non-empty, unique, at most 64 entries, each
   entry at most 128 bytes.
2. A command name is the path a user types after `putnami`, in lower-case words
   separated by one space: `validate`, `features validate`. A tool name is the
   name the tool listing advertises: `sdd.feature_context`, `list_projects`.
   Neither is the flat manifest key (`sdd-features-validate`) or the telemetry
   spelling (`change-verify`).
3. `surface` rides on manifest version 2. There is no version 3. A version 1
   manifest that carries it is refused with `features.unknown_field`. The
   protocol validates the shape and reports `features.invalid_surface`.
   Canonical form sorts both lists.
4. A feature lists only the surface its own project serves. The
   `features-validate` step of `@putnami/sdd` resolves each name against the
   `putnami.extension.json` that sits at the same project root as the manifest:
   command keys, command-group paths, verb names and tool keys. A name that
   file does not serve is `features.unresolved_surface`. A project root with no
   extension manifest is not resolved. Resolution reads no other extension, no
   lock and no installed state.
5. `sdd.feature_context` and `putnami features inspect|list` return the
   surface as declared.

## Rejected alternatives

- **A version 3 manifest.** It would force every committed version 2 manifest
  that lists a surface to migrate, rename the version 2 conformance vector, and
  make the TypeScript reader refuse a version it ignores today. The protocol is
  `preview`, so nothing guarantees an external consumer that a rung would
  protect.
- **List any command or tool that is loaded.** Resolution would need the
  extension catalog on the job and tool wires, which neither carries, and a rule
  for an extension that is not installed. The same declaration would then pass
  in one workspace and fail in another.
- **List another project's surface.** A feature in the core could name a
  command an extension serves, but nothing in the worktree proves the name, and
  a rename in the extension leaves a stale claim in a file it does not own.
- **Flat keys.** `sdd-features-validate` is a wire key. A reader would have to
  translate it before showing a user what to type.
- **Resolve in the protocol.** The protocol package reads no project files.
  Whether a name is served is a fact of the declaring project, so the tool that
  already reads the worktree decides it.

## Consequences

- A feature in a project with no extension manifest, such as the core CLI,
  lists a surface that nothing resolves. Its own surface tests keep the claim
  honest.
- A renamed command or tool in an extension fails `features-validate` until the
  declaration follows.
- A feature in the core does not list the commands an extension serves. The
  extension's own feature does.
- An older strict reader refuses a version 2 manifest that carries a surface.
  Every reader in this repository builds from source.
