# ADR 0015 — The specs ratchet keeps its own discovery

- **Status**: accepted
- **Scope**: `@putnami/sdd` (`tooling/sdd-extension`): the
  `specs-ratchet-validate` step of `validate-workspace`

## Context

`specs-ratchet-validate` derives the current enforced-spec floor from the
committed manifests and compares it with the committed `specs.baseline.json`,
over the worktree only. It runs its own workspace-wide discovery: authored
manifests, specs, and repository validation.

The project-scoped `specs-validate` tasks compute the same executable-criteria
projection for their own project and write it as a declared output. Two ways
to share one discovery look attractive: make the ratchet read those
projections, or merge it with `architecture-validate`.

## Decision

The ratchet keeps its own discovery. The duplication with `specs-validate` is
deliberate: the two answer different questions over different scopes.

- **Reading the projections is unsound under a scoped run.** The ratchet asks
  whether any enforced project left the floor, which no subset can answer.
  Under `--impacted` only impacted projects leave a projection, so the ratchet
  would report every unselected enforced project as a regression.
- **Merging with `architecture-validate` merges disjoint cache keys.** The
  ratchet keys on `**/putnami.features.json`, `**/specs/*.json`,
  `**/putnami.json`, `putnami.workspace.json`, and `specs.baseline.json`;
  architecture keys on its own declarations, debt files, and capability
  manifests. Editing either set would re-run both.
- **The cost does not justify either.** On this workspace (47 spec-owning
  projects, 448 executable requirements) the step takes about 90 ms per run,
  mostly process spawn and the workspace view, and it caches.

## Consequences

Reopening this needs a workspace where the step is slow enough to matter *and*
a floor derivation that stays whole under `--impacted`.
