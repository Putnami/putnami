# go.putnami.dev/evidence/readers

Reads the test and coverage reports a CI already wrote into typed facts: JUnit
XML and TRX into test cases, LCOV, Cobertura XML, Clover XML, JaCoCo XML and Go
cover profiles into line-level file coverage, and ArchUnit, deptrac and
import-linter results into architecture rules. The observe extension builds the change
evidence document (`protocols/evidence`) from them, and the TypeScript
extension reads its bun reports through it.

`@putnami/evidence-readers` is an unpublished internal module. It is not in
`putnami.support.json` and is not a supported public API.

## Readers

Every reader takes the report's bytes and returns its result and diagnostics.
`ReadFile(path, reader)` reads a file first.

| Reader | Format | Result |
| --- | --- | --- |
| `ReadJUnit` | JUnit XML, root `testsuites` or `testsuite` | `TestReport`: the root's stated totals, and one `TestCase` per `testcase` element in document order |
| `ReadTRX` | TRX, the report of `dotnet test --logger trx`, root `TestRun` | `TestReport`: the `ResultSummary` counters, and one `TestCase` per top-level `UnitTestResult` in document order |
| `ReadLCOV` | LCOV tracefile | `CoverageReport` |
| `ReadCobertura` | Cobertura XML | `CoverageReport`, with the report's source roots |
| `ReadClover` | Clover XML | `CoverageReport` |
| `ReadJaCoCo` | JaCoCo XML, root `report`, as the JaCoCo Maven plugin, the Gradle `jacoco` plugin and Kover write it | `CoverageReport` |
| `ReadGoCover` | `go test -coverprofile`, mode `set`, `count` or `atomic` | `CoverageReport` |
| `ReadSBOM` | CycloneDX 1.4, 1.5 or 1.6 JSON (`bomFormat: CycloneDX`), SPDX 2.3 JSON (`spdxVersion: SPDX-2.3`) | `SBOM`: the format, its spec version and one `Package` per component or package |

A `TestCase` keeps its name, classname, enclosing suites (outermost first),
file and line, status (`passed`, `failed`, `skipped`), duration in
milliseconds, and its failure or skip. Its file is the case's `file`
attribute, else the one of its nearest enclosing suite. A `failure` or an
`error` element fails the case.

A TRX case keeps the result's `testName` as its name, the `className` of the
`TestMethod` its `testId` defines, cut at its first comma, as its classname,
and one suite named by the file name of that method's `codeBase`, the test
assembly, as `JunitXml.TestLogger` names it. A TRX report states no test file.
`Passed`, `PassedButRunAborted`, `Completed` and `Warning` pass; `Failed`
fails, and `Error`, `Timeout` and `Aborted` fail as an error, with the
`ErrorInfo` message and stack trace; `NotExecuted`, `Inconclusive`,
`NotRunnable`, `Pending`, `Disconnected` and `InProgress` skip, with the
`ErrorInfo` message. The inner results of a data-driven test are not cases:
their parent stands for them. The errors total counts the `error`, `timeout`
and `aborted` counters, and the skipped total is `total` less `executed`: the
VSTest logger leaves `notExecuted` at 0 for a skipped test.

A `FileCoverage` holds the file's instrumented lines, in increasing order,
with their hit counts, and its line and function totals counted from them. A
line stated twice keeps its largest count. A file whose report lists no line
or no function keeps the totals the report states: LCOV's `LF`, `LH`, `FNF` and
`FNH`, a Clover file's `methods` and `coveredmethods` metrics. A Go block marks
every line from its start line to its end line; a block stated twice sums its
counts in `count` and `atomic` mode.

JaCoCo records which instructions of a line ran, not how often: a line with a
covered instruction (`ci`) has one hit, a line with missed instructions only
(`mi`) none, and a line with neither is no line. A source file's `LINE` and
`METHOD` counters are its stated totals. The packages of every `group`, as
`report-aggregate` writes them per module, are read.

## Software bills of materials

A software bill of materials (SBOM) lists the packages a build uses.
`ReadSBOM` reads the one a CI tool wrote, such as osv-scanner, Trivy or Syft,
and queries no vulnerability database. A `Package` keeps its name, version,
package URL (`purl`), licences and known vulnerabilities, each an id and a
severity: `critical`, `high`, `medium`, `low`, `info`, `none` or `unknown`.

