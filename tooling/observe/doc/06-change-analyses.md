# How `putnami observe` explains a change

`putnami observe` reads the repository's manifests, the git diff, the test and
coverage reports and the lockfiles. It runs no package manager, no
orchestrator and no test.

## Components

Four detectors read the repository:

| Detector | Reads | Source |
|---|---|---|
| Nx | `graph.json` from `nx graph --file`, else `nx.json` and `project.json` | `orchestrator` for a graph export, `workspace` otherwise |
| Node workspaces | `package.json` `workspaces`, `pnpm-workspace.yaml` | `workspace` |
| Composer | each `composer.json` | `manifest` |
| Go | `go.work`, each `go.mod` | `workspace` for a `go.work` member, `manifest` otherwise |

A component is a directory. When two detectors name one directory, the higher
source wins: `orchestrator`, then `workspace`, then `manifest`. On a tie, the
detector listed first in the table wins. An edge to a directory that is not a
component is dropped. When no detector finds a component, the repository is
one `fallback` component `.` of ecosystem `unknown`.

A changed file belongs to the innermost component that holds it. A file
outside every component, such as a root lockfile in a Node workspace, belongs
to none. A component is `changed` when one of its files changed, `dependent`
when it reaches a changed component through its edges, and `unaffected`
otherwise.

"Changed" wins over "dependent": a change that edits a component and one that
depends on it marks both changed. The summary keeps the edge between them. A
dependent component's line names the components it depends on; a changed
component's line names the other changed components it reaches through its
edges, directly or through other components. A line names at most 10
components, then how many more:

```
  components  2 changed, 0 dependent, 0 unaffected
    changed   api (example.com/shop/api) [go, workspace], depends on pricing
    changed   pricing (example.com/shop/pricing) [go, workspace]
```

The evidence document writes the direct edges of every component, whatever
its impact.

## Tests and coverage

A JUnit case goes to the first of:

1. The component that holds its test file: the case's `file`, else its
   suite's, else its class name when that names a file (Vitest).
2. The component of the Go package its class or suite names
   (`gotestsum --junitfile`).
3. The component that holds the report.

A case that none of them places is counted in `observe.tests_unattributed`.

A coverage report names files. A path is read in this order: absolute under
the repository; absolute from another checkout, by its trailing segments;
relative to a Cobertura source root; relative to the report's component, to
the repository root, then to the report's directory; and, for a Go cover
profile, as an import path. A file that is none of them is counted in
`observe.coverage_unmapped`. When two reports instrument one line, the line
keeps its largest count. A component's coverage is the sum of its files'
covered and instrumented lines.

## Coverage of changed lines

The changed lines are the lines the diff from the merge base to the head adds
or modifies, read with no context lines. For each changed file that still
exists and that a coverage report instruments, `putnami observe` counts its
instrumented changed lines, the covered ones, and lists the uncovered line
numbers. Without a base or a coverage report, there is no changed-line
coverage.

## Dependency changes

Each changed lockfile is read at the merge base and at the head:

| Lockfile | Ecosystem | Packages |
|---|---|---|
| `package-lock.json` | `npm` | `packages`, else the nested `dependencies` of version 1; links left out |
| `pnpm-lock.yaml` | `npm` | the keys of `packages`, versions 5 to 9 |
| `yarn.lock` | `npm` | classic and Berry entries; `workspace:`, `link:`, `portal:` and `file:` left out |
| `composer.lock` | `composer` | `packages` and `packages-dev`; path packages left out |
| `go.mod` | `go` | the requires, after the `go.mod` replace directives; modules of the repository and local replacements left out |

`go.sum` holds checksums, not versions, and is not read. A package found on
one side only is `added` or `removed`. A package whose versions differ is
`upgraded` or `downgraded`, or `changed` when the versions do not compare or
the package has several versions. A lockfile that cannot be read gets
`observe.lockfile_unreadable`. The dependency change names the component that
holds the lockfile, when one does.

## Diagnostics

| Code | Meaning |
|---|---|
| `observe.report_unread` | A reader refused a report: it is malformed or too large. |
| `observe.tests_unattributed` | Test cases of a report belong to no component. |
| `observe.coverage_unmapped` | Files of a coverage report are not files of the repository. |
| `observe.lockfile_unreadable` | A changed lockfile cannot be read at the base or the head. |
| `observe.changed_lines_unread` | The diff's changed lines cannot be read. |
