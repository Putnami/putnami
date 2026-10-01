# Test

The test command discovers and runs tests using Bun's built-in test runner. It produces JUnit XML reports and optional LCOV coverage output.

## Overview

- Discovers test files by glob pattern (`*.test.ts`, `*.spec.ts`, and `.tsx`/`.js`/`.jsx` variants)
- Skips automatically if no test files are found
- Runs a generate phase before testing (to ensure generated code is up to date)
- Produces JUnit XML reports for CI integration
- Supports LCOV coverage reporting
- Extracts detailed failure information including assertion details

## Usage

### Basic Usage

```bash
putnami test .
```

### Common Patterns

```bash
# Watch mode — re-runs on file changes
putnami test . --watch

# Generate coverage report (validation cadence)
putnami test . --enforce-coverage

# Fail when line coverage is below 80% (validation cadence — pass --enforce-coverage)
putnami test . --enforce-coverage --coverage-threshold 80

# Run a specific test by name
putnami test . --test "my test name"

# Increase timeout for slow tests
putnami test . --timeout 30000

# Stop after first failure
putnami test . --bail 1

# Show Bun's complete transcript in normal output
putnami test . --test-verbose
```

### Advanced Usage

```bash
# Filter tests by regex pattern
putnami test . --test-name-pattern "auth.*login"

# Run only tests marked with test.only()
putnami test . --only

# Update snapshot files
putnami test . --update-snapshots

# Re-run each test file 3 times (flake detection)
putnami test . --rerun-each 3

# Run tests concurrently
putnami test . --concurrent
```

## Execution Flow

```text
generate ──→ discover ──→ run ──→ parse
```

1. **Generate** — runs the build generate phase to ensure `.gen/` is current
2. **Discover** — scans the project for test files matching `**/*.{test,spec}.{ts,tsx,js,jsx}` (excludes `node_modules`, `.gen`, and `dist` directories)
3. **Run** — executes `bun test` with discovered files and configured options
4. **Parse** — reads JUnit XML and optional LCOV output, extracts failures and metrics

If no test files are found, the command returns `SKIP` (exit 0) unless `--pass-with-no-tests` is `false`.

## Test File Conventions

Test files must match one of these patterns:

- `*.test.ts` / `*.test.tsx`
- `*.spec.ts` / `*.spec.tsx`
- `*.test.js` / `*.test.jsx`
- `*.spec.js` / `*.spec.jsx`

Place tests in a `test/` directory or colocate them with source files. Both patterns are discovered.

## Output

### Structured outcome and transcript

The default output is summary-first. TypeScript reports one deterministic recap
such as `20/22 passed, 1 failed, 1 skipped, 85.2% coverage`; the same counts live
in `result.data.testSummary` for machine clients. Every non-empty Bun output line
is still emitted as a `debug` log event, including successful test detail. A
runtime-event session recorder can therefore retain the complete transcript
without copying it into the result or the diagnostics. `--test-verbose` promotes
the same lines to `info`; it does not add Bun arguments or change structured
counts, coverage verdicts, job status, or exit code. Normal machine output
suppresses the debug transcript and reports those omitted records and bytes;
the complete sanitized transcript is retained in the session's `events.jsonl`
artifact.

Failure diagnostics are a bounded causal view of that transcript. At most 16
are emitted, including an accounting diagnostic when more exist, and each
message is at most 1,024 bytes of valid UTF-8. The omitted count is recorded as
`testSummary.failureDetailsTruncated`; the complete transcript remains in the
debug event stream and the session artifact. Transcript logs are deliberately
absent from action-cache entries: a warm cache hit replays the structured
summary and verdict, but not the transcript, so `--test-verbose` exposes full
detail only when the test task executes.

### JUnit XML

A JUnit report is written to the output directory for CI consumption. It contains test suite names, individual test results, and failure details.

### Test cases

`result.data.testCases` lists one entry per test case of the JUnit report:

