package test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/go/extension/internal/toolchain"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

const raceRequirement = "race-on-touched-packages"

// A changed Go file names its directory, a changed testdata file names the
// package that holds the testdata directory, and nothing else names a package.
func TestRacePackageDirs_NamesThePackagesTheChangeTouched(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", raceRequirement, "the-race-step-runs-on-the-packages-the-change-touched")
	got := racePackageDirs([]string{
		"main.go",
		"internal/x/y.go",
		"internal/x/y_test.go",
		"internal/z/testdata/case.json",
		"internal/z/testdata/deep/more.txt",
		"pkg/testdata/fixture.go",
		"testdata/root.txt",
		"README.md",
		"go.mod",
		"internal/x/doc.md",
		"../escape.go",
		"/abs.go",
	})
	want := []string{".", "internal/x", "internal/z", "pkg"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("racePackageDirs = %v, want %v", got, want)
	}
	if got := racePackageDirs(nil); len(got) != 0 {
		t.Errorf("no changed file named %v", got)
	}
}

// raceContext is a solo job context for the module at dir whose step was
// bound the given changed files.
func raceContext(t *testing.T, dir string, files []string, extra pctx.Params) *pctx.Context {
	t.Helper()
	params := pctx.Params{}
	if files != nil {
		encoded, err := json.Marshal(files)
		if err != nil {
			t.Fatal(err)
		}
		params["changed-files"] = encoded
	}
	for name, value := range extra {
		params[name] = value
	}
	return &pctx.Context{
		WorkspaceRoot: dir,
		OutputPath:    t.TempDir(),
		Project:       pctx.Project{Name: "racemod", FullPath: dir},
		Params:        params,
	}
}

// goCall is one invocation the job made through its subprocess seam.
type goCall struct {
	args []string
	env  []string
}

// mockRaceGo serves the race job's go invocations: list answers with the
// given packages, env with the given GOOS and CC, and test with a passing
// transcript. It records every call.
func mockRaceGo(t *testing.T, packages []string, goos, cc string) *[]goCall {
	t.Helper()
	var calls []goCall
	mockGoCommandRaw(t, func(_ string, args []string, _ string, env []string) ([]byte, error) {
		calls = append(calls, goCall{args: slices.Clone(args), env: slices.Clone(env)})
		switch args[0] {
		case "list":
			return []byte(strings.Join(packages, "\n") + "\n"), nil
		case "env":
			return json.Marshal(map[string]string{"GOOS": goos, "CC": cc})
		default:
			return []byte(testEvent("pass", "racemod/internal/x", "TestX", "") + "\n"), nil
		}
	})
	return &calls
}

func callsOf(calls []goCall, command string) []goCall {
	var matched []goCall
	for _, call := range calls {
		if call.args[0] == command {
			matched = append(matched, call)
		}
	}
	return matched
}

// onHost makes the job run as on goos, with a C compiler on PATH or none.
func onHost(t *testing.T, goos string, compiler bool) {
	t.Helper()
	originalOS, originalLook := hostOS, lookPath
	hostOS = goos
	lookPath = func(name string) (string, error) {
		if compiler {
			return "/usr/bin/" + name, nil
		}
		return "", errors.New("not found")
	}
	t.Cleanup(func() { hostOS, lookPath = originalOS, originalLook })
}

// runRaceForResult runs the job on ctx and returns its status, its events,
// and the error member of the result event the job's status produces.
func runRaceForResult(t *testing.T, ctx *pctx.Context) (string, []map[string]any, map[string]any) {
	t.Helper()
	var status string
	events := captureJobEvents(t, func(emit *jsonl.Emitter) {
		var data map[string]any
		var err error
		status, data, err = RunRace(ctx, emit, nil)
		if err != nil {
			t.Fatalf("RunRace: %v", err)
		}
		emit.Result(status, data)
	})
	results := eventsOfType(events, "result")
	if len(results) != 1 {
		t.Fatalf("result events = %v, want one", results)
	}
	resultData, _ := results[0]["data"].(map[string]any)
	jobErr, _ := resultData["error"].(map[string]any)
	return status, events, jobErr
}

// diagnosticSays reports whether an error diagnostic carries every one of want.
func diagnosticSays(events []map[string]any, want ...string) bool {
	for _, diagnostic := range eventsOfType(events, "diagnostic") {
		message, _ := diagnostic["message"].(string)
		if diagnostic["severity"] != "error" {
			continue
		}
		if !slices.ContainsFunc(want, func(w string) bool { return !strings.Contains(message, w) }) {
			return true
		}
	}
	return false
}

