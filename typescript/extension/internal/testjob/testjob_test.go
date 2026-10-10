package testjob

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/exec"
)

// ---- matchTestFile ----

func TestMatchTestFile(t *testing.T) {
	tests := []struct {
		path     string
		expected bool
	}{
		{"test/foo.test.ts", true},
		{"test/foo.spec.ts", true},
		{"test/foo.test.tsx", true},
		{"test/foo.spec.tsx", true},
		{"test/foo.test.js", true},
		{"test/foo.spec.js", true},
		{"test/foo.test.jsx", true},
		{"test/foo.spec.jsx", true},
		{"src/foo.ts", false},
		{"src/foo.js", false},
		{"src/foo.tsx", false},
		{"test/helper.ts", false},
		{"test/setup.ts", false},
		{"test/foo.test.ts.bak", false},
	}

	for _, tt := range tests {
		got := matchTestFile(tt.path)
		if got != tt.expected {
			t.Errorf("matchTestFile(%q) = %v, want %v", tt.path, got, tt.expected)
		}
	}
}

// ---- HasTests ----

func TestHasTests_Empty(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src"), 0755)
	os.WriteFile(filepath.Join(dir, "src", "index.ts"), []byte(""), 0644)

	has, err := HasTests(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if has {
		t.Error("expected HasTests=false when no test files exist")
	}
}

func TestHasTests_FindsTestFiles(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "test"), 0755)
	os.WriteFile(filepath.Join(dir, "test", "foo.test.ts"), []byte(""), 0644)
	os.WriteFile(filepath.Join(dir, "test", "bar.spec.ts"), []byte(""), 0644)

	has, err := HasTests(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !has {
		t.Error("expected HasTests=true when test files exist")
	}
}

