package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	extensionproto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"

	// Aliased: `hooks` is the local variable name throughout this file.
	hookspkg "go.putnami.dev/tooling/cli/internal/hooks"
)

// Lifecycle hooks moved into Engine.Run. They used to
// bracket runJobCommands in the CLI shell, so these tests pin the four
// properties that made that bracket correct: a failing before-hook aborts the
// run, after-hooks run even when the run failed, an after-hook failure can only
// turn success into failure, and a request without hooks runs none.

// requireSh skips a test whose hooks need sh where the product finds none:
// workspace hooks run through the sh on PATH, which Windows has only with Git
// for Windows (decision D-W3).
func requireSh(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("workspace hooks run through sh, and sh is not on PATH")
	}
}

// shPath quotes path for a hook's sh command line. The slash form keeps a
// Windows path's backslashes from reading as escapes; sh on Windows accepts it.
func shPath(path string) string {
	return "'" + strings.ReplaceAll(filepath.ToSlash(path), "'", `'\''`) + "'"
}

// hookRecorder builds a hooks config whose commands append their name to a file,
// so the test can assert both that a hook ran and in which order.
func hookRecorder(t *testing.T) (*wsproto.HooksConfig, func() []string) {
	t.Helper()
	requireSh(t)
	log := filepath.Join(t.TempDir(), "hooks.log")
	cmd := func(name string) string {
		return "printf '%s\\n' " + name + " >> " + shPath(log)
	}
	cfg := &wsproto.HooksConfig{
		CLI: &wsproto.HookPhaseConfig{
			Before: []string{cmd("cli-before")},
			After:  []string{cmd("cli-after")},
		},
		Commands: map[string]*wsproto.HookPhaseConfig{
			"build": {
				Before: []string{cmd("command-before")},
				After:  []string{cmd("command-after")},
			},
		},
	}
	return cfg, func() []string {
		data, err := os.ReadFile(log)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			t.Fatalf("read hook log: %v", err)
		}
		var ran []string
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				ran = append(ran, line)
			}
		}
		return ran
	}
}

// runWithHooks drives Engine.Run over a workspace-less root: the lifecycle stops
// at workspace load, which is enough to observe the hook bracket around it.
func runWithHooks(t *testing.T, hooks *wsproto.HooksConfig) SessionResult {
	t.Helper()
	var result SessionResult
	_ = captureStderr(t, func() {
		_ = captureStdout(t, func() {
			result, _ = New().Run(context.Background(), Request{
				WorkspaceRoot: t.TempDir(),
				Config:        &wsproto.Config{},
				Commands:      []string{"build"},
				Hooks:         hooks,
			}, nil)
		})
	})
	return result
}

func TestRun_HooksBracketTheLifecycleInOrder(t *testing.T) {
	hooks, ran := hookRecorder(t)
	runWithHooks(t, hooks)

	got := strings.Join(ran(), ",")
	const want = "cli-before,command-before,command-after,cli-after"
	if got != want {
		t.Fatalf("hook order = %q, want %q", got, want)
	}
}

func TestRun_CapturesCloudCapabilityBeforeRepositoryHooks(t *testing.T) {
	requireSh(t)
	t.Setenv(extensionproto.CloudTokenEnv, "hook-must-not-inherit-cloud-capability")
	t.Setenv(extensionproto.CloudCapabilityAfterEnv, "lint,test,build,validate,validate-workspace")
	root := t.TempDir()
	observed := filepath.Join(root, "hook-observed")
	hooks := &wsproto.HooksConfig{CLI: &wsproto.HookPhaseConfig{Before: []string{
		`if [ "${` + extensionproto.CloudTokenEnv + `+x}" = x ] || [ "${` + extensionproto.CloudCapabilityAfterEnv + `+x}" = x ]; then printf leaked; else printf absent; fi > ` + shPath(observed),
	}}}

	var result SessionResult
	_ = captureStderr(t, func() {
		result, _ = New().Run(context.Background(), Request{
			WorkspaceRoot: root,
			Config:        &wsproto.Config{},
			Commands:      []string{"build"},
			Hooks:         hooks,
		}, nil)
	})
	if result.ExitCode != ExitError {
		t.Fatalf("workspace-less run exit code = %d, want %d", result.ExitCode, ExitError)
	}
	data, err := os.ReadFile(observed)
	if err != nil {
		t.Fatalf("read hook observation: %v", err)
	}
	if got := string(data); got != "absent" {
		t.Fatalf("repository hook observed cloud capability: %q", got)
	}
	for _, name := range []string{extensionproto.CloudTokenEnv, extensionproto.CloudCapabilityAfterEnv} {
		if _, present := os.LookupEnv(name); present {
			t.Fatalf("Engine.Run restored %s into the process environment", name)
		}
	}
}

