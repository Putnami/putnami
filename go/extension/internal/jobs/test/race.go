package test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"

	"go.putnami.dev/go/extension/internal/parse"
	"go.putnami.dev/go/extension/internal/toolchain"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/scratch"
)

// raceOptOut is the flag that drops the race step, named by every refusal so a
// host that cannot run the detector has a way forward.
const raceOptOut = "--no-race-changed"

// raceDeadlineMarginCapMs caps the part of the step's scheduler deadline the
// race run keeps to report its own timeout.
const raceDeadlineMarginCapMs int64 = 30_000

// lookPath resolves the C compiler `go env CC` names. The test environment
// inherits this process's PATH, so the answer is the one `go` gets. Tests
// replace it to take the compiler away.
var lookPath = exec.LookPath

// hostOS is the platform `go test` builds for: the one this process runs on.
// Tests replace it.
var hostOS = goruntime.GOOS

// RunRace runs `go test -race` on the packages a change touched: the
// directory of each changed Go file the project owns, and the package that
// holds a changed testdata directory. The CLI binds those files,
// project-relative, as the changed-files input and keys the step on them, so a
// stored verdict answers the same change only. The step runs after the test
// step, and reports no test counts, test cases, coverage or feature
// verification report: the test step owns them, and the race verdict is the
// step's status and its failure diagnostics. A refusal of `go list -race`,
// a C compiler check that fails, and a failed run in which no test ran are
// marked host-unsupported.
func RunRace(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
	if reason := raceSkipReason(ctx.Params); reason != "" {
		emit.Log("info", reason)
		return "SKIP", nil, nil
	}
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

	goBinary, err := toolchain.ResolveGo()
	if err != nil {
		return "FAILED", nil, err
	}
	env, err := buildTestEnv(ctx, dir, raceNeedsCgo(hostOS), goBinary)
	if err != nil {
		emit.Diagnostic("error", err.Error(), "", 0)
		return "FAILED", nil, nil
	}
	// The race run writes no feature verification fragment.
	env = setEnv(env, spectest.FragmentDirEnv, "")

	emit.PhaseStart("discover")
	packages, err := listRacePackages(goBinary, dir, env, dirs)
	if err == nil && len(packages) > 0 {
		err = checkRaceCompiler(goBinary, dir, env)
	}
	if err != nil {
		emit.PhaseEnd("discover", "failed")
		emit.Diagnostic("error", err.Error(), "", 0)
		emit.MarkHostUnsupported()
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
	return runRacePackages(ctx, emit, goBinary, env, packages), nil, nil
}

// runRacePackages runs `go test -race` over packages, reports the transcript,
// one info line with the counts and on failure the failure diagnostics, and
// returns the status.
func runRacePackages(ctx *pctx.Context, emit *jsonl.Emitter, goBinary string, env, packages []string) string {
	dir := ctx.Project.FullPath
	verbose := ctx.Params.Bool("test-verbose", false, "testVerbose", "verbose", "v")
	useJSON := ctx.Params.Bool("test-json", true, "testJson", "json")

	emit.PhaseStart("run")
	emit.Progress(1, 2, fmt.Sprintf("Running %d package(s) under the race detector...", len(packages)))
	args := raceTestArgs(ctx.Params, verbose, useJSON, os.Getenv(extensionproto.TaskDeadlineMsEnv), packages)
	output, runErr := runGoCommand(goBinary, args, dir, env)
	outputStr := string(output)
	testFailed := runErr != nil
	emitTestTranscript(emit, outputStr, useJSON, verbose, "")
	if testFailed {
		emit.PhaseEnd("run", "failed")
	} else {
		emit.PhaseEnd("run", "success")
	}

	emit.PhaseStart("parse")
	var counts parse.TestCounts
	if useJSON {
		counts = parse.TestJSON(outputStr)
	}
	var failureDiagnostics []parse.ToolDiagnostic
	failureDetailsTruncated := 0
	if testFailed {
		locate := parse.ModuleTestFileLocator(ctx.WorkspaceRoot, dir, readModulePath(dir))
		failureDiagnostics, failureDetailsTruncated = boundFailureDiagnostics(
			parse.TestFailureDiagnosticList(outputStr, locate),
		)
	}
	if summary := formatTestSummary(counts, testFailed, false, 0, false, failureDetailsTruncated); summary != "" {
		emit.Log("info", fmt.Sprintf("Race detector on %d package(s): %s", len(packages), summary))
	}
	emit.PhaseEnd("parse", "success")
	emit.Progress(2, 2, "Done")

	if !testFailed {
		return "OK"
	}
	emitFailureDiagnostics(emit, failureDiagnostics)
	if parse.TestTimedOut(outputStr) {
		emit.MarkTimedOut()
	}
	if raceRunDidNotStart(counts, useJSON, outputStr) {
		emit.Diagnostic("error", fmt.Sprintf("No test ran under the race detector, and the test step passed on the same files: "+
			"this host does not build or run -race test binaries. Pass %s to skip the race detector", raceOptOut), "", 0)
		emit.MarkHostUnsupported()
	}
	return "FAILED"
}

// raceSkipReason names why this run does not race the touched packages, or ""
// when it does: --race runs every package under the detector in the test step,
// and race-changed off turns the step off. The step reads both at run time.
func raceSkipReason(params pctx.Params) string {
	switch {
	case params.Bool("race", false):
		return "--race ran every package under the race detector in the test step"
	case !params.Bool("race-changed", true, "raceChanged"):
		return raceOptOut + " turns the race detector off"
	}
	return ""
}

// raceNeedsCgo reports whether `go test -race` on goos builds through cgo.
// The darwin race runtime needs no cgo, so a darwin host keeps its own cgo
// setting, and one without a C compiler still runs the detector.
func raceNeedsCgo(goos string) bool { return goos != "darwin" }

// raceRunDidNotStart reports whether a failed race run ran no test, from its
// `go test -json` transcript: no test reported an outcome and the detector
// reported no race. Without the JSON transcript it cannot tell, and says no.
func raceRunDidNotStart(counts parse.TestCounts, useJSON bool, output string) bool {
	return useJSON && counts.Total() == 0 && !strings.Contains(output, "WARNING: DATA RACE")
}

// changedFiles reads the changed-files input: the project-relative paths the
// CLI bound to the step. An absent input names no file.
func changedFiles(params pctx.Params) ([]string, error) {
	raw, ok := params["changed-files"]
	if !ok {
		return nil, nil
	}
	var files []string
	if err := json.Unmarshal(raw, &files); err != nil {
		return nil, fmt.Errorf("the changed-files input is not a list of paths: %w", err)
	}
	return files, nil
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

// checkRaceCompiler fails when `go test -race` would not link on this host:
// off darwin the race runtime links through cgo, so the C compiler `go env CC`
// names must resolve.
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
// the C compiler cc, or nil when it can. cc reads as the go tool reads CC
// (splitCompilerCommand), and its first field is the compiler.
func raceCompilerError(goos, cc string, look func(string) (string, error)) error {
	if !raceNeedsCgo(goos) {
		return nil
	}
	fields, err := splitCompilerCommand(cc)
	if err != nil {
		return fmt.Errorf("-race on %s builds through cgo, and go env CC %q does not parse: %v; "+
			"quote it as the go tool expects, or pass %s to skip the race detector", goos, cc, err, raceOptOut)
	}
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

// splitCompilerCommand splits a CC value into fields the way the go tool
// does: fields are separated by spaces, tabs and newlines, and a field may be
// wrapped in single or double quotes, which keep the spaces inside it and
// unescape nothing. An unterminated quote is an error.
func splitCompilerCommand(cc string) ([]string, error) {
	var fields []string
	for {
		cc = strings.TrimLeft(cc, " \t\n\r")
		if cc == "" {
			return fields, nil
		}
		if quote := cc[0]; quote == '"' || quote == '\'' {
			end := strings.IndexByte(cc[1:], quote)
			if end < 0 {
				return nil, fmt.Errorf("unterminated %c string", quote)
			}
			fields = append(fields, cc[1:end+1])
			cc = cc[end+2:]
			continue
		}
		end := strings.IndexAny(cc, " \t\n\r")
		if end < 0 {
			end = len(cc)
		}
		fields = append(fields, cc[:end])
		cc = cc[end:]
	}
}

// raceTestArgs builds the `go test -race` argument list over packages. It
// carries the test step's selection and scheduling flags and none of its
// coverage or benchmark flags. --timeout does not apply: the race run's
// `-timeout` sits below the step's own scheduler deadline (deadline, the
// TaskDeadlineMsEnv value), by a fifth of it and at most
// raceDeadlineMarginCapMs, and without a deadline go test keeps its default.
func raceTestArgs(params pctx.Params, verbose, useJSON bool, deadline string, packages []string) []string {
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
	args = appendTimeoutArg(args, raceTimeoutMs(deadline))
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

// raceTimeoutMs derives the race run's `-timeout`, in milliseconds, from the
// step's scheduler deadline: deadline - min(max(deadline/5, 1), cap), at
// least 1. An absent or unusable deadline derives none.
func raceTimeoutMs(deadline string) string {
	deadlineMs, err := strconv.ParseInt(strings.TrimSpace(deadline), 10, 64)
	if err != nil || deadlineMs <= 0 {
		return ""
	}
	margin := min(max(deadlineMs/5, 1), raceDeadlineMarginCapMs)
	return strconv.FormatInt(max(deadlineMs-margin, 1), 10)
}
