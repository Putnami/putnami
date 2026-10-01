package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	extensionproto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/lifecycle"
	"go.putnami.dev/tooling/cli/internal/engine"
	"go.putnami.dev/tooling/cli/internal/hometest"
	"go.putnami.dev/tooling/cli/internal/launch"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/telemetry"
)

func markTelemetryNoticeShown(t *testing.T, home string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(home, ".putnami-telemetry.json"), []byte(`{"enabled":true,"noticeShownAt":"2026-07-24T00:00:00Z"}`+"\n"), 0o644); err != nil {
		t.Fatalf("mark telemetry notice shown: %v", err)
	}
}

func TestAppRunConsumesLaunchedEnv(t *testing.T) {
	t.Setenv(launch.LaunchedEnv, "abc123")
	app := &App{}

	output := captureStdout(t, func() {
		if code := app.Run(context.Background(), []string{"--version"}); code != ExitSuccess {
			t.Fatalf("Run --version code = %d, want %d", code, ExitSuccess)
		}
	})
	if !strings.Contains(output, "launched from workspace pin") {
		t.Fatalf("version output missing launched annotation: %q", output)
	}
	if got := os.Getenv(launch.LaunchedEnv); got != "" {
		t.Fatalf("%s leaked after App.Run: %q", launch.LaunchedEnv, got)
	}
}

func TestAppRunCapturesCloudCapabilityBeforeDispatch(t *testing.T) {
	const capability = "app-dispatch-private-cloud-capability"
	const after = "lint,test,build,validate,validate-workspace"
	// Its own workspace, because --version is not relaunch-exempt: run from the
	// repository the launcher resolves this worktree's CLI pin, and the test
	// binary guard then refuses the exec with ExitUsage. The two sibling tests
	// dodge that by setting LaunchedEnv, which this one must not do — it asserts
	// the pre-relaunch capture. An empty directory has no pin to resolve.
	t.Chdir(t.TempDir())
	t.Setenv(extensionproto.CloudTokenEnv, capability)
	t.Setenv(extensionproto.CloudCapabilityAfterEnv, after)

	output := captureStdout(t, func() {
		if code := (&App{}).Run(context.Background(), []string{"--version"}); code != ExitSuccess {
			t.Fatalf("Run --version code = %d, want %d", code, ExitSuccess)
		}
	})
	for _, name := range []string{extensionproto.CloudTokenEnv, extensionproto.CloudCapabilityAfterEnv} {
		if _, present := os.LookupEnv(name); present {
			t.Fatalf("%s remained ambient after App.Run dispatch", name)
		}
	}
	if strings.Contains(output, capability) || strings.Contains(output, after) {
		t.Fatal("captured cloud capability control entered command output")
	}
}

func TestAppRunPinnedChildCapturesCapabilityOnlyAfterRelaunch(t *testing.T) {
	const capability = "pinned-child-private-cloud-capability"
	const after = "lint,test,build,validate,validate-workspace"
	t.Setenv(launch.LaunchedEnv, "abc123")
	t.Setenv(extensionproto.CloudTokenEnv, capability)
	t.Setenv(extensionproto.CloudCapabilityAfterEnv, after)

	output := captureStdout(t, func() {
		if code := (&App{}).Run(context.Background(), []string{"--version"}); code != ExitSuccess {
			t.Fatalf("Run --version code = %d, want %d", code, ExitSuccess)
		}
	})
	if !strings.Contains(output, "launched from workspace pin") {
		t.Fatalf("version output missing launched annotation: %q", output)
	}
	for _, name := range []string{launch.LaunchedEnv, extensionproto.CloudTokenEnv, extensionproto.CloudCapabilityAfterEnv} {
		if _, present := os.LookupEnv(name); present {
			t.Fatalf("pinned child retained %s after authoritative capture", name)
		}
	}
}

// unresolvablePinnedWorkspace creates a workspace pinned to a CLI version whose
// only integrity digest is for a platform this test can never run on, and chdirs
// into it. That is the cheapest true bricking scenario: the pin is real, nothing
// here can verify it, and no network is touched.
func unresolvablePinnedWorkspace(t *testing.T) string {
	t.Helper()
	// The suite runs under ./putnamiw, which exports PUTNAMI_NO_RELAUNCH for its
	// own from-source build and passes it to every child. Clear both launcher
	// env vars so these tests exercise the launcher itself rather than silently
	// passing on an inherited opt-out.
	t.Setenv(launch.NoRelaunchEnv, "")
	t.Setenv(launch.LaunchedEnv, "")

	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, wsproto.WorkspaceConfigFilename), []byte(`{"name":"ws"}`), 0o644); err != nil {
		t.Fatalf("write workspace config: %v", err)
	}
	lf := lockfile.NewLockFile()
	entry := lockfile.LockEntry{Version: "1.4.2"}
	entry.SetPlatformIntegrity(lockfile.PlatformKey("plan9", "s390x"),
		"abcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcab")
	lf.SetCLI(entry)
	if err := lockfile.WriteLockFile(ws, lf); err != nil {
		t.Fatalf("write lock: %v", err)
	}
	t.Chdir(ws)
	return ws
}