// signaledError is the error of a command a signal stopped: like an
// exec.ExitError for a killed process, it reports exit code -1.
type signaledError struct{}

func (signaledError) Error() string { return "signal: killed" }
func (signaledError) ExitCode() int { return -1 }

// The job lists the touched directories under -race and runs the packages
// they hold, with cgo on and none of the test step's coverage flags, even
// when the coverage parameters ask for a threshold. It reports the race
// verdict alone: no test counts, summary, test cases or coverage, which the
// test step reports for the same command.
func TestRunRace_RunsTheTouchedPackagesUnderTheDetector(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", raceRequirement, "the-race-step-runs-on-the-packages-the-change-touched")
	spectest.Proves(t, "go/go-project-toolchain", raceRequirement, "the-race-run-measures-no-coverage")
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "go.mod"), "module racemod\n\ngo 1.21\n")
	onHost(t, "linux", true)
	calls := mockRaceGo(t, []string{"racemod/internal/x", "racemod"}, "linux", "gcc")
	ctx := raceContext(t, dir, []string{"internal/x/y.go", "main.go", "internal/x/testdata/a.txt"}, pctx.Params{
		"coverage":           json.RawMessage(`true`),
		"enforce-coverage":   json.RawMessage(`true`),
		"coverage-threshold": json.RawMessage(`99`),
		"coverprofile":       json.RawMessage(`"cover.out"`),
		"count":              json.RawMessage(`"1"`),
	})

	status, events, jobErr := runRaceForResult(t, ctx)
	if status != "OK" || jobErr != nil {
		t.Fatalf("RunRace = %q, error %v; want OK", status, jobErr)
	}

	lists := callsOf(*calls, "list")
	if len(lists) != 1 {
		t.Fatalf("go list calls = %v, want one", lists)
	}
	if args := lists[0].args; !slices.Contains(args, "-race") || !slices.Equal(args[len(args)-2:], []string{".", "./internal/x"}) {
		t.Errorf("go list args = %v, want -race over . and ./internal/x", args)
	}
	tests := callsOf(*calls, "test")
	if len(tests) != 1 {
		t.Fatalf("go test calls = %d, want one", len(tests))
	}
	args := tests[0].args
	if args[1] != "-race" || !slices.Equal(args[len(args)-2:], []string{"racemod", "racemod/internal/x"}) {
		t.Errorf("go test args = %v, want -race over the listed packages, sorted", args)
	}
	for _, arg := range args {
		if strings.HasPrefix(arg, "-cover") || arg == "-outputdir" || arg == "-bench" {
			t.Errorf("go test args = %v carry %s; the race run measures no coverage", args, arg)
		}
	}
	if got := argumentAfter(t, args, "-count"); got != "1" {
		t.Errorf("-count = %q, want the explicit 1", got)
	}
	if !slices.Contains(tests[0].env, "CGO_ENABLED=1") {
		t.Error("go test ran without CGO_ENABLED=1 off darwin")
	}
	if slices.ContainsFunc(tests[0].env, func(entry string) bool {
		return strings.HasPrefix(entry, spectest.FragmentDirEnv+"=") && entry != spectest.FragmentDirEnv+"="
	}) {
		t.Error("the race run collects feature observations; the test step owns the report")
	}
	if entries, err := os.ReadDir(ctx.OutputPath); err != nil || len(entries) != 0 {
		t.Errorf("the race run wrote %v into the command output directory (%v)", entries, err)
	}

	for _, event := range events {
		switch event["type"] {
		case "metric", "summary":
			t.Errorf("the race run emitted a %s event %v; the test step reports the command's counts", event["type"], event)
		case "result":
			data, _ := event["data"].(map[string]any)
			if len(data) != 1 {
				t.Errorf("the race result carries %v; want its status alone", data)
			}
		}
	}
	counted := false
	for _, event := range eventsOfType(events, "log") {
		message, _ := event["message"].(string)
		counted = counted || strings.HasPrefix(message, "Race detector on 2 package(s): 1/1 passed")
	}
	if !counted {
		t.Errorf("no log line reports the race run's counts: %v", events)
	}
}

