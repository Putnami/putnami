package deliverycli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// shellTestSeams routes the interpreter discovery and the harness execution at
// test doubles so the hook's decisions are exercised without running a suite.
type shellTestSeams struct {
	lookErr error
	runErr  error
	// output is the harness transcript the stubbed run replays through the
	// capture, so the failure report is exercised on real-shaped lines.
	output []string

	ran        int
	gotShell   string
	gotHarness string
	gotDir     string
}

func installShellTestSeams(t *testing.T, s *shellTestSeams) {
	t.Helper()
	prevLook, prevRun := shellTestLookPath, shellTestRun
	t.Cleanup(func() { shellTestLookPath, shellTestRun = prevLook, prevRun })
	shellTestLookPath = func(string) (string, error) {
		if s.lookErr != nil {
			return "", s.lookErr
		}
		return "/bin/bash", nil
	}
	shellTestRun = func(shell, harness, dir string, ioctx clicore.IO) (*shellTestCapture, error) {
		s.ran++
		s.gotShell, s.gotHarness, s.gotDir = shell, harness, dir
		capture := &shellTestCapture{}
		for _, line := range s.output {
			capture.observe(line)
			ioctx.Stdout(line)
		}
		return capture, s.runErr
	}
}

// exitStatusError is a runErr that carries a real process exit status, so the
// hook's status recovery is exercised the way exec.Cmd delivers it.
func exitStatusError(t *testing.T, status int) error {
	t.Helper()
	cmd := exec.Command("sh", "-c", "exit "+strconv.Itoa(status)) //nolint:gosec // G204: a literal status in a test
	err := cmd.Run()
	if err == nil {
		t.Fatalf("sh -c 'exit %d' unexpectedly succeeded", status)
	}
	return err
}

// shellTestProject materializes a project carrying the hook's activation marker.
func shellTestProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "tests"), 0o750); err != nil {
		t.Fatalf("mkdir tests: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, shellTestHarness), []byte("#!/usr/bin/env bash\n"), 0o600); err != nil {
		t.Fatalf("write harness: %v", err)
	}
	return dir
}

// TestShellTestRunsTheProjectHarness pins the happy path: the resolved
// interpreter runs the project's own committed harness, with the project as cwd
// so the harness can address its fixtures relatively.
func TestShellTestRunsTheProjectHarness(t *testing.T) {
	seams := &shellTestSeams{}
	installShellTestSeams(t, seams)
	dir := shellTestProject(t)

	var out []string
	err := ShellTest(map[string]any{"json": true, "project": dir}, nil, "", nil,
		clicore.IO{Stdout: func(line string) { out = append(out, line) }, Stderr: func(string) {}})
	if err != nil {
		t.Fatalf("ShellTest: %v", err)
	}
	if seams.ran != 1 {
		t.Fatalf("harness runs = %d, want 1", seams.ran)
	}
	if seams.gotShell != "/bin/bash" {
		t.Fatalf("interpreter = %q, want the resolved bash", seams.gotShell)
	}
	if want := filepath.Join(dir, shellTestHarness); seams.gotHarness != want {
		t.Fatalf("harness = %q, want %q", seams.gotHarness, want)
	}
	if seams.gotDir != dir {
		t.Fatalf("cwd = %q, want the project root %q", seams.gotDir, dir)
	}
	if got := decodeCommandJSON(t, out); got["status"] != "passed" {
		t.Fatalf("status = %v, want passed", got["status"])
	}
}

// TestShellTestFailsWhenTheHarnessFails is the whole point of the hook: the
// harness's exit status IS the verdict, so a failing suite must surface as a
// failing job rather than a diagnostic on a green one.
func TestShellTestFailsWhenTheHarnessFails(t *testing.T) {
	seams := &shellTestSeams{runErr: errors.New("exit status 1")}
	installShellTestSeams(t, seams)

	err := ShellTest(map[string]any{"project": shellTestProject(t)}, nil, "", nil,
		clicore.IO{Stdout: func(string) {}, Stderr: func(string) {}})
	if err == nil {
		t.Fatal("a failing harness must fail the job")
	}
	if !strings.Contains(err.Error(), shellTestHarness) {
		t.Fatalf("failure must name the harness, got %v", err)
	}
}