// A workspace whose CLI pin cannot be honored stops the run at the launcher
// instead of proceeding as some other engine — App.Run must surface the
// launcher's error and its recovery hint, not swallow it.
func TestAppRun_UnresolvablePinFailsClosed(t *testing.T) {
	unresolvablePinnedWorkspace(t)

	output := captureStdoutStderr(t, func() {
		if code := (&App{}).Run(context.Background(), []string{"build"}); code != ExitUsage {
			t.Fatalf("unresolvable pin exit = %d, want %d", code, ExitUsage)
		}
	})
	if !strings.Contains(output, lockfile.LockFilename) {
		t.Fatalf("output should name the lock file, got:\n%s", output)
	}
	if !strings.Contains(output, "Try: putnami pin 1.4.2") {
		t.Fatalf("output should name the recovery command, got:\n%s", output)
	}
}

// The recovery story has to actually work on the workspace the launcher just
// refused to run in: `pin` never enters the launcher, so it can still unpin, and
// the opt-out is read before the lock is.
func TestAppRun_RecoveryPathsSurviveUnresolvablePin(t *testing.T) {
	t.Run("exempt invocation unpins", func(t *testing.T) {
		ws := unresolvablePinnedWorkspace(t)
		output := captureStdoutStderr(t, func() {
			if code := (&App{}).Run(context.Background(), []string{"pin", "--remove"}); code != ExitSuccess {
				t.Fatalf("`pin --remove` exit = %d, want %d", code, ExitSuccess)
			}
		})
		if !strings.Contains(output, "Removed the CLI version pin") {
			t.Fatalf("`pin` must reach its command, not the launcher, got:\n%s", output)
		}
		lf, err := lockfile.ReadLockFile(ws)
		if err != nil {
			t.Fatalf("read lock: %v", err)
		}
		if _, ok := lf.GetCLI(); ok {
			t.Fatal("the pin should be gone, unbricking the workspace")
		}
	})

	t.Run("env opt-out runs the current binary", func(t *testing.T) {
		unresolvablePinnedWorkspace(t)
		t.Setenv(launch.NoRelaunchEnv, "1")
		output := captureStdoutStderr(t, func() {
			if code := (&App{}).Run(context.Background(), []string{"--version"}); code != ExitSuccess {
				t.Fatalf("opt-out --version exit = %d, want %d", code, ExitSuccess)
			}
		})
		if strings.Contains(output, "Try: ") {
			t.Fatalf("%s must suppress the launcher entirely, got:\n%s", launch.NoRelaunchEnv, output)
		}
	})
}

func TestAppRun_ReportsEmptyCommandListParseError(t *testing.T) {
	t.Chdir(t.TempDir())

	output := captureStdoutStderr(t, func() {
		if code := (&App{}).Run(context.Background(), []string{","}); code != ExitUsage {
			t.Fatalf("Run comma-only command list exit = %d, want %d", code, ExitUsage)
		}
	})
	if !strings.Contains(output, "no command") {
		t.Fatalf("comma-only command list should report its parse error, got:\n%s", output)
	}
}

func TestRunWithConfigHooks_BeforeFailureRecordsTelemetryEnd(t *testing.T) {
	home := hometest.Temp(t)
	if err := telemetry.Enable(); err != nil {
		t.Fatalf("enable telemetry: %v", err)
	}
	markTelemetryNoticeShown(t, home)

	parsed := ParseArgs([]string{"build"}, nil, nil)
	cfg := &wsproto.Config{Hooks: &wsproto.HooksConfig{
		CLI: &wsproto.HookPhaseConfig{Before: []string{"false"}},
	}}
	if code := (&App{}).runTerminalSession(context.Background(), cfg, parsed, t.TempDir(), nil); code != ExitError {
		t.Fatalf("runTerminalSession exit code = %d, want %d", code, ExitError)
	}

	events, err := telemetry.Show()
	if err != nil {
		t.Fatalf("show telemetry: %v", err)
	}
	if len(events) != 1 || events[0].Name != "session:end" {
		t.Fatalf("telemetry events = %#v, want one session:end", events)
	}
	if success, _ := events[0].Data["success"].(bool); success {
		t.Fatalf("session:end success = true, want false: %#v", events[0].Data)
	}
	if got := events[0].Data["errorCategory"]; got != "failure" {
		t.Fatalf("session:end errorCategory = %v, want failure", got)
	}
}