func TestRun_FailingBeforeHookAbortsBeforeTheLifecycle(t *testing.T) {
	hooks, ran := hookRecorder(t)
	hooks.CLI.Before = []string{"false"}

	result := runWithHooks(t, hooks)
	if result.ExitCode != ExitError {
		t.Fatalf("exit code = %d, want %d", result.ExitCode, ExitError)
	}
	// Neither the command before-hook nor either after-hook may run once the CLI
	// before-hook failed: the run never started.
	if got := ran(); len(got) != 0 {
		t.Fatalf("hooks ran after a failing before-hook: %v", got)
	}
}

func TestRun_AfterHookFailureOnlyDowngradesSuccess(t *testing.T) {
	// The run itself already failed (no workspace), so a failing after-hook must
	// not overwrite its code.
	hooks, _ := hookRecorder(t)
	hooks.CLI.After = []string{"false"}
	if code := runWithHooks(t, hooks).ExitCode; code != ExitError {
		t.Fatalf("exit code = %d, want the run's own %d", code, ExitError)
	}

	// And with no hooks at all, nothing runs and the code is untouched.
	if code := runWithHooks(t, nil).ExitCode; code != ExitError {
		t.Fatalf("exit code without hooks = %d, want %d", code, ExitError)
	}
}

func TestRunAfterHooks_TurnsSuccessIntoFailure(t *testing.T) {
	hooks, ran := hookRecorder(t)
	hooks.Commands["build"].After = []string{"false"}
	req := &Request{WorkspaceRoot: t.TempDir(), Commands: []string{"build"}, Hooks: hooks}

	var code int
	var err error
	_ = captureStderr(t, func() {
		code, err = runAfterHooks(context.Background(), req, false, ExitSuccess)
	})
	if err == nil {
		t.Fatal("a failing after-hook must report an error")
	}
	if code != ExitError {
		t.Fatalf("exit code = %d, want %d", code, ExitError)
	}
	// The CLI after-hook still runs after the command after-hook failed.
	if got := strings.Join(ran(), ","); got != "cli-after" {
		t.Fatalf("hooks ran = %q, want the cli after-hook to still run", got)
	}
}

func TestRunHooks_NilHooksAreANoOp(t *testing.T) {
	t.Parallel()
	req := &Request{WorkspaceRoot: t.TempDir(), Commands: []string{"build"}}
	if err := runBeforeHooks(context.Background(), req, false); err != nil {
		t.Fatalf("nil hooks before: %v", err)
	}
	code, err := runAfterHooks(context.Background(), req, false, ExitSuccess)
	if err != nil || code != ExitSuccess {
		t.Fatalf("nil hooks after = (%d, %v), want (%d, nil)", code, err, ExitSuccess)
	}
}

// --- after-hooks are cleanup, so an abort must not skip them ---

// TestRunAfterHooks_StillRunAfterAbort pins the cleanup contract: the run
// context is already canceled (Ctrl-C), and both after-hooks still run. Passing
// the canceled context straight to exec.CommandContext killed them instantly,
// so a hook that released a lock or tore down a container never fired on the
// one path where it mattered most.
func TestRunAfterHooks_StillRunAfterAbort(t *testing.T) {
	hooks, ran := hookRecorder(t)
	req := &Request{WorkspaceRoot: t.TempDir(), Commands: []string{"build"}, Hooks: hooks}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var code int
	var err error
	_ = captureStderr(t, func() {
		code, err = runAfterHooks(ctx, req, false, ExitSuccess)
	})
	if err != nil {
		t.Fatalf("cleanup after-hooks failed under an aborted run: %v", err)
	}
	if code != ExitSuccess {
		t.Fatalf("exit code = %d, want %d", code, ExitSuccess)
	}
	if got := strings.Join(ran(), ","); got != "command-after,cli-after" {
		t.Fatalf("hooks ran = %q, want both after-hooks to run despite the abort", got)
	}
}