func TestHasTests_SkipsNodeModules(t *testing.T) {
	dir := t.TempDir()
	// A test file only inside node_modules must not count.
	os.MkdirAll(filepath.Join(dir, "node_modules", "pkg", "test"), 0755)
	os.WriteFile(filepath.Join(dir, "node_modules", "pkg", "test", "pkg.test.ts"), []byte(""), 0644)
	os.MkdirAll(filepath.Join(dir, "src"), 0755)
	os.WriteFile(filepath.Join(dir, "src", "index.ts"), []byte(""), 0644)

	has, err := HasTests(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if has {
		t.Error("expected HasTests=false when only node_modules contains tests")
	}
}

func TestHasTests_SkipsDotGenAndDist(t *testing.T) {
	dir := t.TempDir()
	// Test files only inside .gen and dist must not count.
	os.MkdirAll(filepath.Join(dir, ".gen", "src"), 0755)
	os.MkdirAll(filepath.Join(dir, "dist", "test"), 0755)
	os.WriteFile(filepath.Join(dir, ".gen", "src", "gen.test.ts"), []byte(""), 0644)
	os.WriteFile(filepath.Join(dir, "dist", "test", "dist.test.js"), []byte(""), 0644)

	has, err := HasTests(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if has {
		t.Error("expected HasTests=false when only .gen and dist contain tests")
	}
}

// ---- ExtractAssertionDetails ----

func TestExtractAssertionDetails_NoFailures(t *testing.T) {
	logs := `
✓ passes [10ms]
✓ also passes [5ms]
`
	result := ExtractAssertionDetails(logs)
	if len(result) != 0 {
		t.Errorf("expected 0 details, got %d", len(result))
	}
}

func TestExtractAssertionDetails_WithFailure(t *testing.T) {
	logs := `
src/foo.test.ts:
error: Expected 1 to equal 2
Expected: 2
Received: 1
✗ my test name [50ms]
`
	result := ExtractAssertionDetails(logs)
	// Should find details for 'my test name'
	if len(result) == 0 {
		t.Error("expected at least one detail extracted")
	}
}

func TestExtractAssertionDetails_ANSIStripped(t *testing.T) {
	// ANSI color codes should be stripped
	logs := "\x1b[31m✗\x1b[0m test with ansi [10ms]\n"
	result := ExtractAssertionDetails(logs)
	// The ✗ prefix is present after stripping — no preceding detail lines
	// so result may be empty but should not panic
	_ = result
}

func TestExtractAssertionDetails_WithNestedTestName(t *testing.T) {
	logs := `
error: Expected "foo" to equal "bar"
Expected: "bar"
Received: "foo"
✗ suite > nested > my test [10ms]
`
	result := ExtractAssertionDetails(logs)
	if _, ok := result["my test"]; !ok {
		t.Errorf("expected 'my test' in results, got keys: %v", keys(result))
	}
}

func TestExtractAssertionDetails_MultipleFailures(t *testing.T) {
	logs := `
error: Expected 1 to equal 2
Expected: 2
Received: 1
✗ first test [10ms]

error: Expected true to be false
Expected: false
Received: true
✗ second test [20ms]
`
	result := ExtractAssertionDetails(logs)
	if len(result) != 2 {
		t.Errorf("expected 2 details, got %d: %v", len(result), result)
	}
}

// ---- ExtractUnhandledErrors ----

func TestExtractUnhandledErrors_None(t *testing.T) {
	logs := "✓ test passes [10ms]\n"
	errors := ExtractUnhandledErrors(logs)
	if len(errors) != 0 {
		t.Errorf("expected 0 errors, got %d", len(errors))
	}
}

func TestExtractUnhandledErrors_Single(t *testing.T) {
	logs := `
# Unhandled error between tests
TypeError: Cannot read properties of undefined
at src/module.ts:42:5
-------------------------------
`
	errors := ExtractUnhandledErrors(logs)
	if len(errors) != 1 {
		t.Fatalf("expected 1 error, got %d", len(errors))
	}
	e := errors[0]
	if e.Type != "TypeError" {
		t.Errorf("expected type 'TypeError', got %q", e.Type)
	}
	if e.Message != "TypeError: Cannot read properties of undefined" {
		t.Errorf("unexpected message: %q", e.Message)
	}
	if e.File != "src/module.ts" {
		t.Errorf("expected file 'src/module.ts', got %q", e.File)
	}
	if e.Line != 42 {
		t.Errorf("expected line 42, got %d", e.Line)
	}
}

func TestExtractUnhandledErrors_Multiple(t *testing.T) {
	logs := `
# Unhandled error between tests
ReferenceError: foo is not defined
at src/a.ts:10:1
-------------------------------
some other output
# Unhandled error between tests
SyntaxError: Unexpected token
at src/b.ts:5:3
-------------------------------
`
	errors := ExtractUnhandledErrors(logs)
	if len(errors) != 2 {
		t.Fatalf("expected 2 errors, got %d", len(errors))
	}
	if errors[0].Type != "ReferenceError" {
		t.Errorf("expected 'ReferenceError', got %q", errors[0].Type)
	}
	if errors[1].Type != "SyntaxError" {
		t.Errorf("expected 'SyntaxError', got %q", errors[1].Type)
	}
}

func TestExtractUnhandledErrors_NoErrorType(t *testing.T) {
	// Block present but no matching error pattern — should be skipped
	logs := `
# Unhandled error between tests
some message without type
-------------------------------
`
	errors := ExtractUnhandledErrors(logs)
	if len(errors) != 0 {
		t.Errorf("expected 0 errors when no pattern match, got %d", len(errors))
	}
}

func TestExtractUnhandledErrors_NoLocation(t *testing.T) {
	logs := `
# Unhandled error between tests
TypeError: Something went wrong
-------------------------------
`
	errors := ExtractUnhandledErrors(logs)
	if len(errors) != 1 {
		t.Fatalf("expected 1 error, got %d", len(errors))
	}
	if errors[0].File != "" {
		t.Errorf("expected empty file, got %q", errors[0].File)
	}
	if errors[0].Line != 0 {
		t.Errorf("expected line 0, got %d", errors[0].Line)
	}
}

func TestExtractUnhandledErrors_WithANSI(t *testing.T) {
	logs := "\x1b[31m# Unhandled error between tests\x1b[0m\n\x1b[33mTypeError\x1b[0m: oops\nat src/x.ts:1:1\n-------------------------------\n"
	errors := ExtractUnhandledErrors(logs)
	if len(errors) != 1 {
		t.Fatalf("expected 1 error with ANSI codes, got %d", len(errors))
	}
	if errors[0].Type != "TypeError" {
		t.Errorf("expected TypeError, got %q", errors[0].Type)
	}
}

// ---- ParseResults ----

func TestParseResults_WithJUnit(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "structured-reports", "test-results-parse-into-a-structured-report")
	dir := t.TempDir()

	// Write JUnit XML — tests/failures/assertions are on the <testsuites> root
	junitXML := `<?xml version="1.0" encoding="UTF-8"?>
<testsuites tests="2" failures="0" assertions="2" time="0.5">
  <testsuite name="test/foo.test.ts" tests="2" failures="0" errors="0" time="0.5">
    <testcase name="should pass" classname="test/foo.test.ts" time="0.2"/>
    <testcase name="also passes" classname="test/foo.test.ts" time="0.3"/>
  </testsuite>
</testsuites>`
	os.WriteFile(filepath.Join(dir, "results.junit.xml"), []byte(junitXML), 0644)

	testSummary, covSummary, files, err := ParseResults(dir, "", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if testSummary == nil {
		t.Fatal("expected non-nil test summary")
	}
	if testSummary.Total != 2 {
		t.Errorf("expected 2 total tests, got %d", testSummary.Total)
	}
	if testSummary.Passed != 2 {
		t.Errorf("expected 2 passed, got %d", testSummary.Passed)
	}
	if covSummary != nil {
		t.Error("expected nil coverage when coverage=false")
	}
	if files != nil {
		t.Error("expected nil files when coverage=false")
	}
}

func TestParseResults_WithCoverage(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "structured-reports", "coverage-parses-into-a-per-project-report")
	dir := t.TempDir()
	projectDir := t.TempDir()

	// Write JUnit
	junitXML := `<?xml version="1.0" encoding="UTF-8"?>
<testsuites>
  <testsuite name="tests" tests="1" failures="0">
    <testcase name="pass" classname="tests"/>
  </testsuite>
</testsuites>`
	os.WriteFile(filepath.Join(dir, "results.junit.xml"), []byte(junitXML), 0644)

	// Write LCOV
	os.MkdirAll(filepath.Join(projectDir, "src"), 0755)
	os.WriteFile(filepath.Join(projectDir, "src", "index.ts"), []byte("line1\nline2\nline3\n"), 0644)

	lcov := "SF:src/index.ts\nDA:1,1\nDA:2,1\nDA:3,0\nend_of_record\n"
	os.WriteFile(filepath.Join(dir, "lcov.info"), []byte(lcov), 0644)

	testSummary, covSummary, files, err := ParseResults(dir, projectDir, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if testSummary == nil {
		t.Fatal("expected non-nil test summary")
	}
	if covSummary == nil {
		t.Fatal("expected non-nil coverage summary")
	}
	if files == nil {
		t.Fatal("expected non-nil files")
	}
}

func TestParseResults_NoLCOVFile(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "structured-reports", "a-missing-coverage-file-yields-an-empty-result")
	dir := t.TempDir()

	junitXML := `<?xml version="1.0" encoding="UTF-8"?>
<testsuites>
  <testsuite name="tests" tests="1" failures="0">
    <testcase name="pass" classname="tests"/>
  </testsuite>
</testsuites>`
	os.WriteFile(filepath.Join(dir, "results.junit.xml"), []byte(junitXML), 0644)
	// No lcov.info file

	testSummary, covSummary, _, err := ParseResults(dir, "", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if testSummary == nil {
		t.Fatal("expected non-nil test summary")
	}
	if covSummary != nil {
		t.Error("expected nil coverage when lcov.info is missing")
	}
}

func TestParseResults_MissingJUnitFile(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "structured-reports", "a-missing-test-report-yields-an-empty-result")
	dir := t.TempDir()

	// ParseJUnitFile returns empty summary for non-existent file (not an error)
	testSummary, _, _, err := ParseResults(dir, "", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if testSummary == nil {
		t.Fatal("expected non-nil test summary")
	}
	if testSummary.Total != 0 {
		t.Errorf("expected 0 total tests for missing file, got %d", testSummary.Total)
	}
}

// ---- CleanCoverageShards ----

func TestCleanCoverageShards_RemovesShardsKeepsReports(t *testing.T) {
	dir := t.TempDir()

	// Simulate the output dir after a coverage run: the merged report and junit
	// alongside the per-worker .tmp shards bun's lcov reporter leaves behind.
	os.WriteFile(filepath.Join(dir, "lcov.info"), []byte("SF:src/index.ts\nend_of_record\n"), 0644)
	os.WriteFile(filepath.Join(dir, "results.junit.xml"), []byte("<testsuites/>"), 0644)
	os.WriteFile(filepath.Join(dir, ".lcov.info.0.tmp"), []byte("shard0"), 0644)
	os.WriteFile(filepath.Join(dir, ".lcov.info.1.tmp"), []byte("shard1"), 0644)
	os.WriteFile(filepath.Join(dir, ".lcov.info.123.tmp"), []byte("shard123"), 0644)

	if err := CleanCoverageShards(dir); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// All shards must be gone.
	shards, _ := filepath.Glob(filepath.Join(dir, ".lcov.info.*.tmp"))
	if len(shards) != 0 {
		t.Errorf("expected all .tmp shards removed, found %d: %v", len(shards), shards)
	}
	// The intended reports must be preserved.
	for _, keep := range []string{"lcov.info", "results.junit.xml"} {
		if _, err := os.Stat(filepath.Join(dir, keep)); err != nil {
			t.Errorf("expected %s to be preserved: %v", keep, err)
		}
	}
}

func TestCleanCoverageShards_NoShardsIsNoOp(t *testing.T) {
	dir := t.TempDir()
	// A coverage-disabled run leaves no shards — only the junit report.
	os.WriteFile(filepath.Join(dir, "results.junit.xml"), []byte("<testsuites/>"), 0644)

	if err := CleanCoverageShards(dir); err != nil {
		t.Fatalf("expected no error when no shards present, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "results.junit.xml")); err != nil {
		t.Errorf("expected results.junit.xml to be preserved: %v", err)
	}
}

func TestCleanCoverageShards_MissingDir(t *testing.T) {
	// A missing output dir must not error — the glob simply matches nothing.
	if err := CleanCoverageShards(filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Fatalf("expected no error for missing dir, got: %v", err)
	}
}

// ---- buildTestArgs ----

func TestBuildTestArgs_Defaults(t *testing.T) {
	args := buildTestArgs("/out", TestParams{})
	if args[0] != "test" {
		t.Errorf("expected first arg 'test', got %q", args[0])
	}
	// Should have junit reporter
	found := false
	for _, a := range args {
		if a == "--reporter=junit" {
			found = true
		}
	}
	if !found {
		t.Error("expected --reporter=junit")
	}
}

func TestBuildTestArgs_Parallel(t *testing.T) {
	cases := []struct {
		parallel string
		want     string
	}{
		{"", ""},
		{"false", ""},
		{"true", "--parallel"},
		{"4", "--parallel=4"},
	}
	for _, tc := range cases {
		args := buildTestArgs("/out", TestParams{Parallel: tc.parallel})
		var got string
		for _, a := range args {
			if strings.HasPrefix(a, "--parallel") {
				got = a
			}
		}
		if got != tc.want {
			t.Errorf("Parallel=%q: got %q, want %q", tc.parallel, got, tc.want)
		}
	}
}

func TestBuildTestArgs_WithTimeout(t *testing.T) {
	args := buildTestArgs("/out", TestParams{Timeout: 5000})
	found := false
	for _, a := range args {
		if a == "--timeout=5000" {
			found = true
		}
	}
	if !found {
		t.Error("expected --timeout=5000")
	}
}

func TestBuildTestArgs_WithCoverage(t *testing.T) {
	args := buildTestArgs("/out", TestParams{Coverage: true})
	foundCov, foundReporter, foundDir := false, false, false
	for _, a := range args {
		if a == "--coverage" {
			foundCov = true
		}
		if a == "--coverage-reporter=lcov" {
			foundReporter = true
		}
		if a == "--coverage-dir=/out" {
			foundDir = true
		}
	}
	if !foundCov || !foundReporter || !foundDir {
		t.Error("expected coverage flags")
	}
}

func TestBuildTestArgs_WithBail(t *testing.T) {
	args := buildTestArgs("/out", TestParams{Bail: 3})
	found := false
	for _, a := range args {
		if a == "--bail=3" {
			found = true
		}
	}
	if !found {
		t.Error("expected --bail=3")
	}
}

func TestBuildTestArgs_WithTestPattern(t *testing.T) {
	args := buildTestArgs("/out", TestParams{Test: "my-test"})
	found := false
	for _, a := range args {
		if a == "-t=my-test" {
			found = true
		}
	}
	if !found {
		t.Error("expected -t=my-test")
	}
}

func TestBuildTestArgs_AllFlags(t *testing.T) {
	args := buildTestArgs("/out", TestParams{
		Timeout:         10000,
		Concurrent:      "true",
		PassWithNoTests: true,
		UpdateSnapshots: true,
		Test:            "pattern",
		Only:            true,
		Todo:            true,
		Coverage:        true,
		Bail:            1,
	})
	expected := map[string]bool{
		"--timeout=10000":      false,
		"--concurrent=true":    false,
		"--pass-with-no-tests": false,
		"--update-snapshots":   false,
		"-t=pattern":           false,
		"--only":               false,
		"--todo":               false,
		"--coverage":           false,
		"--bail=1":             false,
	}
	for _, a := range args {
		if _, ok := expected[a]; ok {
			expected[a] = true
		}
	}
	for flag, found := range expected {
		if !found {
			t.Errorf("expected %s in args", flag)
		}
	}
}

// ---- mockExecRun helper ----

func withMockExec(t *testing.T, fn func(string, []string, ...exec.Option) (*exec.Result, error)) {
	t.Helper()
	orig := execRunFunc
	t.Cleanup(func() { execRunFunc = orig })
	execRunFunc = fn
}

// ---- RunTests ----

func TestRunTests_Success(t *testing.T) {
	dir := t.TempDir()
	outDir := filepath.Join(dir, "output")

	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: true, ExitCode: 0, Stdout: "all tests passed", Stderr: ""}, nil
	})

	ok, logs, err := RunTests("bun", dir, outDir, TestParams{Timeout: 5000})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Error("expected success")
	}
	if logs == "" {
		t.Error("expected non-empty logs")
	}
}

