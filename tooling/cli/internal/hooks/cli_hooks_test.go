package hooks

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/hometest"
)

func intPtr(v int) *int    { return &v }
func boolPtr(v bool) *bool { return &v }

// skipWithoutSh guards the tests that actually spawn a shell.
func skipWithoutSh(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("sh not available on windows")
	}
}

func TestRunCLIHooks_Nil(t *testing.T) {
	err := RunCLIHooks(context.Background(), nil, "before", t.TempDir(), false)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
}

func TestRunCLIHooks_NoCLI(t *testing.T) {
	cfg := &wsproto.HooksConfig{}
	err := RunCLIHooks(context.Background(), cfg, "before", t.TempDir(), false)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
}

func TestRunCommandHooks_Nil(t *testing.T) {
	err := RunCommandHooks(context.Background(), nil, "before", []string{"publish"}, t.TempDir(), false)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
}

func TestRunCommandHooks_NoMatch(t *testing.T) {
	cfg := &wsproto.HooksConfig{
		Commands: map[string]*wsproto.HookPhaseConfig{
			"build": {Before: []string{"echo build"}},
		},
	}
	err := RunCommandHooks(context.Background(), cfg, "before", []string{"publish"}, t.TempDir(), false)
	if err != nil {
		t.Fatalf("expected nil error for non-matching command, got %v", err)
	}
}

func TestRunCommandHooks_Executes(t *testing.T) {
	skipWithoutSh(t)

	root := t.TempDir()
	marker := filepath.Join(t.TempDir(), "hook-ran")
	cfg := &wsproto.HooksConfig{
		Commands: map[string]*wsproto.HookPhaseConfig{
			"publish": {Before: []string{"touch " + marker}},
		},
	}
	err := RunCommandHooks(context.Background(), cfg, "before", []string{"publish"}, root, false)
	if err != nil {
		t.Fatalf("hook failed: %v", err)
	}

	if _, err := os.Stat(marker); os.IsNotExist(err) {
		t.Error("hook command did not execute (marker file missing)")
	}
}

func TestRunCommandHooks_Wildcard(t *testing.T) {
	skipWithoutSh(t)

	marker := filepath.Join(t.TempDir(), "wildcard-ran")
	cfg := &wsproto.HooksConfig{
		Commands: map[string]*wsproto.HookPhaseConfig{
			"*": {Before: []string{"touch " + marker}},
		},
	}
	err := RunCommandHooks(context.Background(), cfg, "before", []string{"anything"}, t.TempDir(), false)
	if err != nil {
		t.Fatalf("hook failed: %v", err)
	}

	if _, err := os.Stat(marker); os.IsNotExist(err) {
		t.Error("wildcard hook did not execute")
	}
}

func TestRunCommandHooks_AfterPhase(t *testing.T) {
	cfg := &wsproto.HooksConfig{
		Commands: map[string]*wsproto.HookPhaseConfig{
			"publish": {
				Before: []string{"echo before"},
				After:  []string{"echo after"},
			},
		},
	}
	// "before" phase should not run "after" hooks
	err := RunCommandHooks(context.Background(), cfg, "after", []string{"publish"}, t.TempDir(), false)
	if err != nil {
		t.Fatalf("hook failed: %v", err)
	}
}

func TestRunCommandHooks_FailingCommand(t *testing.T) {
	skipWithoutSh(t)

	cfg := &wsproto.HooksConfig{
		Commands: map[string]*wsproto.HookPhaseConfig{
			"publish": {Before: []string{"exit 1"}},
		},
	}
	err := RunCommandHooks(context.Background(), cfg, "before", []string{"publish"}, t.TempDir(), false)
	if err == nil {
		t.Fatal("expected error for failing hook")
	}
}

func TestRunCommandHooks_DedupesCommands(t *testing.T) {
	skipWithoutSh(t)

	counter := filepath.Join(t.TempDir(), "count")
	cfg := &wsproto.HooksConfig{
		Commands: map[string]*wsproto.HookPhaseConfig{
			"publish": {Before: []string{"echo x >> " + counter}},
		},
	}
	// Duplicate command names should only trigger hooks once
	err := RunCommandHooks(context.Background(), cfg, "before", []string{"publish", "publish"}, t.TempDir(), false)
	if err != nil {
		t.Fatalf("hook failed: %v", err)
	}

	data, _ := os.ReadFile(counter)
	lines := 0
	for _, b := range data {
		if b == '\n' {
			lines++
		}
	}
	if lines != 1 {
		t.Errorf("expected hook to run once, ran %d times", lines)
	}
}

