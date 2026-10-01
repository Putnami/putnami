package serve

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
	pexec "go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/sdk/extension/proctree"
)

// ---- ResolvePort ----

func TestResolvePort_ExplicitParam(t *testing.T) {
	os.Unsetenv("PORT")
	port := ResolvePort(8080)
	if port != 8080 {
		t.Errorf("expected 8080, got %d", port)
	}
}

func TestResolvePort_EnvVar(t *testing.T) {
	os.Setenv("PORT", "4000")
	defer os.Unsetenv("PORT")

	port := ResolvePort(0)
	if port != 4000 {
		t.Errorf("expected 4000 from PORT env, got %d", port)
	}
}

func TestResolvePort_ParamTakesPrecedenceOverEnv(t *testing.T) {
	os.Setenv("PORT", "4000")
	defer os.Unsetenv("PORT")

	port := ResolvePort(9000)
	if port != 9000 {
		t.Errorf("expected param 9000 to win over env, got %d", port)
	}
}

func TestResolvePort_DefaultWhenNothingSet(t *testing.T) {
	os.Unsetenv("PORT")

	port := ResolvePort(0)
	if port != 3000 {
		t.Errorf("expected default 3000, got %d", port)
	}
}

func TestResolvePort_InvalidEnvVar(t *testing.T) {
	os.Setenv("PORT", "not-a-number")
	defer os.Unsetenv("PORT")

	port := ResolvePort(0)
	if port != 3000 {
		t.Errorf("expected default 3000 for invalid PORT env, got %d", port)
	}
}

// ---- ResolveEntrypoint ----

func TestResolveEntrypoint_ExplicitEntrypoint(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "serve-entrypoint", "an-explicit-entrypoint-is-used-as-given")
	ep, err := ResolveEntrypoint("/any/path", "src/server.ts")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ep != "src/server.ts" {
		t.Errorf("expected 'src/server.ts', got %q", ep)
	}
}

func TestResolveEntrypoint_NoPackageJSON(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "serve-entrypoint", "a-missing-package-manifest-is-a-user-facing-error")
	dir := t.TempDir()
	_, err := ResolveEntrypoint(dir, "")
	if err == nil {
		t.Error("expected error when no package.json")
	}
}

func TestResolveEntrypoint_PackageJSONNoServeExport(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "serve-entrypoint", "a-missing-serve-export-is-a-user-facing-error")
	dir := t.TempDir()
	content := `{"name": "@test/pkg", "exports": {"./index": "./dist/index.js"}}`
	os.WriteFile(dir+"/package.json", []byte(content), 0644)

	_, err := ResolveEntrypoint(dir, "")
	if err == nil {
		t.Error("expected error when no ./serve export")
	}
}

func TestResolveEntrypoint_PackageJSONWithServeExport(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "serve-entrypoint", "the-entrypoint-resolves-from-the-serve-export")
	dir := t.TempDir()
	content := `{"name": "@test/pkg", "exports": {"./serve": "./dist/serve.js"}}`
	os.WriteFile(dir+"/package.json", []byte(content), 0644)

	ep, err := ResolveEntrypoint(dir, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ep != "./dist/serve.js" {
		t.Errorf("expected './dist/serve.js', got %q", ep)
	}
}

// ---- buildServeArgs ----

func TestBuildServeArgs_Basic(t *testing.T) {
	args := buildServeArgs("src/serve.ts", 3000, Params{}, false)
	if args[0] != "run" || args[1] != "--no-orphans" || args[2] != "--port=3000" {
		t.Errorf("expected 'run --no-orphans --port=3000', got %v", args[:3])
	}
	if args[len(args)-1] != "src/serve.ts" {
		t.Errorf("expected last arg 'src/serve.ts', got %q", args[len(args)-1])
	}
}

func TestBuildServeArgs_Production(t *testing.T) {
	args := buildServeArgs("src/serve.ts", 8080, Params{}, true)
	foundNoInstall, foundSmol := false, false
	for _, a := range args {
		if a == "--no-install" {
			foundNoInstall = true
		}
		if a == "--smol" {
			foundSmol = true
		}
	}
	if !foundNoInstall || !foundSmol {
		t.Error("expected --no-install --smol in production mode")
	}
}

