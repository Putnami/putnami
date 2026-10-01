package lifecycle

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	registryproto "go.putnami.dev/protocol/registry"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/commands/sharedtest"
	"go.putnami.dev/tooling/cli/internal/iox"
)

// spyInstall replaces installRunner with a spy that records calls and returns
// ret, restoring the original when the test ends.
func spyInstall(t *testing.T, ret error) *int {
	t.Helper()
	orig := installRunner
	t.Cleanup(func() { installRunner = orig })
	var calls int
	installRunner = func(_ context.Context, _ string, _ *wsproto.Config, _ []string, _ LifecycleEnv) error {
		calls++
		return ret
	}
	return &calls
}

// resetBootstrapEnv neutralizes the bootstrap env vars for a hermetic test and
// restores them afterward (EnsureWorkspaceBootstrap sets workspaceBootstrappedEnv
// via os.Setenv on the install path).
func resetBootstrapEnv(t *testing.T) {
	t.Helper()
	t.Setenv(workspaceBootstrappedEnv, "")
	t.Setenv(noAutoInstallEnv, "")
}

func TestEnsureWorkspaceBootstrap_NoopGuards(t *testing.T) {
	resetBootstrapEnv(t)
	calls := spyInstall(t, nil)

	EnsureWorkspaceBootstrap(context.Background(), "", nil, BootstrapOptions{Command: "build"})
	EnsureWorkspaceBootstrap(context.Background(), t.TempDir(), nil, BootstrapOptions{Command: "build"})

	if *calls != 0 {
		t.Errorf("nil cfg / empty wsRoot must not install; got %d calls", *calls)
	}
}

func TestEnsureWorkspaceBootstrap_RunsInstallWhenStale(t *testing.T) {
	resetBootstrapEnv(t)
	calls := spyInstall(t, nil)
	ws := t.TempDir()
	writeLock(t, ws, "putnami.lock.json", `{}`) // stale: no marker yet

	EnsureWorkspaceBootstrap(context.Background(), ws, &wsproto.Config{}, BootstrapOptions{Command: "build"})

	if *calls != 1 {
		t.Errorf("a stale workspace must install once; got %d calls", *calls)
	}
	if os.Getenv(workspaceBootstrappedEnv) != ws {
		t.Error("install path must stamp the re-entrancy env so nested putnami skips")
	}
}

// The bootstrap install must mark its context as an implicit install so
// installArtifacts skips the committed-lock write.
func TestEnsureWorkspaceBootstrap_MarksInstallContextImplicit(t *testing.T) {
	resetBootstrapEnv(t)
	orig := installRunner
	t.Cleanup(func() { installRunner = orig })
	var ran, gotImplicit bool
	installRunner = func(ctx context.Context, _ string, _ *wsproto.Config, _ []string, _ LifecycleEnv) error {
		ran = true
		gotImplicit = shared.IsImplicitInstall(ctx)
		return nil
	}
	ws := t.TempDir()
	writeLock(t, ws, "putnami.lock.json", `{}`) // stale: no marker yet

	EnsureWorkspaceBootstrap(context.Background(), ws, &wsproto.Config{}, BootstrapOptions{Command: "build"})

	if !ran {
		t.Fatal("a stale workspace must run the install")
	}
	if !gotImplicit {
		t.Error("bootstrap install must mark its context implicit so the lock is not rewritten")
	}
}