func TestRunCLIHooks_Executes(t *testing.T) {
	skipWithoutSh(t)

	marker := filepath.Join(t.TempDir(), "cli-hook-ran")
	cfg := &wsproto.HooksConfig{
		CLI: &wsproto.HookPhaseConfig{
			Before: []string{"touch " + marker},
		},
	}
	err := RunCLIHooks(context.Background(), cfg, "before", t.TempDir(), false)
	if err != nil {
		t.Fatalf("hook failed: %v", err)
	}

	if _, err := os.Stat(marker); os.IsNotExist(err) {
		t.Error("CLI hook did not execute")
	}
}

func TestPhaseCommands(t *testing.T) {
	hook := &wsproto.HookPhaseConfig{
		Before: []string{"cmd1"},
		After:  []string{"cmd2", "cmd3"},
	}

	before := phaseCommands(hook, "before")
	if len(before) != 1 || before[0] != "cmd1" {
		t.Errorf("before = %v, want [cmd1]", before)
	}

	after := phaseCommands(hook, "after")
	if len(after) != 2 {
		t.Errorf("after = %v, want [cmd2, cmd3]", after)
	}

	unknown := phaseCommands(hook, "unknown")
	if unknown != nil {
		t.Errorf("unknown phase = %v, want nil", unknown)
	}

	nilResult := phaseCommands(nil, "before")
	if nilResult != nil {
		t.Errorf("nil hook = %v, want nil", nilResult)
	}
}

// --- workspace hooks run under the extension-hook contract ---

// TestRunCLIHooks_TimeoutFires pins the bound itself: a workspace
// hook used to have none, so a blocking hook wedged every invocation in the
// workspace until the user hit Ctrl-C.
func TestRunCLIHooks_TimeoutFires(t *testing.T) {
	skipWithoutSh(t)

	cfg := &wsproto.HooksConfig{
		CLI: &wsproto.HookPhaseConfig{
			Before:    []string{"sleep 30"},
			TimeoutMs: intPtr(150),
		},
	}

	start := time.Now()
	err := RunCLIHooks(context.Background(), cfg, "before", t.TempDir(), false)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a hook that outlives its timeout must fail the run")
	}
	if !strings.Contains(err.Error(), "timed out after 150ms") {
		t.Fatalf("error = %v, want a timeout naming the configured bound", err)
	}
	if !strings.Contains(err.Error(), "hooks.cli.before") {
		t.Fatalf("error = %v, want the declaring config path", err)
	}
	// The bound has to be the hook's, not the run's: a 30s sleep that returns
	// after 150ms proves the deadline killed it.
	if elapsed > 10*time.Second {
		t.Fatalf("timeout took %s, want the configured 150ms bound to fire", elapsed)
	}
}

// A shell hook's timeout applies to the process tree, not only to `sh` itself.
// On Linux, killing only the shell left its sleep child alive and cmd.Wait did
// not return until the full 30 seconds elapsed, defeating both this timeout and
// the engine's bounded after-hook cleanup.
func TestRunCLIHooks_TimeoutKillsChildProcess(t *testing.T) {
	skipWithoutSh(t)

	cfg := &wsproto.HooksConfig{
		CLI: &wsproto.HookPhaseConfig{
			Before:    []string{"sleep 30"},
			TimeoutMs: intPtr(150),
		},
	}
	start := time.Now()
	if err := RunCLIHooks(context.Background(), cfg, "before", t.TempDir(), false); err == nil {
		t.Fatal("a hook whose child outlives the timeout must fail")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("process-tree timeout took %s, want the child killed with its shell", elapsed)
	}
}

// TestRunCommandHooks_DefaultTimeoutApplies pins that a group WITHOUT a
// timeoutMs still gets one — the extension-hook default — rather than none.
func TestRunCommandHooks_DefaultTimeoutApplies(t *testing.T) {
	if DefaultHookTimeoutMs != 120_000 {
		t.Fatalf("DefaultHookTimeoutMs = %d, want the 120s extension-hook default", DefaultHookTimeoutMs)
	}
	collected := collectHooks(&wsproto.HookPhaseConfig{Before: []string{"true"}}, "hooks.cli", "before")
	if len(collected) != 1 {
		t.Fatalf("collected %d hooks, want 1", len(collected))
	}
	// 0 is the "unset" marker runShellCommand turns into DefaultHookTimeoutMs;
	// what must never happen is an unbounded context.
	if collected[0].timeoutMs != 0 {
		t.Fatalf("timeoutMs = %d, want 0 (unset → default)", collected[0].timeoutMs)
	}
	if collected[0].source != "hooks.cli.before" {
		t.Fatalf("source = %q, want hooks.cli.before", collected[0].source)
	}
}