// The race run's `-timeout` sits below the step's own scheduler deadline and
// ignores --timeout, which the test step's run reads; without a deadline go
// test keeps its default.
func TestRaceTestArgs_TimeoutSitsBelowTheStepDeadline(t *testing.T) {
	params := pctx.Params{"timeout": json.RawMessage(`"90000"`)}
	for deadline, want := range map[string]string{
		"600000":  "570000ms",
		"60000":   "48000ms",
		"1":       "1ms",
		"":        "",
		"soon":    "",
		"-600000": "",
	} {
		args := raceTestArgs(params, false, true, deadline, []string{"racemod"})
		got := ""
		if slices.Contains(args, "-timeout") {
			got = argumentAfter(t, args, "-timeout")
		}
		if got != want {
			t.Errorf("deadline %q: -timeout = %q, want %q (args %v)", deadline, got, want, args)
		}
	}
}

// A darwin host keeps its own cgo setting: the darwin race runtime needs no
// cgo, so a Mac without a C compiler, whose go tool turns cgo off, still runs
// the detector. Off darwin the run forces cgo on.
func TestRunRace_ADarwinHostKeepsItsCgoSetting(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", raceRequirement, "a-host-that-cannot-run-the-race-detector-fails-naming-the-opt-out")
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "go.mod"), "module racemod\n\ngo 1.21\n")
	t.Setenv("CGO_ENABLED", "0")
	for goos, want := range map[string]string{"darwin": "CGO_ENABLED=0", "linux": "CGO_ENABLED=1"} {
		onHost(t, goos, goos != "darwin")
		calls := mockRaceGo(t, []string{"racemod"}, goos, "clang")
		status, _, jobErr := runRaceForResult(t, raceContext(t, dir, []string{"main.go"}, nil))
		if status != "OK" || jobErr != nil {
			t.Fatalf("%s: RunRace = %q, error %v; want OK", goos, status, jobErr)
		}
		tests := callsOf(*calls, "test")
		if len(tests) != 1 || !slices.Contains(tests[0].env, want) {
			t.Errorf("%s: go test env does not hold %s", goos, want)
		}
	}
}

// A change that names no package runs nothing, and neither does a module
// where the touched directories hold no package, a run with --race, whose test
// step runs every package under the detector, or one with --no-race-changed.
func TestRunRace_SkipsWithoutATouchedPackage(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", raceRequirement, "the-race-step-runs-on-the-packages-the-change-touched")
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "go.mod"), "module racemod\n\ngo 1.21\n")
	for name, tc := range map[string]struct {
		files    []string
		params   pctx.Params
		packages []string
		lists    int
	}{
		"no input":                  {files: nil},
		"no Go file":                {files: []string{"README.md", "go.mod"}},
		"a deleted package's files": {files: []string{"gone/gone.go"}, lists: 1},
		"--race": {files: []string{"main.go"}, packages: []string{"racemod"},
			params: pctx.Params{"race": json.RawMessage(`true`)}},
		"--no-race-changed": {files: []string{"main.go"}, packages: []string{"racemod"},
			params: pctx.Params{"race-changed": json.RawMessage(`false`)}},
	} {
		calls := mockRaceGo(t, tc.packages, "linux", "")
		status, _, err := RunRace(raceContext(t, dir, tc.files, tc.params), jsonl.New(), nil)
		if err != nil || status != "SKIP" {
			t.Errorf("%s: RunRace = %q, %v; want SKIP", name, status, err)
		}
		if got := len(callsOf(*calls, "list")); got != tc.lists {
			t.Errorf("%s: go list ran %d times, want %d", name, got, tc.lists)
		}
		if got := callsOf(*calls, "test"); len(got) != 0 {
			t.Errorf("%s: go test ran: %v", name, got)
		}
	}
}