// TestShellTestFailureCarriesTheHarnessExitStatus is the second acceptance
// criterion of the shell-test failure report. The harness spends its exit status as a channel — the
// ci-runner encodes the failing suite's index into it — so the recorded error
// must name that status, not the CLI's generic failure code. The job's own exit
// code stays inside the stable taxonomy: a harness exiting 2 must not reach the
// caller as "usage".
func TestShellTestFailureCarriesTheHarnessExitStatus(t *testing.T) {
	seams := &shellTestSeams{runErr: exitStatusError(t, 13)}
	installShellTestSeams(t, seams)

	err := ShellTest(map[string]any{"project": shellTestProject(t)}, nil, "", nil,
		clicore.IO{Stdout: func(string) {}, Stderr: func(string) {}})
	if err == nil {
		t.Fatal("a failing harness must fail the job")
	}
	if !strings.Contains(err.Error(), "exit status 13") {
		t.Fatalf("recorded error must name the harness's own status, got %q", err.Error())
	}
	var exitErr *clicore.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("failure must be exit-coded, got %T", err)
	}
	if exitErr.Code != clicore.ExitFailure {
		t.Fatalf("job exit code = %d, want %d — the taxonomy must not carry the harness's status",
			exitErr.Code, clicore.ExitFailure)
	}
}

// TestShellTestFailureReEmitsFailingAssertions is the first acceptance
// criterion of the shell-test failure report. The transcript streams as INFO log events, which every
// failure renderer filters out, so the failing TAP assertions have to be
// restated on the diagnostic channel to be readable from CI at all.
func TestShellTestFailureReEmitsFailingAssertions(t *testing.T) {
	seams := &shellTestSeams{
		runErr: exitStatusError(t, 12),
		output: []string{
			"# --- timeout_recovery_process_test.sh ---",
			"ok 1 - the spawned group is alive before the reap",
			"not ok 2 - the reaper empties a live process group",
			"  # want: 0",
			"not ok 3 - no descendant of the gate session outlives it",
		},
	}
	installShellTestSeams(t, seams)

	var stdout, stderr []string
	err := ShellTest(map[string]any{"project": shellTestProject(t)}, nil, "", nil,
		clicore.IO{
			Stdout: func(line string) { stdout = append(stdout, line) },
			Stderr: func(line string) { stderr = append(stderr, line) },
		})
	if err == nil {
		t.Fatal("a failing harness must fail the job")
	}
	// The live transcript is untouched: the failure report is additive.
	if len(stdout) != len(seams.output) {
		t.Fatalf("streamed %d transcript lines, want %d", len(stdout), len(seams.output))
	}
	diagnostics := strings.Join(stderr, "\n")
	for _, want := range []string{
		"not ok 2 - the reaper empties a live process group",
		"not ok 3 - no descendant of the gate session outlives it",
	} {
		if !strings.Contains(diagnostics, want) {
			t.Fatalf("diagnostics must restate %q, got:\n%s", want, diagnostics)
		}
	}
	if strings.Contains(diagnostics, "ok 1 - the spawned group is alive") {
		t.Fatalf("only FAILING assertions belong on the diagnostic channel, got:\n%s", diagnostics)
	}
	if !strings.Contains(err.Error(), "not ok 2 - the reaper empties a live process group") {
		t.Fatalf("the recorded error must quote the failing assertions, got %q", err.Error())
	}
}

// TestShellTestFailureFallsBackToTheTranscriptTail covers the harness that dies
// before it asserts anything — a missing tool, a syntax error, a kill. There is
// no `not ok` line to quote, and reporting nothing would reproduce the very
// blindness this hook's failure report exists to end.
func TestShellTestFailureFallsBackToTheTranscriptTail(t *testing.T) {
	seams := &shellTestSeams{
		runErr: exitStatusError(t, 1),
		output: []string{"ci-runner tests: required tool 'jq' is not on PATH"},
	}
	installShellTestSeams(t, seams)

	var stderr []string
	err := ShellTest(map[string]any{"project": shellTestProject(t)}, nil, "", nil,
		clicore.IO{Stdout: func(string) {}, Stderr: func(line string) { stderr = append(stderr, line) }})
	if err == nil {
		t.Fatal("a failing harness must fail the job")
	}
	if !strings.Contains(strings.Join(stderr, "\n"), "required tool 'jq' is not on PATH") {
		t.Fatalf("a harness with no failing assertion must report its tail, got:\n%s", strings.Join(stderr, "\n"))
	}
}

