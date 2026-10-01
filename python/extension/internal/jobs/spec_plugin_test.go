package jobs

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

// The plugin's verdict capture is exercised through a REAL pytest run,
// resolved through the repository's own uv workspace: a generated suite binds
// passing, failing, skipped, parametrized, and malformed markers, and the
// test asserts the fragments the plugin left behind. That is the honest
// shape — the verdict in a fragment must be what the runner observed.

const specPluginSuite = `import pytest


@pytest.mark.putnami_proves("py/demo", "lifecycle", "passing-check")
def test_passes():
    assert True


@pytest.mark.putnami_proves(feature="py/demo", requirement="lifecycle", check="failing-check")
def test_fails():
    assert False


@pytest.mark.putnami_proves("py/demo", "lifecycle", "skipped-check")
@pytest.mark.skip(reason="deliberate")
def test_skipped():
    assert True


@pytest.mark.putnami_proves("py/demo", "lifecycle")
def test_malformed_marker():
    assert True


@pytest.mark.putnami_proves("py/demo", "lifecycle", "param-check")
@pytest.mark.parametrize("value", [1, 2])
def test_param(value):
    assert value > 0


MEASUREMENT = {"name": "demo.flush.duration", "aggregation": "p95", "value": 12.5, "unit": "ms"}
CORRECTED_MEASUREMENT = {"name": "demo.flush.duration", "aggregation": "p95", "value": 24.5, "unit": "ms"}


@pytest.fixture
def skip_in_teardown():
    yield
    pytest.skip("deliberate teardown skip")


@pytest.mark.putnami_observes("py/demo", "lifecycle", "measured-check")
def test_measures(record_property):
    record_property("putnami_measurement", MEASUREMENT)


@pytest.mark.putnami_observes("py/demo", "lifecycle", "failed-measured-check")
def test_measures_but_fails(record_property):
    record_property("putnami_measurement", MEASUREMENT)
    assert False


@pytest.mark.putnami_observes("py/demo", "lifecycle", "skipped-measured-check")
@pytest.mark.skip(reason="deliberate")
def test_measures_but_skipped(record_property):
    record_property("putnami_measurement", MEASUREMENT)


@pytest.mark.putnami_observes("py/demo", "lifecycle", "teardown-skipped-measured-check")
def test_measures_but_teardown_skips(skip_in_teardown, record_property):
    record_property("putnami_measurement", MEASUREMENT)


@pytest.mark.putnami_observes("py/demo", "lifecycle", "corrected-measured-check")
def test_corrects_measurement(record_property):
    record_property("putnami_measurement", "malformed first measurement")
    record_property("putnami_measurement", CORRECTED_MEASUREMENT)


@pytest.mark.putnami_observes("py/demo", "lifecycle", "unrecorded-measured-check")
def test_forgets_to_measure():
    assert True
`

// skipUnlessPytestResolvable skips a test that needs the workspace's Python
// environment when uv cannot materialize it. The hosted native DAG runs with
// no package index (only Go and npm reads are brokered), so `uv run` cannot
// create the example library's environment there; that is an environment
// limit, not a plugin defect, and the message names it. Online hosts and hosts
// with a synced .venv proceed to the real pytest run.
func skipUnlessPytestResolvable(t *testing.T, repoRoot string) {
	t.Helper()
	cmd := exec.Command("uv",
		"run", "--package", "py_example_library", "--directory", repoRoot, "--extra", "dev",
		"python", "-c", "import pytest")
	cmd.Dir = repoRoot
	cmd.Env = MakeTestEnv(repoRoot, repoRoot, nil)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("uv cannot resolve the example library's pytest environment here (%v):\n%s", err, output)
	}
}