func TestRunTests_Failure(t *testing.T) {
	dir := t.TempDir()
	outDir := filepath.Join(dir, "output")

	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: false, ExitCode: 1, Stdout: "", Stderr: "test failed"}, nil
	})

	ok, logs, err := RunTests("bun", dir, outDir, TestParams{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("expected failure")
	}
	if logs == "" {
		t.Error("expected non-empty logs")
	}
}

func TestRunTests_ExecError(t *testing.T) {
	dir := t.TempDir()
	outDir := filepath.Join(dir, "output")

	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		return nil, os.ErrNotExist
	})

	_, _, err := RunTests("bun", dir, outDir, TestParams{})
	if err == nil {
		t.Error("expected error on exec failure")
	}
}

func TestRunTests_CreatesOutputDir(t *testing.T) {
	dir := t.TempDir()
	outDir := filepath.Join(dir, "nested", "output")

	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: true, ExitCode: 0}, nil
	})

	_, _, err := RunTests("bun", dir, outDir, TestParams{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(outDir); err != nil {
		t.Error("expected output directory to be created")
	}
}

// ---- RunTests: host platform identity scrub ----

// The `bun test` subprocess must not be able to tell that the HARNESS HOST is a
// managed runtime. A CI worker that is itself a Cloud Run service exports
// K_SERVICE; application code reads it as the production signal and refuses
// test-only behavior ("NoSigningKeyError: no signing key configured and
// allowEphemeral is false") — deterministic on that worker, invisible locally.
//
// This drives the REAL exec.Run rather than the mock, because the scrub lives in
// the option set RunTests passes, not in buildTestEnv's map: a mock that ignores
// the options would pass no matter what.
func TestRunTests_ScrubsHostPlatformIdentityFromSubprocess(t *testing.T) {
	dir := t.TempDir()
	outDir := filepath.Join(dir, "output")
	fakeBun := envProbeBun(t)

	t.Setenv("K_SERVICE", "ci-worker")
	t.Setenv("K_REVISION", "ci-worker-00042-abc")
	t.Setenv("K_CONFIGURATION", "ci-worker")
	t.Setenv("GAE_ENV", "standard")

	ok, logs, err := RunTests(fakeBun, dir, outDir, TestParams{})
	if err != nil {
		t.Fatalf("RunTests: %v", err)
	}
	if !ok {
		t.Fatalf("probe script failed: %s", logs)
	}
	for _, name := range []string{"K_SERVICE", "K_REVISION", "K_CONFIGURATION", "GAE_ENV"} {
		if want := name + "=[]"; !strings.Contains(logs, want) {
			t.Errorf("%s leaked into the test subprocess; probe output:\n%s", name, logs)
		}
	}
}

// On a hosted run the engine describes the job to the extension in two
// variables. `bun test` must see neither: a repository's tests are not jobs of
// the run.
func TestRunTests_ScrubsTheJobVariablesOfAHostedRun(t *testing.T) {
	dir := t.TempDir()
	outDir := filepath.Join(dir, "output")
	fakeBun := envProbeBun(t)

	t.Setenv("PUTNAMI_OFFLINE_DEPENDENCIES", "1")
	t.Setenv("PUTNAMI_JOB_CREDENTIAL_FD", "7")

	ok, logs, err := RunTests(fakeBun, dir, outDir, TestParams{})
	if err != nil {
		t.Fatalf("RunTests: %v", err)
	}
	if !ok {
		t.Fatalf("probe script failed: %s", logs)
	}
	for _, name := range []string{"PUTNAMI_OFFLINE_DEPENDENCIES", "PUTNAMI_JOB_CREDENTIAL_FD"} {
		if want := name + "=[]"; !strings.Contains(logs, want) {
			t.Errorf("%s leaked into the test subprocess; probe output:\n%s", name, logs)
		}
	}
}

// The scrub must not cost the harness contract: FORCE_COLOR, the database
// binding credential and the inherited PATH all still reach `bun test`.
func TestRunTests_PreservesHarnessEnvironment(t *testing.T) {
	dir := t.TempDir()
	outDir := filepath.Join(dir, "output")
	fakeBun := envProbeBun(t)

	t.Setenv("K_SERVICE", "ci-worker")
	t.Setenv(envDatabaseTestBindings, `{"protocolVersion":1,"databases":{}}`)
	t.Setenv("GOOGLE_CLOUD_PROJECT", "my-project")

	ok, logs, err := RunTests(fakeBun, dir, outDir, TestParams{})
	if err != nil {
		t.Fatalf("RunTests: %v", err)
	}
	if !ok {
		t.Fatalf("probe script failed: %s", logs)
	}
	for _, want := range []string{
		"FORCE_COLOR=[1]",
		`DATABASE_TEST_BINDINGS=[{"protocolVersion":1,"databases":{}}]`,
		// GOOGLE_CLOUD_PROJECT is deliberately NOT scrubbed: it is a capability
		// (which project a client SDK addresses), not a deployment identity.
		"GOOGLE_CLOUD_PROJECT=[my-project]",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("missing %q in probe output:\n%s", want, logs)
		}
	}
	if !strings.Contains(logs, "PATH_SET=[yes]") {
		t.Errorf("inherited PATH did not reach the subprocess:\n%s", logs)
	}
}