// TestShellTestCaptureDeduplicatesFailingAssertions: ci-runner's harness already
// re-states its failing assertions on stderr so `bash tests/run.sh` is readable
// on its own, and the capture watches both fds — without dedup every assertion
// would be reported (and counted) twice.
func TestShellTestCaptureDeduplicatesFailingAssertions(t *testing.T) {
	capture := &shellTestCapture{}
	for _, line := range []string{
		"not ok 1 - the reaper empties a live process group",
		"  # want: 0",
		"ci-runner tests: FAILED timeout_recovery_process_test.sh (exit 1) — failing assertions:",
		"not ok 1 - the reaper empties a live process group",
	} {
		capture.observe(line)
	}

	evidence := capture.evidence()
	if len(evidence) != 1 {
		t.Fatalf("evidence = %v, want the single distinct failing assertion", evidence)
	}
	if capture.failed != 1 {
		t.Fatalf("failed count = %d, want 1 — a re-stated assertion is not a second failure", capture.failed)
	}
}

// TestShellTestCaptureIsConcurrencySafe: os/exec copies stdout and stderr on
// their own goroutines whenever the writers are not *os.File, so a harness that
// writes both fds reaches observe from two goroutines at once. Unguarded, the
// dedup map races and the runtime kills the process with a concurrent map write
// — turning the failure path into a crash. Run under -race to see a regression.
func TestShellTestCaptureIsConcurrencySafe(t *testing.T) {
	capture := &shellTestCapture{}

	var wg sync.WaitGroup
	for fd := range 2 {
		wg.Go(func() {
			for i := range 200 {
				capture.observe("not ok " + strconv.Itoa(i) + " - assertion")
				capture.observe("  # fd " + strconv.Itoa(fd))
			}
		})
	}
	wg.Wait()

	// Both goroutines report the SAME assertions, so dedup must still hold: the
	// count is the distinct set, not the number of writes.
	if capture.failed != 200 {
		t.Fatalf("failed count = %d, want the 200 distinct assertions", capture.failed)
	}
	if got := len(capture.evidence()); got != shellTestMaxFailureLines+1 {
		t.Fatalf("evidence lines = %d, want %d capped assertions plus one elision marker",
			got, shellTestMaxFailureLines+1)
	}
}

// TestShellTestRunCapturesBothStreams drives the REAL shellTestRun against a
// harness that writes interleaved stdout and stderr, which is the only way to
// exercise the two copying goroutines the capture is locked for.
func TestShellTestRunCapturesBothStreams(t *testing.T) {
	shell, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash is not on PATH: %v", err)
	}
	dir := t.TempDir()
	harness := filepath.Join(dir, "harness.sh")
	script := "#!/usr/bin/env bash\n" +
		"for i in $(seq 1 200); do\n" +
		"  echo \"not ok $i - stdout assertion\"\n" +
		"  echo \"not ok $i - stderr assertion\" >&2\n" +
		"done\n" +
		"exit 7\n"
	if err := os.WriteFile(harness, []byte(script), 0o700); err != nil { //nolint:gosec // G306: a test harness must be executable
		t.Fatalf("write harness: %v", err)
	}

	var mu sync.Mutex
	var lines int
	sink := func(string) {
		mu.Lock()
		lines++
		mu.Unlock()
	}

	capture, runErr := shellTestRun(shell, harness, dir,
		clicore.IO{Stdout: sink, Stderr: sink})
	if shellTestExitStatus(runErr) != 7 {
		t.Fatalf("exit status = %d, want the harness's own 7", shellTestExitStatus(runErr))
	}
	if lines != 400 {
		t.Fatalf("streamed %d lines, want all 400 from both fds", lines)
	}
	if capture.failed != 400 {
		t.Fatalf("captured %d distinct failing assertions, want 400 across both fds", capture.failed)
	}
}

