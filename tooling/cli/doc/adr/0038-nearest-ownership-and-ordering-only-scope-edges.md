# ADR 0038 — The nearest project owns a path, and an implicit scope edge orders without carrying impact

- **Status**: accepted
- **Scope**: `@putnami/cli-model` (`workspace` path ownership, dependency
  graph, impact walk), `@putnami/cli` (`--impacted`, `find_owner`,
  `why_impacted`, change plan)

## Context

In a workspace with activated scopes, one Markdown line under a scope
directory selected 1110 jobs across 106 of 111 projects. Two mechanisms
composed. Ownership walked every ancestor, so a file under `<scope>/libs/core/`
had two owners and every child edit was also a scope-self edit. And impact
propagation read the implicit include→scope-self edge that `BuildGraph` adds
to every activated scope, so a scope-self change reached every include.

That implicit edge exists to order the schedule: workspace-level artifacts
run before the workloads that consume them, and `^<job>` resolves to the
scope's job. It says nothing about what an include reads.

## Decision

### 1. Holding a path and reading it are different relations

- A path's directory owner is the deepest project directory that contains it,
  and only that one. A project at the workspace root (`Path == "."`) owns what
  no deeper project claims.
- Reader claims are additive on top of the owner: cross-project asset claims
  (`build.assets`, `options.generate.assets`) and declared file inputs
  (`options.<layer>.filePatterns`) select every claimant whose declaration
  covers the path.
- A change to a scope's `putnami.json` seeds every project whose chain merged
  it (`Project.Scope.ConfigPaths`) with the `scope-config` seed kind. It is a
  reader claim, never an owner claim, whether the scope is activated or not.
  A scope's `tags`, `extensions` and name pattern reach each include; its
  `options` apply to the scope-self only.
- An enclosing project that consumes a nested project's files says so, with
  `"dependencies": ["<nested>"]` or a `filePatterns` entry covering the
  subtree.
- `find_owner`, the change plan's direct-project list and `--impacted` all
  answer the one directory owner.

### 2. Ordering a schedule and carrying an input are different relations

- The implicit include→scope-self edge stays in `DependenciesOf`,
  `DependentsOf`, their transitive forms, `TopologicalOrder`, `FindCycle` and
  `DependencyPath`. `^<job>` resolution, `projects info`, the `deps` MCP tool,
  the agent context pack and spec-gate recovery keep their answers.
- It is excluded from `ImpactDependentsOf`, which `--impacted` propagation and
  `DependentPath` (`why_impacted`) walk. `why_impacted` can therefore never
  name an impact that `--impacted` does not perform.
- A child that declares `"dependencies": ["/<scope>"]` gets a full edge,
  `BuildGraph` skips the implicit one, and a scope-self change selects it.

Extension-consumer edges are the mirror case: they widen impact without
ordering the schedule ([ADR 0042](0042-impacted-plans-from-the-commit-and-records-why.md)).
Both are stated in the graph or the impact index, never inferred from file
shape.

## Rejected alternatives

- **A per-file carve-out for documentation paths**: it leaves `infra/**` at
  the whole workspace and teaches the model a filename vocabulary it has
  nowhere else. The fault was an ordering edge read as an input edge.
- **Either fix alone**: with nearest ownership alone, the scope-self still
  hands its change to every include; with the edge fix alone, every nested
  file still has two owners.

## Consequences

- A nested project's file selects the nested project only. An enclosing
  project that reads a nested member declares the relation.
- The build cache hasher still walks into nested project directories for `**`
  and empty pattern sets, so an enclosing project's key can be wider than its
  selection. Skipping nested roots would relocate every enclosing key.
- `tooling/sdd-extension/internal/wsview` keeps its copy of `BuildGraph`: it
  holds only `deps` and `DependenciesOf` and propagates no impact.