func TestBuildServeArgs_Inspect(t *testing.T) {
	args := buildServeArgs("serve.ts", 3000, Params{Inspect: true}, false)
	found := false
	for _, a := range args {
		if a == "--inspect-wait" {
			found = true
		}
	}
	if !found {
		t.Error("expected --inspect-wait")
	}
}

func TestBuildServeArgs_InspectWait(t *testing.T) {
	args := buildServeArgs("serve.ts", 3000, Params{InspectWait: true}, false)
	found := false
	for _, a := range args {
		if a == "--inspect-wait" {
			found = true
		}
	}
	if !found {
		t.Error("expected --inspect-wait")
	}
}

func TestBuildServeArgs_InspectBrk(t *testing.T) {
	args := buildServeArgs("serve.ts", 3000, Params{InspectBrk: true}, false)
	found := false
	for _, a := range args {
		if a == "--inspect-brk" {
			found = true
		}
	}
	if !found {
		t.Error("expected --inspect-brk")
	}
}

func TestBuildServeArgs_ProductionIgnoresInspect(t *testing.T) {
	args := buildServeArgs("serve.ts", 3000, Params{Inspect: true, InspectBrk: true}, true)
	for _, a := range args {
		if a == "--inspect-wait" || a == "--inspect-brk" {
			t.Errorf("production mode should not include inspect flags, found %q", a)
		}
	}
}

func TestBuildServeArgs_Watch(t *testing.T) {
	args := buildServeArgs("serve.ts", 3000, Params{Watch: true}, false)
	found := false
	for _, a := range args {
		if a == "--watch" {
			found = true
		}
	}
	if !found {
		t.Error("expected --watch when params.Watch is true in dev mode")
	}
}

func TestBuildServeArgs_ProductionIgnoresWatch(t *testing.T) {
	args := buildServeArgs("serve.ts", 3000, Params{Watch: true}, true)
	for _, a := range args {
		if a == "--watch" {
			t.Error("production mode should not include --watch")
		}
	}
}

// ---- buildServeEnv ----

func TestBuildServeEnv_DevMode(t *testing.T) {
	os.Unsetenv("NODE_ENV")
	os.Unsetenv("ENV")
	env := buildServeEnv(Params{}, false)
	foundNodeEnv := false
	for _, e := range env {
		if e == "NODE_ENV=development" {
			foundNodeEnv = true
		}
	}
	if !foundNodeEnv {
		t.Error("expected NODE_ENV=development in dev mode")
	}
}

func TestBuildServeEnv_WithPort(t *testing.T) {
	os.Unsetenv("NODE_ENV")
	os.Unsetenv("ENV")
	env := buildServeEnv(Params{Port: 9090}, false)
	found := false
	for _, e := range env {
		if e == "PORT=9090" {
			found = true
		}
	}
	if !found {
		t.Error("expected PORT=9090")
	}
}

func TestBuildServeEnv_WithDebug(t *testing.T) {
	os.Unsetenv("NODE_ENV")
	os.Unsetenv("ENV")
	env := buildServeEnv(Params{Debug: true}, false)
	found := false
	for _, e := range env {
		if e == "LOG_LEVEL=debug" {
			found = true
		}
	}
	if !found {
		t.Error("expected LOG_LEVEL=debug")
	}
}

func TestBuildServeEnv_ProdMode(t *testing.T) {
	os.Unsetenv("NODE_ENV")
	os.Unsetenv("ENV")
	env := buildServeEnv(Params{}, true)
	for _, e := range env {
		if e == "NODE_ENV=development" {
			t.Error("production mode should not set NODE_ENV=development")
		}
	}
}

// ---- mockExecRun helper ----

func withMockExec(t *testing.T, fn func(string, []string, ...pexec.Option) (*pexec.Result, error)) {
	t.Helper()
	orig := execRunFunc
	t.Cleanup(func() { execRunFunc = orig })
	execRunFunc = fn
}