func TestSpecPluginRecordsRealPytestVerdicts(t *testing.T) {
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skipf("uv unavailable: %v", err)
	}
	repoRoot, err := filepath.Abs("../../../..")
	if err != nil {
		t.Fatal(err)
	}
	skipUnlessPytestResolvable(t, repoRoot)

	suiteDir := t.TempDir()
	suitePath := filepath.Join(suiteDir, "test_spec_plugin.py")
	if err := os.WriteFile(suitePath, []byte(specPluginSuite), 0o644); err != nil {
		t.Fatal(err)
	}

	pluginDir, cleanupPlugin, err := provisionSpecPlugin()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupPlugin()
	spec, cleanupFragments, err := provisionSpecVerification(pluginDir)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupFragments()

	cmdArgs := append([]string{
		"run",
		"--package", "py_example_library",
		"--directory", repoRoot,
		"--extra", "dev",
		"pytest",
		"-p", "no:cacheprovider",
	}, spec.specPytestArgs()...)
	cmdArgs = append(cmdArgs, suitePath)

	extra := map[string]string{}
	spec.decorateSpecEnv(extra)
	cmd := exec.Command("uv", cmdArgs...)
	cmd.Dir = suiteDir
	cmd.Env = MakeTestEnv(repoRoot, suiteDir, extra)
	output, runErr := cmd.CombinedOutput()

	// The deliberately failing test must still fail the run: the plugin never
	// alters pytest's own verdict.
	if runErr == nil {
		t.Fatalf("pytest reported success despite the failing case:\n%s", output)
	}

	entries, err := os.ReadDir(spec.fragmentsDir)
	if err != nil {
		t.Fatal(err)
	}
	byCheck := map[string]spectest.Fragment{}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(spec.fragmentsDir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var fragment spectest.Fragment
		if err := json.Unmarshal(data, &fragment); err != nil {
			t.Fatalf("fragment %s unparseable: %v", entry.Name(), err)
		}
		if _, seen := byCheck[fragment.Check]; seen {
			t.Fatalf("check %s produced two distinct fragments:\n%s", fragment.Check, output)
		}
		byCheck[fragment.Check] = fragment
	}

	checks := make([]string, 0, len(byCheck))
	for check := range byCheck {
		checks = append(checks, check)
	}
	sort.Strings(checks)
	// The malformed marker records nothing; the parametrized pair collapses
	// into one content-addressed fragment because both cases pass. A failed,
	// skipped, teardown-skipped, or unrecorded measurement can only be reported
	// by its absence; the corrected measurement keeps its final recording.
	want := []string{"corrected-measured-check", "failing-check", "measured-check", "param-check", "passing-check", "skipped-check"}
	if strings.Join(checks, ",") != strings.Join(want, ",") {
		t.Fatalf("checks = %v, want %v\npytest output:\n%s", checks, want, output)
	}
	for check, status := range map[string]string{
		"passing-check": "passed",
		"failing-check": "failed",
		"skipped-check": "skipped",
		"param-check":   "passed",
	} {
		if got := byCheck[check].Status; got != status {
			t.Errorf("%s status = %q, want %q", check, got, status)
		}
	}
	for _, check := range want {
		fragment := byCheck[check]
		if fragment.Feature != "py/demo" || fragment.Requirement != "lifecycle" {
			t.Errorf("%s binding = %+v", check, fragment)
		}
		if fragment.File != suitePath {
			t.Errorf("%s declaration file = %q, want %q", check, fragment.File, suitePath)
		}
		if !strings.HasPrefix(fragment.Symbol, "test_") || strings.Contains(fragment.Symbol, "[") {
			t.Errorf("%s symbol = %q, want the root test function name", check, fragment.Symbol)
		}
	}

	// The measured producer states a number and never a verdict: the wire
	// refuses an observation carrying both.
	measured := byCheck["measured-check"]
	if measured.Status != "" {
		t.Errorf("measured fragment carries verdict %q", measured.Status)
	}
	if measured.Measurement == nil {
		t.Fatalf("measured fragment has no aggregate: %+v", measured)
	}
	if *measured.Measurement != (spectest.FragmentMeasurement{
		Name: "demo.flush.duration", Aggregation: "p95", Value: 12.5, Unit: "ms",
	}) {
		t.Errorf("measurement = %+v", *measured.Measurement)
	}
	corrected := byCheck["corrected-measured-check"]
	if corrected.Status != "" || corrected.Measurement == nil || corrected.Measurement.Value != 24.5 {
		t.Errorf("corrected measurement = %+v, want the final recorded aggregate", corrected)
	}
	if _, exists := byCheck["teardown-skipped-measured-check"]; exists {
		t.Errorf("teardown-skipped measuring test published %+v", byCheck["teardown-skipped-measured-check"])
	}
	for _, check := range []string{"passing-check", "failing-check", "skipped-check", "param-check"} {
		if byCheck[check].Measurement != nil || byCheck[check].Window != nil {
			t.Errorf("%s carries measured fields: %+v", check, byCheck[check])
		}
	}
	// The window is the call phase's own span, in the same second-precision
	// RFC 3339 shape Go writes.
	if measured.Window == nil {
		t.Fatalf("measured fragment has no window: %+v", measured)
	}
	start, startErr := time.Parse(time.RFC3339, measured.Window.Start)
	end, endErr := time.Parse(time.RFC3339, measured.Window.End)
	if startErr != nil || endErr != nil {
		t.Fatalf("window %+v is not RFC 3339: %v / %v", *measured.Window, startErr, endErr)
	}
	if end.Before(start) {
		t.Errorf("window %+v ends before it starts", *measured.Window)
	}
	if !strings.HasSuffix(measured.Window.Start, "Z") || !strings.HasSuffix(measured.Window.End, "Z") {
		t.Errorf("window %+v is not the second-precision UTC shape Go writes", *measured.Window)
	}

	// The malformed marker must be surfaced as a pytest warning, not guessed.
	if !strings.Contains(string(output), "no spec observation was recorded") {
		t.Errorf("malformed marker drew no warning:\n%s", output)
	}
	// So must a measuring test that never recorded its aggregate.
	if !strings.Contains(string(output), "needs record_property") {
		t.Errorf("unrecorded measurement drew no warning:\n%s", output)
	}
}

// TestSpecPluginInertWithoutAdapter pins the inertness contract: the marker is
// plain metadata for pytest without the plugin, so a bare run (no -p, no
// PYTHONPATH entry, no fragment directory) executes every test exactly as
// before and writes nothing.
func TestSpecPluginInertWithoutAdapter(t *testing.T) {
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skipf("uv unavailable: %v", err)
	}
	repoRoot, err := filepath.Abs("../../../..")
	if err != nil {
		t.Fatal(err)
	}
	skipUnlessPytestResolvable(t, repoRoot)
	suiteDir := t.TempDir()
	suite := "import pytest\n\n\n@pytest.mark.putnami_proves(\"py/demo\", \"lifecycle\", \"passing-check\")\ndef test_passes():\n    assert True\n"
	suitePath := filepath.Join(suiteDir, "test_spec_inert.py")
	if err := os.WriteFile(suitePath, []byte(suite), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("uv",
		"run", "--package", "py_example_library", "--directory", repoRoot, "--extra", "dev",
		"pytest", "-p", "no:cacheprovider", suitePath)
	cmd.Dir = suiteDir
	cmd.Env = MakeTestEnv(repoRoot, suiteDir, nil)
	output, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("bare pytest failed: %v\n%s", runErr, output)
	}
	if !strings.Contains(string(output), "1 passed") {
		t.Fatalf("expected the bare run to pass the test:\n%s", output)
	}
}
