"""Putnami executable-spec pytest plugin.

Binds ordinary pytest tests to the executable-spec verification wire: a test
that protects a declared check carries the ``putnami_proves`` marker, and the
check's verdict is written as a process-local fragment the Putnami test
adapter merges into the project's ``putnami-feature-verification`` report
artifact.

Threshold checks have a measured producer beside that acceptance one: a test
carrying the ``putnami_observes`` marker publishes the aggregate it recorded
through pytest's own ``record_property`` fixture, under the reserved
``putnami_measurement`` property name. A fragment carries a verdict or a
measurement, never both, so a producer can state a number but never its own
verdict. ``record_property`` is core pytest, so a measuring test stays as
inert under bare pytest as a marker is.

The plugin is loaded only by the Putnami test adapter (``-p putnami_spectest``
on the pytest command line, with this file placed on ``PYTHONPATH``), so plain
``pytest`` without the adapter runs every test exactly as before — the marker
alone is inert metadata. The plugin is deliberately dumb: it states what THIS
process observed — feature, requirement, check, verdict or measurement,
declaration site — and nothing else. The adapter owns validation, project
attribution, and merging; core recomputes every verdict against the authored
criterion; and no marker can make a gate green that the test's own outcome
does not support, because the verdict is read from pytest's own run report.

Standard library plus pytest only. The fragment JSON matches the Go
``spectest.Fragment`` shape read by the shared adapter merge
(``tooling/extension-sdk/specreport``).
"""

import datetime
import hashlib
import json
import numbers
import os
import tempfile

import pytest

FRAGMENT_DIR_ENV = "PUTNAMI_SPEC_FRAGMENTS"
MARKER = "putnami_proves"
MEASURE_MARKER = "putnami_observes"
MEASUREMENT_PROPERTY = "putnami_measurement"

# The staged measurement of the current item, kept between its call and
# teardown reports so a teardown failure can still withhold the observation.
_PENDING_ATTR = "_putnami_spec_pending_measurement"

_MALFORMED_BINDING = (
    "%s marker needs feature, requirement, and check as non-empty strings; "
    "no spec observation was recorded"
)

_warned = set()


def pytest_configure(config):
    config.addinivalue_line(
        "markers",
        "%s(feature, requirement, check): bind this test to a declared "
        "executable-spec check; its verdict is reported to the Putnami spec "
        "gate" % MARKER,
    )
    config.addinivalue_line(
        "markers",
        "%s(feature, requirement, check): bind this test to a declared "
        "executable-spec threshold check; record the observed aggregate with "
        "record_property(%r, {...}) and it is reported to the Putnami spec "
        "gate" % (MEASURE_MARKER, MEASUREMENT_PROPERTY),
    )


@pytest.hookimpl(wrapper=True)
def pytest_runtest_makereport(item, call):
    report = yield
    directory = os.environ.get(FRAGMENT_DIR_ENV, "")
    if not directory:
        return report
    _report_verdict(item, report, directory)
    _report_measurement(item, call, report, directory)
    return report


def _report_verdict(item, report, directory):
    """Publishes the verdict of an acceptance check bound with the
    ``putnami_proves`` marker."""
    marker = item.get_closest_marker(MARKER)
    if marker is None:
        return
    binding = _binding(marker)
    if binding is None:
        _warn_once(item, _MALFORMED_BINDING % MARKER)
        return
    status = _status(report)
    if status is None:
        return
    fragment = dict(binding)
    fragment["status"] = status
    fragment["file"] = _declaration_file(item)
    fragment["symbol"] = _root_name(item)
    _write_fragment(directory, fragment)


def _report_measurement(item, call, report, directory):
    """Publishes the aggregate of a threshold check bound with the
    ``putnami_observes`` marker.

    The observation is staged when the call phase passes and published only
    after teardown also passed, mirroring Go's cleanup-time rule: a test that
    failed or was skipped in ANY phase publishes NOTHING, because its
    measurement was taken under conditions the test itself rejected, and an
    absent observation resolves as missing — the fail-closed direction. A
    measurement fragment has no verdict field to carry that rejection, so
    withholding it is the only honest report.
    """
    marker = item.get_closest_marker(MEASURE_MARKER)
    if marker is None:
        return
    if report.when == "call":
        _stage_measurement(item, call, report, marker)
        return
    if report.when != "teardown":
        return
    fragment = getattr(item, _PENDING_ATTR, None)
    if fragment is None or not report.passed:
        return
    _write_fragment(directory, fragment)


