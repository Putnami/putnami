package test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"go.putnami.dev/go/extension/internal/parse"
	"go.putnami.dev/go/extension/internal/toolchain"
	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/scratch"
)

// raceOptOut is the flag that drops the race step, named by every refusal so a
// host that cannot run the detector has a way forward.
const raceOptOut = "--no-race-changed"

// lookPath resolves the C compiler `go env CC` names. The test environment
// inherits this process's PATH, so the answer is the one `go` gets. Tests
// replace it to take the compiler away.
var lookPath = exec.LookPath

// RunRace runs `go test -race` on the packages a change touched: the
// directory of each changed Go file the project owns, and the package that
// holds a changed testdata directory. The CLI binds those files,
// project-relative, as the changed-files input and keys the step on them, so a
// stored verdict answers the same change only. The run measures no coverage
// and publishes no feature verification report: the test step owns both.
func RunRace(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
	files, err := changedFiles(ctx.Params)
	if err != nil {
		emit.Diagnostic("error", err.Error(), "", 0)
		return "FAILED", nil, nil
	}
	dir := ctx.Project.FullPath
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		emit.Log("info", "No go.mod found, skipping the race detector")
		return "SKIP", nil, nil
	}
	dirs := racePackageDirs(files)
	if len(dirs) == 0 {
		emit.Log("info", "No changed Go package, skipping the race detector")
		return "SKIP", nil, nil
	}
	verbose := ctx.Params.Bool("test-verbose", false, "testVerbose", "verbose", "v")
	useJSON := ctx.Params.Bool("test-json", true, "testJson", "json")

	goBinary, err := toolchain.ResolveGo()
	if err != nil {
		return "FAILED", nil, err
	}
	env, err := buildTestEnv(ctx, dir, true, goBinary)
	if err != nil {
		emit.Diagnostic("error", err.Error(), "", 0)
		return "FAILED", nil, nil
	}
	// An inherited fragment directory would collect this run's observations
	// into a report the test step owns.
	env = setEnv(env, spectest.FragmentDirEnv, "")

	emit.PhaseStart("discover")
	packages, err := listRacePackages(goBinary, dir, env, dirs)
	if err == nil && len(packages) > 0 {
		err = checkRaceCompiler(goBinary, dir, env)
	}
	if err != nil {
		emit.PhaseEnd("discover", "failed")
		emit.Diagnostic("error", err.Error(), "", 0)
		return "FAILED", nil, nil
	}
	if len(packages) == 0 {
		emit.PhaseEnd("discover", "skipped")
		emit.Log("info", "No changed directory holds a Go package, skipping the race detector")
		return "SKIP", nil, nil
	}
	emit.PhaseEnd("discover", "success")

	if goScratch, err := scratch.New("putnami-go-tmp-"); err != nil {
		emit.Diagnostic("warning", "create Go work directory scratch: "+err.Error(), "", 0)
	} else {
		defer func() { _ = goScratch.Remove() }()
		if env, err = withGoTempDir(env, goScratch.Path()); err != nil {
			emit.Diagnostic("warning", err.Error(), "", 0)
		}
	}

	emit.PhaseStart("run")
	emit.Progress(1, 2, fmt.Sprintf("Running %d package(s) under the race detector...", len(packages)))
	output, runErr := runGoCommand(goBinary, raceTestArgs(ctx.Params, verbose, useJSON, packages), dir, env)
	outputStr := string(output)
	testFailed := runErr != nil
	emitTestTranscript(emit, outputStr, useJSON, verbose, "")
	if testFailed {
		emit.PhaseEnd("run", "failed")
	} else {
		emit.PhaseEnd("run", "success")
	}

	emit.PhaseStart("parse")
	var testCounts parse.TestCounts
	if useJSON {
		testCounts = parse.TestJSON(outputStr)
		emitTestCounts(emit, testCounts)
	}
	locate := parse.ModuleTestFileLocator(ctx.WorkspaceRoot, dir, readModulePath(dir))
	var failureDiagnostics []parse.ToolDiagnostic
	failureDetailsTruncated := 0
	if testFailed {
		failureDiagnostics, failureDetailsTruncated = boundFailureDiagnostics(
			parse.TestFailureDiagnosticList(outputStr, locate),
		)
	}
	if summary := formatTestSummary(testCounts, testFailed, false, 0, false, failureDetailsTruncated); summary != "" {
		emit.Summary(summary)
	}
	emit.PhaseEnd("parse", "success")
	emit.Progress(2, 2, "Done")

	resultData := buildResultData(testCounts, parse.CoverageResult{}, false, 0)
	recordFailureDetailsTruncated(resultData, failureDetailsTruncated)
	recordTestCases(resultData, moduleTestCases(outputStr, useJSON, locate), protocolcli.TestCaseMaxBytesPerTask)
	if testFailed {
		emitFailureDiagnostics(emit, failureDiagnostics)
		if parse.TestTimedOut(outputStr) {
			emit.MarkTimedOut()
		}
		return "FAILED", resultData, nil
	}
	return "OK", resultData, nil
}