// ---- KillProcessOnPort ----

func TestKillProcessOnPort_FuserSuccess(t *testing.T) {
	withMockExec(t, func(name string, args []string, opts ...pexec.Option) (*pexec.Result, error) {
		if name == "fuser" {
			return &pexec.Result{Success: true, ExitCode: 0}, nil
		}
		return &pexec.Result{Success: false, ExitCode: 1}, nil
	})

	killed := KillProcessOnPort(8080)
	if !killed {
		t.Error("expected KillProcessOnPort to return true on fuser success")
	}
}

func TestKillProcessOnPort_FuserFails(t *testing.T) {
	withMockExec(t, func(name string, args []string, opts ...pexec.Option) (*pexec.Result, error) {
		return &pexec.Result{Success: false, ExitCode: 1}, nil
	})

	killed := KillProcessOnPort(8080)
	if killed {
		t.Error("expected KillProcessOnPort to return false when fuser fails")
	}
}

func TestKillProcessOnPort_FuserError(t *testing.T) {
	withMockExec(t, func(name string, args []string, opts ...pexec.Option) (*pexec.Result, error) {
		return nil, os.ErrNotExist
	})

	killed := KillProcessOnPort(8080)
	if killed {
		t.Error("expected KillProcessOnPort to return false on exec error")
	}
}

func TestKillProcessOnPort_LsofFallbackKills(t *testing.T) {
	var killedPIDs []string
	withMockExec(t, func(name string, args []string, opts ...pexec.Option) (*pexec.Result, error) {
		switch name {
		case "fuser":
			return &pexec.Result{Success: false, ExitCode: 1}, nil
		case "lsof":
			return &pexec.Result{Success: true, ExitCode: 0, Stdout: "4242\n4243\n"}, nil
		case "kill":
			killedPIDs = append(killedPIDs, args[len(args)-1])
			return &pexec.Result{Success: true, ExitCode: 0}, nil
		}
		return &pexec.Result{Success: false, ExitCode: 1}, nil
	})

	if !KillProcessOnPort(8080) {
		t.Error("expected KillProcessOnPort to return true when the lsof fallback kills a process")
	}
	if len(killedPIDs) != 2 || killedPIDs[0] != "4242" || killedPIDs[1] != "4243" {
		t.Errorf("expected to kill PIDs [4242 4243], got %v", killedPIDs)
	}
}

func TestKillProcessOnPort_LsofNoProcess(t *testing.T) {
	withMockExec(t, func(name string, args []string, opts ...pexec.Option) (*pexec.Result, error) {
		if name == "lsof" {
			return &pexec.Result{Success: true, ExitCode: 0, Stdout: "\n"}, nil
		}
		return &pexec.Result{Success: false, ExitCode: 1}, nil
	})

	if KillProcessOnPort(8080) {
		t.Error("expected KillProcessOnPort to return false when lsof finds no listening process")
	}
}

func TestKillPortUnavailable(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		if err := KillPortUnavailable(goos); err != nil {
			t.Errorf("KillPortUnavailable(%q) = %v, want nil", goos, err)
		}
	}
	err := KillPortUnavailable("windows")
	if err == nil || !strings.Contains(err.Error(), "not available on Windows") {
		t.Errorf("KillPortUnavailable(windows) = %v, want a not-available error", err)
	}
}

// ---- classifyServeExit ----

func TestClassifyServeExit_CleanExit(t *testing.T) {
	ok, err := classifyServeExit(nil, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Error("expected success for nil wait error")
	}
}

func TestClassifyServeExit_NonExitError(t *testing.T) {
	sentinel := errors.New("pipe broke")
	ok, err := classifyServeExit(sentinel, false)
	if ok {
		t.Error("expected failure for non-exit error")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("expected sentinel error to propagate, got %v", err)
	}
}