// Off darwin the race runtime links through cgo, so a C compiler that does not
// resolve refuses the run; CC reads as the go tool reads it, quotes and all,
// and every refusal names the flag that turns it off.
func TestRaceCompilerError_NamesTheOptOut(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", raceRequirement, "a-host-that-cannot-run-the-race-detector-fails-naming-the-opt-out")
	found := func(string) (string, error) { return "/usr/bin/cc", nil }
	missing := func(string) (string, error) { return "", errors.New("not found") }
	only := func(path string) func(string) (string, error) {
		return func(name string) (string, error) {
			if name == path {
				return path, nil
			}
			return "", errors.New("not found")
		}
	}
	const mingw = `C:\Program Files\mingw64\bin\gcc.exe`
	for name, tc := range map[string]struct {
		goos, cc string
		look     func(string) (string, error)
		want     string
	}{
		"darwin needs no cgo":       {goos: "darwin", cc: "", look: missing},
		"a resolved compiler":       {goos: "linux", cc: "gcc -m64", look: found},
		"no compiler configured":    {goos: "linux", cc: " ", look: found, want: "names no C compiler"},
		"a compiler not on PATH":    {goos: "linux", cc: "gcc -m64", look: missing, want: `"gcc"`},
		"windows without mingw64":   {goos: "windows", cc: "gcc", look: missing, want: "windows"},
		"a quoted path with spaces": {goos: "windows", cc: `"` + mingw + `" -m64`, look: only(mingw)},
		"a single-quoted path":      {goos: "linux", cc: `'/opt/cross tools/bin/gcc'`, look: only("/opt/cross tools/bin/gcc")},
		"an unquoted path with spaces": {goos: "windows", cc: mingw, look: only(mingw),
			want: `"C:\\Program"`},
		"an unterminated quote": {goos: "linux", cc: `"/opt/cross tools/bin/gcc`, look: found,
			want: "unterminated"},
	} {
		err := raceCompilerError(tc.goos, tc.cc, tc.look)
		if tc.want == "" {
			if err != nil {
				t.Errorf("%s: %v, want none", name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), raceOptOut) {
			t.Errorf("%s: %v, want an error naming %s and %s", name, err, tc.want, raceOptOut)
		}
	}
}

// CC splits the way the go tool splits it: on spaces, tabs and newlines,
// with a single- or double-quoted field kept whole and not unescaped.
func TestSplitCompilerCommand_ReadsCCAsTheGoToolDoes(t *testing.T) {
	for cc, want := range map[string][]string{
		"":                                nil,
		" \t\n":                           nil,
		"gcc":                             {"gcc"},
		"zig cc -target x86_64-linux":     {"zig", "cc", "-target", "x86_64-linux"},
		`"C:\Program Files\gcc.exe" -m64`: {`C:\Program Files\gcc.exe`, "-m64"},
		`'/opt/a b/gcc'`:                  {"/opt/a b/gcc"},
		`gcc "-I/a b" 'x'`:                {"gcc", "-I/a b", "x"},
		`a"b c"`:                          {`a"b`, `c"`},
		`"a\"b"`:                          {`a\`, `b"`},
	} {
		got, err := splitCompilerCommand(cc)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("splitCompilerCommand(%q) = %q, %v; want %q", cc, got, err, want)
		}
	}
	for _, cc := range []string{`"gcc`, `gcc 'x`} {
		if got, err := splitCompilerCommand(cc); err == nil {
			t.Errorf("splitCompilerCommand(%q) = %q; want an unterminated-quote error", cc, got)
		}
	}
}

// A host that cannot run the detector fails the step before go test starts,
// with a diagnostic that carries what go said and names the opt-out, and a
// result marked host-unsupported, which the CLI never replays.
func TestRunRace_RefusesAHostThatCannotRunTheDetector(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", raceRequirement, "a-host-that-cannot-run-the-race-detector-fails-naming-the-opt-out")
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "go.mod"), "module racemod\n\ngo 1.21\n")
	onHost(t, "linux", false)

	for name, tc := range map[string]struct {
		mock func(t *testing.T) *[]goCall
		want string
	}{
		"an unsupported platform": {
			mock: func(t *testing.T) *[]goCall {
				var calls []goCall
				mockGoCommandRaw(t, func(_ string, args []string, _ string, _ []string) ([]byte, error) {
					calls = append(calls, goCall{args: slices.Clone(args)})
					return []byte("-race is not supported on linux/mips\n"), errors.New("exit status 2")
				})
				return &calls
			},
			want: "-race is not supported on linux/mips",
		},
		"no C compiler": {
			mock: func(t *testing.T) *[]goCall { return mockRaceGo(t, []string{"racemod"}, "linux", "gcc") },
			want: `the C compiler "gcc"`,
		},
	} {
		calls := tc.mock(t)
		status, events, jobErr := runRaceForResult(t, raceContext(t, dir, []string{"main.go"}, nil))
		if status != "FAILED" {
			t.Errorf("%s: status = %q, want FAILED", name, status)
		}
		if got := callsOf(*calls, "test"); len(got) != 0 {
			t.Errorf("%s: go test ran on a host that cannot run the detector: %v", name, got)
		}
		if !diagnosticSays(events, tc.want, raceOptOut) {
			t.Errorf("%s: no error diagnostic names %q and %s; events = %+v", name, tc.want, raceOptOut, events)
		}
		if jobErr["code"] != extensionproto.HostUnsupportedFailureCode {
			t.Errorf("%s: result error = %v, want code %s", name, jobErr, extensionproto.HostUnsupportedFailureCode)
		}
	}
}