func TestRunWithConfigHooks_HookOptOutDiscardsTelemetry(t *testing.T) {
	home := hometest.Temp(t)
	if err := telemetry.Enable(); err != nil {
		t.Fatalf("enable telemetry: %v", err)
	}
	markTelemetryNoticeShown(t, home)

	parsed := ParseArgs([]string{"build"}, nil, nil)
	cfg := &wsproto.Config{Hooks: &wsproto.HooksConfig{
		CLI: &wsproto.HookPhaseConfig{Before: []string{
			`printf '%s\n' '{"enabled":false}' > "$HOME/.putnami-telemetry.json"`,
		}},
	}}
	// The root holds no project and no repository, so the bare run selects
	// every project, finds none, and ends as a usage error after the hook ran.
	if code := (&App{}).runTerminalSession(context.Background(), cfg, parsed, t.TempDir(), nil); code != ExitUsage {
		t.Fatalf("runTerminalSession exit code = %d, want %d", code, ExitUsage)
	}

	events, err := telemetry.Show()
	if err != nil {
		t.Fatalf("show telemetry: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("telemetry events = %#v, want none after hook opt-out", events)
	}
}

func TestRunWithConfigHooks_InteractiveNoticeAppearsOnce(t *testing.T) {
	hometest.Temp(t)
	if err := telemetry.Enable(); err != nil {
		t.Fatalf("enable telemetry: %v", err)
	}
	originalTTY := stderrIsTTY
	stderrIsTTY = func() bool { return true }
	t.Cleanup(func() { stderrIsTTY = originalTTY })
	parsed := ParseArgs([]string{"build"}, nil, nil)
	cfg := &wsproto.Config{Hooks: &wsproto.HooksConfig{
		CLI: &wsproto.HookPhaseConfig{Before: []string{"false"}},
	}}

	first := captureStderr(t, func() {
		if code := (&App{}).runTerminalSession(context.Background(), cfg, parsed, t.TempDir(), nil); code != ExitError {
			t.Fatalf("first run exit code = %d, want %d", code, ExitError)
		}
	})
	if !strings.Contains(first, telemetry.FirstRunNotice) {
		t.Fatalf("first interactive run did not print notice: %q", first)
	}

	second := captureStderr(t, func() {
		if code := (&App{}).runTerminalSession(context.Background(), cfg, parsed, t.TempDir(), nil); code != ExitError {
			t.Fatalf("second run exit code = %d, want %d", code, ExitError)
		}
	})
	if strings.Contains(second, telemetry.FirstRunNotice) {
		t.Fatalf("second interactive run repeated notice: %q", second)
	}
}

// --- buildCommandParams ---

func TestBuildCommandParams_Empty(t *testing.T) {
	t.Parallel()
	params := buildCommandParams(nil)
	if len(params) != 0 {
		t.Errorf("expected empty params, got %v", params)
	}
}

func TestBuildCommandParams_BoolFlag(t *testing.T) {
	t.Parallel()
	params := buildCommandParams([]string{"--transpile"})
	if params["transpile"] != true {
		t.Errorf("transpile = %v, want true", params["transpile"])
	}
}

func TestBuildCommandParams_ValueFlag(t *testing.T) {
	t.Parallel()
	params := buildCommandParams([]string{"--target", "node"})
	if params["target"] != "node" {
		t.Errorf("target = %v, want node", params["target"])
	}
}

func TestBuildCommandParams_EqualsSyntax(t *testing.T) {
	t.Parallel()
	params := buildCommandParams([]string{"--output=jsonl"})
	if params["output"] != "jsonl" {
		t.Errorf("output = %v, want jsonl", params["output"])
	}
}

func TestBuildCommandParams_NoFlag(t *testing.T) {
	t.Parallel()
	params := buildCommandParams([]string{"--no-cache"})
	if params["cache"] != false {
		t.Errorf("cache = %v, want false", params["cache"])
	}
}

func TestBuildCommandParams_SkipsNonFlags(t *testing.T) {
	t.Parallel()
	params := buildCommandParams([]string{"positional", "--flag"})
	if _, ok := params["positional"]; ok {
		t.Error("positional args should be skipped")
	}
	if params["flag"] != true {
		t.Errorf("flag = %v, want true", params["flag"])
	}
}

func TestBuildCommandParams_FlagFollowedByFlag(t *testing.T) {
	t.Parallel()
	params := buildCommandParams([]string{"--verbose", "--quiet"})
	if params["verbose"] != true {
		t.Errorf("verbose = %v, want true", params["verbose"])
	}
	if params["quiet"] != true {
		t.Errorf("quiet = %v, want true", params["quiet"])
	}
}

func TestBuildCommandParams_Multiple(t *testing.T) {
	t.Parallel()
	params := buildCommandParams([]string{"--target", "linux/amd64", "--minify", "--output=jsonl"})
	if params["target"] != "linux/amd64" {
		t.Errorf("target = %v, want linux/amd64", params["target"])
	}
	if params["minify"] != true {
		t.Errorf("minify = %v, want true", params["minify"])
	}
	if params["output"] != "jsonl" {
		t.Errorf("output = %v, want jsonl", params["output"])
	}
}

// --- suppliedFlag ---

func TestSuppliedFlag_Found(t *testing.T) {
	t.Parallel()
	if !suppliedFlag([]string{"--global", "--verbose"}, "--global") {
		t.Error("expected --global to be found")
	}
}

func TestSuppliedFlag_NotFound(t *testing.T) {
	t.Parallel()
	if suppliedFlag([]string{"--verbose"}, "--global", "-g") {
		t.Error("expected flag not to be found")
	}
}

func TestSuppliedFlag_ShortAlias(t *testing.T) {
	t.Parallel()
	if !suppliedFlag([]string{"-g"}, "--global", "-g") {
		t.Error("expected -g to match")
	}
}

func TestSuppliedFlag_Empty(t *testing.T) {
	t.Parallel()
	if suppliedFlag(nil, "--global") {
		t.Error("empty args should return false")
	}
}

// TestSuppliedFlag_IgnoresPassthrough pins the one behavior the deleted
// containsFlag lacked: a spelling after "--" is the user's payload, not a flag
// for the CLI to act on.
func TestSuppliedFlag_IgnoresPassthrough(t *testing.T) {
	t.Parallel()
	if suppliedFlag([]string{"--", "--global"}, "--global") {
		t.Error("a spelling after -- is passthrough, not a supplied flag")
	}
}

// --- parseUpgradeFlags ---

func mustParseUpgradeFlags(t *testing.T, args []string) lifecycle.UpgradeFlags {
	t.Helper()
	flags, err := parseUpgradeFlags(args)
	if err != nil {
		t.Fatalf("parseUpgradeFlags(%v): %v", args, err)
	}
	return flags
}

func TestParseUpgradeFlags_RejectsRemovedAliases(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"--deps", "--putnami-version", "v1.2.3"},
		{"--deps", "--putnami-version=v1.2.3"},
		{"--local"},
	} {
		if _, err := parseUpgradeFlags(args); err == nil {
			t.Errorf("parseUpgradeFlags(%v) accepted a removed pre-OSS alias", args)
		}
	}
}

