# ADR 0008: A written file keeps the repository's layout

- **Status**: accepted
- **Date**: 2026-11-14
- **Scope**: `go.putnami.dev/protocol/workspace` (`protocols/workspace`): the
  configuration renderer; every writer of `putnami.json` and
  `putnami.workspace.json` in `@putnami/cli`, `tooling/stacks` and the
  monorepo tool integrations

## Context

The sync rewrote `putnami.json` and `putnami.workspace.json` from a decoded
document: two-space indented, keys sorted, one array item per line. Biome,
Prettier and oxfmt all write a short array on one line, so a repository whose
lint or format task checks these files failed right after `init`. Formatting
the files once did not last: the next sync that added a dependency rewrote the
whole file in Putnami's layout, and the membership update rewrote
`putnami.workspace.json` even when its content was equal.

Each writer had its own encoder, and none of them read the bytes it replaced.

## Decision

1. **One renderer, in the workspace protocol.** `RenderConfigFile` renders a
   document against the bytes of the file it replaces. Every writer of a
   Putnami configuration file renders through it, and writes only when the
   rendered bytes differ from the file's.
2. **The existing file is the layout.** A value whose content is unchanged
   keeps its bytes. Members keep their order, and a new member takes the place
   the document gives it after the members it follows. The file keeps its
   indentation unit and its line endings. An object keeps one member per line
   or one line, as the file wrote it, and expands when it no longer fits.
3. **Short arrays fit on one line.** A changed or new array of scalars that
   fits in 80 columns, its trailing comma included, is written on one line,
   unless the file it replaces writes a short array one item per line: that
   file keeps its own layout, so a file Putnami wrote before stays as it was.
4. **A new file takes the formatters' layout.** It is indented as the
   `package.json` beside it, or with two spaces, writes one member per line in
   every object, and fits short arrays on one line. Biome indents with tabs by
   default while Prettier and oxfmt indent with two spaces: no single layout
   passes all three with their defaults, so the file follows the repository's
   own `package.json`.

## Consequences

- Adding one dependency to a formatted `putnami.json` changes that one line.
- A sync whose content is unchanged writes nothing, so it touches no
  modification time and leaves no diff.
- The renderer does not run the repository's formatter. A formatter whose
  line width is not 80, such as oxfmt whose default is 100, can still join or
  break an array the sync changed, between 81 and 100 columns for oxfmt.
- A file that is not a JSON object falls back to the layout of a new file.