// changedFiles reads the changed-files input: the project-relative paths the
// CLI bound to the step. An absent input names no file.
func changedFiles(params pctx.Params) ([]string, error) {
	for _, key := range []string{"changed-files", "changedFiles"} {
		raw, ok := params[key]
		if !ok {
			continue
		}
		var files []string
		if err := json.Unmarshal(raw, &files); err != nil {
			return nil, fmt.Errorf("the %s input is not a list of paths: %w", key, err)
		}
		return files, nil
	}
	return nil, nil
}

// racePackageDirs maps changed project-relative files to the directories of
// the packages whose tests they can change, sorted and unique. A Go file names
// its own directory. A file under a testdata directory names the directory
// that holds testdata: the go tool ignores testdata, and the tests beside it
// read it. Any other file names no package, and a path that leaves the project
// is dropped.
func racePackageDirs(files []string) []string {
	var dirs []string
	for _, file := range files {
		file = path.Clean(filepath.ToSlash(file))
		if path.IsAbs(file) || file == ".." || strings.HasPrefix(file, "../") {
			continue
		}
		segments := strings.Split(file, "/")
		if i := slices.Index(segments[:len(segments)-1], "testdata"); i >= 0 {
			dirs = append(dirs, path.Join(append([]string{"."}, segments[:i]...)...))
			continue
		}
		if strings.HasSuffix(file, ".go") {
			dirs = append(dirs, path.Dir(file))
		}
	}
	slices.Sort(dirs)
	return slices.Compact(dirs)
}

// listRacePackages names the packages in dirs that `go list -race` resolves
// in the module rooted at dir, sorted. `-e` and the template drop a directory
// that holds no package: one whose Go files were all deleted, a nested module,
// or a directory the go tool ignores. `-race` makes the listing fail on a
// platform the race detector does not support, before anything builds.
func listRacePackages(goBinary, dir string, env []string, dirs []string) ([]string, error) {
	args := []string{"list", "-race", "-e", "-f", "{{if not .Error}}{{.ImportPath}}{{end}}"}
	for _, d := range dirs {
		if d == "." {
			args = append(args, ".")
		} else {
			args = append(args, "./"+d)
		}
	}
	output, err := runGoCommand(goBinary, args, dir, env)
	if err != nil {
		message := fmt.Sprintf("go list -race in %s: %v", dir, err)
		if said := strings.TrimSpace(string(output)); said != "" {
			message += "\n" + said
		}
		return nil, fmt.Errorf("%s\nIf this host cannot run the race detector, pass %s to skip it", message, raceOptOut)
	}
	var packages []string
	for _, line := range strings.Split(string(output), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			packages = append(packages, line)
		}
	}
	slices.Sort(packages)
	return slices.Compact(packages), nil
}

// checkRaceCompiler fails when `go test -race` would not link on this host.
// Off darwin the race runtime links through cgo, so the C compiler `go env CC`
// names must resolve; `go list -race` does not check it, and the build failure
// `go test` reports instead names no way forward.
func checkRaceCompiler(goBinary, dir string, env []string) error {
	output, err := runGoCommand(goBinary, []string{"env", "-json", "GOOS", "CC"}, dir, env)
	var goEnv struct{ GOOS, CC string }
	if err == nil {
		err = json.Unmarshal(output, &goEnv)
	}
	if err != nil {
		return fmt.Errorf("go env GOOS CC in %s: %w\n%s", dir, err, strings.TrimSpace(string(output)))
	}
	return raceCompilerError(goEnv.GOOS, goEnv.CC, lookPath)
}

// raceCompilerError reports why the race detector cannot link for goos with
// the C compiler cc, or nil when it can. Since Go 1.20 the darwin race runtime
// needs no cgo.
func raceCompilerError(goos, cc string, look func(string) (string, error)) error {
	if goos == "darwin" {
		return nil
	}
	fields := strings.Fields(cc)
	if len(fields) == 0 {
		return fmt.Errorf("-race on %s builds through cgo, and go env CC names no C compiler; "+
			"install one or set CC, or pass %s to skip the race detector", goos, raceOptOut)
	}
	if _, err := look(fields[0]); err != nil {
		return fmt.Errorf("-race on %s builds through cgo, and the C compiler %q (go env CC) is not on PATH; "+
			"install it or set CC, or pass %s to skip the race detector", goos, fields[0], raceOptOut)
	}
	return nil
}

// raceTestArgs builds the `go test -race` argument list over packages. It
// carries the test step's selection and scheduling flags and none of its
// coverage or benchmark flags: the detector's run answers whether the touched
// packages race, not how much they cover.
func raceTestArgs(params pctx.Params, verbose, useJSON bool, packages []string) []string {
	args := []string{"test", "-race"}
	if params.Bool("short", false) {
		args = append(args, "-short")
	}
	if count := params.String("count"); count != "" {
		args = append(args, "-count", count)
	}
	if params.Bool("shuffle", false) {
		args = append(args, "-shuffle", "on")
	}
	args = appendTimeoutArg(args, params.String("timeout"))
	if runPattern := params.String("run"); runPattern != "" {
		args = append(args, "-run", runPattern)
	}
	if verbose {
		args = append(args, "-v")
	}
	if useJSON {
		args = append(args, "-json")
	}
	args = appendParallelArgs(args, params.String("package-parallel", "packageParallel"), params.String("parallel"))
	if params.Bool("failfast", false) {
		args = append(args, "-failfast")
	}
	return append(args, packages...)
}
