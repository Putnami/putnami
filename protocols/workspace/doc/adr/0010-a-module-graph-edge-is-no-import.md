# ADR 0010: A module-graph edge is no import

- **Status**: accepted
- **Date**: 2026-11-27
- **Scope**: `go.putnami.dev/protocol/workspace` (`protocols/workspace`): the
  probe's `dependencySources`; `go.putnami.dev/protocol/extension`: the
  workspace adapter's `moduleGraphEdges` and contract 31;
  `go.putnami.dev/protocol/job`: `workspaceProjects[].moduleGraphDependencies`

## Context

A Go module built outside go.work reads only its own `replace` directives: the
replaces of the modules it depends on do not apply. So a module whose
workspace dependency itself requires another workspace module needs a local
replace for that transitive module too, and `go mod tidy` writes an
`// indirect` require for it. `putnami projects sync` adds those replaces.

The Go probe reports every require and replace as an edge, and attributes an
indirect require or a replace alone to `go-module`, the source of an import,
so the declared-edge check does not call an edge tidy restores a phantom. The
architecture detector counts every direct edge of the graph. A workspace that
makes its modules tidy therefore gains a cross-domain project dependency for
each transitive module, though no line of its code reads it: in one
workspace, 53 new `architecture.undeclared_project_dependency` findings.

The tidy step cannot stop needing those directives: they are what Go's module
mode requires, and the indirect require would still be an edge.

## Decision

1. **A new provider source, `module-graph`.** It names an edge a module graph
   needs and no source of the project imports: for Go, an `// indirect`
   require or a replace alone. Like an import, it backs the edge in the
   declared-edge check: the build resolves through it and tidy restores it. It
   is no import, so the visibility check does not judge it. The edge set does
   not change: impact, ordering and cache keys still follow the edge.
2. **The adapter declares it.** `workspace.moduleGraphEdges` states that the
   adapter's probe answers `module-graph`. It requires
   `workspace.dependencySources`. A manifest that declares it requires
   contract 31 (`ModuleGraphEdgesContract`): a CLI that predates the source
   would reject each probe answer that names it, and the rung makes that CLI
   refuse the extension when it loads instead.
3. **The job context names those edges.** Each `workspaceProjects` entry
   carries `moduleGraphDependencies`, the ids among its `dependencies` whose
   source is `module-graph` and that nothing else backs: no import, no
   `putnami.json` entry, no contract edge and no declared file input that
   reads the target. Sorted. It is an optional version 2 member
   (`protocols/job` ADR 0001): an entry without it names none.
4. **The architecture detector skips them.** An edge only the consumer's
   module graph needs is no project dependency. The architecture protocol's
   documents do not change, so its version does not either.

## Rejected alternative

- **The tidy step resolves transitive workspace modules without a replace.**
  Go's module mode reads the main module's replaces alone, so a go.mod that
  drops them does not build outside go.work, and the indirect require tidy
  writes would remain an edge the detector counts.

## Consequences

- `@putnami/go` declares `moduleGraphEdges` and is stamped at contract 31.
- A workspace can run `putnami projects sync` and `go mod tidy` on every Go
  module without a new architecture finding.