// TestRunCLIHooks_RunsFromWorkspaceRoot pins the working directory: a
// hook-relative path resolves against the workspace root even when the process
// was invoked from a subdirectory, which is the supported way to use the CLI.
func TestRunCLIHooks_RunsFromWorkspaceRoot(t *testing.T) {
	skipWithoutSh(t)

	root := t.TempDir()
	sub := filepath.Join(root, "packages", "app")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	// Same relative name in both directories, different contents: whichever the
	// hook reads names the directory it actually ran in.
	if err := os.WriteFile(filepath.Join(root, "where.txt"), []byte("root"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "where.txt"), []byte("subdir"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "seen")
	t.Chdir(sub)

	cfg := &wsproto.HooksConfig{
		CLI: &wsproto.HookPhaseConfig{Before: []string{"cat ./where.txt > " + out}},
	}
	if err := RunCLIHooks(context.Background(), cfg, "before", root, false); err != nil {
		t.Fatalf("hook failed: %v", err)
	}

	seen, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(seen) != "root" {
		t.Fatalf("hook cwd resolved %q, want the workspace root", string(seen))
	}
}

// TestRunCLIHooks_InjectsWorkspaceEnv pins the two variables a workspace hook
// gets. PUTNAMI_WORKSPACE_ROOT survives a hook that cd's away; PUTNAMI_HOOK lets
// one script serve several phases.
func TestRunCLIHooks_InjectsWorkspaceEnv(t *testing.T) {
	skipWithoutSh(t)

	root := t.TempDir()
	out := filepath.Join(t.TempDir(), "env")
	cfg := &wsproto.HooksConfig{
		CLI: &wsproto.HookPhaseConfig{
			After: []string{`printf '%s|%s\n' "$PUTNAMI_WORKSPACE_ROOT" "$PUTNAMI_HOOK" > ` + out},
		},
	}
	if err := RunCLIHooks(context.Background(), cfg, "after", root, false); err != nil {
		t.Fatalf("hook failed: %v", err)
	}

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(string(data))
	wantRoot, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	if want := wantRoot + "|hooks.cli.after"; got != want {
		t.Fatalf("hook env = %q, want %q", got, want)
	}
}

// TestShellNotFound_NamesGitForWindows pins D-W3: on Windows the missing-sh
// error names Git for Windows as the prerequisite; elsewhere it is unchanged.
func TestShellNotFound_NamesGitForWindows(t *testing.T) {
	spectest.Proves(t, "cli/windows-consumers", "windows-prerequisites-are-named", "a-missing-sh-names-git-for-windows")
	cause := errors.New(`exec: "sh": executable file not found in %PATH%`)
	win := shellNotFoundOn("windows", cause)
	if !strings.Contains(win.Error(), "Git for Windows") || !errors.Is(win, cause) {
		t.Fatalf("windows error = %v, want Git for Windows named and the cause wrapped", win)
	}
	for _, goos := range []string{"linux", "darwin"} {
		if got := shellNotFoundOn(goos, cause).Error(); got != "shell not found: "+cause.Error() {
			t.Fatalf("%s error = %q, want the unchanged message", goos, got)
		}
	}
}

// TestRunCLIHooks_MissingShellNamesThePrerequisite runs a hook with no `sh`
// on PATH: it fails before spawning anything, with this platform's
// missing-shell error (on Windows, the one that names Git for Windows).
func TestRunCLIHooks_MissingShellNamesThePrerequisite(t *testing.T) {
	spectest.Proves(t, "cli/windows-consumers", "windows-prerequisites-are-named", "a-missing-sh-names-git-for-windows")
	t.Setenv("PATH", t.TempDir())
	cfg := &wsproto.HooksConfig{CLI: &wsproto.HookPhaseConfig{Before: []string{"echo never"}}}
	err := RunCLIHooks(context.Background(), cfg, "before", t.TempDir(), false)
	if err == nil {
		t.Fatal("a hook with no sh on PATH must fail")
	}
	if !strings.Contains(err.Error(), "shell not found") {
		t.Fatalf("error = %v, want the missing-shell error", err)
	}
	if runtime.GOOS == "windows" && !strings.Contains(err.Error(), "Git for Windows") {
		t.Fatalf("error = %v, want Git for Windows named", err)
	}
}

// TestShellFlag pins the default unconditionally, on every platform: `-c`, not
// the `-lc` that sourced the developer's profile before every hook.
func TestShellFlag(t *testing.T) {
	if got := shellFlag(false); got != "-c" {
		t.Fatalf("shellFlag(false) = %q, want -c (no login shell by default)", got)
	}
	if got := shellFlag(true); got != "-lc" {
		t.Fatalf("shellFlag(true) = %q, want -lc", got)
	}
}

// TestRunCLIHooks_LoginShellIsOptIn is the behavioral half of TestShellFlag: a
// profile that exports a marker must reach the hook only when the group asked
// for a login shell.
//
// The opt-in half runs FIRST and gates the assertion: if this platform's `sh`
// does not source ~/.profile in login mode, the default-off claim would be
// vacuous rather than true, so the test skips instead of passing for free.
func TestRunCLIHooks_LoginShellIsOptIn(t *testing.T) {
	skipWithoutSh(t)

	home := t.TempDir()
	profile := "export PUTNAMI_TEST_PROFILE_MARKER=sourced\n"
	if err := os.WriteFile(filepath.Join(home, ".profile"), []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}
	hometest.Set(t, home)

	read := func(loginShell bool) string {
		t.Helper()
		out := filepath.Join(t.TempDir(), "marker")
		cfg := &wsproto.HooksConfig{
			CLI: &wsproto.HookPhaseConfig{
				Before:     []string{`printf '%s' "$PUTNAMI_TEST_PROFILE_MARKER" > ` + out},
				LoginShell: boolPtr(loginShell),
			},
		}
		if err := RunCLIHooks(context.Background(), cfg, "before", t.TempDir(), false); err != nil {
			t.Fatalf("hook failed (loginShell=%v): %v", loginShell, err)
		}
		data, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	if got := read(true); got != "sourced" {
		t.Skipf("this platform's sh does not source ~/.profile under -l (marker %q); "+
			"the default-off assertion would be vacuous here", got)
	}
	if got := read(false); got != "" {
		t.Fatalf("default hook saw the profile marker %q: workspace hooks must not source a login profile", got)
	}
}

// TestResolveHookRoot_SurfacesErrors pins that a working directory the CLI
// cannot use fails loudly. The old code assigned `cmd.Dir, _ = os.Getwd()`, and
// an empty cmd.Dir silently means "inherit whatever the process has".
func TestResolveHookRoot_SurfacesErrors(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := resolveHookRoot(file); err == nil {
		t.Fatal("a workspace root that is a file must be rejected")
	} else if !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("error = %v, want a not-a-directory diagnostic", err)
	}

	missing := filepath.Join(t.TempDir(), "gone")
	if _, err := resolveHookRoot(missing); err == nil {
		t.Fatal("a workspace root that does not exist must be rejected")
	}

	// No workspace at all (a hook from the global config, fired outside any
	// workspace): the process cwd stands in, and ITS error is returned rather
	// than discarded.
	sentinel := errors.New("getwd exploded")
	original := getwd
	getwd = func() (string, error) { return "", sentinel }
	t.Cleanup(func() { getwd = original })

	_, err := resolveHookRoot("")
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want the os.Getwd error to surface", err)
	}
}

