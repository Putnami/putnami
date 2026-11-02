# How `putnami observe` explains a change

`putnami observe` reads the repository's manifests, the git diff, the test and
coverage reports, the lockfiles and the software bills of materials (SBOM).
It runs no package manager, no orchestrator, no test and no SBOM tool, and
queries no vulnerability database.

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

## Licences and vulnerabilities

An SBOM lists the packages of a build with their licences and, for
CycloneDX, their known vulnerabilities. The CI writes it, with osv-scanner,
Trivy or Syft; `putnami observe` reads one for the head (`--sbom`, or
discovered) and one for the base (`--base-sbom`):

| Format | Versions | Vulnerabilities |
|---|---|---|
| CycloneDX JSON | 1.4, 1.5, 1.6 | each entry of `vulnerabilities`, with the highest severity of its ratings |
| SPDX JSON | 2.3 | each `SECURITY` `advisory` external reference, severity `unknown` |

osv-scanner writes no vulnerability in SPDX: use CycloneDX for them.

A package of the SBOM matches a dependency change by the ecosystem its
package URL names (`npm`, `composer`, `golang` is `go`), its name and its
version, a leading `v` ignored; a package without a package URL matches by
name and version. For each change, the base SBOM gives the `before` state,
unless the package was added, and the head SBOM the `after` state, unless it
was removed: its licences and its vulnerabilities, each an id and a severity,
the most severe first. A package the SBOM does not list gets no state, and
the summary says so. Without a base SBOM, every base state is unknown.
osv-scanner names a vulnerable package without its version: when one SBOM
lists two versions of a package, both get its vulnerabilities. A
state holds at most 20 licences and 200 vulnerabilities.

The summary names the SBOMs after the dependency count, then gives each
change one line:

```text
  dependencies  3 changes, SBOM head CycloneDX 1.5, base CycloneDX 1.5
    upgraded   lodash 4.17.20 -> 4.17.21, in package-lock.json
               fixes 2 (GHSA-35jh-r3h4-6jhm high, GHSA-29mw-wpgm-hmr9 medium), introduces 0, keeps 3; license MIT
    removed    minimist 1.2.5, in package-lock.json
               fixes 1 (GHSA-xvch-5gv4-984h critical); license MIT
    added      zod 3.23.8, in package-lock.json
               introduces 1 (GHSA-mrhx-c7xc-m3w5 high); license MIT
```

Without a base SBOM, an upgrade reads `3 (…) known at the head, base
unknown`. A line names at most 5 vulnerabilities. Without an SBOM, the
summary and the evidence document are what they were.

## Diagnostics

| Code | Meaning |
|---|---|
| `observe.report_unread` | A reader refused a report or an SBOM: it is malformed, too large or of an unsupported version. Its side is unknown. |
| `observe.tests_unattributed` | Test cases of a report belong to no component. |
| `observe.coverage_unmapped` | Files of a coverage report are not files of the repository. |
| `observe.lockfile_unreadable` | A changed lockfile cannot be read at the base or the head. |
| `observe.changed_lines_unread` | The diff's changed lines cannot be read. |
| `observe.sbom_ambiguous` | One side has several SBOMs, so none is read and that side is unknown. |
