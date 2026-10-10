# Test

**Command:** `putnami test [project]`

Runs Go tests with `go test -json`, parses results into structured metrics, and optionally generates a coverage profile and HTML report.

## Overview

- Runs `go test ./...` across all packages
- Coverage is instrumented by default over the packages the module owns, listed with `go list ./...` and passed as `-coverpkg`; a module nested under the project directory is not one of them. The configured `coverage-threshold` is enforced; opt out of failing with `--no-enforce-coverage`, or out of measuring with `--coverage=false`
- Parses `go test -json` output into pass/fail/skip counts
- Parses `coverage.out` for per-file statement coverage
- Emits metrics for test counts and coverage percentage
- Warns on files with zero coverage

**Activation:** Any project containing `go.mod` or `*_test.go` files.

## Execution Flow

1. **Discover**: check for `go.mod` to identify the project as a Go module; skip if not found
2. **Run**: execute `go test ./... -json [flags]`, capturing output
3. **Parse**:
   - Parse JSON test events (one object per line) → pass/fail/skip counts
   - Parse `coverage.out` → per-file statement coverage
   - Emit metrics and diagnostics
   - Generate HTML report if `--coverhtml` is set

### Workspace batching

Go projects that are ready at the same moment and have the same effective test
flags, toolchain, workspace, and synthesized test environment may share one
multi-package `go test -json` invocation. The scheduler groups only projects
that are already ready; it never waits for a batch to fill. Package events are
attributed back to the owning module, so every project still gets its own
status, diagnostics, metrics, cache entry, and terminal row.