// The bootstrap install's human output must never reach stdout under a
// structured output mode: stdout is the invoking command's machine contract.
// Until slice A5a that was enforced by reassigning the process-global
// os.Stdout for the duration of the install; now the writer is threaded on
// LifecycleEnv.Out, so this test asserts BOTH halves — the install family is
// handed stderr, and nothing lands on the real stdout.
func TestEnsureWorkspaceBootstrap_RoutesStructuredInstallOutputToStderr(t *testing.T) {
	resetBootstrapEnv(t)
	orig := installRunner
	t.Cleanup(func() { installRunner = orig })
	ws := t.TempDir()
	writeLock(t, ws, "putnami.lock.json", `{}`) // stale: no marker yet
	var gotWriter io.Writer
	installRunner = func(_ context.Context, _ string, _ *wsproto.Config, _ []string, env LifecycleEnv) error {
		gotWriter = env.Out
		iox.Fprintln(env.out(), "human install output")
		// A stray write to the process stdout is what the deleted global swap used
		// to absorb. It must now be visible as a failure, not silently redirected.
		return nil
	}

	// captureProcessOutput swaps os.Stderr for a pipe, so the writer to compare
	// against is the one live DURING the call, not the restored original.
	var wantWriter io.Writer
	stdout, stderr := captureProcessOutput(t, func() {
		wantWriter = os.Stderr
		EnsureWorkspaceBootstrap(context.Background(), ws, &wsproto.Config{}, BootstrapOptions{Command: "build", Output: "jsonl"})
	})

	if gotWriter != wantWriter {
		t.Fatalf("structured bootstrap handed the install family %v, want os.Stderr", gotWriter)
	}
	if stdout != "" {
		t.Fatalf("structured bootstrap wrote to stdout: %q", stdout)
	}
	if !strings.Contains(stderr, "human install output") {
		t.Fatalf("implicit install output was not routed to stderr: %q", stderr)
	}
}

// The human path keeps stdout: only a structured mode gives it up.
func TestEnsureWorkspaceBootstrap_HumanInstallKeepsStdout(t *testing.T) {
	resetBootstrapEnv(t)
	orig := installRunner
	t.Cleanup(func() { installRunner = orig })
	ws := t.TempDir()
	writeLock(t, ws, "putnami.lock.json", `{}`) // stale: no marker yet
	var gotWriter io.Writer
	installRunner = func(_ context.Context, _ string, _ *wsproto.Config, _ []string, env LifecycleEnv) error {
		gotWriter = env.Out
		return nil
	}

	EnsureWorkspaceBootstrap(context.Background(), ws, &wsproto.Config{}, BootstrapOptions{Command: "build"})

	if gotWriter != os.Stdout {
		t.Fatalf("human bootstrap handed the install family %v, want os.Stdout", gotWriter)
	}
}

// The bootstrap must hand the engine-backed workspace-job runner down to the
// install it triggers; without it, a fresh checkout's `putnami install` would
// reach commands.runWorkspaceJob with nothing to run the workspace-install pass.
func TestEnsureWorkspaceBootstrap_ForwardsTheWorkspaceJobRunner(t *testing.T) {
	resetBootstrapEnv(t)
	orig := installRunner
	t.Cleanup(func() { installRunner = orig })
	ws := t.TempDir()
	writeLock(t, ws, "putnami.lock.json", `{}`) // stale: no marker yet
	var gotRunner bool
	installRunner = func(_ context.Context, _ string, _ *wsproto.Config, _ []string, env LifecycleEnv) error {
		gotRunner = env.RunJob != nil
		return nil
	}

	runner := func(context.Context, WorkspaceJobRequest) (WorkspaceJobResult, error) {
		return WorkspaceJobResult{Outcome: WorkspaceJobOK}, nil
	}
	EnsureWorkspaceBootstrap(context.Background(), ws, &wsproto.Config{}, BootstrapOptions{Command: "build", RunJob: runner})

	if !gotRunner {
		t.Fatal("bootstrap must forward BootstrapOptions.RunJob to the install it triggers")
	}
}

// The bootstrap hands the install the step a hosted run takes before its first
// repository code: a hosted build starts its cache provider there, so the
// build that follows reads the remote cache through it.
func TestEnsureWorkspaceBootstrap_ForwardsTheStepBeforeRepositoryCode(t *testing.T) {
	resetBootstrapEnv(t)
	orig := installRunner
	t.Cleanup(func() { installRunner = orig })
	ws := t.TempDir()
	writeLock(t, ws, "putnami.lock.json", `{}`) // stale: no marker yet
	var called bool
	installRunner = func(ctx context.Context, _ string, _ *wsproto.Config, _ []string, env LifecycleEnv) error {
		if env.BeforeRepositoryCode == nil {
			t.Fatal("the install received no BeforeRepositoryCode")
		}
		return env.BeforeRepositoryCode(ctx)
	}

	EnsureWorkspaceBootstrap(context.Background(), ws, &wsproto.Config{}, BootstrapOptions{
		Command:              "build",
		BeforeRepositoryCode: func(context.Context) error { called = true; return nil },
	})

	if !called {
		t.Fatal("bootstrap must forward BootstrapOptions.BeforeRepositoryCode to the install it triggers")
	}
}