def _stage_measurement(item, call, report, marker):
    """Builds the fragment a passing measuring test will publish at teardown.

    The window is the call phase's own span — pytest's start and stop instants
    for the test body — which is exactly what an invocation-window criterion
    evaluates.
    """
    if not report.passed:
        return
    binding = _binding(marker)
    if binding is None:
        _warn_once(item, _MALFORMED_BINDING % MEASURE_MARKER)
        return
    measurement = _measurement(report.user_properties)
    if measurement is None:
        _warn_once(
            item,
            "%s needs record_property(%r, {name, aggregation, value, unit}) "
            "with a finite numeric value; no spec observation was recorded"
            % (MEASURE_MARKER, MEASUREMENT_PROPERTY),
        )
        return
    fragment = dict(binding)
    fragment["measurement"] = measurement
    fragment["window"] = {"start": _instant(call.start), "end": _instant(call.stop)}
    fragment["file"] = _declaration_file(item)
    fragment["symbol"] = _root_name(item)
    setattr(item, _PENDING_ATTR, fragment)


def _measurement(user_properties):
    """Resolves the observed aggregate from the reserved recorded property.

    The shape is checked, never guessed: a property that is not a mapping of
    exactly name, aggregation, value, unit — with a finite real value that is
    not a bool — is malformed and warned about. The last recording wins, which
    is what a reader of the test body expects.
    """
    latest = None
    found = False
    for name, value in user_properties:
        if name != MEASUREMENT_PROPERTY:
            continue
        latest = value
        found = True
    if not found or not isinstance(latest, dict) or set(latest) != {"name", "aggregation", "value", "unit"}:
        return None
    if any(not isinstance(latest[key], str) or not latest[key] for key in ("name", "aggregation", "unit")):
        return None
    observed = latest["value"]
    if isinstance(observed, bool) or not isinstance(observed, numbers.Real):
        return None
    observed = float(observed)
    if observed != observed or observed in (float("inf"), float("-inf")):
        return None
    return {
        "name": latest["name"],
        "aggregation": latest["aggregation"],
        "value": observed,
        "unit": latest["unit"],
    }


def _instant(epoch_seconds):
    """Renders an epoch instant as the same second-precision RFC 3339
    timestamp Go writes, so a window means the same thing whichever runtime
    observed it."""
    moment = datetime.datetime.fromtimestamp(epoch_seconds, datetime.timezone.utc)
    return moment.replace(microsecond=0).strftime("%Y-%m-%dT%H:%M:%SZ")


def _binding(marker):
    """Resolves (feature, requirement, check) from positional or keyword
    marker arguments; anything else is malformed and warned about, never
    guessed."""
    names = ("feature", "requirement", "check")
    args = list(marker.args)
    if len(args) > len(names) or set(marker.kwargs) - set(names):
        return None
    values = {}
    for index, name in enumerate(names):
        if index < len(args):
            if name in marker.kwargs:
                return None
            values[name] = args[index]
        elif name in marker.kwargs:
            values[name] = marker.kwargs[name]
        else:
            return None
    for value in values.values():
        if not isinstance(value, str) or not value:
            return None
    return values


def _status(report):
    """Maps one run-phase report onto the observation vocabulary. A skip in
    setup (skip marker) or in call (pytest.skip) is ``skipped``; a failure in
    any phase — a fixture error included — is ``failed``, because the
    protecting test did not protect; a passing call is ``passed``. A passing
    setup or teardown reports nothing (the call verdict does)."""
    if report.when == "setup":
        if report.skipped:
            return "skipped"
        if report.failed:
            return "failed"
        return None
    if report.when == "call":
        if report.passed:
            return "passed"
        if report.skipped:
            return "skipped"
        return "failed"
    if report.when == "teardown" and report.failed:
        return "failed"
    return None


def _declaration_file(item):
    path = getattr(item, "path", None)
    if path is None:
        path = getattr(item, "fspath", "")
    return os.path.abspath(str(path))


def _root_name(item):
    """Reduces a parametrized node to the test function that declares it:
    test_export[case-2] names test_export, which is what a reader can find in
    the file the fragment points at."""
    name = getattr(item, "originalname", None)
    if name:
        return name
    return item.name.split("[", 1)[0]


def _warn_once(item, message):
    """Surfaces one producer mistake per test, once: a malformed binding or a
    missing measurement is never guessed at, and repeating it for every phase
    of the same test would bury it."""
    key = (item.nodeid, message)
    if key in _warned:
        return
    _warned.add(key)
    item.warn(pytest.PytestWarning(message))


def _write_fragment(directory, fragment):
    """Publishes one fragment atomically under a content-derived name, so
    identical observations collapse and concurrent writers never interleave.
    Failures are deliberately silent: reporting is the adapter's concern, and
    a full disk must not turn a green test red."""
    try:
        encoded = json.dumps(fragment, sort_keys=True).encode("utf-8")
        digest = hashlib.sha256(encoded).hexdigest()[:32]
        final = os.path.join(directory, digest + ".json")
        handle = tempfile.NamedTemporaryFile(
            mode="wb", dir=directory, prefix=".fragment-", suffix=".tmp", delete=False
        )
        try:
            with handle:
                handle.write(encoded)
            os.replace(handle.name, final)
        except OSError:
            try:
                os.unlink(handle.name)
            except OSError:
                pass
    except Exception:  # noqa: BLE001 - reporting never fails the test run
        pass
