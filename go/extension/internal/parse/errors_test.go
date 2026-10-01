package parse

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	"go.putnami.dev/sdk/extension/jsonl"
)

// captureDiagnostics captures JSONL events emitted to stdout during a function call.
func captureDiagnostics(t *testing.T, fn func(emit *jsonl.Emitter)) []map[string]any {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	origStdout := os.Stdout
	os.Stdout = w

	emit := jsonl.New()
	fn(emit)

	w.Close()
	os.Stdout = origStdout

	var buf bytes.Buffer
	io.Copy(&buf, r)
	r.Close()

	var events []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err == nil {
			events = append(events, event)
		}
	}
	return events
}

// --- GoBuildErrors ---

func TestGoBuildErrors_SingleError(t *testing.T) {
	output := "main.go:42:10: undefined: foo\n"
	events := captureDiagnostics(t, func(emit *jsonl.Emitter) {
		GoBuildErrors(output, "/project", "/project", emit)
	})

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0]["severity"] != "error" {
		t.Errorf("severity = %v, want error", events[0]["severity"])
	}
	if events[0]["message"] != "undefined: foo" {
		t.Errorf("message = %v, want %q", events[0]["message"], "undefined: foo")
	}
	loc := events[0]["location"].(map[string]any)
	if loc["file"] != "main.go" {
		t.Errorf("file = %v, want %q", loc["file"], "main.go")
	}
	if loc["line"] != float64(42) {
		t.Errorf("line = %v, want 42", loc["line"])
	}
}

func TestGoBuildErrors_CapturesColumn(t *testing.T) {
	output := "main.go:42:10: undefined: foo\n"
	events := captureDiagnostics(t, func(emit *jsonl.Emitter) {
		GoBuildErrors(output, "/project", "/project", emit)
	})

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	loc := events[0]["location"].(map[string]any)
	if loc["line"] != float64(42) {
		t.Errorf("line = %v, want 42", loc["line"])
	}
	if loc["column"] != float64(10) {
		t.Errorf("column = %v, want 10 (the toolchain column must be carried through)", loc["column"])
	}
	if events[0]["message"] != "undefined: foo" {
		t.Errorf("message = %v, want %q", events[0]["message"], "undefined: foo")
	}
}

func TestGoBuildErrors_MultipleErrors(t *testing.T) {
	output := "main.go:10:5: syntax error\nutil.go:20:3: undefined: bar\n"
	events := captureDiagnostics(t, func(emit *jsonl.Emitter) {
		GoBuildErrors(output, "/proj", "/proj", emit)
	})

	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
}

func TestGoBuildErrors_NonMatchingOutput(t *testing.T) {
	// Non-matching output should emit the entire output as a single diagnostic
	output := "some random error text"
	events := captureDiagnostics(t, func(emit *jsonl.Emitter) {
		GoBuildErrors(output, "/proj", "/proj", emit)
	})

	if len(events) != 1 {
		t.Fatalf("expected 1 fallback event, got %d", len(events))
	}
	if events[0]["message"] != "some random error text" {
		t.Errorf("message = %v, want %q", events[0]["message"], "some random error text")
	}
}

func TestGoBuildErrors_EmptyOutput(t *testing.T) {
	events := captureDiagnostics(t, func(emit *jsonl.Emitter) {
		GoBuildErrors("", "/proj", "/proj", emit)
	})
	if len(events) != 0 {
		t.Errorf("expected 0 events for empty output, got %d", len(events))
	}
}

// --- LintErrors ---

func TestLintErrors_GolangciLint(t *testing.T) {
	output := "main.go:10:5: unused variable (deadcode)\nutil.go:20: deprecated function\n"
	events := captureDiagnostics(t, func(emit *jsonl.Emitter) {
		result := LintErrors(output, "/proj", "/proj", emit)
		if result.Errors != 2 {
			t.Errorf("count = %d, want 2", result.Errors)
		}
	})

	// 2 diagnostic events + 1 metric event
	if len(events) != 3 {
		t.Fatalf("expected 3 events (2 diagnostics + 1 metric), got %d", len(events))
	}
}