func TestClassifyServeExit_SignalExitsAreSuccess(t *testing.T) {
	// A long-running dev server killed by SIGINT (130) / SIGTERM (143) is an
	// expected shutdown, not a failure.
	for _, code := range []int{0, 128, 130, 143} {
		waitErr := exitErrorWithCode(t, code)
		ok, err := classifyServeExit(waitErr, false)
		if err != nil {
			t.Fatalf("code %d: unexpected error: %v", code, err)
		}
		if !ok {
			t.Errorf("code %d: expected success", code)
		}
	}
}

func TestClassifyServeExit_RealFailureCode(t *testing.T) {
	// A genuine non-zero, non-signal exit (e.g. 1) is a failure.
	waitErr := exitErrorWithCode(t, 1)
	ok, err := classifyServeExit(waitErr, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("expected failure for exit code 1")
	}
}

// TestClassifyServeExit_StoppedOnWindowsIsSuccess pins that a stop the
// extension relayed reports success on Windows whatever the exit code, and
// that nothing changes elsewhere. 1 is the code a kill leaves; 58 is the low
// byte of STATUS_CONTROL_C_EXIT, which a shell cannot exit with whole.
func TestClassifyServeExit_StoppedOnWindowsIsSuccess(t *testing.T) {
	for _, code := range []int{1, 58} {
		waitErr := exitErrorWithCode(t, code)
		if ok, err := classifyServeExitOn("windows", waitErr, true); !ok || err != nil {
			t.Errorf("windows, stopped, exit %d: (%v, %v), want success", code, ok, err)
		}
		if ok, err := classifyServeExitOn("windows", waitErr, false); ok || err != nil {
			t.Errorf("windows, not stopped, exit %d: (%v, %v), want a failure", code, ok, err)
		}
		for _, goos := range []string{"linux", "darwin"} {
			if ok, err := classifyServeExitOn(goos, waitErr, true); ok || err != nil {
				t.Errorf("%s, stopped, exit %d: (%v, %v), want a failure as before", goos, code, ok, err)
			}
		}
	}
	sentinel := errors.New("pipe broke")
	if ok, err := classifyServeExitOn("windows", sentinel, true); ok || !errors.Is(err, sentinel) {
		t.Errorf("windows, stopped, wait error: (%v, %v), want the error", ok, err)
	}
}

// exitErrorWithCode runs a process that exits with the given code and returns
// the resulting *exec.ExitError (wrapped in the same way RunBunServe sees it).
func exitErrorWithCode(t *testing.T, code int) error {
	t.Helper()
	// `exit 128` etc. are valid shell exit codes (mod 256).
	cmd := exec.Command("sh", "-c", "exit "+strconv.Itoa(code))
	err := cmd.Run()
	if code == 0 {
		if err != nil {
			t.Fatalf("expected nil error for exit 0, got %v", err)
		}
		return nil
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("expected *exec.ExitError for exit %d, got %T: %v", code, err, err)
	}
	return err
}

// ---- superviseServe ----

// withMockSignalGroup swaps the package signalGroup for the duration of a test.
func withMockSignalGroup(t *testing.T, fn func(pgid int, sig syscall.Signal) error) {
	t.Helper()
	orig := signalGroup
	t.Cleanup(func() { signalGroup = orig })
	signalGroup = fn
}

func TestSuperviseServe_NaturalExit(t *testing.T) {
	// No signal: the supervisor returns the process's own wait error.
	doneCh := make(chan error, 1)
	sentinel := errors.New("process exited")
	doneCh <- sentinel
	sigCh := make(chan os.Signal, 1)

	got, stopped := superviseServe(nil, sigCh, doneCh, terminateGrace)
	if !errors.Is(got, sentinel) {
		t.Errorf("expected the natural wait error to be returned, got %v", got)
	}
	if stopped {
		t.Error("a natural exit reported as stopped")
	}
}