// A failed race run in which no test ran, such as a race build that does not
// link or a binary the detector cannot start, names the opt-out and is marked
// host-unsupported. A failed test, a race or goroutine limit the race runtime
// reports before any test finished, a test that hangs past the go test timeout
// and a run a signal stopped are verdicts on the change or the run, and are
// not. The timed-out run is marked timed out.
func TestRunRace_ARunInWhichNoTestRanIsAHostFailure(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", raceRequirement, "a-host-that-cannot-run-the-race-detector-fails-naming-the-opt-out")
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "go.mod"), "module racemod\n\ngo 1.21\n")
	onHost(t, "linux", true)
	transcript := func(lines ...string) string { return strings.Join(lines, "\n") + "\n" }
	exitStatus := errors.New("exit status 1")
	for name, tc := range map[string]struct {
		output   string
		runErr   error
		host     bool
		timedOut bool
	}{
		"a race build that does not link": {host: true, runErr: exitStatus, output: transcript(
			testEvent("output", "racemod", "", "# racemod.test\n"),
			testEvent("output", "racemod", "", "runtime/race: undefined reference to __libc_stack_end\n"),
			testEvent("fail", "racemod", "", ""))},
		"a detector that cannot start": {host: true, runErr: exitStatus, output: transcript(
			testEvent("output", "racemod", "", "FATAL: ThreadSanitizer: unsupported VMA range\n"),
			testEvent("fail", "racemod", "", ""))},
		"a failed test": {runErr: exitStatus, output: transcript(
			testEvent("run", "racemod", "TestX", ""),
			testEvent("fail", "racemod", "TestX", ""),
			testEvent("fail", "racemod", "", ""))},
		"a race before any test": {runErr: exitStatus, output: transcript(
			testEvent("output", "racemod", "", "WARNING: DATA RACE\n"),
			testEvent("fail", "racemod", "", ""))},
		"a test that hangs past the timeout": {runErr: exitStatus, timedOut: true, output: transcript(
			testEvent("run", "racemod", "TestHang", ""),
			testEvent("output", "racemod", "TestHang", "=== RUN   TestHang\n"),
			testEvent("output", "racemod", "", "panic: test timed out after 2s\n"),
			testEvent("fail", "racemod", "", ""))},
		"a run a signal stopped": {runErr: signaledError{}, output: transcript(
			testEvent("run", "racemod", "TestSlow", ""),
			testEvent("output", "racemod", "TestSlow", "=== RUN   TestSlow\n"))},
		"the race runtime's goroutine limit": {runErr: exitStatus, output: transcript(
			testEvent("run", "racemod", "TestFanOut", ""),
			testEvent("output", "racemod", "", "race: limit on 8128 simultaneously alive goroutines is exceeded, dying\n"),
			testEvent("fail", "racemod", "", ""))},
	} {
		mockGoCommandRaw(t, func(_ string, args []string, _ string, _ []string) ([]byte, error) {
			switch args[0] {
			case "list":
				return []byte("racemod\n"), nil
			case "env":
				return json.Marshal(map[string]string{"GOOS": "linux", "CC": "gcc"})
			}
			return []byte(tc.output), tc.runErr
		})
		status, events, jobErr := runRaceForResult(t, raceContext(t, dir, []string{"main.go"}, nil))
		if status != "FAILED" {
			t.Errorf("%s: status = %q, want FAILED", name, status)
		}
		if got := jobErr["code"] == extensionproto.HostUnsupportedFailureCode; got != tc.host {
			t.Errorf("%s: host-unsupported = %v (error %v), want %v", name, got, jobErr, tc.host)
		}
		if got := jobErr["timedOut"] == true; got != tc.timedOut {
			t.Errorf("%s: timed out = %v (error %v), want %v", name, got, jobErr, tc.timedOut)
		}
		if got := diagnosticSays(events, "No test ran under the race detector", raceOptOut); got != tc.host {
			t.Errorf("%s: opt-out diagnostic = %v, want %v; events = %+v", name, got, tc.host, events)
		}
	}
}