func TestParseUpgradeFlags_VersionPositional(t *testing.T) {
	t.Parallel()
	flags := mustParseUpgradeFlags(t, []string{"--deps", "v1.2.3"})
	if flags.Version != "v1.2.3" {
		t.Errorf("Version = %q, want v1.2.3", flags.Version)
	}
}

func TestParseUpgradeFlags_ChannelDryRun(t *testing.T) {
	t.Parallel()
	flags := mustParseUpgradeFlags(t, []string{"--channel", "canary", "--dry-run"})
	if flags.Channel != "canary" {
		t.Errorf("Channel = %q, want canary", flags.Channel)
	}
	if !flags.DryRun {
		t.Error("DryRun = false, want true")
	}
}

func TestParseUpgradeFlags_Release(t *testing.T) {
	t.Parallel()
	flags := mustParseUpgradeFlags(t, []string{"--deps", "--release", "rs_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"})
	if flags.Release != "rs_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Errorf("Release = %q, want immutable release id", flags.Release)
	}
}

func TestParseUpgradeFlags_FromSource(t *testing.T) {
	t.Parallel()
	flags := mustParseUpgradeFlags(t, []string{"--from-source"})
	if !flags.FromSource {
		t.Error("--from-source should select FromSource")
	}
}

// TestParseUpgradeFlags_RejectsUnknownChannel pins the third demonstrated silent
// failure mode: --channel was never enum-validated, so any string was
// forwarded to the release registry.
func TestParseUpgradeFlags_RejectsUnknownChannel(t *testing.T) {
	t.Parallel()
	if _, err := parseUpgradeFlags([]string{"--channel", "nightly"}); err == nil {
		t.Fatal("--channel nightly should be rejected")
	}
}

func TestParseUpgradeFlags_RejectsUnknownFlag(t *testing.T) {
	t.Parallel()
	if _, err := parseUpgradeFlags([]string{"--clii"}); err == nil {
		t.Fatal("a misspelled built-in flag should be rejected")
	}
}

func TestParseUpgradeFlags_RejectsMissingValue(t *testing.T) {
	t.Parallel()
	if _, err := parseUpgradeFlags([]string{"--channel"}); err == nil {
		t.Fatal("--channel with no value should be rejected")
	}
}

// --- applyEnvOverrides ---

func TestApplyEnvOverrides_OutputFromEnv(t *testing.T) {
	t.Setenv("PUTNAMI_OUTPUT", "jsonl")
	g := &GlobalFlags{}
	engine.ApplyEnvOverrides(g, &wsproto.Config{})
	if g.Output != "jsonl" {
		t.Errorf("Output = %q, want jsonl", g.Output)
	}
}

func TestApplyEnvOverrides_OutputNotOverriddenIfSet(t *testing.T) {
	t.Setenv("PUTNAMI_OUTPUT", "jsonl")
	g := &GlobalFlags{Output: "cloud-logging"}
	engine.ApplyEnvOverrides(g, &wsproto.Config{})
	if g.Output != "cloud-logging" {
		t.Errorf("Output = %q, should not be overridden", g.Output)
	}
}

func TestApplyEnvOverrides_DebugFromEnv(t *testing.T) {
	t.Setenv("PUTNAMI_DEBUG", "true")
	g := &GlobalFlags{}
	engine.ApplyEnvOverrides(g, &wsproto.Config{})
	if !g.Debug {
		t.Error("Debug should be set from env")
	}
	if !g.Verbose {
		t.Error("Verbose should also be set when Debug is set")
	}
}

func TestApplyEnvOverrides_VerboseFromEnv(t *testing.T) {
	t.Setenv("PUTNAMI_VERBOSE", "true")
	g := &GlobalFlags{}
	engine.ApplyEnvOverrides(g, &wsproto.Config{})
	if !g.Verbose {
		t.Error("Verbose should be set from env")
	}
}

func TestApplyEnvOverrides_QuietFromEnv(t *testing.T) {
	t.Setenv("PUTNAMI_QUIET", "true")
	g := &GlobalFlags{}
	engine.ApplyEnvOverrides(g, &wsproto.Config{})
	if !g.Quiet {
		t.Error("Quiet should be set from env")
	}
}

func TestApplyEnvOverrides_NoColorFromEnv(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	g := &GlobalFlags{}
	engine.ApplyEnvOverrides(g, &wsproto.Config{})
	if g.Color == nil || *g.Color {
		t.Error("Color should be false when NO_COLOR is set")
	}
}

func TestApplyEnvOverrides_OutputFromConfig(t *testing.T) {
	t.Parallel()
	g := &GlobalFlags{}
	cfg := &wsproto.Config{Output: "cloud-logging"}
	engine.ApplyEnvOverrides(g, cfg)
	if g.Output != "cloud-logging" {
		t.Errorf("Output = %q, want cloud-logging from config", g.Output)
	}
}