func TestSuperviseServe_SignalForwardsToBunOnly(t *testing.T) {
	// On signal, the supervisor must forward SIGTERM to bun's pid — never to a
	// process group, which bun shares with the extension and its launcher —
	// and return once the process is reaped.
	var gotSignals []syscall.Signal
	withMockSignalGroup(t, func(pgid int, sig syscall.Signal) error {
		t.Errorf("signaled process group %d with %v; a served bun shares the job's group", pgid, sig)
		return nil
	})
	origProcess := signalProcess
	t.Cleanup(func() { signalProcess = origProcess })
	signalProcess = func(pid int, sig syscall.Signal) error {
		if pid != 4321 {
			t.Errorf("signaled pid %d, want bun's 4321", pid)
		}
		gotSignals = append(gotSignals, sig)
		return nil
	}

	proc := &os.Process{Pid: 4321}
	doneCh := make(chan error, 1)
	sigCh := make(chan os.Signal, 1)

	// Deliver a signal, then have the process exit shortly after (simulating a
	// graceful shutdown in response to SIGTERM).
	sigCh <- syscall.SIGTERM
	go func() {
		time.Sleep(10 * time.Millisecond)
		doneCh <- nil
	}()

	got, stopped := superviseServe(proc, sigCh, doneCh, terminateGrace)
	if got != nil {
		t.Errorf("expected nil wait error after graceful shutdown, got %v", got)
	}
	if !stopped {
		t.Error("an exit after a termination signal not reported as stopped")
	}
	if len(gotSignals) == 0 || gotSignals[0] != syscall.SIGTERM {
		t.Errorf("expected SIGTERM to be sent to the group, got %v", gotSignals)
	}
}

// ---- terminateProcessGroup ----

func TestTerminateProcessGroup_NilProcess(t *testing.T) {
	doneCh := make(chan error, 1)
	sentinel := errors.New("done")
	doneCh <- sentinel
	if got := terminateProcessGroup(nil, doneCh, terminateGrace); !errors.Is(got, sentinel) {
		t.Errorf("expected wait error for nil process, got %v", got)
	}
}

func TestTerminateProcessGroup_EscalatesToSIGKILL(t *testing.T) {
	// A process that ignores SIGTERM must be escalated to SIGKILL after grace.
	var gotSignals []syscall.Signal
	withMockSignalGroup(t, func(pgid int, sig syscall.Signal) error {
		gotSignals = append(gotSignals, sig)
		return nil
	})

	proc := &os.Process{Pid: 9999}
	doneCh := make(chan error, 1)
	// The process only "exits" after SIGKILL would have been sent.
	go func() {
		time.Sleep(60 * time.Millisecond)
		doneCh <- nil
	}()

	// Tiny grace so the timer fires before the simulated exit.
	_ = terminateProcessGroup(proc, doneCh, 10*time.Millisecond)

	if len(gotSignals) != 2 {
		t.Fatalf("expected SIGTERM then SIGKILL, got %v", gotSignals)
	}
	if gotSignals[0] != syscall.SIGTERM || gotSignals[1] != syscall.SIGKILL {
		t.Errorf("expected [SIGTERM SIGKILL], got %v", gotSignals)
	}
}

