# How `putnami observe` reads a Go repository

The Go detector finds the modules of a repository from its `go.work` and
`go.mod` files. It reads files only: it runs no `go` command and reaches no
module cache or network. The change analyses use what it finds to name the
changed and dependent modules, attribute tests and coverage, and diff the
dependency versions. Until those analyses land, the summary and the evidence
document do not show Go modules yet.

## Components

Each Go module is a component:

- A directory that the root `go.work` lists in `use` is a component of source
  `workspace`.
- Any other `go.mod` is a component of source `manifest`. The search skips
  `.git`, `node_modules`, `vendor`, `testdata` and the directories whose name
  starts with `.` or `_`, as the `go` command does.

A component's id is its directory relative to the repository root, `.` for
the root. Its name is the module path and its ecosystem is `go`. A file
belongs to the innermost module whose directory holds it, so a file outside
every module belongs to no component.

## Dependency edges

A module depends on another module of the repository when:

- its `go.mod` requires that module's path, after the `replace` directives of
  its `go.mod` and of the root `go.work`; or
- a local `replace` directive of its `go.mod`, such as
  `replace example.com/pricing => ../pricing`, points to that module's
  directory.

A require of a module outside the repository is no edge.

## Reports

Go reports name packages and files by import path:

| Report | Field |
|---|---|
| `go test -json` | `Package` of each event |
| `gotestsum --junitfile`, `go-junit-report` | the `name` of each `testsuite` |
| `go test -coverprofile` | the file name at the start of each line |

The detector maps an import path to the repository path below the module
whose path is its longest prefix, and to that module. For example, in
[Putnami/demo-observe-go](https://github.com/Putnami/demo-observe-go),
`github.com/Putnami/demo-observe-go/pricing/format.go` maps to
`pricing/format.go` in the `pricing` module.

## Dependency versions

Each module's `go.mod` is its lockfile. Its locked dependencies are its
requires, each with the version its `replace` directives select and whether
it is `// indirect`. A require replaced by a local directory, or of a module
of the repository, is not a locked dependency. `go.sum` holds checksums, not
a selection, so the detector does not read it. Versions compare in Go's
semantic version order, pseudo-versions included.

## Diagnostics

| Code | Meaning |
|---|---|
| `observe.go_manifest_unreadable` | A `go.work` or `go.mod` cannot be read. A module without a readable `go.mod` is not a component. |
| `observe.go_manifest_invalid` | A `go.work` or `go.mod` cannot be parsed. A `go.mod` that only the lenient grammar reads keeps its module and requires, without its `replace` directives; one that no grammar reads is a component without a name or edges. |
| `observe.go_duplicate_module` | Two `go.mod` files declare one module path. The first directory, in path order, keeps the name. |
| `observe.go_walk_bounded` | The search for `go.mod` stopped after 200,000 entries. |