func TestApplyEnvOverrides_VerboseFromConfig(t *testing.T) {
	t.Parallel()
	g := &GlobalFlags{}
	v := true
	cfg := &wsproto.Config{Verbose: &v}
	engine.ApplyEnvOverrides(g, cfg)
	if !g.Verbose {
		t.Error("Verbose should be set from config")
	}
}

// --- resolveProfile ---

func TestResolveProfile_DefaultsToDev(t *testing.T) {
	t.Setenv("PUTNAMI_PROFILE", "")
	g := &GlobalFlags{}
	if err := resolveProfile(g, &wsproto.Config{}); err != nil {
		t.Fatalf("resolveProfile: %v", err)
	}
	if g.EnvProfile != "dev" {
		t.Errorf("EnvProfile = %q, want dev", g.EnvProfile)
	}
}

func TestResolveProfile_FlagWins(t *testing.T) {
	t.Setenv("PUTNAMI_PROFILE", "test")
	g := &GlobalFlags{EnvProfile: "production"}
	cfg := &wsproto.Config{Profile: "dev"}
	if err := resolveProfile(g, cfg); err != nil {
		t.Fatalf("resolveProfile: %v", err)
	}
	if g.EnvProfile != "production" {
		t.Errorf("EnvProfile = %q, want production (flag wins)", g.EnvProfile)
	}
}

func TestResolveProfile_FromEnv(t *testing.T) {
	t.Setenv("PUTNAMI_PROFILE", "test")
	g := &GlobalFlags{}
	cfg := &wsproto.Config{Profile: "production"}
	if err := resolveProfile(g, cfg); err != nil {
		t.Fatalf("resolveProfile: %v", err)
	}
	if g.EnvProfile != "test" {
		t.Errorf("EnvProfile = %q, want test (env beats config)", g.EnvProfile)
	}
}

func TestResolveProfile_FromConfig(t *testing.T) {
	t.Setenv("PUTNAMI_PROFILE", "")
	g := &GlobalFlags{}
	cfg := &wsproto.Config{Profile: "production"}
	if err := resolveProfile(g, cfg); err != nil {
		t.Fatalf("resolveProfile: %v", err)
	}
	if g.EnvProfile != "production" {
		t.Errorf("EnvProfile = %q, want production (from config)", g.EnvProfile)
	}
}

func TestResolveProfile_InvalidFlagHintsTraceProfile(t *testing.T) {
	t.Setenv("PUTNAMI_PROFILE", "")
	g := &GlobalFlags{EnvProfile: "/tmp/trace.json"}
	err := resolveProfile(g, &wsproto.Config{})
	if err == nil {
		t.Fatal("expected a usage error for a path-shaped --profile value")
	}
	if !strings.Contains(err.Error(), "--trace-profile") {
		t.Errorf("error should point at --trace-profile, got %q", err.Error())
	}
}

func TestResolveProfile_InvalidEnvNoTraceHint(t *testing.T) {
	t.Setenv("PUTNAMI_PROFILE", "staging")
	g := &GlobalFlags{}
	err := resolveProfile(g, &wsproto.Config{})
	if err == nil {
		t.Fatal("expected a usage error for an invalid PUTNAMI_PROFILE value")
	}
	if strings.Contains(err.Error(), "--trace-profile") {
		t.Errorf("non-flag error should not mention --trace-profile, got %q", err.Error())
	}
}

// The buildSessionStats unit tests lived here until an earlier change deleted the
// helper with the v1 session writer it fed. The buckets they asserted are the
// canonical reducer's, pinned at the source in internal/jobs (result_reduce)
// and on the wire by internal/cli's v2 session-file golden.

// --- App.Run simple paths ---

func TestAppRun_Version(t *testing.T) {
	// Help and version render from the binary alone. Without its own
	// workspace this runs under the repository's CLI pin, and the launcher
	// would exec away the whole test process — see launch.isTestBinary.
	t.Chdir(t.TempDir())
	app, err := NewApp()
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	code := app.Run(context.Background(), []string{"--version"})
	if code != ExitSuccess {
		t.Errorf("--version exit code = %d, want %d", code, ExitSuccess)
	}
}

func TestAppRun_VersionMachineDeclaration(t *testing.T) {
	t.Chdir(t.TempDir())
	out := captureStdout(t, func() {
		if code := (&App{}).Run(context.Background(), []string{"--version", "--output=json"}); code != ExitSuccess {
			t.Fatalf("--version --output=json exit = %d", code)
		}
	})
	var result protocolcli.ResultV2
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("decode version result: %v\n%s", err, out)
	}
	if result.ProtocolVersion != protocolcli.ResultProtocolVersion || result.Command != "version" {
		t.Fatalf("version result = %+v", result)
	}
	data, ok := result.Data.(map[string]any)
	if !ok || data["version"] != Version {
		t.Fatalf("version data = %#v, want version %q", result.Data, Version)
	}
}

func TestAppRun_Help(t *testing.T) {
	t.Chdir(t.TempDir())
	app, err := NewApp()
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	code := app.Run(context.Background(), []string{"--help"})
	if code != ExitSuccess {
		t.Errorf("--help exit code = %d, want %d", code, ExitSuccess)
	}
}

func TestAppRun_NoArgs(t *testing.T) {
	t.Chdir(t.TempDir())
	app, err := NewApp()
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	code := app.Run(context.Background(), []string{})
	if code != ExitSuccess {
		t.Errorf("no args exit code = %d, want %d", code, ExitSuccess)
	}
}

