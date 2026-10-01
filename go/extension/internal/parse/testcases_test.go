package parse

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
)

// testJSONEvent renders one `go test -json` event as test2json prints it.
func testJSONEvent(t *testing.T, action, pkg, test, output string, elapsed float64) string {
	t.Helper()
	data, err := json.Marshal(TestEvent{Action: action, Package: pkg, Test: test, Output: output, Elapsed: elapsed})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// writeTestModule lays out a module example.com/mod at <workspace>/mod with a
// test file in its root package and one in its pkg package.
func writeTestModule(t *testing.T) (workspace, moduleRoot string) {
	t.Helper()
	workspace = t.TempDir()
	moduleRoot = filepath.Join(workspace, "mod")
	for path, content := range map[string]string{
		"go.mod":               "module example.com/mod\n\ngo 1.25.7\n",
		"root_test.go":         "package mod\n",
		"pkg/fail_test.go":     "package pkg\n",
		"pkg/skip_test.go":     "package pkg\n",
		"pkg/nested/a_test.go": "package nested\n",
	} {
		full := filepath.Join(moduleRoot, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return workspace, moduleRoot
}

func TestTestCaseListReportsEveryTerminalRun(t *testing.T) {
	workspace, moduleRoot := writeTestModule(t)
	const pkg = "example.com/mod/pkg"
	output := strings.Join([]string{
		testJSONEvent(t, "start", pkg, "", "", 0),
		// A failing test that logs before it fails.
		testJSONEvent(t, "run", pkg, "TestFail", "", 0),
		testJSONEvent(t, "output", pkg, "TestFail", "=== RUN   TestFail\n", 0),
		testJSONEvent(t, "output", pkg, "TestFail", "    fail_test.go:12: got 1, want 2\n", 0),
		testJSONEvent(t, "output", pkg, "TestFail", "        extra detail\n", 0),
		testJSONEvent(t, "output", pkg, "TestFail", "--- FAIL: TestFail (0.25s)\n", 0),
		testJSONEvent(t, "fail", pkg, "TestFail", "", 0.25),
		// A skipped test with its reason.
		testJSONEvent(t, "run", pkg, "TestSkip", "", 0),
		testJSONEvent(t, "output", pkg, "TestSkip", "=== RUN   TestSkip\n", 0),
		testJSONEvent(t, "output", pkg, "TestSkip", "    skip_test.go:7: needs a database\n", 0),
		testJSONEvent(t, "output", pkg, "TestSkip", "--- SKIP: TestSkip (0.00s)\n", 0),
		testJSONEvent(t, "skip", pkg, "TestSkip", "", 0),
		// A passing test with a passing subtest: each is a case of its own.
		testJSONEvent(t, "run", pkg, "TestParent", "", 0),
		testJSONEvent(t, "output", pkg, "TestParent", "=== RUN   TestParent\n", 0),
		testJSONEvent(t, "run", pkg, "TestParent/child", "", 0),
		testJSONEvent(t, "output", pkg, "TestParent/child", "=== RUN   TestParent/child\n", 0),
		testJSONEvent(t, "output", pkg, "TestParent/child", "    fail_test.go:30: a log line\n", 0),
		testJSONEvent(t, "output", pkg, "TestParent/child", "    --- PASS: TestParent/child (1.50s)\n", 0),
		testJSONEvent(t, "pass", pkg, "TestParent/child", "", 1.5),
		testJSONEvent(t, "output", pkg, "TestParent", "--- PASS: TestParent (1.50s)\n", 0),
		testJSONEvent(t, "pass", pkg, "TestParent", "", 1.5),
		// A test that never reached a result is not a case.
		testJSONEvent(t, "run", pkg, "TestHung", "", 0),
		testJSONEvent(t, "output", pkg, "TestHung", "=== RUN   TestHung\n", 0),
		// Package-level output and a package-level fail are not cases.
		testJSONEvent(t, "output", pkg, "", "FAIL\n", 0),
		testJSONEvent(t, "fail", pkg, "", "", 2),
		"not a JSON event",
	}, "\n") + "\n"

	got := TestCaseList(output, ModuleTestFileLocator(workspace, moduleRoot, "example.com/mod"))
	want := []protocolcli.TestCase{
		{
			Name:       "TestFail",
			Suite:      pkg,
			Status:     protocolcli.TestCaseStatusFailed,
			DurationMs: 250,
			Output:     "fail_test.go:12: got 1, want 2\n    extra detail",
			File:       "mod/pkg/fail_test.go",
			Line:       12,
		},
		{
			Name:   "TestSkip",
			Suite:  pkg,
			Status: protocolcli.TestCaseStatusSkipped,
			Output: "skip_test.go:7: needs a database",
			File:   "mod/pkg/skip_test.go",
			Line:   7,
		},
		{Name: "TestParent", Suite: pkg, Status: protocolcli.TestCaseStatusPassed, DurationMs: 1500},
		{Name: "TestParent/child", Suite: pkg, Status: protocolcli.TestCaseStatusPassed, DurationMs: 1500},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("TestCaseList =\n  %#v\nwant\n  %#v", got, want)
	}
}

func TestTestCaseListFailingSubtestDedentsItsOutput(t *testing.T) {
	workspace, moduleRoot := writeTestModule(t)
	output := strings.Join([]string{
		testJSONEvent(t, "run", "example.com/mod", "TestTable", "", 0),
		testJSONEvent(t, "run", "example.com/mod", "TestTable/empty", "", 0),
		testJSONEvent(t, "output", "example.com/mod", "TestTable/empty", "=== RUN   TestTable/empty\n", 0),
		testJSONEvent(t, "output", "example.com/mod", "TestTable/empty", "        root_test.go:9: want error\n", 0),
		testJSONEvent(t, "output", "example.com/mod", "TestTable/empty", "            got nil\n", 0),
		testJSONEvent(t, "output", "example.com/mod", "TestTable/empty", "    --- FAIL: TestTable/empty (0.00s)\n", 0),
		testJSONEvent(t, "fail", "example.com/mod", "TestTable/empty", "", 0.0004),
		testJSONEvent(t, "output", "example.com/mod", "TestTable", "--- FAIL: TestTable (0.00s)\n", 0),
		testJSONEvent(t, "fail", "example.com/mod", "TestTable", "", 0.001),
	}, "\n")

	got := TestCaseList(output, ModuleTestFileLocator(workspace, moduleRoot, "example.com/mod"))
	want := []protocolcli.TestCase{
		{Name: "TestTable", Suite: "example.com/mod", Status: protocolcli.TestCaseStatusFailed, DurationMs: 1},
		{
			Name:   "TestTable/empty",
			Suite:  "example.com/mod",
			Status: protocolcli.TestCaseStatusFailed,
			Output: "root_test.go:9: want error\n    got nil",
			File:   "mod/root_test.go",
			Line:   9,
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("TestCaseList =\n  %#v\nwant\n  %#v", got, want)
	}
}

func TestTestCaseListReportsEachRunOfARepeatedTest(t *testing.T) {
	output := strings.Join([]string{
		testJSONEvent(t, "run", "example.com/mod", "TestFlaky", "", 0),
		testJSONEvent(t, "pass", "example.com/mod", "TestFlaky", "", 0.002),
		testJSONEvent(t, "run", "example.com/mod", "TestFlaky", "", 0),
		testJSONEvent(t, "output", "example.com/mod", "TestFlaky", "    root_test.go:4: flaked\n", 0),
		testJSONEvent(t, "fail", "example.com/mod", "TestFlaky", "", 0.003),
	}, "\n")

	got := TestCaseList(output, nil)
	want := []protocolcli.TestCase{
		{Name: "TestFlaky", Suite: "example.com/mod", Status: protocolcli.TestCaseStatusPassed, DurationMs: 2},
		{Name: "TestFlaky", Suite: "example.com/mod", Status: protocolcli.TestCaseStatusFailed, DurationMs: 3, Output: "root_test.go:4: flaked"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("TestCaseList =\n  %#v\nwant\n  %#v", got, want)
	}
}

func TestTestCaseListIgnoresOutputThatIsNotJSON(t *testing.T) {
	output := "=== RUN   TestA\n--- FAIL: TestA (0.00s)\nFAIL\nexit status 1\n"
	if got := TestCaseList(output, nil); len(got) != 0 {
		t.Fatalf("TestCaseList = %#v, want no case from plain output", got)
	}
}

func TestTestCaseListOmitsALocationItCannotResolve(t *testing.T) {
	workspace, moduleRoot := writeTestModule(t)
	locate := ModuleTestFileLocator(workspace, moduleRoot, "example.com/mod")
	for _, test := range []struct {
		name string
		pkg  string
		line string
	}{
		{name: "file missing from the package directory", pkg: "example.com/mod/pkg", line: "    root_test.go:3: boom\n"},
		{name: "package outside the module", pkg: "example.com/other", line: "    fail_test.go:3: boom\n"},
		{name: "package path that only shares a prefix", pkg: "example.com/modern/pkg", line: "    fail_test.go:3: boom\n"},
		{name: "first location unresolvable, later one not used", pkg: "example.com/mod/pkg", line: "    gone_test.go:3: setup\n    fail_test.go:4: boom\n"},
		{name: "location without indentation", pkg: "example.com/mod/pkg", line: "fail_test.go:3: printed by the test\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			output := strings.Join([]string{
				testJSONEvent(t, "output", test.pkg, "TestA", test.line, 0),
				testJSONEvent(t, "fail", test.pkg, "TestA", "", 0),
			}, "\n")
			got := TestCaseList(output, locate)
			if len(got) != 1 {
				t.Fatalf("TestCaseList = %#v, want one case", got)
			}
			if got[0].File != "" || got[0].Line != 0 {
				t.Fatalf("case location = %q:%d, want none", got[0].File, got[0].Line)
			}
			if got[0].Output == "" {
				t.Fatalf("case output is empty, want the test's lines")
			}
		})
	}
}

func TestModuleTestFileLocatorResolvesOnlyModuleFiles(t *testing.T) {
	workspace, moduleRoot := writeTestModule(t)
	locate := ModuleTestFileLocator(workspace, moduleRoot, "example.com/mod")
	for _, test := range []struct {
		pkg, file, want string
	}{
		{pkg: "example.com/mod", file: "root_test.go", want: "mod/root_test.go"},
		{pkg: "example.com/mod/pkg/nested", file: "a_test.go", want: "mod/pkg/nested/a_test.go"},
		{pkg: "example.com/mod/pkg", file: "nested/a_test.go"},
		{pkg: "example.com/mod/pkg", file: "pkg"},
		{pkg: "example.com/mod", file: "pkg"},
		{pkg: "example.com/mod", file: ""},
	} {
		if got := locate(test.pkg, test.file); got != test.want {
			t.Errorf("locate(%q, %q) = %q, want %q", test.pkg, test.file, got, test.want)
		}
	}
	if got := ModuleTestFileLocator(workspace, moduleRoot, "")("example.com/mod", "root_test.go"); got != "" {
		t.Errorf("locator without a module path resolved %q, want nothing", got)
	}
	outside := ModuleTestFileLocator(filepath.Join(workspace, "elsewhere"), moduleRoot, "example.com/mod")
	if got := outside("example.com/mod", "root_test.go"); got != "" {
		t.Errorf("locator resolved %q outside the workspace, want nothing", got)
	}
}

func TestDurationMsClampsToTheInt64Range(t *testing.T) {
	for _, test := range []struct {
		seconds float64
		want    int64
	}{
		{seconds: 0.25, want: 250},
		{seconds: 0.0004, want: 0},
		{seconds: 0, want: 0},
		{seconds: -1, want: 0},
		{seconds: math.NaN(), want: 0},
		{seconds: math.Inf(1), want: math.MaxInt64},
		{seconds: 1e300, want: math.MaxInt64},
	} {
		if got := durationMs(test.seconds); got != test.want {
			t.Errorf("durationMs(%v) = %d, want %d", test.seconds, got, test.want)
		}
	}
}

// rootModuleLocator lays out module modulePath at the root of a fresh
// workspace with the given slash-separated files, and returns its locator.
func rootModuleLocator(t *testing.T, modulePath string, files ...string) TestFileLocator {
	t.Helper()
	root := t.TempDir()
	for _, file := range files {
		full := filepath.Join(root, filepath.FromSlash(file))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("package x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return ModuleTestFileLocator(root, root, modulePath)
}

func TestTestDiagnosticListResolvesTheFailingPackagesTestFile(t *testing.T) {
	workspace, moduleRoot := writeTestModule(t)
	output := strings.Join([]string{
		testJSONEvent(t, "output", "example.com/mod/pkg", "TestSub", "    fail_test.go:12: got 1, want 2\n", 0),
		testJSONEvent(t, "fail", "example.com/mod/pkg", "TestSub", "", 0),
		testJSONEvent(t, "output", "example.com/mod", "TestRoot", "    root_test.go:3: boom\n", 0),
		testJSONEvent(t, "fail", "example.com/mod", "TestRoot", "", 0),
	}, "\n")

	got := TestDiagnosticList(output, ModuleTestFileLocator(workspace, moduleRoot, "example.com/mod"))
	want := []ToolDiagnostic{
		// The sub-package file lives in the package directory, not the module root.
		{Severity: "error", Description: "got 1, want 2", File: "mod/pkg/fail_test.go", Line: 12},
		// A root-package file keeps the path it always had.
		{Severity: "error", Description: "boom", File: DiagPath(workspace, moduleRoot, "root_test.go"), Line: 3},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("TestDiagnosticList =\n  %+v\nwant\n  %+v", got, want)
	}
	if want[1].File != "mod/root_test.go" {
		t.Fatalf("root-package path = %q, want mod/root_test.go", want[1].File)
	}
}

func TestTestDiagnosticListOmitsALocationItCannotResolve(t *testing.T) {
	workspace, moduleRoot := writeTestModule(t)
	output := strings.Join([]string{
		testJSONEvent(t, "output", "example.com/mod/pkg", "TestSub", "    gone_test.go:5: boom\n", 0),
		testJSONEvent(t, "fail", "example.com/mod/pkg", "TestSub", "", 0),
	}, "\n")
	locate := ModuleTestFileLocator(workspace, moduleRoot, "example.com/mod")

	got := TestDiagnosticList(output, locate)
	want := []ToolDiagnostic{{Severity: "error", Description: "boom"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("TestDiagnosticList = %+v, want the message without a location", got)
	}
	// With no located diagnostic, the failure list carries the raw output.
	failures := TestFailureDiagnosticList(output, locate)
	if len(failures) != 2 || failures[1].Category != "GO_TEST_FAILURE" ||
		!strings.Contains(failures[1].Description, "gone_test.go:5: boom") {
		t.Fatalf("TestFailureDiagnosticList = %+v, want the message and the raw output", failures)
	}
}
