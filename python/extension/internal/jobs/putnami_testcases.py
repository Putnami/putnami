"""Putnami per-test-case pytest plugin.

Records one JSON line per test item pytest ran into the file that
``PUTNAMI_TEST_CASES_FILE`` names: the case name, the test module that
collected it, the file and line that declare it, its outcome over every run phase, its duration, and the failure
text or skip reason. The Putnami test adapter reads the file after pytest
exits and reports each line as one test case of the task.

The adapter loads the plugin with ``-p putnami_testcases`` and puts this file
on ``PYTHONPATH``. Without ``PUTNAMI_TEST_CASES_FILE`` the plugin registers
nothing, so pytest runs exactly as it does without the plugin. The plugin
states what pytest reported and nothing else: the adapter converts paths,
applies the contract bounds, and a record never changes the run's verdict.

Outcome rules, over the reports of one item:

- ``failed`` when any phase failed: a setup error, a failing call, a teardown
  error, or a strict XPASS, which pytest reports as a failure.
- otherwise ``skipped`` when setup skipped, or when the last call report
  skipped; an xfail is a skip, as pytest reports it.
- otherwise ``passed`` when the last call report passed.
- otherwise nothing is written.

The last call report decides because subtests report before the call that
holds them, and a rerun plugin reports the final attempt last.

Standard library plus pytest only.
"""

import json
import os

CASES_FILE_ENV = "PUTNAMI_TEST_CASES_FILE"

_RECORDER_NAME = "putnami-testcases-recorder"

_CAPTURED_PREFIXES = ("Captured stdout", "Captured stderr")


def pytest_configure(config):
    path = os.environ.get(CASES_FILE_ENV, "")
    if not path:
        return
    # A pytest-xdist worker forwards its reports to the controller, which calls
    # the same hooks, so only the controller records and one process writes.
    if hasattr(config, "workerinput"):
        return
    try:
        sink = open(path, "ab")
    except OSError:
        return
    recorder = _Recorder(sink, str(getattr(config, "rootpath", None) or config.rootdir))
    config.add_cleanup(recorder.close)
    config.pluginmanager.register(recorder, _RECORDER_NAME)


class _Recorder:
    """Collects the reports of each item and writes one line when pytest
    finishes the item. Every failure to record is silent: reporting never
    turns a test run red."""

    def __init__(self, sink, root):
        self._sink = sink
        self._root = root
        self._pending = {}

    def pytest_runtest_logreport(self, report):
        try:
            self._observe(report)
        except Exception:  # noqa: BLE001 - reporting never fails the test run
            pass

    def pytest_runtest_logfinish(self, nodeid, location):
        try:
            self._finish(nodeid, location)
        except Exception:  # noqa: BLE001 - reporting never fails the test run
            pass

    def close(self):
        try:
            self._sink.close()
        except OSError:
            pass

    def _observe(self, report):
        case = self._pending.get(report.nodeid)
        if case is None:
            case = {
                "durations": {},
                "failures": [],
                "captured": "",
                "setup_skip": None,
                "call": None,
            }
            self._pending[report.nodeid] = case
        case["durations"][report.when] = float(report.duration or 0.0)
        if report.failed:
            case["failures"].append(report.longreprtext)
            case["captured"] = _captured(report)
        elif report.when == "setup" and report.skipped:
            case["setup_skip"] = _skip_reason(report)
        elif report.when == "call" and report.skipped:
            case["call"] = ("skipped", _skip_reason(report))
        elif report.when == "call" and report.passed:
            case["call"] = ("passed", "")

    def _finish(self, nodeid, location):
        case = self._pending.pop(nodeid, None)
        if case is None:
            return
        status, output = _verdict(case)
        if status is None:
            return
        module, separator, name = nodeid.partition("::")
        record = {
            "name": name if separator else nodeid,
            "status": status,
            "duration": sum(case["durations"].values()),
            "output": output,
        }
        # The node ID names the module that collected the item, relative to
        # the rootdir. The location names where the test is declared, which
        # for an inherited test is the base class's module.
        if separator and module:
            record["module"] = os.path.normpath(os.path.join(self._root, module))
        if status == "failed" and case["captured"]:
            record["captured"] = case["captured"]
        if location and location[0]:
            record["file"] = os.path.normpath(os.path.join(self._root, str(location[0])))
            lineno = location[1]
            if isinstance(lineno, int) and lineno >= 0:
                record["line"] = lineno + 1
        line = json.dumps(record, allow_nan=False) + "\n"
        self._sink.write(line.encode("ascii"))
        self._sink.flush()


def _verdict(case):
    if case["failures"]:
        return "failed", "\n\n".join(case["failures"])
    if case["setup_skip"] is not None:
        return "skipped", case["setup_skip"]
    if case["call"] is not None:
        return case["call"]
    return None, ""


def _skip_reason(report):
    """Returns the reason pytest gives for a skip: the xfail reason for an
    expected failure, the ``Skipped: ...`` message otherwise."""
    if hasattr(report, "wasxfail"):
        reason = str(report.wasxfail or "")
        return "XFAIL: " + reason if reason else "XFAIL"
    longrepr = report.longrepr
    if isinstance(longrepr, tuple) and len(longrepr) == 3:
        return str(longrepr[2])
    return report.longreprtext


def _captured(report):
    """Returns the stdout and stderr the item printed up to this report, in
    pytest's own section order."""
    parts = []
    for title, content in report.sections:
        if title.startswith(_CAPTURED_PREFIXES) and content:
            parts.append("----- %s -----\n%s" % (title, content.rstrip("\n")))
    return "\n".join(parts)