func TestAppRun_HelpForCommand(t *testing.T) {
	t.Chdir(t.TempDir())
	app, err := NewApp()
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	code := app.Run(context.Background(), []string{"build", "--help"})
	if code != ExitSuccess {
		t.Errorf("build --help exit code = %d, want %d", code, ExitSuccess)
	}
}

func TestAppRun_HelpForStructuredSubcommand(t *testing.T) {
	t.Chdir(t.TempDir())
	app, err := NewApp()
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}

	output := captureStdout(t, func() {
		code := app.Run(context.Background(), []string{"workspace", "init", "--help"})
		if code != ExitSuccess {
			t.Fatalf("workspace init --help exit code = %d, want %d", code, ExitSuccess)
		}
	})

	if !strings.Contains(output, "putnami workspace init") {
		t.Fatal("expected workspace init help header")
	}
	if !strings.Contains(output, "--project-path <path>") {
		t.Error("expected workspace init flag in help output")
	}
	if !strings.Contains(output, "--channel <channel>") {
		t.Error("expected the workspace init channel flag in help output")
	}
}

func TestAppRun_HelpCommandForStructuredSubcommand(t *testing.T) {
	t.Chdir(t.TempDir())
	app, err := NewApp()
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}

	output := captureStdout(t, func() {
		code := app.Run(context.Background(), []string{"help", "projects", "create"})
		if code != ExitSuccess {
			t.Fatalf("help projects create exit code = %d, want %d", code, ExitSuccess)
		}
	})

	if !strings.Contains(output, "putnami projects create") {
		t.Fatal("expected projects create help header")
	}
	if !strings.Contains(output, "--path <path>") {
		t.Error("expected projects create path flag in help output")
	}
}

func TestAppRun_UnknownStructuredSubcommand(t *testing.T) {
	t.Parallel()
	app, err := NewApp()
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	// "cache" is a structured command but needs a workspace; without one it errors
	// Use an unknown subcommand to test the error path
	code := app.Run(context.Background(), []string{"cache", "unknown-sub"})
	if code != ExitUsage {
		t.Errorf("cache unknown-sub exit code = %d, want %d", code, ExitUsage)
	}
}

func TestAppRun_UnknownInstallableSubcommandOutsideWorkspace(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "extensions",
			args: []string{"extensions", "unknown-sub"},
			want: "putnami: unknown subcommand: extensions unknown-sub",
		},
		{
			name: "templates",
			args: []string{"templates", "unknown-sub"},
			want: "putnami: unknown subcommand: templates unknown-sub",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Chdir(t.TempDir())

			app, err := NewApp()
			if err != nil {
				t.Fatalf("NewApp: %v", err)
			}

			output := captureStdoutStderr(t, func() {
				code := app.Run(context.Background(), tt.args)
				if code != ExitUsage {
					t.Fatalf("%v exit code = %d, want %d", tt.args, code, ExitUsage)
				}
			})

			if !strings.Contains(output, tt.want) {
				t.Fatalf("output missing %q\nfull output:\n%s", tt.want, output)
			}
			if strings.Contains(output, "no workspace found") {
				t.Fatalf("unknown subcommand should not be masked by workspace lookup\nfull output:\n%s", output)
			}
		})
	}
}

func TestAppRun_CompletionNoShell(t *testing.T) {
	t.Parallel()
	app, err := NewApp()
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	code := app.Run(context.Background(), []string{"completion"})
	if code != ExitUsage {
		t.Errorf("completion with no shell exit code = %d, want %d", code, ExitUsage)
	}
}

func TestAppRun_MigrateUnknown(t *testing.T) {
	t.Parallel()
	app, err := NewApp()
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	code := app.Run(context.Background(), []string{"migrate", "unknown"})
	if code != ExitUsage {
		t.Errorf("migrate unknown exit code = %d, want %d", code, ExitUsage)
	}
}

func TestAppRun_TelemetryUnknownSub(t *testing.T) {
	t.Parallel()
	app, err := NewApp()
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	code := app.Run(context.Background(), []string{"telemetry", "unknown-sub"})
	if code != ExitUsage {
		t.Errorf("telemetry unknown-sub exit code = %d, want %d", code, ExitUsage)
	}
}

func TestAppRun_DevUnknownSub(t *testing.T) {
	t.Parallel()
	app, err := NewApp()
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	code := app.Run(context.Background(), []string{"dev", "unknown-sub"})
	if code != ExitUsage {
		t.Errorf("dev unknown-sub exit code = %d, want %d", code, ExitUsage)
	}
}

// --- parseFlags ---