The extension derives the subprocess environment through `WorkspaceBuildEnv`.
That shared policy maps the CLI's managed Go cache, pins
`GOTOOLCHAIN=local`, applies the Putnami proxy/checksum defaults, and removes an
inherited `GOWORK=off` before invoking packages governed by a `go.work` file.
Coverage writes a temporary profile, then a module-filtered `coverage.out` (and
optional HTML) under each project's output directory before applying that
project's coverage threshold. Whether one profile covers the whole group or
each project gets its own `go test` process is the
[coverage scope](#coverage-scope-in-a-batch).

Projects remain separate when their effective flags or
`DATABASE_TEST_BINDINGS` differ. Options that cannot be attributed safely—such
as disabled JSON output, fail-fast execution, or absolute shared profile/output
paths—also keep singleton execution. Go's default `go test` vet analysis remains
enabled; this extension does not define a separate vet task.

#### Batch size

By default a batch has no size limit: it takes every compatible project that is
ready. A batch finishes when its slowest project finishes, and until then
`go test` keeps each package's linked test binary and build files in its
scratch directory. A large batch can therefore hold a lot of disk, and the
dependants of its fast projects wait for its slowest one.

Set `batch-max-projects` to cap how many projects share one `go test`
invocation:

```jsonc
// putnami.workspace.json
{
  "options": {
    "@putnami/go:test": { "batch-max-projects": 6 }
  }
}
```

- The value must be a positive integer written as a JSON number. Any other
  value, such as `0`, `-1`, `2.5` or `"6"`, fails the run before any task
  starts.
- `1` runs every project in its own `go test` invocation.
- When the option is unset, batches stay unlimited and group exactly as they
  did before the option existed.
- A project can set its own value in its `putnami.json` options. It then
  batches only with projects that resolve the same value.
- When a cap applies, the scheduler pairs the longest-running ready project
  with the shortest ones, so two slow projects do not share one batch.

The option only decides how projects are grouped. It is not a `putnami test`
flag, and it never enters a test task's cache key, so changing it keeps every
cached test result.

A smaller cap has three costs:

- More `go test` invocations, each paying a few seconds of process start and
  package graph load.
- Two batches that compile the same dependency at the same instant both compile
  it. The Go build cache is shared, so only exact overlaps pay.
- The test PostgreSQL connection budget is split over more processes.

### Structured outcome and transcript

The default output is summary-first. Go reports one deterministic recap such as
`20/22 passed, 1 failed, 1 skipped, 85.2% coverage`; the same counts live in
`result.data.testSummary` for machine clients. Every non-empty `go test` output
line is still emitted as a `debug` log event, including successful test detail.
A runtime-event session recorder can therefore retain the complete transcript
without copying it into the result or the diagnostics. `--test-verbose` promotes
those lines to `info` and passes Go's `-v` switch. It changes visibility and
available verbose detail, never the structured counts, coverage verdict, job
status, or exit code. Normal machine output suppresses the debug transcript and
reports those omitted records and bytes; the complete sanitized transcript is
retained in the session's `events.jsonl` artifact.

Failure diagnostics are a bounded causal view of that transcript. At most 16
are emitted, including an accounting diagnostic when more exist, and each
message is at most 1,024 bytes of valid UTF-8. The omitted count is recorded as
`testSummary.failureDetailsTruncated`; the complete transcript remains in the
debug event stream and the session artifact. Transcript logs are deliberately
absent from action-cache entries: a warm cache hit replays the structured
summary and verdict, but not the transcript, so `--test-verbose` exposes full
detail only when the test task executes.

## Usage

### Basic

```bash
putnami test .
```

### Race detector

```bash
putnami test . --race
```

### Filter tests

```bash
# Run only tests matching a pattern
putnami test . --run TestMyFunction

# Run only short tests
putnami test . --short
```

### Package and test concurrency

`--package-parallel N` maps to Go's `-p N` and limits how many packages are
built and tested concurrently. It is independent from `--parallel N`, which
maps to `-parallel N` and limits `t.Parallel` tests inside each test binary.
`package-parallel` has no default, so projects that omit it retain Go's native
package scheduling.

```bash
# Test at most four packages at once, with up to eight parallel tests per binary
putnami test . --package-parallel 4 --parallel 8
```

### Coverage output

```bash
# Write coverage profile and HTML report to a directory
putnami test . --outputdir coverage

# Custom coverage file name
putnami test . --coverprofile coverage.txt

# Atomic coverage mode (required for race detector + coverage)
putnami test . --race --covermode atomic
```

### Benchmark tests

```bash
# Run all benchmarks
putnami test . --bench .

# Run benchmarks for 5 seconds
putnami test . --bench BenchmarkMyFunc --benchtime 5s

# Run benchmarks N times
putnami test . --bench . --benchtime 100x
```

### Flaky test detection

```bash
# Run each test 5 times in randomised order
putnami test . --count 5 --shuffle
```

### Two coverage cadences

Coverage has two independent switches. `coverage` (default `true`) decides
whether the run **measures**; `enforce-coverage` (default `true`) decides
whether a configured `coverage-threshold` may **fail** it.

Both default on so a coverage drop fails the run that caused it instead of
surfacing later in CI. That costs CPU: instrumentation forces `-count 1`, which
bypasses Go's test result cache.

```bash
# Default: instrument, report, and enforce the threshold
putnami test .

# Keep measuring and reporting, but never fail on the threshold
putnami test . --no-enforce-coverage

# Skip instrumentation entirely — the fast inner loop
putnami test . --coverage=false
```

A project opts out of coverage entirely with `coverage: false` in its config.
That opt-out is authoritative: `enforce-coverage` never overrides it, so an
opted-out project is neither measured nor gated.

### Enforce a minimum coverage threshold

Fail the test job when total statement coverage drops below a percentage. No
threshold is configured by default (`0`, which disables the gate); it accepts an
integer or decimal percentage. Once set, it is enforced on every run unless you
pass `--no-enforce-coverage`. A configured threshold does not turn coverage on
by itself — that is the `coverage` policy's job.

```bash
# Fail if statement coverage is below 80%
putnami test . --coverage-threshold 80
```

The threshold can also be configured — and shared — at the workspace, language,
or project level via the standard option-merging rules (more specific scopes win):

```jsonc
// putnami.workspace.json — applies to every project's test job
{
  "options": {
    "test": { "coverage-threshold": 80 },          // all languages
    "@putnami/go:test": { "coverage-threshold": 85 } // Go only
  }
}
```

```jsonc
// <project>/putnami.json — applies to this project only
{
  "options": {
    "test": { "coverage-threshold": 90 }
  }
}
```

When the threshold is set but no coverage data is produced, the job fails rather
than passing silently.

A `coverage.out` that exists but cannot be read to its end gives no percentage:
the job reports a `COVERAGE_PROFILE_UNREADABLE` diagnostic that names the file
and the read error. With a threshold the diagnostic is an error and the job
fails; without one it is a warning. A solo run's summary says
`coverage profile unreadable`; a batched run's summary omits coverage. Profile
lines have no length limit.

### Coverage scope in a batch

When several projects share one batched `go test`, `coverage-scope` decides
whose tests count toward each project's coverage. A solo run always counts the
project's own tests only; the scope matters only when the scheduler batches.

| Scope | What runs | What counts toward project `a` |
|-------|-----------|--------------------------------|
| `batch` (default) | One `go test` over every batched project, with `-coverpkg` naming the packages of all of them, as an explicit list | The tests of every project in the batch. A line of `a` that another project's test reaches counts as covered. |
| `project` | One `go test` per batched project, each with `-coverpkg` naming that project's own packages only, as an explicit list | The tests of `a` only, the same as a solo run |

With `project` scope, a project's `coverage.out` is the same whichever projects
share its batch. Its `-coverpkg` no longer names its batch mates, which should
let the Go build cache reuse its instrumented packages across batches; that
reuse is not measured yet. A project whose threshold relied on lines that another
project's tests reach measures lower under `project` scope; it must add its own
tests to keep its threshold.

```jsonc
// putnami.workspace.json — measure every Go project against its own tests
{
  "options": {
    "@putnami/go:test": { "coverage-scope": "project" }
  }
}
```

The scope keys the test task's cache entry, and projects with different scopes
never share a batch. Any value other than `batch` or `project` fails the test
job before `go test` starts. See
[ADR 0008](adr/0008-coverage-scope-decides-whose-tests-count.md).

### Test process environment

The `go test` subprocess inherits the extension's environment with two
deliberate edits.

`GOWORK` is re-pointed at the governing `go.work` (a leaked `GOWORK=off` would
send each framework `require ... v0.0.0` placeholder to the proxy as a doomed
404), and the **host platform identity block is removed**: `K_SERVICE`,
`K_REVISION`, `K_CONFIGURATION`, the `CLOUD_RUN_*`, `GAE_*` and `AWS_LAMBDA_*`
families, and the rest of the list in
[`hostenv`](../../../tooling/extension-sdk/doc/06-hostenv.md). Those variables
describe the machine that launched the harness — a CI worker that is itself a
Cloud Run service, say — not the code under test, and application code that
reads them as a production signal would otherwise refuse test-only behavior
inside a test.

On a hosted run, `PUTNAMI_OFFLINE_DEPENDENCIES` and `PUTNAMI_JOB_CREDENTIAL_FD`
are removed too. They describe the job to the extension, and your tests are not
jobs of the run. Module downloads stay off in the test process: it keeps
`GOPROXY=off`, `GONOPROXY=none`, and `-mod=readonly` in `GOFLAGS`.

Credentials are **not** scrubbed: `GOOGLE_APPLICATION_CREDENTIALS`, the AWS
credential variables, `GOOGLE_CLOUD_PROJECT` and `DATABASE_TEST_BINDINGS` all
survive, so an integration test that talks to a real backend still can. The
scrub applies to the inherited environment only — `APP_ENV=test`, `FORCE_COLOR`,
`CGO_ENABLED=1` under `--race`, and the resolved database binding are all set
afterwards and still win.

The `config-merge` task is unaffected: it is a separate extension process
launched with the CLI's own environment, and it still reads `K_SERVICE` (and
declares it as a cache-key input) to skip merging on a cloud deployment.

