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

// The job lists the touched directories under -race and runs the packages
// they hold, with cgo on and none of the test step's coverage flags, even
// when the coverage parameters ask for a threshold.
func TestRunRace_RunsTheTouchedPackagesUnderTheDetector(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", raceRequirement, "the-race-step-runs-on-the-packages-the-change-touched")
	spectest.Proves(t, "go/go-project-toolchain", raceRequirement, "the-race-run-measures-no-coverage")
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "go.mod"), "module racemod\n\ngo 1.21\n")
	calls := mockRaceGo(t, []string{"racemod/internal/x", "racemod"}, "darwin", "clang")
	ctx := raceContext(t, dir, []string{"internal/x/y.go", "main.go", "internal/x/testdata/a.txt"}, pctx.Params{
		"coverage":           json.RawMessage(`true`),
		"enforce-coverage":   json.RawMessage(`true`),
		"coverage-threshold": json.RawMessage(`99`),
		"coverprofile":       json.RawMessage(`"cover.out"`),
		"count":              json.RawMessage(`"1"`),
		"timeout":            json.RawMessage(`"90000"`),
	})

	status, data, err := RunRace(ctx, jsonl.New(), nil)
	if err != nil || status != "OK" {
		t.Fatalf("RunRace = %q, %v; want OK", status, err)
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
	if got := argumentAfter(t, args, "-timeout"); got != "90000ms" {
		t.Errorf("-timeout = %q, want 90000ms", got)
	}
	if !slices.Contains(tests[0].env, "CGO_ENABLED=1") {
		t.Error("go test ran without CGO_ENABLED=1")
	}
	if slices.ContainsFunc(tests[0].env, func(entry string) bool {
		return strings.HasPrefix(entry, spectest.FragmentDirEnv+"=") && entry != spectest.FragmentDirEnv+"="
	}) {
		t.Error("the race run collects feature observations; the test step owns the report")
	}
	if _, ok := data["coverageSummary"]; ok {
		t.Errorf("result data %v carries a coverage summary", data)
	}
	if entries, err := os.ReadDir(ctx.OutputPath); err != nil || len(entries) != 0 {
		t.Errorf("the race run wrote %v into the command output directory (%v)", entries, err)
	}
}

// A change that names no package runs nothing, and neither does a module
// where the touched directories hold no package.
func TestRunRace_SkipsWithoutATouchedPackage(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", raceRequirement, "the-race-step-runs-on-the-packages-the-change-touched")
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "go.mod"), "module racemod\n\ngo 1.21\n")
	for name, tc := range map[string]struct {
		files    []string
		packages []string
		lists    int
	}{
		"no input":                  {files: nil},
		"no Go file":                {files: []string{"README.md", "go.mod"}},
		"a deleted package's files": {files: []string{"gone/gone.go"}, lists: 1},
	} {
		calls := mockRaceGo(t, tc.packages, "linux", "")
		status, _, err := RunRace(raceContext(t, dir, tc.files, nil), jsonl.New(), nil)
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
// resolve refuses the run; every refusal names the flag that turns it off.
func TestRaceCompilerError_NamesTheOptOut(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", raceRequirement, "a-host-that-cannot-run-the-race-detector-fails-naming-the-opt-out")
	found := func(string) (string, error) { return "/usr/bin/cc", nil }
	missing := func(string) (string, error) { return "", errors.New("not found") }
	for name, tc := range map[string]struct {
		goos, cc string
		look     func(string) (string, error)
		want     string
	}{
		"darwin needs no cgo":     {goos: "darwin", cc: "", look: missing},
		"a resolved compiler":     {goos: "linux", cc: "gcc -m64", look: found},
		"no compiler configured":  {goos: "linux", cc: " ", look: found, want: "names no C compiler"},
		"a compiler not on PATH":  {goos: "linux", cc: "gcc -m64", look: missing, want: `"gcc"`},
		"windows without mingw64": {goos: "windows", cc: "gcc", look: missing, want: "windows"},
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

// A host that cannot run the detector fails the step before go test starts,
// with a diagnostic that carries what go said and names the opt-out.
func TestRunRace_RefusesAHostThatCannotRunTheDetector(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", raceRequirement, "a-host-that-cannot-run-the-race-detector-fails-naming-the-opt-out")
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "go.mod"), "module racemod\n\ngo 1.21\n")
	original := lookPath
	lookPath = func(string) (string, error) { return "", errors.New("not found") }
	t.Cleanup(func() { lookPath = original })

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
		var calls *[]goCall
		var status string
		events := captureJobEvents(t, func(emit *jsonl.Emitter) {
			calls = tc.mock(t)
			status, _, _ = RunRace(raceContext(t, dir, []string{"main.go"}, nil), emit, nil)
		})
		if status != "FAILED" {
			t.Errorf("%s: status = %q, want FAILED", name, status)
		}
		if got := callsOf(*calls, "test"); len(got) != 0 {
			t.Errorf("%s: go test ran on a host that cannot run the detector: %v", name, got)
		}
		named := false
		for _, diagnostic := range eventsOfType(events, "diagnostic") {
			message, _ := diagnostic["message"].(string)
			named = named || (diagnostic["severity"] == "error" && strings.Contains(message, tc.want) &&
				strings.Contains(message, raceOptOut))
		}
		if !named {
			t.Errorf("%s: no error diagnostic names %q and %s; events = %+v", name, tc.want, raceOptOut, events)
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

	status, _, err := RunRace(raceContext(t, dir, []string{"clean/clean.go"}, nil), jsonl.New(), nil)
	if err != nil || status != "OK" {
		t.Errorf("a change to the clean package: RunRace = %q, %v; want OK, the racy package untouched", status, err)
	}
	var data map[string]any
	status, data, err = RunRace(raceContext(t, dir, []string{"racy/racy_test.go"}, nil), jsonl.New(), nil)
	if err != nil || status != "FAILED" {
		t.Fatalf("a change to the racy package: RunRace = %q, %v; want FAILED", status, err)
	}
	summary, _ := data["testSummary"].(map[string]any)
	if summary == nil || summary["failed"] != 1 {
		t.Errorf("test summary = %v, want the racy test failed", data["testSummary"])
	}
}