func TestParseFlags_BooleanFlags(t *testing.T) {
	t.Parallel()
	tests := []struct {
		arg   string
		check func(GlobalFlags) bool
		desc  string
	}{
		{"--verbose", func(g GlobalFlags) bool { return g.Verbose }, "Verbose"},
		{"-v", func(g GlobalFlags) bool { return g.Verbose }, "Verbose (short)"},
		{"--debug", func(g GlobalFlags) bool { return g.Debug && g.Verbose }, "Debug implies Verbose"},
		{"--quiet", func(g GlobalFlags) bool { return g.Quiet }, "Quiet"},
		{"--no-cache", func(g GlobalFlags) bool { return g.NoCache }, "NoCache"},
		{"--plan", func(g GlobalFlags) bool { return g.Plan }, "Plan"},
		{"--continue-on-error", func(g GlobalFlags) bool { return g.ContinueOnErr }, "ContinueOnErr"},
		{"--watch", func(g GlobalFlags) bool { return g.Watch }, "Watch"},
		{"-w", func(g GlobalFlags) bool { return g.Watch }, "Watch (short)"},
		{"--dry-run", func(g GlobalFlags) bool { return g.DryRun }, "DryRun"},
		{"--help", func(g GlobalFlags) bool { return g.Help }, "Help"},
		{"-h", func(g GlobalFlags) bool { return g.Help }, "Help (short)"},
		{"--version", func(g GlobalFlags) bool { return g.Version }, "Version"},
		{"-V", func(g GlobalFlags) bool { return g.Version }, "Version (short)"},
		{"--all", func(g GlobalFlags) bool { return g.All && g.Projects == "*" }, "All sets Projects"},
		{"--impacted", func(g GlobalFlags) bool { return g.Impacted && g.Projects == "[impacted]" }, "Impacted sets Projects"},
		{"--impacted-strict", func(g GlobalFlags) bool { return g.ImpactedStrict }, "ImpactedStrict"},
		{"--man", func(g GlobalFlags) bool { return g.HelpMan && g.Help }, "Man implies Help"},
		{"--markdown", func(g GlobalFlags) bool { return g.HelpMD && g.Help }, "Markdown implies Help"},
		{"--color", func(g GlobalFlags) bool { return g.Color != nil && *g.Color }, "Color true"},
		{"--no-color", func(g GlobalFlags) bool { return g.Color != nil && !*g.Color }, "Color false"},
	}

	for _, tt := range tests {
		g, remaining, err := parseFlags([]string{tt.arg})
		if err != nil {
			t.Errorf("parseFlags(%q): %v", tt.arg, err)
			continue
		}
		if !tt.check(g) {
			t.Errorf("parseFlags(%q): %s not set correctly", tt.arg, tt.desc)
		}
		if len(remaining) != 0 {
			t.Errorf("parseFlags(%q): expected no remaining, got %v", tt.arg, remaining)
		}
	}
}

func TestParseFlags_ValueFlags(t *testing.T) {
	t.Parallel()
	tests := []struct {
		args  []string
		check func(GlobalFlags) bool
		desc  string
	}{
		{[]string{"--max-parallel", "4"}, func(g GlobalFlags) bool { return g.MaxParallel == 4 }, "MaxParallel"},
		{[]string{"--cache-trust", "ci"}, func(g GlobalFlags) bool { return g.CacheTrust == "ci" }, "CacheTrust"},
		{[]string{"--max-parallel", "auto"}, func(g GlobalFlags) bool { return g.MaxParallelMode == "auto" }, "MaxParallel auto"},
		{[]string{"--max-parallel", "eco"}, func(g GlobalFlags) bool { return g.MaxParallelMode == "eco" }, "MaxParallel eco"},
		{[]string{"--max-parallel", "max"}, func(g GlobalFlags) bool { return g.MaxParallelMode == "max" }, "MaxParallel max"},
		{[]string{"--max-parallel", "AUTO"}, func(g GlobalFlags) bool { return g.MaxParallelMode == "auto" }, "MaxParallel uppercase auto"},
		{[]string{"--retry", "3"}, func(g GlobalFlags) bool { return g.Retry == 3 }, "Retry"},
		{[]string{"--output", "jsonl"}, func(g GlobalFlags) bool { return g.Output == "jsonl" }, "Output"},
		{[]string{"--projects", "app1,app2"}, func(g GlobalFlags) bool { return g.Projects == "app1,app2" }, "Projects"},
		{[]string{"--tag", "frontend"}, func(g GlobalFlags) bool { return g.FilterTag == "frontend" }, "FilterTag"},
		{[]string{"--exclude-tag", "slow"}, func(g GlobalFlags) bool { return g.ExcludeTag == "slow" }, "ExcludeTag"},
		{[]string{"--exclude", "legacy"}, func(g GlobalFlags) bool { return g.Exclude == "legacy" }, "Exclude"},
		{[]string{"--baseline", "develop"}, func(g GlobalFlags) bool { return g.Baseline == "develop" }, "Baseline"},
		{[]string{"--trace-profile", "/tmp/trace.json"}, func(g GlobalFlags) bool { return g.TraceProfile == "/tmp/trace.json" }, "TraceProfile"},
		{[]string{"--profile", "production"}, func(g GlobalFlags) bool { return g.EnvProfile == "production" }, "EnvProfile"},
	}

	for _, tt := range tests {
		g, remaining, err := parseFlags(tt.args)
		if err != nil {
			t.Errorf("parseFlags(%v): %v", tt.args, err)
			continue
		}
		if !tt.check(g) {
			t.Errorf("parseFlags(%v): %s not set correctly", tt.args, tt.desc)
		}
		if len(remaining) != 0 {
			t.Errorf("parseFlags(%v): expected no remaining, got %v", tt.args, remaining)
		}
	}
}

// TestParseFlags_ValueFlagAtEnd pins the A1c change: a value-taking global flag
// with nothing after it used to be dropped in silence, so the user's request
// simply did not happen.
func TestParseFlags_ValueFlagAtEnd(t *testing.T) {
	t.Parallel()
	for _, arg := range []string{"--max-parallel", "--projects", "--output", "--retry", "--cache-trust"} {
		if _, _, err := parseFlags([]string{arg}); err == nil {
			t.Errorf("parseFlags(%q): expected a missing-value error", arg)
		}
	}
}