// With the real toolchain, a racy package the change did not touch never
// runs, and the touched one fails with the race the detector reports.
func TestRunRace_TheDetectorRunsOnTheTouchedPackageOnly(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", raceRequirement, "the-race-step-runs-on-the-packages-the-change-touched")
	goBinary, err := toolchain.ResolveGo()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "go.mod"), "module racemod\n\ngo 1.21\n")
	for _, pkg := range []string{"clean", "racy"} {
		if err := os.MkdirAll(filepath.Join(dir, pkg), 0o755); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(dir, pkg, pkg+".go"), "package "+pkg+"\n")
	}
	mustWrite(t, filepath.Join(dir, "clean", "clean_test.go"),
		"package clean\n\nimport \"testing\"\n\nfunc TestClean(t *testing.T) {}\n")
	mustWrite(t, filepath.Join(dir, "racy", "racy_test.go"), strings.Join([]string{
		"package racy",
		"",
		"import \"testing\"",
		"",
		"func TestRacy(t *testing.T) {",
		"\tvar n int",
		"\tdone := make(chan struct{})",
		"\tgo func() { n++; close(done) }()",
		"\tn++",
		"\t<-done",
		"}",
		"",
	}, "\n"))
	env := append(toolchain.WorkspaceBuildEnv(nil, dir, goBinary), "CGO_ENABLED=1")
	if _, err := listRacePackages(goBinary, dir, env, []string{"."}); err != nil {
		t.Skipf("this host cannot run the race detector: %v", err)
	}
	if err := checkRaceCompiler(goBinary, dir, env); err != nil {
		t.Skipf("this host cannot run the race detector: %v", err)
	}

	status, _, jobErr := runRaceForResult(t, raceContext(t, dir, []string{"clean/clean.go"}, nil))
	if status != "OK" || jobErr != nil {
		t.Errorf("a change to the clean package: RunRace = %q, error %v; want OK, the racy package untouched", status, jobErr)
	}
	status, events, jobErr := runRaceForResult(t, raceContext(t, dir, []string{"racy/racy_test.go"}, nil))
	if status != "FAILED" {
		t.Fatalf("a change to the racy package: RunRace = %q; want FAILED", status)
	}
	if jobErr["code"] == extensionproto.HostUnsupportedFailureCode {
		t.Errorf("a detected race is marked host-unsupported: %v", jobErr)
	}
	if !diagnosticSays(events, "race detected") && !diagnosticSays(events, "DATA RACE") {
		t.Errorf("no error diagnostic reports the race; events = %+v", eventsOfType(events, "diagnostic"))
	}
}

// A changed package whose test hangs under the detector fails as timed out,
// below the step's deadline, and is not reported as a host that cannot run
// the detector.
func TestRunRace_AHungTestIsATimeoutNotAHostFailure(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", raceRequirement, "a-host-that-cannot-run-the-race-detector-fails-naming-the-opt-out")
	goBinary, err := toolchain.ResolveGo()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "go.mod"), "module racemod\n\ngo 1.21\n")
	mustWrite(t, filepath.Join(dir, "hang.go"), "package racemod\n")
	mustWrite(t, filepath.Join(dir, "hang_test.go"), strings.Join([]string{
		"package racemod",
		"",
		"import (",
		"\t\"testing\"",
		"\t\"time\"",
		")",
		"",
		"func TestHang(t *testing.T) { time.Sleep(time.Hour) }",
		"",
	}, "\n"))
	env := append(toolchain.WorkspaceBuildEnv(nil, dir, goBinary), "CGO_ENABLED=1")
	if _, err := listRacePackages(goBinary, dir, env, []string{"."}); err != nil {
		t.Skipf("this host cannot run the race detector: %v", err)
	}
	if err := checkRaceCompiler(goBinary, dir, env); err != nil {
		t.Skipf("this host cannot run the race detector: %v", err)
	}
	t.Setenv(extensionproto.TaskDeadlineMsEnv, "3000")

	status, events, jobErr := runRaceForResult(t, raceContext(t, dir, []string{"hang_test.go"}, nil))
	if status != "FAILED" || jobErr["timedOut"] != true {
		t.Fatalf("a hung test: RunRace = %q, error %v; want FAILED and timed out", status, jobErr)
	}
	if jobErr["code"] == extensionproto.HostUnsupportedFailureCode {
		t.Errorf("a hung test is marked host-unsupported: %v", jobErr)
	}
	if diagnosticSays(events, "No test ran under the race detector") {
		t.Errorf("a hung test names the race detector opt-out; events = %+v", eventsOfType(events, "diagnostic"))
	}
}
