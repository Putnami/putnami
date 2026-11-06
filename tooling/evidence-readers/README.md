# go.putnami.dev/evidence/readers

Reads the test and coverage reports a CI already wrote into typed facts: JUnit
XML and TRX into test cases, and LCOV, Cobertura XML, Clover XML and Go cover profiles
into line-level file coverage. The observe extension builds the change
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

## Paths

A reader reports each path as the report states it, in slash form, and
resolves none: a Clover file's `path` attribute wins over its `name`, a
Cobertura file is relative to one of the report's `Sources`, and a Go profile
names import paths (`example.com/calc/calc.go`). Mapping a path to the
repository, and a case or a file to a component, is the caller's.

## Diagnostics

A reader never panics and never returns half a report. A report it cannot
read returns an empty result and one error diagnostic:

| Code | Meaning |
| --- | --- |
| `readers.malformed` | The report does not decode, its root is not its format's, or it states an impossible value: a line below 1, a negative count, an unknown TRX outcome, an LCOV record without `end_of_record`, JUnit suites nested deeper than `MaxSuiteDepth` (100). |
| `readers.too_large` | The report is larger than `MaxReportBytes` (256 MiB), or its files mark more than `MaxLineMarks` (4,000,000) lines; a Go block marks every line of its span. |
| `readers.missing` | `ReadFile`: the file does not exist, or the path is empty. |
| `readers.unreadable` | `ReadFile`: the file exists but cannot be read. |

`ReadFile` sets the diagnostic's field to the report's path.

## Fixtures

`testdata/` holds real reports from Jest, Vitest, node:test, c8, PHPUnit,
gotestsum, `go test` and `dotnet test` with xUnit, NUnit and MSTest, each with the golden output of its reader, and the
malformed reports with the code each returns. `testdata/SOURCES.md` names
the tool versions and commands.