// TestShellTestCaptureBoundsWhatAFailureRestates keeps the report proportionate:
// a harness that fails everything must not turn one red task into thousands of
// diagnostic events, and the elision has to be visible rather than silent.
func TestShellTestCaptureBoundsWhatAFailureRestates(t *testing.T) {
	capture := &shellTestCapture{}
	for i := range shellTestMaxFailureLines + 7 {
		capture.observe("not ok " + strconv.Itoa(i) + " - assertion")
	}

	evidence := capture.evidence()
	if len(evidence) != shellTestMaxFailureLines+1 {
		t.Fatalf("evidence lines = %d, want %d capped assertions plus one elision marker",
			len(evidence), shellTestMaxFailureLines+1)
	}
	if !strings.Contains(evidence[len(evidence)-1], "7 more failing assertion") {
		t.Fatalf("the elision must be stated, got %q", evidence[len(evidence)-1])
	}
}

// TestShellTestFailsWithoutAHarness is the deliberate INVERSE of the retired
// image-build hook's
// soft-skip. The hook activates on the harness file, so a missing one means the
// plan and the tree disagree — and a test that cannot run must never be reported
// as a test that passed.
func TestShellTestFailsWithoutAHarness(t *testing.T) {
	seams := &shellTestSeams{}
	installShellTestSeams(t, seams)

	err := ShellTest(map[string]any{"project": t.TempDir()}, nil, "", nil,
		clicore.IO{Stdout: func(string) {}, Stderr: func(string) {}})
	if err == nil {
		t.Fatal("a missing harness must fail loudly, never skip green")
	}
	if seams.ran != 0 {
		t.Fatalf("harness runs = %d with no harness present, want 0", seams.ran)
	}
	var exitErr *clicore.ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != clicore.ExitUsage {
		t.Fatalf("err = %v, want a usage ExitError", err)
	}
}

// TestShellTestFailsWithoutBash: an interpreter-less host cannot exercise the
// contract, and — again unlike that hook — must say so rather than pass.
func TestShellTestFailsWithoutBash(t *testing.T) {
	seams := &shellTestSeams{lookErr: errors.New("executable file not found in $PATH")}
	installShellTestSeams(t, seams)

	err := ShellTest(map[string]any{"project": shellTestProject(t)}, nil, "", nil,
		clicore.IO{Stdout: func(string) {}, Stderr: func(string) {}})
	if err == nil {
		t.Fatal("a host without bash must fail, never report a green suite")
	}
	if seams.ran != 0 {
		t.Fatalf("harness runs = %d without an interpreter, want 0", seams.ran)
	}
}

// TestShellTestResolvesTheProjectByName mirrors the image verbs' context
// resolution: the runtime runs in the task's declared cwd, not the caller's, so
// a resolved project name — not the process cwd — decides which tree is tested.
func TestShellTestResolvesTheProjectByName(t *testing.T) {
	seams := &shellTestSeams{}
	installShellTestSeams(t, seams)

	workspaceRoot := t.TempDir()
	projectDir := filepath.Join(workspaceRoot, "images", "ci-runner")
	if err := os.MkdirAll(filepath.Join(projectDir, "tests"), 0o750); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "putnami.json"),
		[]byte(`{"name":"images/ci-runner"}`), 0o600); err != nil {
		t.Fatalf("write project manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, shellTestHarness),
		[]byte("#!/usr/bin/env bash\n"), 0o600); err != nil {
		t.Fatalf("write harness: %v", err)
	}

	err := ShellTest(map[string]any{"app": "images/ci-runner"}, nil, workspaceRoot, nil,
		clicore.IO{Stdout: func(string) {}, Stderr: func(string) {}})
	if err != nil {
		t.Fatalf("ShellTest: %v", err)
	}
	// macOS resolves /var through a symlink, so compare the evaluated paths.
	wantDir, err := filepath.EvalSymlinks(projectDir)
	if err != nil {
		t.Fatalf("resolve project path: %v", err)
	}
	gotDir, err := filepath.EvalSymlinks(seams.gotDir)
	if err != nil {
		t.Fatalf("resolve reported path: %v", err)
	}
	if gotDir != wantDir {
		t.Fatalf("cwd = %q, want the resolved project %q", gotDir, wantDir)
	}
}
