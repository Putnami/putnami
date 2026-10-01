# ADR 0002 — One workspace catalog owns shared dependency versions

- **Status**: accepted
- **Scope**: `@putnami/typescript` (`typescript/extension`)

## Context

A version repeated in every member's `package.json` drifts, and the drift shows
up as two copies of a package that expects to be a singleton. Bun's catalog
protocol fixes the declaration: a member writes `"catalog:"` and the version
lives on the root manifest. Bun then fails the install when the referenced
catalog entry does not exist, which is the state of every freshly scaffolded
project whose template ships `"catalog:"`.

Rewriting the root manifest through a decoded map scrambles its top-level key
order, so unrelated keys move on every install and hide the real change.

## Decision

Workspace install seeds the catalog before running Bun. It collects every
`@putnami/*` package a member references through `catalog:`, resolves which
catalog each reference selects (bare `catalog:` selects the default,
`catalog:<name>` a named one), and adds a `latest` entry where the selected
catalog lacks one. Existing entries, Putnami or not, stay exactly as they are.

Catalog discovery accepts both placements Bun accepts: top-level
`catalog`/`catalogs` and nested `workspaces.catalog`/`workspaces.catalogs`. A
write goes back to the placement it was read from.
`typescript/extension/internal/catalog` is the single reader: install, upgrade
and the npm publisher all resolve `catalog:` through it.

A root-manifest rewrite recovers the top-level key order from the original bytes
and preserves it.

## Invariants

- Seeding never overwrites an existing catalog entry.
- A seeded entry lands in the catalog the reference selected, at the placement
  the manifest already uses.
- A workspace with no `catalog:` reference gains no catalog.
- A root-manifest rewrite preserves the top-level key order it read.
- `catalog:` is resolved through one reader, never re-parsed ad hoc.

## Rejected alternatives

- **Pin versions in every member.** That is the drift this prevents.
- **Write concrete versions from the template.** They are stale the day after
  they are committed.
- **Let Bun's "failed to resolve catalog" be the message.** It is a dead end
  right after `putnami projects create`.
- **Seed third-party dependencies too.** Choosing `latest` for them is a
  supply-chain decision that belongs to the user.
- **Re-encode the root manifest from a map.** Simpler code, unreviewable diffs.

## Consequences

- A seeded `latest` is a starting point; the upgrade path turns it into a
  resolved release. Seeding runs first, resolution second.
- Install mutates the workspace root manifest, so install is a workspace-scoped
  task.
- A new dependency protocol is taught to the one catalog reader, never to a
  second parser.
