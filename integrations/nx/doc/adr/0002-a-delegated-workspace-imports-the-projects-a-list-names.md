# ADR 0002 — A delegated workspace imports the projects a list names

- **Status**: accepted
- **Date**: 2026-11-10
- **Scope**: `integrations/nx` (`@putnami/nx`), `integrations/turbo`
  (`@putnami/turbo`) and `tooling/cli` (`putnami init --from`); amends
  [ADR 0001](0001-the-adoption-mode-is-a-workspace-option.md)

## Context

ADR 0001 gives a Nx workspace one adoption mode, and Turborepo follows it. A
large repository cannot move every TypeScript project to Putnami's runner at
once: one project's scripts must first run what `@putnami/typescript` runs,
and its team must trust the result before the next project moves. With one
mode per workspace, a team moves every project or none.

## Decision

- `options["@putnami/nx"]` and `options["@putnami/turbo"]` in
  `putnami.workspace.json` take `import`, a list of project paths and globs.
  An entry matches a project's workspace-relative directory: a leading `/`
  is ignored, so `/packages/pricing` and `packages/pricing` are one entry,
  and `*`, `?` and `[...]` match within one path segment, as Go's
  `path.Match` does.
- In delegate mode, the sync writes each project an entry selects as import
  mode writes it, and every other project as delegate mode writes it. Import
  mode reads no list: it already imports every TypeScript project.
- An imported TypeScript project's `putnami.json` names
  `@putnami/typescript` beside the tool's extension, and the sync owns that
  entry: a `putnami.json` that names extensions is claimed by those alone,
  so the TypeScript extension's probe does not claim a project the sync
  wrote with the tool's extension only. This corrects ADR 0001, which
  counted on that probe; the Turborepo sync already wrote both.
- A sync job parameter `import` outranks the option, as `mode` does, so a
  rerun of `init` previews the list it would record without writing it.
- An entry that selects no project is reported as `<tool>.import_unmatched`,
  a warning: a typo shows, and an entry for a project not created yet does
  not fail the sync.
- `putnami init --from nx|turbo --import <path>,<path>` adds the paths the
  list does not hold yet and declares `@putnami/typescript`. On a workspace
  that exists it previews without `--apply`, as a mode change does.
- `putnami validate` needs no new check: it compares each `putnami.json`
  with what the sync writes in the recorded mode and list, and fails with
  `<tool>.sync_stale`.

## Rejected alternatives

- **The mode in each project's `putnami.json`.** ADR 0001 rejects it, and
  this decision keeps that: the sync owns those files and rewrites them, so
  a per-file mode would be read back from what the sync wrote. The list
  lives in the workspace options, one place a reviewer reads.
- **A `delegate` list in import mode.** It moves the other way: a team that
  adopts a large repository starts from what runs today, its tool.
- **A third mode, `mixed`.** Two modes and a list say the same with no new
  value to validate, and `delegate` with an empty list stays what it was.

## Consequences

- One repository runs some projects on Putnami's runner and cache and the
  others through its tool, and the workspace file says which.
- A project leaves the list by an edit of `putnami.workspace.json`, then
  `putnami projects sync`: `init` only adds entries.
- Rush and moon may read the same option when their own issue asks for it.