// TestTerminate_FailedSIGTERMKillsAtOnce pins the bound on a stop whose SIGTERM
// could not be sent, as on Windows for a single process: SIGKILL follows at
// once instead of after a grace that waits on nothing.
func TestTerminate_FailedSIGTERMKillsAtOnce(t *testing.T) {
	var gotSignals []syscall.Signal
	doneCh := make(chan error, 1)
	send := func(_ int, sig syscall.Signal) error {
		gotSignals = append(gotSignals, sig)
		if sig == syscall.SIGTERM {
			return errors.New("SIGTERM not supported")
		}
		doneCh <- nil
		return nil
	}

	returned := make(chan error, 1)
	go func() { returned <- terminate(&os.Process{Pid: 4321}, doneCh, time.Hour, send) }()
	select {
	case err := <-returned:
		if err != nil {
			t.Errorf("terminate returned %v, want the wait error nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("terminate waited out the grace after a SIGTERM that failed")
	}
	if len(gotSignals) != 2 || gotSignals[0] != syscall.SIGTERM || gotSignals[1] != syscall.SIGKILL {
		t.Errorf("signals = %v, want [SIGTERM SIGKILL]", gotSignals)
	}
}

// TestTerminateProcessGroup_ReapsRealGrandchild is the regression guard for the
// orphaned-grandchild bug. A shell is started in its own process
// group; it spawns a background copy of the test binary that records when it
// receives SIGTERM. Signaling the *group* (negative PGID) must reach that
// grandchild, not just the group leader.
func TestTerminateProcessGroup_ReapsRealGrandchild(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "serve-supervision", "the-whole-child-process-group-is-terminated")
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	dir := t.TempDir()
	readyMarker := filepath.Join(dir, "grandchild-ready")
	terminatedMarker := filepath.Join(dir, "grandchild-terminated")
	// The trailing wait prevents sh from replacing itself with the helper, so
	// the helper is a real grandchild in the shell-led process group.
	script := `"$1" -test.run=^TestProcessGroupGrandchildHelper$ & wait`
	cmd := exec.Command("sh", "-c", script, "parent", os.Args[0])
	cmd.Env = append(os.Environ(),
		"PUTNAMI_PROCESS_GROUP_HELPER=1",
		"PUTNAMI_PROCESS_GROUP_READY="+readyMarker,
		"PUTNAMI_PROCESS_GROUP_TERMINATED="+terminatedMarker,
	)
	tree := proctree.New(cmd)

	if err := tree.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = tree.Close() })

	doneCh := make(chan error, 1)
	go func() { doneCh <- cmd.Wait() }()

	// Do not signal until the grandchild has installed its handler; otherwise a
	// scheduler delay could turn this into a test of the default signal action.
	if !waitFileExists(readyMarker, 2*time.Second) {
		_ = signalGroup(cmd.Process.Pid, syscall.SIGKILL)
		<-doneCh
		t.Fatal("grandchild did not become ready")
	}

	// Terminate the whole group using the real signalGroup.
	terminateProcessGroup(cmd.Process, doneCh, terminateGrace)

	// Assert the grandchild itself handled the group signal. kill(pid, 0) is not
	// suitable here: it reports zombies as alive and can observe a different
	// process if a busy CI worker reuses the PID before the polling window ends.
	if !waitFileExists(terminatedMarker, 2*time.Second) {
		// Best-effort cleanup if the group signal genuinely missed the child.
		_ = signalGroup(cmd.Process.Pid, syscall.SIGKILL)
		t.Error("grandchild did not receive process-group termination")
	}
}

// TestProcessGroupGrandchildHelper runs only when launched by the integration
// test above. Keeping the signal-aware child in Go avoids shell-specific trap
// and job-control behavior while still exercising a real OS process group.
func TestProcessGroupGrandchildHelper(t *testing.T) {
	if os.Getenv("PUTNAMI_PROCESS_GROUP_HELPER") != "1" {
		return
	}

	sigCh := make(chan os.Signal, 1)
	// A Windows console process group receives the group's stop request as
	// os.Interrupt (CTRL_BREAK_EVENT); a Unix process group, as SIGTERM.
	signal.Notify(sigCh, syscall.SIGTERM, os.Interrupt)
	if err := os.WriteFile(os.Getenv("PUTNAMI_PROCESS_GROUP_READY"), []byte("ready"), 0o600); err != nil {
		os.Exit(2)
	}
	<-sigCh
	if err := os.WriteFile(os.Getenv("PUTNAMI_PROCESS_GROUP_TERMINATED"), []byte("terminated"), 0o600); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func waitFileExists(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, err := os.Stat(path)
	return err == nil
}

// runnerProcessGroupKillDelay mirrors processGroupKillDelay in
// tooling/cli/internal/jobs/runner.go: the delay between the SIGTERM the CLI
// job runner sends a job's process group and its SIGKILL.
const runnerProcessGroupKillDelay = 5 * time.Second

func TestTerminateGrace_EndsBeforeTheRunnerKillsTheJob(t *testing.T) {
	if terminateGrace >= runnerProcessGroupKillDelay {
		t.Errorf("terminateGrace = %v, want less than the runner's %v so the extension escalates and reaps bun first", terminateGrace, runnerProcessGroupKillDelay)
	}
}
