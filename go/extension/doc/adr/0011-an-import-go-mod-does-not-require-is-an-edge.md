# ADR 0011 — An import that go.mod does not require is an edge and a warning

- **Status**: accepted
- **Scope**: `@putnami/go` (`go/extension`), the workspace probe

## Context

ADR 0001 derives a Go project's edges from its `go.mod`: `require` lines and
`replace` targets. `go.work` also builds an import of a workspace module that
the importing `go.mod` does not require. A module nested inside a required
module is the common case: requiring `example.com/provider` does not provide
`example.com/provider/client` when `provider/client` has its own `go.mod`.

The probe reported no edge for such an import. A change to the imported module
did not select the importer under `--impacted` and did not change its cache
key, so the workspace build passed on a graph that missed a dependency. No
command reported the import. A module-mode build, `go mod tidy` included,
cannot resolve it.

The import scan already read each module's sources to attribute its edges, but
it stopped once every required module was seen. Finding an unrequired import
means reading every file.

## Decision

**The probe reads every `.go` file of each module that `go mod tidy` reads.
An import of a workspace
module that the `go.mod` does not require becomes an edge, attributed
`go-module`, and an `unrequired-import` warning on that `go.mod`.**

- The edge makes `--impacted`, the cache key and the visibility check follow
  the import that `go.work` builds.
- The warning names the `require` and the local `replace` to add. A `replace`
  without a `require` does not resolve the import in module mode, so it still
  warns.
- The scan follows `go mod tidy`: it skips a file only `//go:build ignore`
  admits, a file with a malformed or repeated `//go:build` line, and a file
  whose name starts with `.` or `_`, so it never asks for a `require` that
  tidy would remove.
- A module is credited only when its own tree provides the imported package:
  the package directory holds a `.go` file the scan reads, and no directory
  between it and the module's `go.mod` holds another `go.mod`. A nested module
  the probe does not know is therefore never mistaken for its parent.
- The release-set metadata keeps reading `require` lines only: a published
  module carries what its `go.mod` states.
- The new edge can close a dependency cycle, for example when an external test
  package imports a module that requires the tested one. The workspace then
  refuses the graph, as it does for the same cycle stated with `require`
  lines, and the refusal carries the `unrequired-import` warning that names
  the edge.
- The probe renders its candidates concurrently. Each candidate reads its own
  sources and the imported modules' directories, and only reads the shared
  indexes. The answer keeps the sorted candidate order.

A `go.mod` added below a workspace module, in a directory that is not a
candidate, changes which module provides a package without changing a watched
input. The recorded answer then keeps the edge until a `.go` file, a
candidate's `go.mod` or `go.work` changes. The nested-module rule of the
import scan has the same limit.

## Cost

Measured on this repository, 89 modules and 3,380 Go files:

- Rendering serially, the probe took 250 to 275 ms before the change and 370
  to 420 ms with the full walk. The machine load of these two runs was not
  recorded.
- Under a load ratio above 7, concurrent rendering took 450 to 1,900 ms, about
  half the time of serial rendering in the same process.

The probe reruns when one of its watched inputs changes (`go.mod`, `.go`
files, `go.work`, `go.work.sum`, `putnami.json`, `putnami.workspace.json`) or
when the set of candidate directories changes.

## Rejected alternatives

- **A warning without an edge.** The graph would stay wrong until someone
  edits `go.mod`, and a cached test of the importer would keep passing against
  a changed dependency.
- **A check that runs on demand.** It cannot add the edge, and a check nobody
  runs reports nothing.
- **Resolve the import with `go list`.** ADR 0001 rejects it: it needs a
  toolchain and maybe a network, and it reports absolute paths.
