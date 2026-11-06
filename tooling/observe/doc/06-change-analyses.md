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
| Rush | `rush.json`, each project's `package.json` ([Rush](11-rush.md)) | `orchestrator` |
| Node workspaces | `package.json` `workspaces`, `pnpm-workspace.yaml` | `workspace` |
| Composer | each `composer.json` | `manifest` |
| Go | `go.work`, each `go.mod` | `workspace` for a `go.work` member, `manifest` otherwise |
| Python | each `pyproject.toml`, with the root's `[tool.uv.workspace]` | `workspace` for a uv workspace member, `manifest` otherwise |
| Gradle | `settings.gradle.kts` or `settings.gradle`, and each project's build file | `workspace` |
| Maven | the root `pom.xml` and its modules, recursively | `workspace` |
| Swift Package Manager | each `Package.swift` and its targets, from `swiftpm.json` as `swift package describe --type json` writes it, else the manifest and the directories under `Sources` and `Tests` | `workspace` |
| Xcode | each `.xcodeproj` and the local packages its `project.pbxproj` references | `manifest` |
| Dart | each `pubspec.yaml`, with the root's pub `workspace` and `melos.yaml` | `workspace` for a pub workspace or melos package, `manifest` otherwise |
| .NET | each `.csproj`, `.fsproj` and `.vbproj`, with the root's `.sln` and `.slnx` | `workspace` for a project a root solution lists, `manifest` otherwise; see [the .NET detector](15-dotnet.md) |
| Ruby | each packwerk `package.yml`, `*.gemspec` and `Gemfile` | `workspace` for a pack, `manifest` otherwise |
| Cargo | each `Cargo.toml`, with the root's `[workspace]` | `workspace` for a Cargo workspace member, `manifest` otherwise |
| Bazel | `bazel-graph.json` from `bazel query 'deps(//...)' --output=streamed_jsonproto`, else the `BUILD` and `BUILD.bazel` files | `orchestrator` for a query export, `manifest` otherwise |

A component is a directory. When two detectors name one directory, the higher
source wins: `orchestrator`, then `workspace`, then `manifest`. On a tie, the
detector listed first in the table wins. An edge to a directory that is not a
component is dropped. When no detector finds a component, the repository is
one `fallback` component `.` of ecosystem `unknown`.

A changed file belongs to the innermost component that holds it. A file
outside every component, such as a root lockfile in a Node workspace or
`common/config/rush/pnpm-lock.yaml` in a Rush monorepo, belongs to none. A
component is `changed` when one of its files changed, `dependent` when it
reaches a changed component through its edges, and `unaffected` otherwise.

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

A JUnit or TRX case goes to the first of:

1. The package of its Bazel test target, for a log under `bazel-testlogs`;
   see [the Bazel detector](09-bazel.md#tests-and-coverage).
2. The component that holds its test file: the case's `file`, else its
   suite's, else its class name when that names a file (Vitest), else its
   dotted class name read as a Python module (pytest). pytest writes
   `tests.test_api.TestLogin` for the class `TestLogin` of `tests/test_api.py`,
   and its default `xunit2` report has no `file`: the longest leading part
   that names a `.py` file is the test file. The class name is relative to
   pytest's `rootdir`; see
   [the Python detector](07-python.md#reports). Else its dotted class name
   read as a Dart test file (tojunit): `test.price` is `test/price_test.dart`,
   and the longest tail of a class name written from an absolute path that
   names a file is the test file; see [the Dart detector](14-dart.md#reports).
   rspec_junit_formatter and minitest-reporters write the `file`; see
   [the Ruby detector](16-ruby.md#reports).
3. The component whose test source root holds the file its JVM class name
   names: `com.acme.PriceTest` is `src/test/kotlin/com/acme/PriceTest.kt` or
   `.java`, under `src/test` or `src/androidTest`. See
   [Gradle and Maven](12-jvm.md#reports).
4. The component of the Go package its class or suite names
   (`gotestsum --junitfile`), or of the Swift test target the `Module` of a
   `Module.Class` class or suite name names (xcbeautify, fastlane `scan`,
   `swift test --xunit-output`); see [the Swift detectors](13-swift.md#reports).
   Or of the .NET project whose test assembly its suite names
   (`Pricing.Tests.dll`, from `dotnet test --logger trx` or
   `JunitXml.TestLogger`).
   A located name that is a file is the case's file.
5. The component of the crate whose package its class name names as a
   cargo-nextest test binary: `pricing`, `pricing::integration` or
   `api::bin/api`; see [the Cargo detector](17-rust.md#reports). A suite name
   is not read for it, so another runner's suite named like a crate stays
   with its report.
6. The component that holds the report.

A case that none of them places is counted in `observe.tests_unattributed`.

A coverage report names files. A JaCoCo file, a package path, is first read
under the `src/main/java` or `src/main/kotlin` of a component; see
[Gradle and Maven](12-jvm.md#reports). A path is read in this order: absolute under
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
| `observe.bazel_graph_missing` | A Bazel workspace has no query export, so its packages have no edge. An info diagnostic that names the export command. |
| `observe.bazel_graph_unreadable` | The Bazel query export cannot be read, so the `BUILD` files give the packages, without edges. |