// TestRunAfterHooks_CancellationBetweenGroupsStillRunsCLICleanup covers the
// transition that an entry-only cancellation check misses: Ctrl-C can arrive
// while the command after-hook is running. That hook stops, but the CLI-level
// cleanup that follows must get the bounded detached context and still run.
func TestRunAfterHooks_CancellationBetweenGroupsStillRunsCLICleanup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh not available on windows")
	}

	root := t.TempDir()
	started := filepath.Join(root, "command-started")
	cleaned := filepath.Join(root, "cli-cleaned")
	hooks := &wsproto.HooksConfig{
		CLI: &wsproto.HookPhaseConfig{
			After: []string{"touch " + cleaned},
		},
		Commands: map[string]*wsproto.HookPhaseConfig{
			"build": {
				After: []string{"touch " + started + "; while :; do :; done"},
			},
		},
	}
	req := &Request{WorkspaceRoot: root, Commands: []string{"build"}, Hooks: hooks}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		// Cancel only once the command hook has demonstrably started: a
		// cancel that fires before the fork lands proves nothing about
		// cancellation BETWEEN groups. The bound is a hang detector, far
		// above any spawn delay host load can add.
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(started); err == nil {
				cancel()
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Errorf("command after-hook did not create %s within 60s; canceling anyway", started)
		cancel()
	}()

	var code int
	_ = captureStderr(t, func() {
		code, _ = runAfterHooks(ctx, req, false, ExitSuccess)
	})
	if code != ExitError {
		t.Fatalf("exit code = %d, want the canceled command hook to fail the run (%d)", code, ExitError)
	}
	if _, err := os.Stat(cleaned); err != nil {
		t.Fatalf("CLI cleanup did not run after cancellation reached the command hook: %v", err)
	}
}

// TestRunAfterHooks_CleanupStaysBounded is the other half: detaching from the
// run's cancellation must not make Ctrl-C unresponsive. A cleanup hook that
// ignores cancellation is cut off by the shared cleanup budget.
func TestRunAfterHooks_CleanupStaysBounded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh not available on windows")
	}

	original := abortCleanupBudget
	abortCleanupBudget = 200 * time.Millisecond
	t.Cleanup(func() { abortCleanupBudget = original })

	hooks, _ := hookRecorder(t)
	// No per-hook timeoutMs on purpose: only the shared budget can stop this.
	hooks.Commands["build"].After = []string{"sleep 30"}
	req := &Request{WorkspaceRoot: t.TempDir(), Commands: []string{"build"}, Hooks: hooks}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	var code int
	_ = captureStderr(t, func() {
		code, _ = runAfterHooks(ctx, req, false, ExitSuccess)
	})
	elapsed := time.Since(start)

	if elapsed > 10*time.Second {
		t.Fatalf("aborted cleanup took %s, want the cleanup budget to cut it off", elapsed)
	}
	if code != ExitError {
		t.Fatalf("exit code = %d, want a cut-off cleanup hook to fail the run (%d)", code, ExitError)
	}
}

// TestAbortCleanupBudget_FitsTheShutdownWindow ties the constant to the promise
// cmd/putnami's signal handler makes: it force-exits 10s after the first signal,
// and BOTH after-hook calls share one budget of this size.
func TestAbortCleanupBudget_FitsTheShutdownWindow(t *testing.T) {
	t.Parallel()
	const shutdownTimeout = 10 * time.Second
	if hookspkg.AbortCleanupBudget <= 0 || hookspkg.AbortCleanupBudget > shutdownTimeout/2 {
		t.Fatalf("AbortCleanupBudget = %s, want a positive budget of at most half the %s shutdown window "+
			"so the rest of the unwind still fits",
			hookspkg.AbortCleanupBudget, shutdownTimeout)
	}
}