| Field | Value |
|-------|-------|
| `name` | The names of the enclosing `describe` blocks, then the test name, joined by ` > ` |
| `suite` | The workspace-relative test file |
| `status` | `passed`, `failed` or `skipped` |
| `durationMs` | The run time Bun reports |
| `output` | For a failed test, the failure message and stack; for a skipped test, its skip message |
| `file`, `line` | The workspace-relative file and the line Bun reports for the test |

The list keeps at most 1,000 entries and at most 1 MiB of encoded entries:
failed first, then skipped, then passed. In a batch, the members share 8 MiB
equally, since their entries travel in one result line.
`result.data.testCasesDropped` counts the entries left out, including a test
whose file lies outside the workspace. The CLI turns each entry into one
`test:case` record of the session stream.

### Spec verification report

A test that protects a declared executable-spec check binds itself
with `specTest` from `@putnami/runtime/spectest`. The job provisions a fresh
fragment directory per run (`PUTNAMI_SPEC_FRAGMENTS`), and after `bun test`
returns — on failing runs too — the fragments are merged, validated, and
published as the reserved `putnami-feature-verification` report artifact, one
per project under batching as well. The report carries observations only;
core recomputes every verdict against the authored criterion, and an absent
report resolves as missing evidence. Without the adapter, `specTest` is inert
and registers the test unchanged.

A **threshold** check is measured rather than judged. Register it with
`observeMeasurement` and return the aggregate the body observed:

```ts
import { observeMeasurement } from '@putnami/runtime/spectest';

observeMeasurement('flush stays under budget', { feature: 'ts/logger', requirement: 'flush-latency', check: 'flush-benchmark' }, async () => {
  const elapsed = await measureFlush();
  expect(elapsed).toBeLessThan(50);
  return { name: 'logger.flush.duration', aggregation: 'p95', value: elapsed, unit: 'ms' };
});
```

The helper publishes the aggregate with the invocation window the body
covered, and nothing else: the fragment carries no verdict, so core recomputes
the threshold result from the target authored in `putnami.features.json`. A
body that throws publishes **nothing** — a measurement fragment has no verdict
field to carry that rejection, so its absence is the honest report, and core
resolves it as missing.

### Coverage (LCOV)

When coverage is collected (the validation cadence — pass `--enforce-coverage`), an LCOV report is written to the output directory. Coverage is computed for project source files only — external dependencies (paths starting with `..`) are excluded. The summary includes:

- Line coverage percentage and counts
- Function coverage percentage and counts

### Coverage Threshold

`--coverage-threshold <pct>` fails the test job when line coverage drops below
the given percentage. The gate is off by default (`0`) and accepts an integer or
decimal. It is enforced only in the validation cadence — pass `--enforce-coverage`;
a configured threshold no longer turns coverage on by itself, so the inner loop
(`putnami test`) stays instrumentation-free. `--enforce-coverage` is distinct from
the per-project `coverage` policy and never overrides a project's `coverage: false`
opt-out. If the threshold is set and coverage is collected but no coverage data is
produced, the job fails rather than passing silently.

It can be shared at the workspace, language, or project level via the standard
option-merging rules (more specific scopes win):

