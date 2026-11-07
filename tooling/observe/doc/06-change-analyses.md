# How `putnami observe` explains a change

`putnami observe` reads the repository's manifests, the git diff, the test and
coverage reports, the lockfiles and the software bills of materials (SBOM).
It runs no package manager, no orchestrator, no test and no SBOM tool, and
queries no vulnerability database.

## Components

Seventeen detectors read the repository:

| Detector | Reads | Source |
|---|---|---|
| Nx | `graph.json` from `nx graph --file`, else `nx.json` and `project.json` | `orchestrator` for a graph export, `workspace` otherwise |
| moon | `moon-graph.json` from `moon project-graph --json`, else `.moon/workspace.yml`, each `moon.yml` and `package.json` | `orchestrator` for a graph export, `workspace` otherwise |
| Pants | `pants-graph.json` from `pants peek ::`, else the `BUILD` files, beside `pants.toml` | `orchestrator` for the export, `workspace` otherwise |
| Rush | `rush.json`, each project's `package.json` ([Rush](/docs/tooling-&-workspace/observe/rush)) | `orchestrator` |
| Node workspaces | `package.json` `workspaces`, `pnpm-workspace.yaml` | `workspace` |
| Composer | each `composer.json` | `manifest` |
| Go | `go.work`, each `go.mod` | `workspace` for a `go.work` member, `manifest` otherwise |
| Python | each `pyproject.toml`, with the root's `[tool.uv.workspace]` | `workspace` for a uv workspace member, `manifest` otherwise |
| Gradle | `settings.gradle.kts` or `settings.gradle`, and each project's build file | `workspace` |
| Maven | the root `pom.xml` and its modules, recursively | `workspace` |
| Swift Package Manager | each `Package.swift` and its targets, from `swiftpm.json` as `swift package describe --type json` writes it, else the manifest and the directories under `Sources` and `Tests` | `workspace` |
| Xcode | each `.xcodeproj` and the local packages its `project.pbxproj` references | `manifest` |
| Dart | each `pubspec.yaml`, with the root's pub `workspace` and `melos.yaml` | `workspace` for a pub workspace or melos package, `manifest` otherwise |
| .NET | each `.csproj`, `.fsproj` and `.vbproj`, with the root's `.sln` and `.slnx` | `workspace` for a project a root solution lists, `manifest` otherwise; see [the .NET detector](/docs/tooling-&-workspace/observe/dotnet) |
| Ruby | each packwerk `package.yml`, `*.gemspec` and `Gemfile` | `workspace` for a pack, `manifest` otherwise |
| Cargo | each `Cargo.toml`, with the root's `[workspace]` | `workspace` for a Cargo workspace member, `manifest` otherwise |
| Bazel | `bazel-graph.json` from `bazel query 'deps(//...)' --output=streamed_jsonproto`, else the `BUILD` and `BUILD.bazel` files | `orchestrator` for a query export, `manifest` otherwise |

A component is a directory. When two detectors name one directory, the higher
source wins: `orchestrator`, then `workspace`, then `manifest`. On a tie, the
detector listed first in the table wins. An edge to a directory that is not a
component is dropped. When no detector finds a component, the fallback split
below gives them.

A changed file belongs to the innermost component that holds it. A file
outside every component, such as a root lockfile in a Node workspace or
`common/config/rush/pnpm-lock.yaml` in a Rush monorepo, belongs to none. A
component is `changed` when one of its files changed, `dependent` when it
reaches a changed component through its edges, and `unaffected` otherwise.

"Changed" wins over "dependent": a change that edits a component and one that
depends on it marks both changed. The summary, short or detailed, keeps the edge between them. A
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

### A repository no detector recognizes

When no detector finds a component, the repository is split:

1. By CODEOWNERS: the first of `.github/CODEOWNERS`, `CODEOWNERS` and
   `docs/CODEOWNERS` that exists is read, in GitHub's order, and each pattern that names a
   directory of the repository is a component of source `fallback-codeowners`.
   A pattern names a directory when, without its leading `/` and a trailing
   `/`, `/*` or `/**`, it holds no wildcard: `/billing/`, `web/` and
   `/catalog/**` do, `*`, `*.md` and `/apps/*/src/` do not.