// TestParseFlags_RejectsUnparseableValues pins the other two silent failures:
// a bad --retry int was dropped (Retry stayed 0) and a bad
// --max-parallel value normalized to auto in internal/jobs/parallel.go.
func TestParseFlags_RejectsUnparseableValues(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"--retry", "bogus"},
		{"--retry=bogus"},
		{"--max-parallel", "bogus"},
		{"--max-parallel=bogus"},
	} {
		if _, _, err := parseFlags(args); err == nil {
			t.Errorf("parseFlags(%v): expected a usage error", args)
		}
	}
}

// TestParseFlags_UnknownFlagsPassThrough keeps the token stream unchanged: the
// global pass never claims a flag it does not know, because those tokens are
// what buildCommandParams hashes into cache keys. Rejecting them (when nothing
// can own them) is the command-scoped pass's job, not this one's.
func TestParseFlags_UnknownFlagsPassThrough(t *testing.T) {
	t.Parallel()
	g, remaining, err := parseFlags([]string{"--verbose", "--custom-flag", "value"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if !g.Verbose {
		t.Error("Verbose should be set")
	}
	if len(remaining) != 2 || remaining[0] != "--custom-flag" || remaining[1] != "value" {
		t.Errorf("remaining = %v, want [--custom-flag value]", remaining)
	}
}

func TestParseFlags_Empty(t *testing.T) {
	t.Parallel()
	g, remaining, err := parseFlags(nil)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if g.Verbose || g.Debug || g.Help {
		t.Error("empty args should produce zero-value flags")
	}
	if len(remaining) != 0 {
		t.Errorf("remaining = %v, want empty", remaining)
	}
}

// --- parseFlags with --flag=value ---

func TestParseFlags_EqualsForm(t *testing.T) {
	t.Parallel()
	tests := []struct {
		arg   string
		check func(GlobalFlags) bool
		desc  string
		known bool // true if the flag should be consumed (not in remaining)
	}{
		{"--max-parallel=8", func(g GlobalFlags) bool { return g.MaxParallel == 8 }, "MaxParallel", true},
		{"--cache-trust=none", func(g GlobalFlags) bool { return g.CacheTrust == "none" }, "CacheTrust", true},
		{"--max-parallel=auto", func(g GlobalFlags) bool { return g.MaxParallelMode == "auto" }, "MaxParallel auto", true},
		{"--max-parallel=eco", func(g GlobalFlags) bool { return g.MaxParallelMode == "eco" }, "MaxParallel eco", true},
		{"--max-parallel=max", func(g GlobalFlags) bool { return g.MaxParallelMode == "max" }, "MaxParallel max", true},
		{"--retry=2", func(g GlobalFlags) bool { return g.Retry == 2 }, "Retry", true},
		{"--output=jsonl", func(g GlobalFlags) bool { return g.Output == "jsonl" }, "Output", true},
		{"--projects=a,b", func(g GlobalFlags) bool { return g.Projects == "a,b" }, "Projects", true},
		{"--tag=web", func(g GlobalFlags) bool { return g.FilterTag == "web" }, "FilterTag", true},
		{"--exclude-tag=slow", func(g GlobalFlags) bool { return g.ExcludeTag == "slow" }, "ExcludeTag", true},
		{"--exclude=old", func(g GlobalFlags) bool { return g.Exclude == "old" }, "Exclude", true},
		{"--baseline=main", func(g GlobalFlags) bool { return g.Baseline == "main" }, "Baseline", true},
		{"--trace-profile=trace.json", func(g GlobalFlags) bool { return g.TraceProfile == "trace.json" }, "TraceProfile", true},
		{"--profile=production", func(g GlobalFlags) bool { return g.EnvProfile == "production" }, "EnvProfile", true},
		{"--unknown=val", func(g GlobalFlags) bool { return true }, "unknown flag", false},
	}

	for _, tt := range tests {
		g, remaining, err := parseFlags([]string{tt.arg})
		if err != nil {
			t.Errorf("parseFlags(%q): %v", tt.arg, err)
			continue
		}
		if tt.known {
			if len(remaining) != 0 {
				t.Errorf("parseFlags(%q): expected empty remaining, got %v", tt.arg, remaining)
			}
			if !tt.check(g) {
				t.Errorf("parseFlags(%q): %s not set correctly", tt.arg, tt.desc)
			}
		} else if len(remaining) == 0 {
			t.Errorf("parseFlags(%q): unknown flag should be in remaining", tt.arg)
		}
	}
}

func TestAutomaticReadPreparationCommands(t *testing.T) {
	t.Parallel()
	tests := []struct {
		command    string
		subcommand string
		want       bool
	}{
		{"workspace", "", true},
		{"workspace", "describe", true},
		{"projects", "", true},
		{"projects", "list", true},
		{"projects", "describe", true},
		{"extensions", "list", true},
		{"context", "map", true},
		{"context", "pack", true},
		{"workspace", "init", false},
		{"extensions", "install", false},
		{"context", "generate", false},
	}
	for _, test := range tests {
		if got := automaticReadPreparationCommand(test.command, test.subcommand); got != test.want {
			t.Errorf("automaticReadPreparationCommand(%q, %q) = %v, want %v", test.command, test.subcommand, got, test.want)
		}
	}
}
