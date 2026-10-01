package jobs

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
)

// The per-case plugin is exercised through a REAL pytest run, resolved through
// the repository's own uv workspace, like the spec plugin: a generated suite
// covers every outcome pytest distinguishes, and the test reads the plugin's
// file back through the adapter's own reader.

const testCasePluginSuite = `import time

import pytest


def test_passes():
    assert True


def test_fails():
    print("printed before failing")
    assert 1 == 2


@pytest.mark.skip(reason="deliberate skip")
def test_skip_marker():
    assert True


def test_skip_in_body():
    pytest.skip("skipped in the body")


@pytest.mark.xfail(reason="known bug")
def test_xfail():
    assert False


@pytest.mark.xfail(reason="fixed now", strict=True)
def test_strict_xpass():
    assert True


@pytest.mark.parametrize("value", [1, 2])
def test_param(value):
    assert value > 0


class TestGroup:
    def test_method(self):
        assert True


@pytest.fixture
def broken_setup():
    raise RuntimeError("setup broke")


def test_setup_error(broken_setup):
    assert True


@pytest.fixture
def broken_teardown():
    yield
    raise RuntimeError("teardown broke")


def test_teardown_error(broken_teardown):
    assert True


def test_sleeps():
    time.sleep(0.05)
`

// testCaseInheritedSuite collects a test its class inherits from
// testCaseBaseModule, which pytest does not collect: the case's suite is the
// collecting module and its file the declaring one.
const (
	testCaseInheritedSuite = `from base_cases import BaseCases


class TestInherited(BaseCases):
    pass
`
	testCaseBaseModule = `class BaseCases:
    def test_shared(self):
        assert True
`
)

// recorderTrace is how pytest's --trace-config names the plugin's recorder
// once it is registered.
const recorderTrace = "putnami_testcases._Recorder object"

// declarationLine returns the 1-based line pytest reports for a test function:
// the line of its first decorator, or of its def when it has none.
func declarationLine(t *testing.T, source, function string) int {
	t.Helper()
	lines := strings.Split(source, "\n")
	for index, line := range lines {
		if !strings.HasPrefix(strings.TrimSpace(line), "def "+function+"(") {
			continue
		}
		first := index
		for first > 0 && strings.HasPrefix(strings.TrimSpace(lines[first-1]), "@") {
			first--
		}
		return first + 1
	}
	t.Fatalf("function %s not in the suite", function)
	return 0
}

