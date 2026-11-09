# ADR 0007: A delegated build graph backs its edges

- **Status**: accepted
- **Date**: 2026-11-09
- **Scope**: `go.putnami.dev/protocol/workspace` (`protocols/workspace`): the
  probe's `dependencySources`; `go.putnami.dev/protocol/extension`: the
  workspace adapter's `buildGraphEdges` and contract 21

## Context

The declared-edge check judges an edge by its provenance: an import backs it,
a declaration alone does not, and a project whose provider reports no
provenance at all is skipped and named in a `workspace.declared_edge` warning.

A delegated stack, such as `@putnami/pants`, takes its edges from the build
tool's own graph: `pants peek ::` or its export. Pants infers them from the
sources' imports, and its build fails without them. None of the protocol's
sources fits such an edge: `go-module` and `package-json` name other
manifests, and `declared` would report every one of them as a phantom. With no
source to claim, the probe reports none, and every run warns once per package
that has an edge.

## Decision

1. **A new provider source, `build-graph`.** It names an edge the build tool a
   delegated stack runs states in its own dependency graph. Like an import, it
   backs the edge in the declared-edge check: the build performs it. It is no
   import, so the visibility check does not judge it. A Pants package's
   `putnami.json` makes it a scope for the packages nested under it, and the
   tool's own graph, not Putnami's scopes, states who may depend on what:
   judging those edges would refuse the graph of every repository whose
   packages nest.
2. **The adapter declares it.** `workspace.buildGraphEdges` states that the
   adapter's probe answers `build-graph`. It requires
   `workspace.dependencySources`, which still gates the request member.
3. **Contract 21 guards it.** A manifest that declares `buildGraphEdges`
   requires contract 21 (`BuildGraphEdgesContract`). A CLI that predates the
   source strict-validates each probe answer and would reject one that names
   it; the rung makes that CLI refuse the extension when it loads instead.

## Consequences

- A Pants package's edges, from the export or from the putnami.json the sync
  wrote, are attributed and judged by the declared-edge check: a run warns no
  `workspace.declared_edge` for them. Visibility judges imports alone.
- The stack core carries the provenance of a component's edges, so another
  delegated stack attributes its edges the same way.
- `@putnami/pants` is stamped at contract 21 and needs a CLI that reads it.
