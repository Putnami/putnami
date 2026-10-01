---
order: 12
---

# Test Command

**Command:** `putnami test [project]`

**Purpose:** Run pytest for a Python project. Tests are discovered by `**/test_*.py` and `**/*_test.py`.

## Usage

```bash
putnami test <project> [options]
```

## Arguments

- `project`: The name or directory of the package to test.

## Options

- `--log`: Stream test logs to stdout (`-s`, `log_cli=true`).
- `--update-snapshots`: Update snapshot files.
- `-t <filter>`: Run a subset of tests by name/path.

## Examples

```bash
putnami test api-server
putnami test api-server --log
putnami test api-server -t test_health
```

## Declared outputs

`test-run` declares exactly **one** output: the reserved spec-verification
report (`putnami-feature-verification.json`). A test bound to a
declared executable-spec check with the `putnami_proves(feature, requirement,
check)` marker has its verdict recorded by the adapter-injected
`putnami_spectest` pytest plugin, and after the run — failing runs included —
the fragments are merged, validated, and written into the per-command output
directory, one report per project under batching too. The declaration is
`optionalEmpty`: a project with no bound test writes none. Without the
adapter, the marker is inert metadata and plain `pytest` behaves exactly as
before (register the marker in your `pyproject.toml` to silence the
unknown-mark warning).

A **threshold** check is measured rather than judged. Bind the test with
`putnami_observes(feature, requirement, check)` and record the aggregate it
observed through pytest's own `record_property` fixture, under the reserved
`putnami_measurement` name:

```python
import pytest


@pytest.mark.putnami_observes("py/example-library", "flush-latency", "flush-benchmark")
def test_flush_stays_under_budget(record_property):
    elapsed = measure_flush()
    assert elapsed < 0.05
    record_property(
        "putnami_measurement",
        {"name": "library.flush.duration", "aggregation": "p95", "value": elapsed * 1000, "unit": "ms"},
    )
```

The plugin publishes the aggregate with the window the call phase covered, and
nothing else: the fragment carries no verdict, so core recomputes the
threshold result from the target authored in `putnami.features.json`. A test
that fails, is skipped, or forgets to record publishes **nothing** — a
measurement fragment has no verdict field to carry that rejection, so its
absence is the honest report, and core resolves it as missing. Because
`record_property` is core pytest, a measuring test is as inert under bare
`pytest` as a marker is.

Beyond that, `pytest` is still invoked with no reporter — no `--cov`, no
`--junitxml` — so the run writes no coverage profile and no JUnit report; its
result travels as the structured `testSummary` payload parsed from `pytest`'s
own output. That is the difference from the Go and TypeScript test tasks,
which own coverage and JUnit files in the per-command output directory.
Adding a reporter here means adding the matching declared output at the same
time, which the extension's own conformance test enforces.

## Per-test results

The adapter also loads its `putnami_testcases` pytest plugin, which records
one line per test item into a scratch file outside the project. After the run,
failing runs included, the adapter puts one entry per case into the result
payload under `testCases`, and the CLI reports each entry as a `test:case`
record of the task. An entry carries:

- `name`: the node ID after the file, such as `TestParser::test_empty[unicode]`.
- `suite`: the workspace-relative test module that collected the test, the
  file part of its node ID.
- `file`: the workspace-relative file that declares the test. It differs from
  `suite` for a test a class inherits from another module.
- `line`: the line pytest reports for the test, when it knows one.
- `status`: `failed` when any phase failed (setup and teardown errors and a
  strict XPASS included), `skipped` for a skip or an xfail, `passed` otherwise.
- `durationMs`: the setup, call, and teardown time together.
- `output`: for a failed case, the failure text, followed by the captured
  stdout and stderr when both fit in the output bound; for a skipped case, the
  reason.

A run keeps at most 1,000 entries and at most 1 MiB of encoded entries, failed
cases first; in a batch, the members share 8 MiB equally. It reports the rest
as `testCasesDropped`. The scratch file is not a task output: it is neither
captured nor restored, and a cache hit replays the entries from the cached
result payload. Without the adapter the plugin is never loaded, and loaded
without its scratch file it records nothing.

## Test process environment

The `pytest` subprocess gets `PYTHONPATH` rooted at the project, `FORCE_COLOR=1`
and the `PUTNAMI_WORKSPACE` / `PUTNAMI_WORKING_DIR` locators, on top of the
extension's inherited environment **minus the host platform identity block**:
`K_SERVICE`, `K_REVISION`, `K_CONFIGURATION`, the `CLOUD_RUN_*`, `GAE_*` and
`AWS_LAMBDA_*` families, and the rest of the list in
[`hostenv`](../../../tooling/extension-sdk/doc/06-hostenv.md).

Those variables describe the machine that launched the harness — a CI worker
that is itself a Cloud Run service, say — not the code under test, and
application code that reads them as a production signal would otherwise refuse
test-only behavior inside a test. Credentials are **not** scrubbed:
`GOOGLE_APPLICATION_CREDENTIALS`, the AWS credential variables and
`GOOGLE_CLOUD_PROJECT` all survive, so an integration test that talks to a real
backend still can.

The scrub is confined to the test paths. `putnami run` and `putnami serve` start
a real application and inherit the environment whole, host identity included.

## Batching

`test-run` is `batchable`: when the scheduler resolves to a single worker it may
group several ready test suites into one extension process, which runs each
project's `pytest` in turn (an extension-loop). Batching amortizes only the
extension-process startup — each project still pays its own `pytest` boot, keeps
its own pass/fail and cache identity, and a failing or crashing suite is
isolated to that project (respawned once on a hard crash or hang) without
failing a batch-mate. See `tooling/cli/doc/04-job-execution.md` for details.