```jsonc
// putnami.workspace.json — applies to every project's test job
{
  "options": {
    "test": { "coverage-threshold": 80 },                  // all languages
    "@putnami/typescript:test": { "coverage-threshold": 85 } // TypeScript only
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

### Failure Details

Failed tests include extracted assertion details from the Bun test output:

- Expected vs. received values
- Assertion type and location
- Stack traces

Unhandled errors between tests (module-level crashes) are extracted and reported as diagnostics with file and line information.

## API Reference

### Flags

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--timeout` | `number` | `5000` | Per-test timeout in milliseconds |
| `--timeout-budget` | `number` | `600000` | Wall-clock timeout for the test subprocess in milliseconds |
| `--update-snapshots` | `boolean` | `false` | Update snapshot files instead of comparing |
| `--rerun-each` | `number` | — | Re-run each test file N times |
| `--only` | `boolean` | — | Only run tests marked with `test.only()` |
| `--todo` | `boolean` | — | Include tests marked with `test.todo()` |
| `--enforce-coverage` | `boolean` | `false` | Validation/CI cadence switch: collect an LCOV report and enforce each project's `coverage-threshold`. The inner loop (no `--enforce-coverage`) skips instrumentation. Distinct from `--coverage`, so it never overrides a project's `coverage: false` opt-out |
| `--coverage` | `boolean` | `true` | Per-project coverage policy. Set `false` (typically in project config) to opt a project out of coverage entirely — authoritative, honored even under `--enforce-coverage` |
| `--coverage-threshold` | `number` | `0` | Minimum required line coverage percentage; fails the job when coverage is below it (`0` disables the gate). Enforced only in the validation cadence (pass `--enforce-coverage`); a configured threshold no longer turns coverage on by itself. Configurable per workspace, language, or project. |
| `--bail` | `number` | — | Exit after N test failures |
| `--test-name-pattern` | `string` | — | Run tests matching this regex pattern |
| `--test` / `-t` | `string` | — | Run tests matching this filter string |
| `--concurrent` | `string` | — | Treat all tests as concurrent |
| `--pass-with-no-tests` | `boolean` | `true` | Pass if no test files are found |
| `--test-verbose` | `boolean` | `false` | Show the complete Bun transcript at info level; changes visibility only, never the invocation or verdict |

### Activation

The test command activates only when the project contains files matching `**/*.test.ts`, `**/*.spec.ts`, `**/*.test.tsx`, or `**/*.spec.tsx`.

### Test process environment

The `bun test` subprocess inherits the extension's environment **minus the host
platform identity block**: `K_SERVICE`, `K_REVISION`, `K_CONFIGURATION`, the
`CLOUD_RUN_*`, `GAE_*` and `AWS_LAMBDA_*` families, and the rest of the list in
[`hostenv`](../../../tooling/extension-sdk/doc/06-hostenv.md).

Those variables describe the machine that launched the harness — a CI worker
that is itself a Cloud Run service, say — not the code under test. Application
code that treats `K_SERVICE` as the production signal
(`allowEphemeral = config.allowEphemeralSigningKey && !process.env.K_SERVICE`)
would otherwise refuse test-only behavior inside a test, failing deterministically
on that host and nowhere else.

Credentials are **not** scrubbed. `GOOGLE_APPLICATION_CREDENTIALS`, the AWS
credential variables, `GOOGLE_CLOUD_PROJECT` and `DATABASE_TEST_BINDINGS` all
survive, so an integration test that talks to a real backend still can. The
scrub applies to the inherited environment only: `FORCE_COLOR=1` and the
resolved database binding are set afterwards and still win.

A test that needs to assert production-guard behaviour should set the variable
itself (`process.env.K_SERVICE = 'x'` inside the test), which is both explicit
and portable.

### Batching

`test-run` is `batchable`: when the scheduler resolves to a single worker it may
group several ready test suites of the same effective configuration into one
extension process, which runs each project's `bun test` in turn (an
extension-loop). This amortizes the extension-process startup while each project
keeps its own suite run, its own captured output directory (coverage, JUnit),
its own coverage-threshold evaluation, and its own pass/fail and cache identity.
A failing or crashing suite is isolated to that project and never fails a
batch-mate. See `tooling/cli/doc/04-job-execution.md` for the scheduler
mechanics.

## Boundaries

- **Scope**: Discovering and running tests, producing reports (JUnit, LCOV)
- **Out of scope**: Test file generation, mocking frameworks, test environment setup. Use Bun's built-in test utilities (`bun:test`) for assertions and mocks.
- **Dependencies**: Requires Bun runtime with `bun:test` support
- **Extension points**: None. Test configuration is controlled entirely through CLI flags and Bun's built-in test runner options.