func TestEnsureWorkspaceBootstrap_SkipsWhenFresh(t *testing.T) {
	resetBootstrapEnv(t)
	calls := spyInstall(t, nil)
	ws := t.TempDir()
	writeLock(t, ws, "putnami.lock.json", `{}`)
	if err := writeInstallState(ws); err != nil {
		t.Fatal(err)
	}

	EnsureWorkspaceBootstrap(context.Background(), ws, &wsproto.Config{}, BootstrapOptions{Command: "build"})

	if *calls != 0 {
		t.Errorf("a current workspace must not install; got %d calls", *calls)
	}
}

func TestEnsureWorkspaceBootstrap_SkipsOnPlanAndDryRun(t *testing.T) {
	for _, tc := range []BootstrapOptions{
		{Command: "build", Plan: true},
		{Command: "build", DryRun: true},
	} {
		resetBootstrapEnv(t)
		calls := spyInstall(t, nil)
		ws := t.TempDir()
		writeLock(t, ws, "putnami.lock.json", `{}`) // stale

		EnsureWorkspaceBootstrap(context.Background(), ws, &wsproto.Config{}, tc)

		if *calls != 0 {
			t.Errorf("%+v must not mutate the workspace; got %d calls", tc, *calls)
		}
	}
}

func TestEnsureWorkspaceBootstrap_SkipsOnNoAutoInstallEnv(t *testing.T) {
	resetBootstrapEnv(t)
	t.Setenv(noAutoInstallEnv, "1")
	calls := spyInstall(t, nil)
	ws := t.TempDir()
	writeLock(t, ws, "putnami.lock.json", `{}`) // stale

	EnsureWorkspaceBootstrap(context.Background(), ws, &wsproto.Config{}, BootstrapOptions{Command: "build"})

	if *calls != 0 {
		t.Errorf("PUTNAMI_NO_AUTO_INSTALL must opt out; got %d calls", *calls)
	}
}

func TestEnsureWorkspaceBootstrap_SkipsInstallFamilyCommands(t *testing.T) {
	for _, cmd := range []string{"install", "deps", "workspace-install", "deps-upgrade", "upgrade"} {
		resetBootstrapEnv(t)
		calls := spyInstall(t, nil)
		ws := t.TempDir()
		writeLock(t, ws, "putnami.lock.json", `{}`) // stale

		EnsureWorkspaceBootstrap(context.Background(), ws, &wsproto.Config{}, BootstrapOptions{Command: cmd})

		if *calls != 0 {
			t.Errorf("command %q does its own install; got %d calls", cmd, *calls)
		}
	}
}

// TestEnsureWorkspaceBootstrap_SkipsTheCredentialSeam pins the seam contract:
// `cloud registry-token` prints one bearer on stdout, so a stale lock must not
// start an implicit install in front of it. Any other `cloud` subcommand keeps
// the first-use install.
func TestEnsureWorkspaceBootstrap_SkipsTheCredentialSeam(t *testing.T) {
	resetBootstrapEnv(t)
	calls := spyInstall(t, nil)
	ws := t.TempDir()
	writeLock(t, ws, "putnami.lock.json", `{}`) // stale: the lock changed since the last install

	EnsureWorkspaceBootstrap(context.Background(), ws, &wsproto.Config{}, BootstrapOptions{
		Command:    registryproto.SeamParentCommand,
		Subcommand: registryproto.SeamSubcommand,
	})
	if *calls != 0 {
		t.Errorf("the credential seam must not run an implicit install; got %d calls", *calls)
	}

	EnsureWorkspaceBootstrap(context.Background(), ws, &wsproto.Config{}, BootstrapOptions{
		Command:    registryproto.SeamParentCommand,
		Subcommand: "status",
	})
	if *calls != 1 {
		t.Errorf("other cloud subcommands keep the first-use install; got %d calls", *calls)
	}
}

func TestEnsureWorkspaceBootstrap_SkipsWhenParentBootstrapped(t *testing.T) {
	resetBootstrapEnv(t)
	calls := spyInstall(t, nil)
	ws := t.TempDir()
	writeLock(t, ws, "putnami.lock.json", `{}`) // stale
	t.Setenv(workspaceBootstrappedEnv, ws)      // a parent putnami already did it

	EnsureWorkspaceBootstrap(context.Background(), ws, &wsproto.Config{}, BootstrapOptions{Command: "build"})

	if *calls != 0 {
		t.Errorf("a parent-bootstrapped workspace must skip; got %d calls", *calls)
	}
}