func TestTestCasePluginRecordsRealPytestCases(t *testing.T) {
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skipf("uv unavailable: %v", err)
	}
	repoRoot, err := filepath.Abs("../../../..")
	if err != nil {
		t.Fatal(err)
	}
	skipUnlessPytestResolvable(t, repoRoot)

	wsRoot := t.TempDir()
	suitePath := filepath.Join(wsRoot, "pkg", "tests", "test_cases.py")
	if err := os.MkdirAll(filepath.Dir(suitePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(suitePath, []byte(testCasePluginSuite), 0o644); err != nil {
		t.Fatal(err)
	}
	inheritedPath := filepath.Join(wsRoot, "pkg", "tests", "test_inherits.py")
	if err := os.WriteFile(inheritedPath, []byte(testCaseInheritedSuite), 0o644); err != nil {
		t.Fatal(err)
	}
	basePath := filepath.Join(wsRoot, "pkg", "tests", "base_cases.py")
	if err := os.WriteFile(basePath, []byte(testCaseBaseModule), 0o644); err != nil {
		t.Fatal(err)
	}

	sink, err := provisionTestCases()
	if err != nil {
		t.Fatal(err)
	}
	defer sink.remove()

	cmdArgs := append([]string{
		"run",
		"--package", "py_example_library",
		"--directory", repoRoot,
		"--extra", "dev",
		"pytest",
		"-p", "no:cacheprovider",
		"--trace-config",
	}, sink.pytestArgs()...)
	cmdArgs = append(cmdArgs, suitePath, inheritedPath)
	extra := map[string]string{}
	sink.decorateEnv(extra)
	cmd := exec.Command("uv", cmdArgs...)
	cmd.Dir = wsRoot
	cmd.Env = MakeTestEnv(repoRoot, wsRoot, extra)
	output, runErr := cmd.CombinedOutput()

	// The failing cases still fail the run: the plugin never alters pytest's
	// own verdict.
	if runErr == nil {
		t.Fatalf("pytest reported success despite the failing cases:\n%s", output)
	}
	if !strings.Contains(string(output), recorderTrace) {
		t.Fatalf("the armed plugin registered no recorder:\n%s", output)
	}

	cases, dropped := sink.read(wsRoot, protocolcli.TestCaseMaxBytesPerTask)
	if dropped != 0 {
		t.Errorf("dropped = %d, want 0\npytest output:\n%s", dropped, output)
	}
	byName := casesByName(cases)
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	want := []string{
		"TestGroup::test_method", "TestInherited::test_shared", "test_fails", "test_param[1]", "test_param[2]", "test_passes",
		"test_setup_error", "test_skip_in_body", "test_skip_marker", "test_sleeps",
		"test_strict_xpass", "test_teardown_error", "test_xfail",
	}
	if !slices.Equal(names, want) || len(cases) != len(want) {
		t.Fatalf("cases = %v (%d records), want each of %v once\npytest output:\n%s", names, len(cases), want, output)
	}

	for name, status := range map[string]string{
		"test_passes":                protocolcli.TestCaseStatusPassed,
		"test_fails":                 protocolcli.TestCaseStatusFailed,
		"test_skip_marker":           protocolcli.TestCaseStatusSkipped,
		"test_skip_in_body":          protocolcli.TestCaseStatusSkipped,
		"test_xfail":                 protocolcli.TestCaseStatusSkipped,
		"test_strict_xpass":          protocolcli.TestCaseStatusFailed,
		"test_param[1]":              protocolcli.TestCaseStatusPassed,
		"test_param[2]":              protocolcli.TestCaseStatusPassed,
		"TestGroup::test_method":     protocolcli.TestCaseStatusPassed,
		"TestInherited::test_shared": protocolcli.TestCaseStatusPassed,
		"test_setup_error":           protocolcli.TestCaseStatusFailed,
		"test_teardown_error":        protocolcli.TestCaseStatusFailed,
		"test_sleeps":                protocolcli.TestCaseStatusPassed,
	} {
		if got := byName[name].Status; got != status {
			t.Errorf("%s status = %q, want %q", name, got, status)
		}
	}

	for name, fragments := range map[string][]string{
		"test_fails":          {"assert 1 == 2", "Captured stdout call", "printed before failing"},
		"test_skip_marker":    {"deliberate skip"},
		"test_skip_in_body":   {"skipped in the body"},
		"test_xfail":          {"XFAIL: known bug"},
		"test_strict_xpass":   {"XPASS(strict)", "fixed now"},
		"test_setup_error":    {"setup broke"},
		"test_teardown_error": {"teardown broke"},
	} {
		for _, fragment := range fragments {
			if !strings.Contains(byName[name].Output, fragment) {
				t.Errorf("%s output lacks %q:\n%s", name, fragment, byName[name].Output)
			}
		}
	}
	if strings.Contains(byName["test_fails"].Output, "\x1b[") {
		t.Errorf("test_fails output carries terminal escapes:\n%q", byName["test_fails"].Output)
	}

	for _, c := range cases {
		if c.Name == "TestInherited::test_shared" {
			continue
		}
		if c.Suite != "pkg/tests/test_cases.py" || c.File != "pkg/tests/test_cases.py" {
			t.Errorf("%s location = (%q, %q), want the workspace-relative suite file", c.Name, c.Suite, c.File)
		}
		if c.Status == protocolcli.TestCaseStatusPassed && c.Output != "" {
			t.Errorf("%s passed but carries output %q", c.Name, c.Output)
		}
	}
	for name, function := range map[string]string{
		"test_passes":            "test_passes",
		"test_skip_marker":       "test_skip_marker",
		"test_param[2]":          "test_param",
		"TestGroup::test_method": "test_method",
		"test_teardown_error":    "test_teardown_error",
	} {
		if got, want := byName[name].Line, declarationLine(t, testCasePluginSuite, function); got != want {
			t.Errorf("%s line = %d, want %d", name, got, want)
		}
	}
	// An inherited test belongs to the module that collected it and is
	// declared in its base class's module.
	inherited := byName["TestInherited::test_shared"]
	if inherited.Suite != "pkg/tests/test_inherits.py" || inherited.File != "pkg/tests/base_cases.py" || inherited.Line != 2 {
		t.Errorf("inherited case location = (suite %q, file %q, line %d), want (pkg/tests/test_inherits.py, pkg/tests/base_cases.py, 2)",
			inherited.Suite, inherited.File, inherited.Line)
	}
	// The duration spans every phase, in milliseconds.
	if got := byName["test_sleeps"].DurationMs; got < 50 {
		t.Errorf("test_sleeps durationMs = %d, want at least the 50 ms it slept", got)
	}
}

// TestTestCasePluginInertWithoutCasesFile pins the inertness contract: loaded
// without PUTNAMI_TEST_CASES_FILE, the plugin registers nothing and pytest
// runs the suite exactly as before.
func TestTestCasePluginInertWithoutCasesFile(t *testing.T) {
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skipf("uv unavailable: %v", err)
	}
	repoRoot, err := filepath.Abs("../../../..")
	if err != nil {
		t.Fatal(err)
	}
	skipUnlessPytestResolvable(t, repoRoot)

	suiteDir := t.TempDir()
	suitePath := filepath.Join(suiteDir, "test_inert.py")
	if err := os.WriteFile(suitePath, []byte("def test_passes():\n    assert True\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sink, err := provisionTestCases()
	if err != nil {
		t.Fatal(err)
	}
	defer sink.remove()

	cmd := exec.Command("uv",
		"run", "--package", "py_example_library", "--directory", repoRoot, "--extra", "dev",
		"pytest", "-p", "no:cacheprovider", "--trace-config", "-p", testCasePluginModule, suitePath)
	cmd.Dir = suiteDir
	cmd.Env = MakeTestEnv(repoRoot, suiteDir, map[string]string{
		"PYTHONPATH":     sink.dir.Path(),
		testCasesFileEnv: "",
	})
	output, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("pytest with the unarmed plugin failed: %v\n%s", runErr, output)
	}
	if !strings.Contains(string(output), "1 passed") {
		t.Fatalf("expected the run to pass the test:\n%s", output)
	}
	// --trace-config lists every registered plugin: the module is loaded, and
	// its recorder is not.
	if !strings.Contains(string(output), "<module '"+testCasePluginModule+"'") {
		t.Fatalf("pytest did not load the plugin module:\n%s", output)
	}
	if strings.Contains(string(output), recorderTrace) {
		t.Fatalf("the plugin registered its recorder without a cases file:\n%s", output)
	}
}
