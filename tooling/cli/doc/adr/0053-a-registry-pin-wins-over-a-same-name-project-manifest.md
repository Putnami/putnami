# ADR 0053 — A registry pin wins over a project manifest of the same name

- **Status**: accepted
- **Scope**: `@putnami/cli` (`internal/extension` discovery,
  `internal/workspace` task index, `internal/engine` impact scope),
  `@putnami/cli-model` (`extension.ExtensionDescription.PinnedOver`)

## Context

A workspace can hold the source of an extension: a project whose root carries
`putnami.extension.json`. Discovery registers each extension name once, and
project manifests register before the workspace `extensions` entries.

A workspace that develops an extension has two reasons to run its published
build instead of its source. When every project runs the source, a change to
the extension re-runs its tasks on every project
([ADR 0042](0042-impacted-plans-from-the-commit-and-records-why.md)), so
`--impacted` selects the whole workspace. And when the workspace stops
listing the path to avoid that, projects still run the source, so a change
that breaks the extension passes its own pull request and fails the next
unrelated one.

## Decision

1. A workspace `extensions` key equal to a project manifest's `name`, and that
   is not that project's path, pins the published build. Discovery sets the
   project manifest aside, and the pinned build serves every project.
2. A path the workspace lists explicitly (`"/tools/acme": ""`) keeps the
   project manifest. Path keys sort before name keys, so an explicit path wins
   when both are listed.
3. When the pinned build does not load (not installed, or refused), the
   project manifest loads instead and discovery warns once to run
   `putnami install`. Pinning never removes an extension that ran before.
4. The project stays an ordinary project that builds, tests, packages and
   publishes itself. The pinned build records the project it replaces
   (`PinnedOver`), and its jobs carry that project's path. A project that
   names the path in its own `extensions` keeps matching the extension's
   jobs, now served by the pinned build, and `--impacted` keeps it an
   extension consumer of that project.

## Consequences

- The extension project's own jobs and the projects that name its path gate
  a source change. Every other project meets the change at the pin bump.
- A project that names the extension by path re-runs its jobs of the
  extension when the source changes, although they run the pinned build. That
  is the place for a job that exercises the source build, such as an image
  that bakes it.
- Discovery keeps one extension per name. Running the source for one project
  and the published build for others in the same run is not supported.