func TestEnsureWorkspaceBootstrap_BestEffortOnInstallFailure(t *testing.T) {
	resetBootstrapEnv(t)
	calls := spyInstall(t, errors.New("network down"))
	ws := t.TempDir()
	writeLock(t, ws, "putnami.lock.json", `{}`) // stale

	// Must not panic or otherwise abort; the command is expected to proceed.
	EnsureWorkspaceBootstrap(context.Background(), ws, &wsproto.Config{}, BootstrapOptions{Command: "build"})

	if *calls != 1 {
		t.Errorf("install should be attempted once; got %d calls", *calls)
	}
}

func TestEnsureWorkspaceBootstrap_RepairsMovedMtimeWithoutInstalling(t *testing.T) {
	resetBootstrapEnv(t)
	calls := spyInstall(t, nil)
	ws := t.TempDir()
	p := writeLock(t, ws, "putnami.lock.json", `{"x":1}`)
	if err := writeInstallState(ws); err != nil {
		t.Fatal(err)
	}
	// Move mtime without changing content (git-pull shape).
	future := time.Now().Add(3 * time.Hour)
	if err := os.Chtimes(p, future, future); err != nil {
		t.Fatal(err)
	}

	EnsureWorkspaceBootstrap(context.Background(), ws, &wsproto.Config{}, BootstrapOptions{Command: "build"})

	if *calls != 0 {
		t.Errorf("a moved mtime with identical content must not install; got %d calls", *calls)
	}
	// The marker should have been repaired so the next command stat-matches.
	st, err := readInstallState(ws)
	if err != nil {
		t.Fatal(err)
	}
	if st.LockFiles[0].ModTime != future.UnixNano() {
		t.Errorf("marker mtime not repaired: got %d want %d", st.LockFiles[0].ModTime, future.UnixNano())
	}
}

func captureProcessOutput(t *testing.T, fn func()) (string, string) {
	t.Helper()

	origStdout := os.Stdout
	origStderr := os.Stderr
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		_ = stdoutW.Close()
		_ = stdoutR.Close()
		t.Fatal(err)
	}

	restored := false
	restore := func() {
		if restored {
			return
		}
		os.Stdout = origStdout
		os.Stderr = origStderr
		restored = true
	}
	defer func() {
		_ = stdoutW.Close()
		_ = stderrW.Close()
		_ = stdoutR.Close()
		_ = stderrR.Close()
	}()
	defer restore()

	os.Stdout = stdoutW
	os.Stderr = stderrW
	stdoutDone := sharedtest.DrainCapturedStream(stdoutR)
	stderrDone := sharedtest.DrainCapturedStream(stderrR)
	fn()

	if err := stdoutW.Close(); err != nil {
		t.Fatal(err)
	}
	if err := stderrW.Close(); err != nil {
		t.Fatal(err)
	}
	restore()

	stdoutResult := <-stdoutDone
	if stdoutResult.Err != nil {
		t.Fatalf("read stdout: %v", stdoutResult.Err)
	}
	stderrResult := <-stderrDone
	if stderrResult.Err != nil {
		t.Fatalf("read stderr: %v", stderrResult.Err)
	}
	return string(stdoutResult.Data), string(stderrResult.Data)
}

func TestCaptureProcessOutput_DrainsBothStreamsBeyondPipeCapacity(t *testing.T) {
	wantOut := strings.Repeat("o", 256*1024)
	wantErr := strings.Repeat("e", 256*1024)
	gotOut, gotErr := captureProcessOutput(t, func() {
		if _, err := io.WriteString(os.Stdout, wantOut); err != nil {
			t.Fatalf("write stdout capacity probe: %v", err)
		}
		if _, err := io.WriteString(os.Stderr, wantErr); err != nil {
			t.Fatalf("write stderr capacity probe: %v", err)
		}
	})
	if gotOut != wantOut {
		t.Fatalf("captured %d stdout bytes, want %d", len(gotOut), len(wantOut))
	}
	if gotErr != wantErr {
		t.Fatalf("captured %d stderr bytes, want %d", len(gotErr), len(wantErr))
	}
}