func TestLintErrors_ResolvesAbsoluteAndCollidingProjectRelativePaths(t *testing.T) {
	// The root comes from the host, so the absolute case is absolute on
	// Windows too, where a linter reports "C:\...\bar.go:10:5: ...".
	workspaceRoot := t.TempDir()
	projectPath := filepath.Join(workspaceRoot, "foo")

	for _, tc := range []struct {
		name string
		file string
		want string
	}{
		{
			name: "absolute golangci path",
			file: filepath.Join(projectPath, "bar.go"),
			want: "foo/bar.go",
		},
		{
			name: "project relative collision",
			file: "foo/bar.go",
			want: "foo/foo/bar.go",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := captureDiagnostics(t, func(emit *jsonl.Emitter) {
				LintErrors(tc.file+":10:5: unused function (unused)\n", workspaceRoot, projectPath, emit)
			})

			if len(events) != 2 {
				t.Fatalf("expected 1 diagnostic and 1 metric, got %d events", len(events))
			}
			location := events[0]["location"].(map[string]any)
			if got := location["file"]; got != tc.want {
				t.Errorf("file = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLintErrors_CapturesColumn(t *testing.T) {
	output := "main.go:10:5: unused variable (deadcode)\n"
	events := captureDiagnostics(t, func(emit *jsonl.Emitter) {
		LintErrors(output, "/proj", "/proj", emit)
	})

	// 1 diagnostic + 1 metric
	if len(events) != 2 {
		t.Fatalf("expected 2 events (1 diagnostic + 1 metric), got %d", len(events))
	}
	loc := events[0]["location"].(map[string]any)
	if loc["line"] != float64(10) {
		t.Errorf("line = %v, want 10", loc["line"])
	}
	if loc["column"] != float64(5) {
		t.Errorf("column = %v, want 5 (linter column must be carried through)", loc["column"])
	}
	if events[0]["message"] != "unused variable (deadcode)" {
		t.Errorf("message = %v, want %q", events[0]["message"], "unused variable (deadcode)")
	}
}

func TestLintErrors_NoColumn(t *testing.T) {
	// Some linters report file:line without a column. The diagnostic must
	// still parse, and omit the column rather than emit a bogus 0.
	output := "util.go:20: deprecated function\n"
	events := captureDiagnostics(t, func(emit *jsonl.Emitter) {
		LintErrors(output, "/proj", "/proj", emit)
	})

	if len(events) != 2 {
		t.Fatalf("expected 2 events (1 diagnostic + 1 metric), got %d", len(events))
	}
	loc := events[0]["location"].(map[string]any)
	if loc["line"] != float64(20) {
		t.Errorf("line = %v, want 20", loc["line"])
	}
	if _, ok := loc["column"]; ok {
		t.Errorf("column should be omitted when the linter provides none, got %v", loc["column"])
	}
	if events[0]["message"] != "deprecated function" {
		t.Errorf("message = %v, want %q", events[0]["message"], "deprecated function")
	}
}

func TestLintErrors_NoErrors(t *testing.T) {
	events := captureDiagnostics(t, func(emit *jsonl.Emitter) {
		result := LintErrors("some non-matching output", "/proj", "/proj", emit)
		if result.Errors != 1 {
			t.Errorf("count = %d, want 1 (fallback)", result.Errors)
		}
	})

	// Should emit a fallback diagnostic + metric
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
}

func TestLintErrors_EmptyInput(t *testing.T) {
	events := captureDiagnostics(t, func(emit *jsonl.Emitter) {
		result := LintErrors("", "/proj", "/proj", emit)
		if result.Errors != 0 {
			t.Errorf("count = %d, want 0", result.Errors)
		}
	})

	if len(events) != 0 {
		t.Errorf("expected 0 events for empty output, got %d", len(events))
	}
}

// --- TestJSON ---

func TestTestJSON_PassedTests(t *testing.T) {
	output := `{"Action":"pass","Test":"TestFoo","Package":"pkg"}
{"Action":"pass","Test":"TestBar","Package":"pkg"}
`
	counts := TestJSON(output)
	if counts.Passed != 2 {
		t.Errorf("Passed = %d, want 2", counts.Passed)
	}
	if counts.Failed != 0 {
		t.Errorf("Failed = %d, want 0", counts.Failed)
	}
	if counts.Total() != 2 {
		t.Errorf("Total = %d, want 2", counts.Total())
	}
}

func TestTestJSON_FailedTests(t *testing.T) {
	output := `{"Action":"pass","Test":"TestFoo","Package":"pkg"}
{"Action":"fail","Test":"TestBar","Package":"pkg"}
`
	counts := TestJSON(output)
	if counts.Passed != 1 {
		t.Errorf("Passed = %d, want 1", counts.Passed)
	}
	if counts.Failed != 1 {
		t.Errorf("Failed = %d, want 1", counts.Failed)
	}
}

func TestTestJSON_SkippedTests(t *testing.T) {
	output := `{"Action":"skip","Test":"TestFoo","Package":"pkg"}
{"Action":"pass","Test":"TestBar","Package":"pkg"}
`
	counts := TestJSON(output)
	if counts.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", counts.Skipped)
	}
	if counts.Total() != 2 {
		t.Errorf("Total = %d, want 2", counts.Total())
	}
}

func TestTestJSON_MixedResults(t *testing.T) {
	output := `{"Action":"pass","Test":"TestA","Package":"pkg"}
{"Action":"fail","Test":"TestB","Package":"pkg"}
{"Action":"skip","Test":"TestC","Package":"pkg"}
{"Action":"output","Test":"TestB","Output":"some output"}
{"Action":"pass","Test":"TestD","Package":"pkg"}
`
	counts := TestJSON(output)
	if counts.Passed != 2 {
		t.Errorf("Passed = %d, want 2", counts.Passed)
	}
	if counts.Failed != 1 {
		t.Errorf("Failed = %d, want 1", counts.Failed)
	}
	if counts.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", counts.Skipped)
	}
}

func TestTestJSON_EmptyOutput(t *testing.T) {
	counts := TestJSON("")
	if counts.Total() != 0 {
		t.Errorf("Total = %d, want 0", counts.Total())
	}
}

func TestTestJSON_InvalidJSON(t *testing.T) {
	output := "not json\nalso not json\n"
	counts := TestJSON(output)
	if counts.Total() != 0 {
		t.Errorf("Total = %d, want 0", counts.Total())
	}
}

func TestTestJSON_EmptyLines(t *testing.T) {
	output := "\n\n{\"Action\":\"pass\",\"Test\":\"TestA\"}\n\n"
	counts := TestJSON(output)
	if counts.Passed != 1 {
		t.Errorf("Passed = %d, want 1", counts.Passed)
	}
}

func TestTestJSON_IgnoresPackageLevelEvents(t *testing.T) {
	// Package-level events have an empty Test field and should not be counted
	output := `{"Action":"pass","Test":"TestFoo","Package":"pkg"}
{"Action":"pass","Package":"pkg"}
{"Action":"fail","Test":"TestBar","Package":"pkg"}
{"Action":"fail","Package":"pkg"}
`
	counts := TestJSON(output)
	if counts.Passed != 1 {
		t.Errorf("Passed = %d, want 1 (should ignore package-level pass)", counts.Passed)
	}
	if counts.Failed != 1 {
		t.Errorf("Failed = %d, want 1 (should ignore package-level fail)", counts.Failed)
	}
	if counts.Total() != 2 {
		t.Errorf("Total = %d, want 2", counts.Total())
	}
	if counts.PackagesFailed != 1 {
		t.Errorf("PackagesFailed = %d, want 1 (tracked outside the test counts)", counts.PackagesFailed)
	}
}

// TestTestJSON_PackageFailureWithoutTestFailure is the counting half of the
// silent-failure bug: every test passes and the package still FAILs (a data
// race after the last test, a post-test panic, a TestMain exit code). The test
// counts must stay clean while PackagesFailed records the failure, so the
// summary has something honest to report.
func TestTestJSON_PackageFailureWithoutTestFailure(t *testing.T) {
	output := `{"Action":"pass","Test":"TestFoo","Package":"pkg"}
{"Action":"pass","Test":"TestBar","Package":"pkg"}
{"Action":"fail","Package":"pkg"}
`
	counts := TestJSON(output)
	if counts.Passed != 2 || counts.Failed != 0 || counts.Total() != 2 {
		t.Errorf("counts = %+v, want 2 passed / 0 failed / 2 total", counts)
	}
	if counts.PackagesFailed != 1 {
		t.Errorf("PackagesFailed = %d, want 1", counts.PackagesFailed)
	}
}

// --- TestFailureDiagnostics ---

// TestTestDiagnostics_WithLocation is the regression guard for the normal,
// already-working case: a test-level failure carrying a `foo_test.go:12: msg`
// line must still produce exactly the located diagnostic it produced before the
// raw-output fallback existed, and nothing else.
func TestTestDiagnostics_WithLocation(t *testing.T) {
	output := `{"Action":"output","Test":"TestFoo","Package":"pkg","Output":"    main_test.go:42: expected true, got false\n"}
{"Action":"fail","Test":"TestFoo","Package":"pkg"}
`
	locate := rootModuleLocator(t, "pkg", "main_test.go")
	events := captureDiagnostics(t, func(emit *jsonl.Emitter) {
		TestFailureDiagnostics(output, locate, emit)
	})

	if len(events) != 1 {
		t.Fatalf("expected 1 diagnostic event, got %d", len(events))
	}
	if events[0]["message"] != "expected true, got false" {
		t.Errorf("message = %v, want %q", events[0]["message"], "expected true, got false")
	}
	loc := events[0]["location"].(map[string]any)
	if loc["file"] != "main_test.go" {
		t.Errorf("file = %v, want %q", loc["file"], "main_test.go")
	}
	if loc["line"] != float64(42) {
		t.Errorf("line = %v, want 42", loc["line"])
	}
}

// Passing t.Log and skipped-helper output use the same source-location shape as
// assertion failures. They must not become errors or suppress the raw fallback
// that carries a package-level panic, race, build/vet error, or TestMain exit.
func TestTestFailureDiagnostics_IgnoresHarmlessLocatedOutput(t *testing.T) {
	output := strings.Join([]string{
		`{"Action":"output","Test":"TestComplexity","Package":"pkg","Output":"    complexity_test.go:121: command count 42/50\n"}`,
		`{"Action":"pass","Test":"TestComplexity","Package":"pkg"}`,
		`{"Action":"output","Test":"TestChildHelper","Package":"pkg","Output":"    helper_test.go:87: child-only helper\n"}`,
		`{"Action":"skip","Test":"TestChildHelper","Package":"pkg"}`,
		`{"Action":"output","Package":"pkg","Output":"panic: failure after tests completed\n"}`,
		`{"Action":"fail","Package":"pkg"}`,
		"",
	}, "\n")

	diagnostics := TestFailureDiagnosticList(output, nil)
	if len(diagnostics) != 1 {
		t.Fatalf("diagnostics = %+v, want only the raw package failure", diagnostics)
	}
	if diagnostics[0].File != "" {
		t.Errorf("diagnostic location = %q, harmless test output was promoted", diagnostics[0].File)
	}
	if diagnostics[0].Category != "GO_TEST_FAILURE" {
		t.Errorf("diagnostic category = %q, want GO_TEST_FAILURE", diagnostics[0].Category)
	}
	if !strings.Contains(diagnostics[0].Description, "panic: failure after tests completed") {
		t.Errorf("diagnostic = %q, want the causal package failure", diagnostics[0].Description)
	}
}

// A located assertion in one package does not explain an independent
// package-level failure in another. The raw fallback must remain alongside the
// source diagnostic or the panic/race/TestMain cause disappears from a batch.
func TestTestFailureDiagnostics_KeepsUnexplainedPackageBesideLocatedFailure(t *testing.T) {
	output := strings.Join([]string{
		`{"Action":"output","Test":"TestAssertion","Package":"pkg/a","Output":"    a_test.go:12: expected true, got false\n"}`,
		`{"Action":"fail","Test":"TestAssertion","Package":"pkg/a"}`,
		`{"Action":"fail","Package":"pkg/a"}`,
		`{"Action":"output","Package":"pkg/b","Output":"panic: failure after pkg/b tests completed\n"}`,
		`{"Action":"fail","Package":"pkg/b"}`,
		"",
	}, "\n")

	diagnostics := TestFailureDiagnosticList(output, rootModuleLocator(t, "pkg", "a/a_test.go"))
	if len(diagnostics) != 2 {
		t.Fatalf("diagnostics = %+v, want the located assertion and raw package failure", diagnostics)
	}
	if diagnostics[0].File != "a/a_test.go" {
		t.Errorf("first diagnostic file = %q, want a/a_test.go", diagnostics[0].File)
	}
	if diagnostics[1].Category != "GO_TEST_FAILURE" {
		t.Errorf("fallback category = %q, want GO_TEST_FAILURE", diagnostics[1].Category)
	}
	if !strings.Contains(diagnostics[1].Description, "panic: failure after pkg/b tests completed") {
		t.Errorf("fallback = %q, want the independent package failure", diagnostics[1].Description)
	}
}

// TestTestDiagnostics_NoLocation_FallbackCount pins that the count-only
// diagnostic is never the whole story. "2 test(s) failed" names no cause, so
// the raw output rides along — this is the shape a race detected inside a
// running test takes, where the report is the only evidence.
func TestTestDiagnostics_NoLocation_FallbackCount(t *testing.T) {
	output := `{"Action":"output","Test":"TestFoo","Output":"WARNING: DATA RACE\n"}
{"Action":"fail","Test":"TestFoo","Package":"pkg"}
{"Action":"fail","Test":"TestBar","Package":"pkg"}
`
	events := captureDiagnostics(t, func(emit *jsonl.Emitter) {
		TestFailureDiagnostics(output, nil, emit)
	})

	if len(events) != 2 {
		t.Fatalf("expected the count plus the raw output, got %d events: %+v", len(events), events)
	}
	if !strings.Contains(events[0]["message"].(string), "2 test(s) failed") {
		t.Errorf("message = %v, want to contain '2 test(s) failed'", events[0]["message"])
	}
	if !strings.Contains(events[1]["message"].(string), "WARNING: DATA RACE") {
		t.Errorf("message = %v, want the raw race report", events[1]["message"])
	}
}

// TestTestDiagnosticList_NoFailures pins that the pure parser still reports
// nothing when there is nothing to parse. Only the failure entry points add the
// raw-output floor on top of it.
func TestTestDiagnosticList_NoFailures(t *testing.T) {
	output := `{"Action":"pass","Test":"TestFoo","Package":"pkg"}
`
	if got := TestDiagnosticList(output, nil); len(got) != 0 {
		t.Errorf("TestDiagnosticList = %+v, want none for passing output", got)
	}
}

// TestTestFailureDiagnostics_SilentFailures covers every shape of failure that
// used to reach the user as "(no details emitted)": the exit code said the run
// failed, yet nothing in the output produced a diagnostic. Each case must now
// emit a non-empty diagnostic naming the cause.
func TestTestFailureDiagnostics_SilentFailures(t *testing.T) {
	raceReport := strings.Join([]string{
		`{"Action":"pass","Test":"TestOne","Package":"pkg"}`,
		`{"Action":"output","Package":"pkg","Output":"==================\n"}`,
		`{"Action":"output","Package":"pkg","Output":"WARNING: DATA RACE\n"}`,
		`{"Action":"output","Package":"pkg","Output":"Write at 0x00c000123456 by goroutine 12:\n"}`,
		`{"Action":"output","Package":"pkg","Output":"  pkg.(*Store).Put()\n"}`,
		`{"Action":"output","Package":"pkg","Output":"Found 1 data race(s)\n"}`,
		`{"Action":"output","Package":"pkg","Output":"FAIL\tpkg\t1.2s\n"}`,
		`{"Action":"fail","Package":"pkg","Elapsed":1.2}`,
		"",
	}, "\n")

	panicAfterTests := strings.Join([]string{
		`{"Action":"pass","Test":"TestOne","Package":"pkg"}`,
		`{"Action":"output","Package":"pkg","Output":"panic: send on closed channel\n"}`,
		`{"Action":"output","Package":"pkg","Output":"goroutine 42 [running]:\n"}`,
		`{"Action":"output","Package":"pkg","Output":"pkg.worker()\n"}`,
		`{"Action":"fail","Package":"pkg","Elapsed":0.4}`,
		"",
	}, "\n")

	cases := []struct {
		name   string
		output string
		want   string
	}{
		{
			name:   "data race with every test passing",
			output: raceReport,
			want:   "WARNING: DATA RACE",
		},
		{
			name:   "panic after the last test completed",
			output: panicAfterTests,
			want:   "panic: send on closed channel",
		},
		{
			name:   "package-level fail with no test-level fail",
			output: "{\"Action\":\"pass\",\"Test\":\"TestOne\",\"Package\":\"pkg\"}\n{\"Action\":\"output\",\"Package\":\"pkg\",\"Output\":\"FAIL\\tpkg\\t0.1s\\n\"}\n{\"Action\":\"fail\",\"Package\":\"pkg\"}\n",
			want:   "FAIL\tpkg\t0.1s",
		},
		{
			name:   "JSON disabled, plain go test output",
			output: "--- FAIL: TestOne (0.00s)\n    one_test.go:12: boom\nFAIL\tpkg\t0.1s\nFAIL\n",
			want:   "--- FAIL: TestOne",
		},
		{
			name:   "unparseable toolchain failure",
			output: "go: updates to go.mod needed; to update it:\n\tgo mod tidy\n",
			want:   "go mod tidy",
		},
		{
			name:   "no output at all",
			output: "",
			want:   "go test failed without any diagnostic output",
		},
		{
			// A race inside a running test does produce a test-level fail, but
			// no `_test.go:NN:` line, so the parser only had "1 test(s) failed"
			// to offer. The report itself must travel with it.
			name: "race attributed to a running test",
			output: strings.Join([]string{
				`{"Action":"output","Test":"TestOne","Output":"WARNING: DATA RACE\n"}`,
				`{"Action":"output","Test":"TestOne","Output":"Found 1 data race(s)\n"}`,
				`{"Action":"fail","Test":"TestOne","Package":"pkg"}`,
				`{"Action":"fail","Package":"pkg"}`,
				"",
			}, "\n"),
			want: "WARNING: DATA RACE",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			diagnostics := TestFailureDiagnosticList(tc.output, nil)
			if len(diagnostics) == 0 {
				t.Fatalf("no diagnostic for a failed run (output %q)", tc.output)
			}
			joined := ""
			for _, diagnostic := range diagnostics {
				if diagnostic.Severity != "error" {
					t.Errorf("severity = %q, want error", diagnostic.Severity)
				}
				if strings.TrimSpace(diagnostic.Description) == "" {
					t.Errorf("empty diagnostic description in %+v", diagnostics)
				}
				joined += diagnostic.Description + "\n"
			}
			if !strings.Contains(joined, tc.want) {
				t.Errorf("diagnostics = %q, want to contain %q", joined, tc.want)
			}

			events := captureDiagnostics(t, func(emit *jsonl.Emitter) {
				TestFailureDiagnostics(tc.output, nil, emit)
			})
			if len(events) == 0 {
				t.Fatalf("emitted no diagnostic event for a failed run (output %q)", tc.output)
			}
		})
	}
}

// TestTestFailureText_DropsVerboseScaffolding pins the signal-to-noise floor:
// `-json` forces -v, so an unwrapped stream is padded with a RUN and a PASS
// line per test. Those are dropped; everything that explains the failure stays.
func TestTestFailureText_DropsVerboseScaffolding(t *testing.T) {
	output := strings.Join([]string{
		`{"Action":"output","Package":"pkg","Output":"=== RUN   TestOne\n"}`,
		`{"Action":"output","Package":"pkg","Output":"--- PASS: TestOne (0.00s)\n"}`,
		`{"Action":"output","Package":"pkg","Output":"=== CONT  TestTwo\n"}`,
		`{"Action":"output","Package":"pkg","Output":"    --- SKIP: TestTwo/sub (0.00s)\n"}`,
		`{"Action":"output","Package":"pkg","Output":"WARNING: DATA RACE\n"}`,
		`{"Action":"output","Package":"pkg","Output":"--- FAIL: TestThree (0.01s)\n"}`,
		`{"Action":"output","Package":"pkg","Output":"FAIL\tpkg\t0.2s\n"}`,
		`{"Action":"fail","Package":"pkg"}`,
		"",
	}, "\n")

	text := TestFailureText(output)
	for _, dropped := range []string{"=== RUN", "--- PASS:", "=== CONT", "--- SKIP:"} {
		if strings.Contains(text, dropped) {
			t.Errorf("excerpt = %q, want no %q scaffolding", text, dropped)
		}
	}
	for _, kept := range []string{"WARNING: DATA RACE", "--- FAIL: TestThree", "FAIL\tpkg\t0.2s"} {
		if !strings.Contains(text, kept) {
			t.Errorf("excerpt = %q, want it to keep %q", text, kept)
		}
	}
}

func TestTestFailureText_KeepsBuildOutput(t *testing.T) {
	output := strings.Join([]string{
		`{"Action":"build-output","Package":"pkg","Output":"# pkg [pkg.test]\n"}`,
		`{"Action":"build-output","Package":"pkg","Output":"pkg_test.go:3:39: cannot use string as int\n"}`,
		`{"Action":"output","Package":"pkg","Output":"FAIL\tpkg [build failed]\n"}`,
		`{"Action":"fail","Package":"pkg"}`,
	}, "\n")

	text := TestFailureText(output)
	for _, want := range []string{"# pkg [pkg.test]", "cannot use string as int", "FAIL\tpkg [build failed]"} {
		if !strings.Contains(text, want) {
			t.Fatalf("failure text = %q, want compile evidence %q", text, want)
		}
	}
}

// TestTestFailureText_KeepsNonJSONOutputVerbatim pins that the scaffolding
// filter is scoped to unwrapped `-json` streams. With `--test-json=false` the
// caller chose the format, so nothing is dropped.
func TestTestFailureText_KeepsNonJSONOutputVerbatim(t *testing.T) {
	output := "=== RUN   TestOne\n--- PASS: TestOne (0.00s)\nFAIL\tpkg\t0.2s\n"
	if got := TestFailureText(output); got != strings.TrimSpace(output) {
		t.Errorf("TestFailureText = %q, want the plain output verbatim", got)
	}
}

// TestTestFailureText_FallsBackAfterOversizedJSONEvent verifies that a test's
// large logged value cannot truncate the parser before the diagnostic that
// follows it. Scanner's default token limit is smaller than a valid JSON output
// event, so the raw stream must be retained and bounded as a whole.
func TestTestFailureText_FallsBackAfterOversizedJSONEvent(t *testing.T) {
	event := func(action, output string) string {
		data, err := json.Marshal(TestEvent{Action: action, Package: "pkg", Output: output})
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	output := strings.Join([]string{
		event("output", "starting test\n"),
		event("output", strings.Repeat("x", 70*1024)+"\n"),
		event("output", "panic: actual failure\n"),
		event("fail", ""),
	}, "\n")

	text := TestFailureText(output)
	if !strings.Contains(text, "panic: actual failure") {
		t.Errorf("excerpt omitted the post-overflow failure: %q", text)
	}
}

// TestTestFailureText_BoundsOutput pins the truncation contract: a huge log is
// never dumped whole, and the race report that explains the failure survives
// even when it sits far from the end of the output.
func TestTestFailureText_BoundsOutput(t *testing.T) {
	var builder strings.Builder
	builder.WriteString("WARNING: DATA RACE\nWrite at 0x00c000123456 by goroutine 12:\n")
	for range 20000 {
		builder.WriteString("ok  \tpkg/filler\t0.01s\n")
	}
	raw := builder.String()

	text := TestFailureText(raw)
	if len(text) >= len(raw) {
		t.Fatalf("output not bounded: %d bytes for a %d byte log", len(text), len(raw))
	}
	if len(text) > maxFailureOutputBytes {
		t.Errorf("excerpt = %d bytes, want at most maxFailureOutputBytes including the marker", len(text))
	}
	if !strings.Contains(text, "WARNING: DATA RACE") {
		t.Errorf("excerpt dropped the race report: %q", text[:min(200, len(text))])
	}
	if !strings.Contains(text, "truncated") {
		t.Errorf("excerpt = %q, want a truncation marker", text[:min(200, len(text))])
	}
}

// TestTestFailureText_KeepsTailWithoutFailureBlock pins the default: with no
// recognizable failure block, the bounded excerpt keeps the end of the output,
// where `go test` prints its final FAIL lines.
func TestTestFailureText_KeepsTailWithoutFailureBlock(t *testing.T) {
	var builder strings.Builder
	for range 20000 {
		builder.WriteString("ok  \tpkg/filler\t0.01s\n")
	}
	builder.WriteString("exit status 2\n")

	text := TestFailureText(builder.String())
	if !strings.HasSuffix(text, "exit status 2") {
		t.Errorf("excerpt does not end with the tail: %q", text[max(0, len(text)-120):])
	}
	if len(text) > maxFailureOutputBytes {
		t.Errorf("excerpt = %d bytes, want bounded", len(text))
	}
}

// --- Coverage ---

func TestCoverageDetails_ValidProfile(t *testing.T) {
	dir := t.TempDir()
	coverFile := filepath.Join(dir, "coverage.out")

	profile := `mode: set
pkg/main.go:10.1,20.1 5 1
pkg/main.go:22.1,30.1 3 0
pkg/util.go:5.1,15.1 4 1
`
	if err := os.WriteFile(coverFile, []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := CoverageDetails(coverFile)
	if err != nil {
		t.Fatalf("CoverageDetails: %v", err)
	}

	if result.TotalStatements != 12 {
		t.Errorf("TotalStatements = %d, want 12", result.TotalStatements)
	}
	if result.CoveredStatements != 9 {
		t.Errorf("CoveredStatements = %d, want 9", result.CoveredStatements)
	}
	if result.Percentage != 75.0 {
		t.Errorf("Percentage = %v, want 75.0", result.Percentage)
	}
	if len(result.Files) != 2 {
		t.Fatalf("len(Files) = %d, want 2", len(result.Files))
	}

	// First file: main.go has 8 stmts total, 5 covered = 62.5%
	if result.Files[0].File != "pkg/main.go" {
		t.Errorf("Files[0].File = %q, want %q", result.Files[0].File, "pkg/main.go")
	}
	if result.Files[0].TotalStatements != 8 {
		t.Errorf("Files[0].TotalStatements = %d, want 8", result.Files[0].TotalStatements)
	}
	if result.Files[0].CoveredStatements != 5 {
		t.Errorf("Files[0].CoveredStatements = %d, want 5", result.Files[0].CoveredStatements)
	}

	// Second file: util.go has 4 stmts total, 4 covered = 100%
	if result.Files[1].File != "pkg/util.go" {
		t.Errorf("Files[1].File = %q, want %q", result.Files[1].File, "pkg/util.go")
	}
	if result.Files[1].Percentage != 100.0 {
		t.Errorf("Files[1].Percentage = %v, want 100.0", result.Files[1].Percentage)
	}
}

func TestCoverage_ValidProfile(t *testing.T) {
	dir := t.TempDir()
	coverFile := filepath.Join(dir, "coverage.out")

	profile := `mode: set
pkg/main.go:10.1,20.1 5 1
pkg/main.go:22.1,30.1 5 0
`
	if err := os.WriteFile(coverFile, []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}

	pct, err := Coverage(coverFile)
	if err != nil {
		t.Fatalf("Coverage: %v", err)
	}
	if pct != 50.0 {
		t.Errorf("Coverage = %v, want 50.0", pct)
	}
}

func TestCoverageDetails_FileNotFound(t *testing.T) {
	_, err := CoverageDetails("/nonexistent/coverage.out")
	if err == nil {
		t.Error("CoverageDetails with nonexistent file should return error")
	}
}

func TestCoverageDetails_EmptyProfile(t *testing.T) {
	dir := t.TempDir()
	coverFile := filepath.Join(dir, "coverage.out")

	profile := "mode: set\n"
	if err := os.WriteFile(coverFile, []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := CoverageDetails(coverFile)
	if err != nil {
		t.Fatalf("CoverageDetails: %v", err)
	}
	if result.TotalStatements != 0 {
		t.Errorf("TotalStatements = %d, want 0", result.TotalStatements)
	}
	if result.Percentage != 0 {
		t.Errorf("Percentage = %v, want 0", result.Percentage)
	}
}

func TestCoverageDetails_ZeroCoverage(t *testing.T) {
	dir := t.TempDir()
	coverFile := filepath.Join(dir, "coverage.out")

	profile := `mode: set
pkg/main.go:10.1,20.1 5 0
pkg/main.go:22.1,30.1 3 0
`
	if err := os.WriteFile(coverFile, []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := CoverageDetails(coverFile)
	if err != nil {
		t.Fatalf("CoverageDetails: %v", err)
	}
	if result.Percentage != 0 {
		t.Errorf("Percentage = %v, want 0", result.Percentage)
	}
	if result.CoveredStatements != 0 {
		t.Errorf("CoveredStatements = %d, want 0", result.CoveredStatements)
	}
}

func TestCoverageDetails_FullCoverage(t *testing.T) {
	dir := t.TempDir()
	coverFile := filepath.Join(dir, "coverage.out")

	profile := `mode: set
pkg/main.go:10.1,20.1 5 1
pkg/main.go:22.1,30.1 3 1
`
	if err := os.WriteFile(coverFile, []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := CoverageDetails(coverFile)
	if err != nil {
		t.Fatalf("CoverageDetails: %v", err)
	}
	if result.Percentage != 100.0 {
		t.Errorf("Percentage = %v, want 100.0", result.Percentage)
	}
}

func TestCoverageDetails_MalformedLines(t *testing.T) {
	dir := t.TempDir()
	coverFile := filepath.Join(dir, "coverage.out")

	profile := `mode: set
malformed line without enough fields
pkg/main.go:10.1,20.1 5 1
line with bad numbers abc def
`
	if err := os.WriteFile(coverFile, []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := CoverageDetails(coverFile)
	if err != nil {
		t.Fatalf("CoverageDetails: %v", err)
	}
	// Should skip malformed lines and parse valid ones
	if result.TotalStatements != 5 {
		t.Errorf("TotalStatements = %d, want 5", result.TotalStatements)
	}
}

func TestCoverageDetails_DeduplicatesBlocks(t *testing.T) {
	dir := t.TempDir()
	coverFile := filepath.Join(dir, "coverage.out")

	// Same block appears multiple times (from different test packages).
	// Correct behavior: deduplicate by block key, take max count.
	profile := `mode: set
pkg/main.go:10.1,20.1 5 0
pkg/main.go:10.1,20.1 5 1
pkg/main.go:10.1,20.1 5 0
pkg/main.go:22.1,30.1 3 0
pkg/main.go:22.1,30.1 3 0
`
	if err := os.WriteFile(coverFile, []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := CoverageDetails(coverFile)
	if err != nil {
		t.Fatalf("CoverageDetails: %v", err)
	}
	// After dedup: 2 unique blocks (5 + 3 = 8 stmts), 5 covered.
	if result.TotalStatements != 8 {
		t.Errorf("TotalStatements = %d, want 8", result.TotalStatements)
	}
	if result.CoveredStatements != 5 {
		t.Errorf("CoveredStatements = %d, want 5", result.CoveredStatements)
	}
	if result.Percentage != 62.5 {
		t.Errorf("Percentage = %v, want 62.5", result.Percentage)
	}
}

// TestCoverageDetails_OversizedLine pins that a profile line longer than
// bufio.Scanner's 64 KiB default does not end the parse: the blocks after it
// still count.
func TestCoverageDetails_OversizedLine(t *testing.T) {
	coverFile := filepath.Join(t.TempDir(), "coverage.out")
	longFile := "pkg/" + strings.Repeat("x", 70*1024) + ".go"
	profile := "mode: set\n" +
		longFile + ":1.1,2.1 4 0\n" +
		"pkg/main.go:1.1,2.1 4 1\n"
	if err := os.WriteFile(coverFile, []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := CoverageDetails(coverFile)
	if err != nil {
		t.Fatalf("CoverageDetails: %v", err)
	}
	if result.TotalStatements != 8 || result.CoveredStatements != 4 || len(result.Files) != 2 {
		t.Fatalf("result = %d/%d statements over %d files, want 4/8 over 2", result.CoveredStatements, result.TotalStatements, len(result.Files))
	}
}

// TestCoverageDetails_ReadErrorFailsTheParse pins that a read error returns
// an error naming the cause, never the blocks read before it.
func TestCoverageDetails_ReadErrorFailsTheParse(t *testing.T) {
	cause := errors.New("device went away")
	profile := io.MultiReader(
		strings.NewReader("mode: set\npkg/main.go:1.1,2.1 4 1\n"),
		iotest.ErrReader(cause),
	)
	result, err := coverageDetails(profile)
	if !errors.Is(err, cause) {
		t.Fatalf("coverageDetails error = %v, want %v", err, cause)
	}
	if result.TotalStatements != 0 || len(result.Files) != 0 {
		t.Fatalf("coverageDetails returned a partial result: %+v", result)
	}
}

// TestCoverageDetails_UnreadableProfileNamesPathAndCause reads a directory:
// it opens, then every read fails. The cause is the host's own read error,
// "is a directory" on Unix and "Incorrect function." on Windows.
func TestCoverageDetails_UnreadableProfileNamesPathAndCause(t *testing.T) {
	coverFile := t.TempDir()
	dir, err := os.Open(coverFile)
	if err != nil {
		t.Fatal(err)
	}
	_, readErr := dir.Read(make([]byte, 1))
	_ = dir.Close()
	var pathErr *fs.PathError
	if !errors.As(readErr, &pathErr) {
		t.Fatalf("reading a directory = %v, want a path error", readErr)
	}
	cause := pathErr.Err.Error()

	_, err = CoverageDetails(coverFile)
	if !errors.Is(err, ErrCoverageProfileUnreadable) {
		t.Fatalf("CoverageDetails error = %v, want ErrCoverageProfileUnreadable", err)
	}
	if strings.Count(err.Error(), coverFile) != 1 || !strings.Contains(err.Error(), cause) {
		t.Fatalf("error %q does not name the profile once and the cause %q", err, cause)
	}
}

func TestCoverageDetails_MissingProfileIsNotUnreadable(t *testing.T) {
	_, err := CoverageDetails(filepath.Join(t.TempDir(), "coverage.out"))
	if !errors.Is(err, fs.ErrNotExist) || errors.Is(err, ErrCoverageProfileUnreadable) {
		t.Fatalf("CoverageDetails error = %v, want a not-exist error only", err)
	}
}

func TestTestJSON_CountsPastOversizedEvent(t *testing.T) {
	event := func(action, test, output string) string {
		data, err := json.Marshal(TestEvent{Action: action, Package: "pkg", Test: test, Output: output})
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	output := strings.Join([]string{
		event("output", "TestA", strings.Repeat("x", 70*1024)+"\n"),
		event("pass", "TestA", ""),
		event("fail", "TestB", ""),
	}, "\n")
	if counts := TestJSON(output); counts.Passed != 1 || counts.Failed != 1 {
		t.Fatalf("counts = %+v, want 1 passed and 1 failed", counts)
	}
}

func TestTestCounts_Total(t *testing.T) {
	c := TestCounts{Passed: 3, Failed: 1, Skipped: 2}
	if c.Total() != 6 {
		t.Errorf("Total = %d, want 6", c.Total())
	}
}

func TestTestCounts_ZeroTotal(t *testing.T) {
	c := TestCounts{}
	if c.Total() != 0 {
		t.Errorf("Total = %d, want 0", c.Total())
	}
}

// --- CoverageDiagnostics ---

func TestCoverageDiagnostics_UncoveredFiles(t *testing.T) {
	result := CoverageResult{
		TotalStatements:   15,
		CoveredStatements: 5,
		Percentage:        33.33,
		Files: []FileCoverage{
			{File: "pkg/covered.go", TotalStatements: 5, CoveredStatements: 5, Percentage: 100},
			{File: "pkg/uncovered.go", TotalStatements: 10, CoveredStatements: 0, Percentage: 0},
		},
	}

	events := captureDiagnostics(t, func(emit *jsonl.Emitter) {
		CoverageDiagnostics(result, "/proj", "/proj", emit)
	})

	if len(events) != 1 {
		t.Fatalf("expected 1 diagnostic for uncovered file, got %d", len(events))
	}
	if events[0]["severity"] != "warning" {
		t.Errorf("severity = %v, want warning", events[0]["severity"])
	}
	msg := events[0]["message"].(string)
	if !strings.Contains(msg, "0%") || !strings.Contains(msg, "10 statements") {
		t.Errorf("message = %q, want to contain '0%%' and '10 statements'", msg)
	}
}

func TestCoverageDiagnostics_AllCovered(t *testing.T) {
	result := CoverageResult{
		TotalStatements:   10,
		CoveredStatements: 10,
		Percentage:        100,
		Files: []FileCoverage{
			{File: "pkg/main.go", TotalStatements: 10, CoveredStatements: 10, Percentage: 100},
		},
	}

	events := captureDiagnostics(t, func(emit *jsonl.Emitter) {
		CoverageDiagnostics(result, "/proj", "/proj", emit)
	})

	if len(events) != 0 {
		t.Errorf("expected 0 diagnostics when all files covered, got %d", len(events))
	}
}

func TestCoverageDiagnostics_SkipsEmptyFiles(t *testing.T) {
	result := CoverageResult{
		Files: []FileCoverage{
			{File: "pkg/empty.go", TotalStatements: 0, CoveredStatements: 0, Percentage: 0},
		},
	}

	events := captureDiagnostics(t, func(emit *jsonl.Emitter) {
		CoverageDiagnostics(result, "/proj", "/proj", emit)
	})

	if len(events) != 0 {
		t.Errorf("expected 0 diagnostics for files with no statements, got %d", len(events))
	}
}
