# go.putnami.dev/evidence/readers

Reads the test and coverage reports a CI already wrote into typed facts: JUnit
XML into test cases, and LCOV, Cobertura XML, Clover XML and Go cover profiles
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
| `ReadLCOV` | LCOV tracefile | `CoverageReport` |
| `ReadCobertura` | Cobertura XML | `CoverageReport`, with the report's source roots |
| `ReadClover` | Clover XML | `CoverageReport` |
| `ReadGoCover` | `go test -coverprofile`, mode `set`, `count` or `atomic` | `CoverageReport` |

A `TestCase` keeps its name, classname, enclosing suites (outermost first),
file and line, status (`passed`, `failed`, `skipped`), duration in
milliseconds, and its failure or skip. Its file is the case's `file`
attribute, else the one of its nearest enclosing suite. A `failure` or an
`error` element fails the case.

A `FileCoverage` holds the file's instrumented lines, in increasing order,
with their hit counts, and its line and function totals counted from them. A
line stated twice keeps its largest count. A file whose report lists no line
or no function keeps the totals the report states: LCOV's `LF`, `LH`, `FNF` and
`FNH`, a Clover file's `methods` and `coveredmethods` metrics. A Go block marks
every line from its start line to its end line; a block stated twice sums its
counts in `count` and `atomic` mode.

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
| `readers.malformed` | The report does not decode, its root is not its format's, or it states an impossible value: a line below 1, a negative count, an LCOV record without `end_of_record`, JUnit suites nested deeper than `MaxSuiteDepth` (100). |
| `readers.too_large` | The report is larger than `MaxReportBytes` (256 MiB), or its files mark more than `MaxLineMarks` (4,000,000) lines; a Go block marks every line of its span. |
| `readers.missing` | `ReadFile`: the file does not exist, or the path is empty. |
| `readers.unreadable` | `ReadFile`: the file exists but cannot be read. |

`ReadFile` sets the diagnostic's field to the report's path.

## Fixtures

`testdata/` holds real reports from Jest, Vitest, node:test, c8, PHPUnit,
gotestsum and `go test`, each with the golden output of its reader, and the
malformed reports with the code each returns. `testdata/SOURCES.md` names
the tool versions and commands.