| Format | Packages | Licences | Vulnerabilities |
| --- | --- | --- | --- |
| CycloneDX | every component, nested ones included; a `group` prefixes the name | each licence `id`, else `name`, and each `expression` | each entry of `vulnerabilities`, on every component an `affects` ref names: by `bom-ref`, by the `#` part of a BOM-Link, or by package URL with or without its version. The severity is the highest of its `ratings`. |
| SPDX | every package the document does not describe as itself | `licenseConcluded`, else `licenseDeclared`, unless `NOASSERTION` or `NONE` | each `SECURITY` `advisory` external reference, named by the last segment of its locator, severity `unknown` |

A CycloneDX `affects` entry that states versions narrows the vulnerability
to them: a version stated `unaffected` leaves, and exact affected versions,
without an affected range, keep only those. osv-scanner names a package
without its version and states none, so when the SBOM lists two versions of
one package, both get its vulnerabilities.

Most SPDX writers, osv-scanner included, write no vulnerability: use
CycloneDX for them. `ParsePURL` reads a package URL into its type, its name
(namespace and name, decoded) and its version.

## Architecture rules

An `ArchitectureRule` is one rule an architecture checker evaluated: its
name as the checker gives it, its status (`kept` or `broken`), the number of
violations the checker counted, and the violations its report details. A
`Violation` keeps its message, the class or module that violates the rule
(`Subject`), and the file and line the checker names. Mapping a class or a
module to a repository file is the caller's.

| Reader | Format | Rules |
| --- | --- | --- |
| `ArchUnitRule` | One case of a JUnit report whose test runs ArchUnit rules; `UsesArchUnit` reports whether a test source imports `com.tngtech.archunit` | A passing case is a kept rule named by the case; a skipped case, or one that ended in an error, none. A failing case is a broken rule named by its `Rule '...'` clause, counting the `N` of `was violated (N times)`. Each later line of the message, else of the failure body when it lists more, is a violation, an indented line continuing the one above (the slices and dependencies of a cycle): its subject is the first `<...>` unit cut to its top-level class, its file and line those of `in (File.java:N)`. A failure that names no rule is one violation. |
| `ReadDeptrac` | The report of `deptrac --formatter=junit` | One rule per layer suite, named by the layer, broken when a case fails. Each `failure` is a violation: `Depender:N must not depend on Dependent (A on B)` gives the depending class and the line. The `Unmatched skipped violations` suite is no rule. |
| `ReadImportLinter` | The text `lint-imports` prints, such as `lint-imports > import-linter.txt`, with or without colour codes | One rule per `Name KEPT` or `Name BROKEN` line. Under `Broken contracts`, each import chain of a contract (`- importer -> imported (l.N)` and its indented lines) is a violation of its first importer at its first line; a broken contract that lists no chain counts one. |

## Paths

A reader reports each path as the report states it, in slash form, and
resolves none: a Clover file's `path` attribute wins over its `name`, a
Cobertura file is relative to one of the report's `Sources`, a JaCoCo file is
its package path and source file name (`com/acme/pricing/Price.kt`, the
source file name alone in the default package), and a Go profile
names import paths (`example.com/calc/calc.go`). Mapping a path to the
repository, and a case or a file to a component, is the caller's.

## Diagnostics

A reader never panics and never returns half a report. A report it cannot
read returns an empty result and one error diagnostic:

| Code | Meaning |
| --- | --- |
| `readers.malformed` | The report does not decode, its root is not its format's, or it states an impossible value: a line below 1, a negative count, an unknown TRX outcome, an LCOV record without `end_of_record`, JUnit suites or JaCoCo groups nested deeper than `MaxSuiteDepth` (100). |
| `readers.too_large` | The report is larger than `MaxReportBytes` (256 MiB), or its files mark more than `MaxLineMarks` (4,000,000) lines; a Go block marks every line of its span. |
| `readers.missing` | `ReadFile`: the file does not exist, or the path is empty. |
| `readers.unreadable` | `ReadFile`: the file exists but cannot be read. |

`ReadFile` sets the diagnostic's field to the report's path.

## Fixtures

`testdata/` holds real reports from Jest, Vitest, node:test, c8, PHPUnit,
gotestsum, `go test`, JaCoCo under Maven and Gradle, and `dotnet test` with xUnit, NUnit and MSTest, each with the golden output of its reader, and the
malformed reports with the code each returns. `testdata/SOURCES.md` names
the tool versions and commands.