### Test database bindings

Go test jobs run with `APP_ENV=test`. Before launching `go test`, the extension
resolves `DATABASE_TEST_BINDINGS` in this order:

1. An externally set `DATABASE_TEST_BINDINGS` — forwarded untouched, always wins.
2. The binding this invocation provisioned. When the project commits an
   `infra/requirements.json` declaring a database, the `test` pipeline runs a
   `test-env-up` step that resolves the `--infra` policy (`auto` locally,
   `require` on CI, `skip` to opt out) and, in `auto` mode with Docker
   available, starts a throwaway Postgres container and writes the resolved
   binding into a private, owner-only file for this run alone. A `runOn:
   finally` `test-env-down` step closes the lifecycle; the container itself is
   deliberately kept running so the next run reuses it, and `--infra-down`
   tears every workspace-labeled container down. The credential never travels
   as an inherited environment variable and never reaches a cache key, a log
   line, a container label, or a session record.
3. The project's test config (`conf/.env.test.yaml`, merged config under
   `.gen/conf/.env.test.yaml`, and adjacent secrets), when it declares a test
   database.

Use the canonical test binding shape when you need full control:

```yaml
databaseTest:
  protocolVersion: 1
  mode: skip
  isolation: database
  reuse: bundle-template
  applyMigrations: true
  databases:
    default:
      engine: postgres
      schema: public
      connection:
        host: localhost
        port: 5432
        database: myapp_test
        user: postgres
        password: postgres
        ssl: false
```

