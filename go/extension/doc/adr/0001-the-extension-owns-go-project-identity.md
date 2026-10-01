# ADR 0001 — The extension owns Go project identity and the workspace replace closure

- **Status**: accepted
- **Scope**: `@putnami/go` (`go/extension`)

## Context

An orchestrator that models every language's project graph drifts from the
toolchain's graph, and the drift surfaces as a project that vanishes from a
selection or an edge the cache never invalidates on.

Module-mode `go` commands ignore `go.work`. A member module that transitively
requires another workspace module needs its own relative `replace` directive,
or the build resolves the requirement over the network: it fails offline, or
it builds a published version instead of the source in the tree.

Both answers are cache-key inputs. A probe answer that varies with the host,
the request order or the asking reason gives one tree two workspace
snapshots.

## Decision

The extension declares a `workspace` adapter and answers both halves.

**Probe.** `__putnami workspace-probe` reads each candidate's `go.mod`. It
reports the module path as the project's source identity, and its dependency
edges as repo-relative paths resolved from `require` lines (direct and
indirect) and from `replace` targets in module-path and local `./`/`../`
form. An external module contributes no edge; a project never depends on
itself. The answer comes from the tree alone: no `go` invocation, no clock, no
absolute path, no host fact.

**Sync.** `workspace-sync` maintains the workspace replace closure. It appends
missing directives, sorted, after existing content, and never rewrites
existing bytes. It then runs `go mod tidy` on changed modules, best effort. It
runs once per workspace and fails closed without a resolved project
selection. It warns when a selected project has a `go.mod` that `go.work`
does not list, because the closure is defined over `go.work` members.

The adapter's `inputs` are `go.mod`, `**/*.go` and `go.work`. They exclude
`go.sum` and `go.work.sum`, which build and tidy rewrite constantly.

## Invariants

- The probe answer is identical across runs, request orders, asking reasons
  and hosts.
- The probe never reports an edge to an external module or to the project
  itself.
- Sync only appends; a second run over its own output changes nothing.
- A dry run writes nothing.
- Sync never aligns a `module` line to a project name: `@putnami/go` and
  `go.putnami.dev/go/extension` are different identifiers.

## Rejected alternatives

- **Keep project discovery in the orchestrator.** Every language addition
  edits core, and core's graph can disagree with the toolchain's unnoticed.
- **Derive the graph with `go list`.** It needs a toolchain, maybe a network,
  and reports absolute paths; none of that can key a cache entry.
- **Rewrite `go.mod` into a canonical form.** Unreviewable diffs, and it
  discards directives a human added on purpose.
- **Rely on `go.work` alone.** Module-mode commands ignore it.
- **Include `go.sum` in the inputs.** Every tidy would re-probe without
  changing a reported fact.

## Consequences

- A new probe fact must be derivable from the tree without running a tool.
- While the workspace builds its own extensions from source, the closure is
  maintained in two places: `syncTask` runs on a prepared runtime, and
  preparing that runtime needs the closure. The duplication is safe only
  because the codemod is append-only and convergent, and a shared corpus pins
  both implementations byte for byte. Removing either requires proving the
  bootstrap no longer needs it.