2. Else by top-level directories: each one that holds a source file at any
   depth, such as a `.go`, `.py`, `.ts` or `.c` file, is a component of source
   `fallback-directories`. A source file has a programming-language
   extension; configuration, data, documents and images are none. `.github`,
   `docs`, `vendor`, `node_modules` and dot directories are left out, and dot
   and `node_modules` directories are not read below the top level.
3. Else the repository is one `fallback` component `.`.

These components are of ecosystem `unknown`, are named after their directory
and have no edges: a change marks its own components `changed` and every other
one `unaffected`. A file outside every component, such as a root `README.md`,
belongs to none. Tests and coverage are placed by file path, as below. The
`observe.fallback_split` diagnostic says how the split was made:

```
  components  1 changed, 0 dependent, 2 unaffected
    changed   billing [unknown, fallback-codeowners]
```

## Tests and coverage

A JUnit or TRX case goes to the first of:

1. The package of its Bazel test target, for a log under `bazel-testlogs`;
   see [the Bazel detector](/docs/tooling-&-workspace/observe/bazel#tests-and-coverage).
2. The component that holds its test file: the case's `file`, else its
   suite's, else its class name when that names a file (Vitest), else its
   dotted class name read as a Python module (pytest). pytest writes
   `tests.test_api.TestLogin` for the class `TestLogin` of `tests/test_api.py`,
   and its default `xunit2` report has no `file`: the longest leading part
   that names a `.py` file is the test file. The class name is relative to
   pytest's `rootdir`; see
   [the Python detector](/docs/tooling-&-workspace/observe/python#reports). Else its dotted class name
   read as a Dart test file (tojunit): `test.price` is `test/price_test.dart`,
   and the longest tail of a class name written from an absolute path that
   names a file is the test file; see [the Dart detector](/docs/tooling-&-workspace/observe/dart#reports).
   rspec_junit_formatter and minitest-reporters write the `file`; see
   [the Ruby detector](/docs/tooling-&-workspace/observe/ruby#reports).
3. The component whose test source root holds the file its JVM class name
   names: `com.acme.PriceTest` is `src/test/kotlin/com/acme/PriceTest.kt` or
   `.java`, under `src/test` or `src/androidTest`. See
   [Gradle and Maven](/docs/tooling-&-workspace/observe/jvm#reports).
4. The component of the Go package its class or suite names
   (`gotestsum --junitfile`), or of the Swift test target the `Module` of a
   `Module.Class` class or suite name names (xcbeautify, fastlane `scan`,
   `swift test --xunit-output`); see [the Swift detectors](/docs/tooling-&-workspace/observe/swift#reports).
   Or of the .NET project whose test assembly its suite names
   (`Pricing.Tests.dll`, from `dotnet test --logger trx` or
   `JunitXml.TestLogger`).
   A located name that is a file is the case's file.
5. The component of the crate whose package its class name names as a
   cargo-nextest test binary: `pricing`, `pricing::integration` or
   `api::bin/api`; see [the Cargo detector](/docs/tooling-&-workspace/observe/rust#reports). A suite name
   is not read for it, so another runner's suite named like a crate stays
   with its report.
6. The component that holds the report.

A case that none of them places is counted in `observe.tests_unattributed`.

A coverage report names files. A JaCoCo file, a package path, is first read
under the `src/main/java` or `src/main/kotlin` of a component; see
[Gradle and Maven](/docs/tooling-&-workspace/observe/jvm#reports). A path is read in this order: absolute under
the repository; absolute from another checkout, by its trailing segments;
relative to a Cobertura source root; relative to the report's component, to
the repository root, then to the report's directory; and, for a Go cover
profile, as an import path. A file that is none of them is counted in
`observe.coverage_unmapped`. A pytest-cov report (`pytest --cov
--cov-report=xml`) names the directory pytest ran in as its `<source>` and
each file relative to it, so it maps whether pytest ran at the repository root
or in one project's directory. When two reports instrument one line, the line
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
| `pnpm-lock.yaml` | `npm` | the keys of `packages`, versions 5 to 9; `file:` and `link:` packages left out |
| `yarn.lock` | `npm` | classic and Berry entries; `workspace:`, `link:`, `portal:` and `file:` left out |
| `npm-shrinkwrap.json` of a Rush monorepo | `npm` | as `package-lock.json` |
| `composer.lock` | `composer` | `packages` and `packages-dev`; path packages left out |
| `go.mod` | `go` | the requires, after the `go.mod` replace directives; modules of the repository and local replacements left out |
| `uv.lock` | `pypi` | each `[[package]]`; `editable`, `virtual`, `directory` and `path` sources left out |
| `poetry.lock` | `pypi` | each `[[package]]`; `directory` and `file` sources and `develop` packages left out |
| `Cargo.lock` | `cargo` | each `[[package]]` with a `source`; the workspace crates and path dependencies, which have none, left out |
| a Pants resolve lockfile | `pypi` | each locked requirement of the PEX JSON lock a `[python.resolves]` entry of `pants.toml` names |
| `gradle.lockfile`, `settings-gradle.lockfile` | `maven` | each `group:artifact:version` line, as `group:artifact` |
| a Gradle version catalog, `gradle/*.versions.toml` | `maven` | each library as `group:artifact` and each plugin as its id, at its version, `version.ref` or rich version |
| `Package.resolved` | `swift` | each pin of format versions 1 to 3, at its version, else its revision |
| `Podfile.lock` | `cocoapods` | each entry of `PODS`, by its root pod; pods from a `:path:` left out |
| `pubspec.lock` | `pub` | each entry of `packages`; `path` and `sdk` sources left out |
| `packages.lock.json` | `nuget` | each package each target framework resolves; `Project` entries left out |
| `Directory.Packages.props`, without `packages.lock.json` in the repository | `nuget` | each `PackageVersion` with a literal `Include` and `Version` |
| `Gemfile.lock` | `gem` | the specs of the `GEM` and `GIT` sources, the platform after the first `-` dropped; `PATH` sources left out |

A Rush monorepo keeps its lockfile under `common/config/rush/`: `pnpm-lock.yaml`,
`npm-shrinkwrap.json` or `yarn.lock`, by the package manager `rush.json` names,
and in pnpm mode one `pnpm-lock.yaml` per subspace under
`common/config/subspaces/<name>/`. Its `@rush-temp/` packages are Rush's own
projects and are left out. `go.sum` holds checksums, not versions, and is not
read. A Python package
name is read in its normalized form (PEP 503): lower case, with each run of
`-`, `_` and `.` as one `-`, so `Typing_Extensions` is `typing-extensions`. A package found on
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
package URL names (`npm`, `composer`, `golang` is `go`, `pypi`, `maven`, `pub`,
`nuget`, `gem`, `cargo`), its name, normalized for `pypi` and written `group:artifact` for `maven`, and its version, a leading `v` ignored. A package
without a package URL matches by its name as written and its version, so a
Python package without one matches only when the SBOM writes its normalized
name. For each change, the base SBOM gives the `before` state, unless the
package was added, and the head SBOM the `after` state, unless it
was removed: its licences and its vulnerabilities, each an id and a severity,
the most severe first. A package the SBOM does not list gets no state, and
the summary says so. Without a base SBOM, every base state is unknown.
osv-scanner names a vulnerable package without its version: when one SBOM
lists two versions of a package, both get its vulnerabilities. A
state holds at most 20 licences and 200 vulnerabilities.

The short summary counts the licence changes, the vulnerabilities introduced
and fixed, and the changes whose base or head state no SBOM lists:

```text
  dependencies  3 changes, 0 license changes, vulnerabilities 1 introduced and 3 fixed
```

The detailed summary, which `--details` prints, names the SBOMs after the
dependency count, then gives each change one line:

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
unknown` in the detailed summary, and counts as a change with a side no SBOM
lists in the short one. A line names at most 5 vulnerabilities. Without an SBOM, the
summary and the evidence document are what they were.

## Decision records

A team that keeps its decisions as MADR files (Markdown Architectural
Decision Records) sees the decisions that govern the files a change touches.
`putnami observe` reads them; it converts nothing and requires no record.

The records are the `.md` files under `docs/decisions` and `docs/adr`, or
under the directories `--decisions` names, comma-separated and relative to the
repository root.
`README.md`, `index.md` and a file whose name holds `template` are not
records. Each record gives:

| Field | Read from |
|---|---|
| Title | The front matter `title`, else the first `#` heading, else the file name. |
| Status | The front matter `status`, else a `Status:` line (`* Status: accepted`, `- **Status**: Accepted`), else the first line of a `## Status` section. Its first word gives `proposed`, `accepted`, `rejected`, `deprecated` or `superseded`; anything else is `unknown`. |
| Date | The front matter `date`, else a `Date:` line, when it is a `YYYY-MM-DD` date. |
| Scope | The front matter `scope`, a list or one pattern, else the list items of a `## Scope` section. |

A scope pattern is a path from the repository root. A pattern without glob
characters governs the path and everything under it; `*` and `?` match within
one directory and `**` any number of directories:

```markdown
---
status: accepted
date: 2025-03-04
scope:
  - src/payments/
---

# Payments own the ledger
```

```markdown
# One queue per domain

## Status

Superseded by [0002](0002-payments-own-the-ledger.md)

## Scope

- `src/**/queue/*.go`
```

The detailed summary (`--details`) and the evidence document list each decision whose scope matches
a changed file, with the files it governs, and each record the change adds,
modifies, renames or deletes, a record moved out of the decision directories
included; a deleted record is read at the base. A change
that deletes an accepted decision gets the warning `observe.decision_deleted`:

```text
  decisions  3 decisions
    accepted   docs/decisions/0001-charges-are-synchronous.md: Charges are synchronous (deleted by the change)
    accepted   docs/decisions/0002-payments-own-the-ledger.md: Payments own the ledger (governs 1 changed file)
    superseded docs/decisions/0003-one-queue-per-domain.md: One queue per domain (governs 1 changed file)
  warning observe.decision_deleted: the change deletes the accepted decision docs/decisions/0001-charges-are-synchronous.md (Charges are synchronous)
```

A repository without decision records gets no `decisions` line and no
diagnostic. The list says which decisions govern the change; it does not
judge whether the change follows them.

## Diagnostics

| Code | Meaning |
|---|---|
| `observe.report_unread` | A reader refused a report or an SBOM: it is malformed, too large or of an unsupported version. Its side is unknown. |
| `observe.tests_unattributed` | Test cases of a report belong to no component. |
| `observe.coverage_unmapped` | Files of a coverage report are not files of the repository. |
| `observe.lockfile_unreadable` | A changed lockfile cannot be read at the base or the head. |
| `observe.changed_lines_unread` | The diff's changed lines cannot be read. |
| `observe.sbom_ambiguous` | One side has several SBOMs, so none is read and that side is unknown. |
| `observe.bazel_graph_missing` | A Bazel workspace has no query export, so its packages have no edge. An info diagnostic that names the export command. |
| `observe.bazel_graph_unreadable` | The Bazel query export cannot be read, so the `BUILD` files give the packages, without edges. |
| `observe.decision_unread` | A decision record or directory cannot be read, or a `--decisions` entry is not a directory of the repository. |
| `observe.decision_deleted` | The change deletes an accepted decision record. |
| `observe.decisions_truncated` | The change concerns more than 1000 decisions, or a decision names more than 100 scope patterns or governs more than 1000 changed files; the rest is left out. |
| `observe.fallback_split` | No detector found a component, so the repository is split by CODEOWNERS, by its top-level directories, or kept as one component. An info diagnostic that names the split. |
| `observe.fallback_walk_bounded` | A top-level directory holds 200000 entries before any source file, so the directory split leaves it out. |