For the common case, put connection fields under `database` and policy under
`databaseTest`; the extension wraps them in the canonical binding. `engine`
defaults to `postgres`, and `schema` defaults to `public` when omitted.

```yaml
databaseTest:
  mode: skip
  isolation: database
  applyMigrations: true
database:
  default:
    schema: public
    host: localhost
    port: 5432
    database: myapp_test
    user: postgres
    password: postgres
    ssl: false
```

If `DATABASE_TEST_BINDINGS` is already set in the environment, the extension
preserves it and derives nothing — neither from a provisioned container nor
from config.

## API Reference

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--enforce-coverage` | `true` | Enforce each project's `coverage-threshold`. On by default so a drop fails the run that caused it. `--no-enforce-coverage` keeps measuring and reporting but never fails. Distinct from `--coverage`, so it never overrides a project's `coverage: false` opt-out |
| `--coverage` | `true` | Per-project coverage policy, and the sole authority on whether a run instruments. Set `false` (typically in project config) to opt a project out of coverage entirely and skip the instrumentation cost |
| `--race` | `false` | Enable race detector |
| `--timeout <duration>` | — | Test timeout (e.g. `30s`, `5m`) |
| `--short` | `false` | Run only tests that check `testing.Short()` |
| `--run <pattern>` | — | Run only tests matching this regex pattern |
| `--count <n>` | — | Run each test `n` times |
| `--shuffle` | `false` | Randomise test execution order (Go 1.17+) |
| `--bench <pattern>` | — | Run benchmarks matching this regex pattern |
| `--benchtime <duration>` | — | Duration per benchmark (e.g. `1s`, `100x`) |
| `--test-verbose` | `false` | Show the complete transcript at info level and pass `-v` to `go test`; changes visibility, not the verdict |
| `--test-json` | `true` | Run `go test` with `-json` for structured parsing |
| `--parallel <n>` | GOMAXPROCS | Maximum test parallelism |
| `--failfast` | `false` | Stop on first test failure |
| `--coverprofile <file>` | `coverage.out` | Coverage profile output file name |
| `--covermode <mode>` | `set` | Coverage mode: `set`, `count`, or `atomic` |
| `--outputdir <dir>` | job output path | Directory for coverage profile and HTML |
| `--coverhtml` | `false` | Generate HTML coverage report alongside the profile |
| `--coverage-threshold <pct>` | `0` | Minimum required statement coverage percentage; fails the job when coverage is below it (`0` disables the gate). Enforced on every run unless `--no-enforce-coverage`; a configured threshold does not turn coverage on by itself. Configurable per workspace, language, or project. |
| `--coverage-scope <scope>` | `batch` | Whose tests count toward a batched project's coverage: `batch` counts every batched project's tests, `project` counts the project's own tests only. Keys the task's cache entry; any other value fails the job. See [Coverage scope in a batch](#coverage-scope-in-a-batch) |
| `--remote-build-cache` | `true` | Back the Go build cache with the run's shared object cache when a cache provider offers one. `--remote-build-cache=false` keeps the local Go cache alone. See [Remote build cache](./build.md#remote-build-cache) |

### Emitted metrics

| Metric | Unit | Description |
|--------|------|-------------|
| `tests-total` | count | Total number of tests |
| `tests-passed` | count | Tests that passed |
| `tests-failed` | count | Tests that failed |
| `tests-skipped` | count | Tests that were skipped |
| `coverage` | percent | Overall statement coverage percentage |
| `coverage-statements-total` | count | Total Go statements across all packages |
| `coverage-statements-covered` | count | Statements executed by at least one test |

### Summary label

The job recap shows:

```
20/20 passed, 85.2% coverage
```

### Result data

The job outputs a structured result:

```json
{
  "status": "FAILED",
  "data": {
    "testSummary": {
      "passed": 4,
      "failed": 18,
      "skipped": 0,
      "total": 22,
      "failureDetailsTruncated": 4
    },
    "coverageSummary": {
      "totalStatements": 420,
      "coveredStatements": 357,
      "percentage": 85.0,
      "files": [
        {
          "file": "internal/handler/handler.go",
          "totalStatements": 42,
          "coveredStatements": 42,
          "percentage": 100.0
        }
      ]
    }
  }
}
```

### Test cases

With `--test-json` on, the default, `result.data.testCases` lists one entry per
test that reached a result, subtests included:

| Field | Value |
|-------|-------|
| `name` | The test name as `go test` reports it, such as `TestParse/empty` for a subtest |
| `suite` | The package import path |
| `status` | `passed`, `failed` or `skipped` |
| `durationMs` | The run time `go test` reports |
| `output` | For a failed or skipped test, the lines it printed, without the `=== RUN` and `--- FAIL` lines |
| `file`, `line` | For a failed or skipped test, the first `name_test.go:N` location in its output, workspace-relative, when that file exists in the package directory |

The list keeps at most 1,000 entries and at most 1 MiB of encoded entries:
failed first, then skipped, then passed. In a batch, the members share 8 MiB
equally, since their entries travel in one result line.
`result.data.testCasesDropped` counts the entries left out. The CLI turns each
entry into one `test:case` record of the session stream. With
`--test-json=false` the job reports no entries.

## Coverage Details

Coverage is measured using Go's native coverage tool. Before `go test`, the job
lists the packages the module owns with `go list ./...` and passes that list as
`-coverpkg` (`-cover -coverpkg=example.com/a,example.com/a/x,...`); `./...`
still selects the tests to run:

- **Profile format**: `pkg:file.go:start.column,end.column:statements:execCount`
- **Percentage**: weighted by statement count, not line count
- **Scope**: every package of the project's module, including those with no
  tests. A module nested under the project directory with its own `go.mod`,
  for example a generated `clients/go`, is a different project: its packages
  are not part of this project's coverage even when a test imports them, and
  `coverage.out` carries none of their blocks. A directory pattern would
  instrument them, because `go test` matches `-coverpkg` by directory prefix,
  which is why the list is explicit. In a batched run with the default
  `coverage-scope: batch`, the tests of the other batched projects also count;
  see [Coverage scope in a batch](#coverage-scope-in-a-batch)
- **HTML report**: generated by `go tool cover -html` when `--coverhtml` is set
- **Zero-coverage files**: emitted as warnings in the diagnostics output
- **Location**: `coverage.out` and `coverage.html` in the per-command output
  directory (`.putnami/out/{project}/test/`), which the manifest declares as
  this task's only outputs — see [Declared Outputs](build.md#declared-outputs).
  Both are optional: a project with `coverage: false` (and no explicit
  `--coverprofile`/`--covermode`/`--coverhtml`) produces no profile at all.

### Coverage modes

| Mode | Description | Use case |
|------|-------------|----------|
| `set` | Whether each statement was executed (default) | Standard coverage reports |
| `count` | How many times each statement was executed | Hot-path identification |
| `atomic` | Thread-safe count (required with `-race`) | Concurrent test suites |

## Boundaries

- **Scope**: project-only — workspace-level runs are not supported
- **Test discovery**: Go's `*_test.go` convention; no additional configuration required
- **Coverage**: statement-level (not branch or decision coverage)
- **Benchmarks**: run in the same `go test` invocation as regular tests; use `--run ^$` to skip regular tests when benchmarking