// TestRunCLIHooks_RootErrorFailsTheGroup pins that the root is resolved once,
// before any command runs, so a bad root aborts the group instead of running
// half of it from an inherited directory.
func TestRunCLIHooks_RootErrorFailsTheGroup(t *testing.T) {
	skipWithoutSh(t)

	marker := filepath.Join(t.TempDir(), "should-not-exist")
	cfg := &wsproto.HooksConfig{
		CLI: &wsproto.HookPhaseConfig{Before: []string{"touch " + marker}},
	}

	missing := filepath.Join(t.TempDir(), "gone")
	if err := RunCLIHooks(context.Background(), cfg, "before", missing, false); err == nil {
		t.Fatal("an unusable workspace root must fail the hook group")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("hook ran despite an unusable workspace root")
	}
}

// TestRunCLIHooks_CanceledRunIsReportedAsStopped keeps the two failure modes
// distinguishable: a hook killed because the run was stopped must not be
// reported as having blown a timeout it never reached.
func TestRunCLIHooks_CanceledRunIsReportedAsStopped(t *testing.T) {
	skipWithoutSh(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	cfg := &wsproto.HooksConfig{
		CLI: &wsproto.HookPhaseConfig{Before: []string{"true"}},
	}
	err := RunCLIHooks(ctx, cfg, "before", t.TempDir(), false)
	if err == nil {
		t.Fatal("a hook cannot succeed under a canceled run context")
	}
	if !strings.Contains(err.Error(), "stopped before it finished") {
		t.Fatalf("error = %v, want a stopped-run diagnostic", err)
	}
}
