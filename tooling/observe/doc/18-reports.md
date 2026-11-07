# Reports

`putnami observe` reads test reports, coverage reports, software bills of
materials (SBOM) and architecture results with one set of readers, `@putnami/evidence-readers`. The
TypeScript extension reads bun's test and coverage reports with the same
readers. This page says what each reader keeps from a report and when it
rejects one.

## Test reports

A test report becomes a list of test cases. Each case keeps its name, its
class name, its enclosing suites, its file and line when the report states
them, its status (`passed`, `failed` or `skipped`), its duration and its
failure or skip message.

| Format | Recognized by | What it gives |
|---|---|---|
| JUnit XML | A root `testsuites` or `testsuite` element | One case per `testcase` element. A `failure` or an `error` element fails the case. A case without a `file` takes the one of its nearest suite. |
| TRX | A root `TestRun` element, as `dotnet test --logger trx` writes it | One case per top-level `UnitTestResult`. Its class name comes from the test method, and its suite is the test assembly. A TRX report names no test file. |

TRX outcomes map to the three statuses:

| TRX outcome | Status |
|---|---|
| `Passed`, `PassedButRunAborted`, `Completed`, `Warning` | `passed` |
| `Failed`, `Error`, `Timeout`, `Aborted` | `failed`, with the error message and stack trace |
| `NotExecuted`, `Inconclusive`, `NotRunnable`, `Pending`, `Disconnected`, `InProgress` | `skipped` |

The inner results of a data-driven test are not cases: their parent stands
for them.

## Coverage reports

A coverage report becomes, for each file, its instrumented lines with their
hit counts, and its line and function totals.

| Format | What it gives |
|---|---|
| LCOV | Each `DA` line and its count. A file that lists no line keeps its `LF` and `LH` totals. |
| Cobertura XML | Each line and its hits. Paths are relative to one of the report's `sources`. |
| Clover XML | Each line and its count. A file's `path` wins over its `name`. |
| JaCoCo XML, Kover included | Whether each line ran, not how often: a line with a covered instruction counts one hit. A file is its package path and source file name, such as `com/acme/pricing/Price.kt`. The modules of a `report-aggregate` report are all read. |
| Go cover profile | Each block marks every line from its start to its end. Paths are import paths, such as `example.com/calc/calc.go`. |

A line stated twice keeps its largest count. `putnami observe` then maps each
path to a file of the repository, and each file to its component: see
[How `putnami observe` explains a change](/docs/tooling-&-workspace/observe/change-analyses#tests-and-coverage).

## Software bills of materials

An SBOM becomes a list of packages, each with its name, version, package URL,
licences and known vulnerabilities. A vulnerability has an id and a severity:
`critical`, `high`, `medium`, `low`, `info`, `none` or `unknown`.

| Format | Licences | Vulnerabilities |
|---|---|---|
| CycloneDX 1.4, 1.5 or 1.6, JSON | Each licence id, else its name, and each expression | Each entry of `vulnerabilities`, on every component it affects. The severity is the highest of its ratings. A version stated `unaffected` is left out. |
| SPDX 2.3, JSON | The concluded licence, else the declared one | Each `SECURITY` `advisory` reference, with severity `unknown` |

osv-scanner names a vulnerable package without its version: when the SBOM
lists two versions of one package, both get its vulnerabilities. Most SPDX
writers, osv-scanner included, record no vulnerability: use CycloneDX for
them.

## Rejected reports

A reader never returns half a report. It rejects a report whole, and
`putnami observe` reports it as `observe.report_unread`, when:

- the report does not decode, or its root is not its format's;
- it states an impossible value, such as a line below 1, a negative count, an
  unknown TRX outcome or an LCOV record without `end_of_record`;
- its suites or groups nest deeper than 100 levels;
- it is larger than 256 MiB, or marks more than 4,000,000 lines;
- it is an SBOM of another format or version.

The other reports of the job are still read.

## Architecture results

The ArchUnit cases of a JUnit report, the deptrac JUnit report and the text of
`lint-imports` become architecture rules, each kept or broken with its
violations. See [Architecture rules](/docs/tooling-&-workspace/observe/architecture).