// envProbeEnv, when set, makes this test binary an env probe instead of a test
// run: a stand-in for `bun` that starts on every host a test runs on, Windows
// included, where a shell script does not.
const envProbeEnv = "PUTNAMI_TS_TESTJOB_ENV_PROBE"

func TestMain(m *testing.M) {
	if _, ok := os.LookupEnv(envProbeEnv); ok {
		printEnvProbe()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// printEnvProbe ignores its arguments and prints the environment entries the
// scrub contract is about, an unset entry as an empty value.
func printEnvProbe() {
	for _, name := range []string{"K_SERVICE", "K_REVISION", "K_CONFIGURATION", "GAE_ENV", "FORCE_COLOR", "DATABASE_TEST_BINDINGS", "GOOGLE_CLOUD_PROJECT", "PUTNAMI_OFFLINE_DEPENDENCIES", "PUTNAMI_JOB_CREDENTIAL_FD"} {
		fmt.Printf("%s=[%s]\n", name, os.Getenv(name))
	}
	pathSet := "no"
	if os.Getenv("PATH") != "" {
		pathSet = "yes"
	}
	fmt.Printf("PATH_SET=[%s]\n", pathSet)
}

// envProbeBun returns this test binary as a stand-in for `bun` that
// prints the environment it receives (see printEnvProbe) for the rest of the
// test.
func envProbeBun(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(envProbeEnv, "1")
	return self
}

// ---- resolveTestTimeout ----

func TestResolveTestTimeout_DefaultsWhenUnset(t *testing.T) {
	if got := resolveTestTimeout(TestParams{}); got != DefaultTestTimeout {
		t.Errorf("expected default %v, got %v", DefaultTestTimeout, got)
	}
}

func TestResolveTestTimeout_HonorsConfiguredBudget(t *testing.T) {
	if got := resolveTestTimeout(TestParams{WallClockTimeout: 90 * time.Second}); got != 90*time.Second {
		t.Errorf("expected 90s, got %v", got)
	}
}

func TestRunTests_PassesTimeoutOption(t *testing.T) {
	dir := t.TempDir()
	var gotOpts int
	withMockExec(t, func(_ string, _ []string, opts ...exec.Option) (*exec.Result, error) {
		gotOpts = len(opts)
		return &exec.Result{Success: true}, nil
	})
	if _, _, err := RunTests("bun", dir, filepath.Join(dir, "out"), TestParams{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Dir + UnsetEnv + Env + Timeout = 4 options.
	if gotOpts != 4 {
		t.Errorf("expected 4 exec options (Dir, UnsetEnv, Env, Timeout), got %d", gotOpts)
	}
}

// helper
func keys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

// ---- buildTestEnv: spec-verification fragment directory ----

// TestBuildTestEnv_SpecFragmentsDir pins the producer seam of the executable
// spec gate: a provisioned fragment directory reaches the `bun test`
// subprocess as PUTNAMI_SPEC_FRAGMENTS, and outside a Putnami-provisioned run
// (empty or blank) nothing is forwarded — the helper then stays inert.
func TestBuildTestEnv_SpecFragmentsDir(t *testing.T) {
	env := buildTestEnv(TestParams{SpecFragmentsDir: "/scratch/fragments"})
	if got := env["PUTNAMI_SPEC_FRAGMENTS"]; got != "/scratch/fragments" {
		t.Errorf("PUTNAMI_SPEC_FRAGMENTS = %q, want the provisioned directory", got)
	}
	for _, blank := range []string{"", "   "} {
		env := buildTestEnv(TestParams{SpecFragmentsDir: blank})
		if _, ok := env["PUTNAMI_SPEC_FRAGMENTS"]; ok {
			t.Errorf("blank fragment dir %q must not be forwarded", blank)
		}
	}
}

// ---- buildTestEnv: DATABASE_TEST_BINDINGS pass-through parity ----

// TestBuildTestEnv_DatabaseTestBindingsPassthrough pins the precedence contract
// mirrored from the Go extension: an externally set DATABASE_TEST_BINDINGS is
// forwarded to the `bun test` subprocess UNTOUCHED (external always wins), a
// binding this invocation PROVISIONED is used when no external one is present,
// and a blank/unset pair forwards nothing (the runtime's conf-YAML fallback then
// applies). FORCE_COLOR is always present.
func TestBuildTestEnv_DatabaseTestBindingsPassthrough(t *testing.T) {
	t.Run("forwards a set binding untouched", func(t *testing.T) {
		// Leading/trailing whitespace is preserved: the value must pass through
		// exactly as CI injected it, so the runtime parses the same bytes that
		// folded into the cache key.
		raw := ` {"protocolVersion":1,"mode":"require","databases":{}} `
		t.Setenv(envDatabaseTestBindings, raw)
		env := buildTestEnv(TestParams{})
		if env["FORCE_COLOR"] != "1" {
			t.Errorf("FORCE_COLOR = %q, want 1", env["FORCE_COLOR"])
		}
		if got := env[envDatabaseTestBindings]; got != raw {
			t.Errorf("%s = %q, want untouched %q", envDatabaseTestBindings, got, raw)
		}
	})

	// An external binding must never be shadowed by a locally provisioned one:
	// it is the value the cache key folded, so executing against a different
	// database than the key describes is the exact hole this ordering closes.
	t.Run("external beats a provisioned binding", func(t *testing.T) {
		t.Setenv(envDatabaseTestBindings, "external-binding")
		env := buildTestEnv(TestParams{DatabaseBinding: "provisioned-binding"})
		if got := env[envDatabaseTestBindings]; got != "external-binding" {
			t.Errorf("%s = %q, want the external value to win", envDatabaseTestBindings, got)
		}
	})

	t.Run("uses the provisioned binding when no external one is set", func(t *testing.T) {
		t.Setenv(envDatabaseTestBindings, "")
		env := buildTestEnv(TestParams{DatabaseBinding: "provisioned-binding"})
		if got := env[envDatabaseTestBindings]; got != "provisioned-binding" {
			t.Errorf("%s = %q, want the provisioned value", envDatabaseTestBindings, got)
		}
	})

	t.Run("omits a blank binding", func(t *testing.T) {
		t.Setenv(envDatabaseTestBindings, "   ")
		env := buildTestEnv(TestParams{})
		if _, ok := env[envDatabaseTestBindings]; ok {
			t.Errorf("blank %s must not be forwarded", envDatabaseTestBindings)
		}
	})

	t.Run("omits an unset binding", func(t *testing.T) {
		saved, had := os.LookupEnv(envDatabaseTestBindings)
		os.Unsetenv(envDatabaseTestBindings)
		t.Cleanup(func() {
			if had {
				os.Setenv(envDatabaseTestBindings, saved)
			}
		})
		env := buildTestEnv(TestParams{})
		if _, ok := env[envDatabaseTestBindings]; ok {
			t.Errorf("unset %s must not be forwarded", envDatabaseTestBindings)
		}
		if env["FORCE_COLOR"] != "1" {
			t.Errorf("FORCE_COLOR = %q, want 1", env["FORCE_COLOR"])
		}
	})
}

// TestManifestDeclaresDatabaseTestBindingsInput pins the cross-language manifest
// parity: the TS extension's test task must declare DATABASE_TEST_BINDINGS as a
// "from":"env" cache-key input, byte-identical to the Go extension's test-exec
// task. That declaration is what folds an EXTERNALLY supplied binding into the
// job's cache key — the case where the value never travels through an
// invocation-scoped artifact whose producing action the key could fold instead.
func TestManifestDeclaresDatabaseTestBindingsInput(t *testing.T) {
	// Test cwd is the package dir; the manifest lives at the extension root.
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.extension.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var manifest struct {
		Tasks map[string]struct {
			Inputs map[string]struct {
				From string `json:"from"`
			} `json:"inputs"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	task, ok := manifest.Tasks["test-run"]
	if !ok {
		t.Fatal("manifest has no test-run task")
	}
	input, ok := task.Inputs[envDatabaseTestBindings]
	if !ok {
		t.Fatalf("test-run task must declare a %q input for planner/cache-key parity with the Go extension", envDatabaseTestBindings)
	}
	if input.From != "env" {
		t.Errorf("%s input from = %q, want \"env\"", envDatabaseTestBindings, input.From)
	}
	// The env-var name is a cross-language constant; guard against drift.
	if !strings.EqualFold(envDatabaseTestBindings, "DATABASE_TEST_BINDINGS") {
		t.Errorf("env constant drifted to %q", envDatabaseTestBindings)
	}
}
