package lint

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/exec"
)

// emptyBiomeJSON is a valid empty Biome JSON report.
const emptyBiomeJSON = `{"diagnostics":[],"command":"","summary":{"errors":0,"warnings":0,"infos":0,"changed":0,"unchanged":0,"skipped":0}}`

// containsArg reports whether args contains the exact value.
func containsArg(args []string, value string) bool {
	for _, a := range args {
		if a == value {
			return true
		}
	}
	return false
}

func TestBiomeRunContext_UsesConfigRootForWorkspaceConfig(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "lint-covers-the-project", "the-workspace-configuration-resolves-from-the-config-root")
	workspaceDir := t.TempDir()
	projectDir := filepath.Join(workspaceDir, "sites", "putnami.dev")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}

	runDir, target := biomeRunContext(projectDir, filepath.Join(workspaceDir, "biome.json"))
	if runDir != workspaceDir {
		t.Fatalf("run dir = %q, want %q", runDir, workspaceDir)
	}
	if target != "sites/putnami.dev" {
		t.Fatalf("target = %q, want sites/putnami.dev", target)
	}
}

func TestBiomeRunContext_KeepsProjectRootForExternalConfig(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "lint-covers-the-project", "an-external-configuration-stays-anchored-at-the-project-root")
	projectDir := t.TempDir()
	configDir := t.TempDir()

	runDir, target := biomeRunContext(projectDir, configDir)
	if runDir != projectDir {
		t.Fatalf("run dir = %q, want %q", runDir, projectDir)
	}
	if target != "." {
		t.Fatalf("target = %q, want .", target)
	}
}

// fakeBiomeEnv names what this test binary does when it runs as biome. A test
// sets it and passes the test binary as the biome executable, so the fake
// starts on every host a test runs on, Windows included, where a shell script
// does not.
const fakeBiomeEnv = "PUTNAMI_TS_LINT_FAKE_BIOME"

// The behaviors of the fake biome.
const (
	// fakeBiomeClean prints its arguments to stderr and an empty report to
	// stdout, and exits 0.
	fakeBiomeClean = "clean"
	// fakeBiomeRecordingRayon writes RAYON_NUM_THREADS, or "unset", to the file
	// PUTNAMI_TEST_RAYON_OUTPUT names, prints an empty report and exits 0.
	fakeBiomeRecordingRayon = "rayon"
	// fakeBiomeFailing prints an empty report, an error on stderr, and exits 1.
	fakeBiomeFailing = "failing"
	// fakeBiomeDiagnostics prints a report with one diagnostic and exits 1.
	fakeBiomeDiagnostics = "diagnostics"
)

// biomeDiagnosticsJSON is the report fakeBiomeDiagnostics prints.
const biomeDiagnosticsJSON = `{"diagnostics":[{"category":"lint/style/useConst","severity":"warning","message":"Use const instead of let","location":{"path":"src/index.ts","start":{"line":1,"column":1},"end":{"line":1,"column":10}},"tags":[]}],"command":"lint","summary":{"errors":0,"warnings":1,"infos":0,"changed":0,"unchanged":0,"skipped":0}}`

func TestMain(m *testing.M) {
	if behavior, ok := os.LookupEnv(fakeBiomeEnv); ok {
		os.Exit(runFakeBiome(behavior, os.Args[1:]))
	}
	os.Exit(m.Run())
}

// runFakeBiome behaves as biome does in the named behavior and returns the
// exit code.
func runFakeBiome(behavior string, args []string) int {
	switch behavior {
	case fakeBiomeClean:
		fmt.Fprintf(os.Stderr, "ARGS:%s\n", strings.Join(args, " "))
		fmt.Println(emptyBiomeJSON)
		return 0
	case fakeBiomeRecordingRayon:
		value, ok := os.LookupEnv("RAYON_NUM_THREADS")
		if !ok {
			value = "unset"
		}
		if err := os.WriteFile(os.Getenv("PUTNAMI_TEST_RAYON_OUTPUT"), []byte(value), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		fmt.Println(emptyBiomeJSON)
		return 0
	case fakeBiomeFailing:
		fmt.Println(emptyBiomeJSON)
		fmt.Fprintln(os.Stderr, "some error occurred")
		return 1
	case fakeBiomeDiagnostics:
		fmt.Println(biomeDiagnosticsJSON)
		return 1
	default:
		fmt.Fprintf(os.Stderr, "unknown fake biome behavior %q\n", behavior)
		return 2
	}
}

// fakeBiome returns this test binary as a biome executable that behaves as
// behavior says for the rest of the test.
func fakeBiome(t *testing.T, behavior string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(fakeBiomeEnv, behavior)
	return self
}

// mockBiome returns a biome that prints an empty report and exits 0.
func mockBiome(t *testing.T) string {
	t.Helper()
	return fakeBiome(t, fakeBiomeClean)
}

// mockBiomeRecordingRayon records the environment Biome actually receives,
// then emits a valid empty report.
func mockBiomeRecordingRayon(t *testing.T) (string, string) {
	t.Helper()
	output := filepath.Join(t.TempDir(), "rayon.txt")
	t.Setenv("PUTNAMI_TEST_RAYON_OUTPUT", output)
	return fakeBiome(t, fakeBiomeRecordingRayon), output
}

func unsetEnvForTest(t *testing.T, name string) {
	t.Helper()
	value, present := os.LookupEnv(name)
	if err := os.Unsetenv(name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if present {
			_ = os.Setenv(name, value)
		} else {
			_ = os.Unsetenv(name)
		}
	})
}

func readRecordedRayon(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestBiomeCommandsBindPutnamiCPUBudgetToRayon(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "lint-covers-the-project", "the-scheduler-cpu-budget-is-bound-to-the-linter")
	commands := []struct {
		name string
		run  func(string, string) error
	}{
		{name: "check", run: func(bin, project string) error {
			_, _, err := Check(bin, project, "/config", false, 0, "")
			return err
		}},
		{name: "combined check", run: func(bin, project string) error {
			_, _, err := CheckAll(bin, project, "/config", 0, "")
			return err
		}},
		{name: "format", run: func(bin, project string) error {
			_, _, err := Format(bin, project, "/config", false)
			return err
		}},
	}
	for _, command := range commands {
		t.Run(command.name, func(t *testing.T) {
			unsetEnvForTest(t, "RAYON_NUM_THREADS")
			t.Setenv("PUTNAMI_CPU_BUDGET", "3")
			bin, output := mockBiomeRecordingRayon(t)
			if err := command.run(bin, t.TempDir()); err != nil {
				t.Fatalf("Biome command failed: %v", err)
			}
			if got := readRecordedRayon(t, output); got != "3" {
				t.Fatalf("RAYON_NUM_THREADS = %q, want scheduler budget 3", got)
			}
		})
	}
}

func TestBiomeCommandsPreserveExplicitRayonOverride(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "lint-covers-the-project", "an-explicit-thread-setting-is-never-overridden")
	t.Setenv("PUTNAMI_CPU_BUDGET", "3")
	t.Setenv("RAYON_NUM_THREADS", "7")
	bin, output := mockBiomeRecordingRayon(t)
	if _, _, err := CheckAll(bin, t.TempDir(), "/config", 0, ""); err != nil {
		t.Fatalf("Biome command failed: %v", err)
	}
	if got := readRecordedRayon(t, output); got != "7" {
		t.Fatalf("RAYON_NUM_THREADS = %q, want explicit override 7", got)
	}
}

func TestBiomeCommandsIgnoreInvalidPutnamiCPUBudget(t *testing.T) {
	for _, budget := range []string{"", "0", "-2", "invalid"} {
		t.Run(budget, func(t *testing.T) {
			unsetEnvForTest(t, "RAYON_NUM_THREADS")
			t.Setenv("PUTNAMI_CPU_BUDGET", budget)
			bin, output := mockBiomeRecordingRayon(t)
			if _, _, err := Format(bin, t.TempDir(), "/config", false); err != nil {
				t.Fatalf("Biome command failed: %v", err)
			}
			if got := readRecordedRayon(t, output); got != "unset" {
				t.Fatalf("RAYON_NUM_THREADS = %q, want invalid budget ignored", got)
			}
		})
	}
}

// mockBiomeFailing returns a biome that exits 1 and writes to stderr.
func mockBiomeFailing(t *testing.T) string {
	t.Helper()
	return fakeBiome(t, fakeBiomeFailing)
}

// mockBiomeWithDiagnostics returns a biome that exits 1 and reports
// diagnostics.
func mockBiomeWithDiagnostics(t *testing.T) string {
	t.Helper()
	return fakeBiome(t, fakeBiomeDiagnostics)
}

func TestCheck_Success(t *testing.T) {
	biomeBin := mockBiome(t)
	projectDir := t.TempDir()

	report, success, err := Check(biomeBin, projectDir, "/config", false, 0, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !success {
		t.Error("expected success=true")
	}
	if len(report.Diagnostics) != 0 {
		t.Errorf("expected 0 diagnostics, got %d", len(report.Diagnostics))
	}
}

func TestCheck_WithFixFlag(t *testing.T) {
	biomeBin := mockBiome(t)
	projectDir := t.TempDir()

	_, success, err := Check(biomeBin, projectDir, "/config", true, 0, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !success {
		t.Error("expected success=true")
	}
}

func TestCheck_FailureWithStderr_ReturnsError(t *testing.T) {
	biomeBin := mockBiomeFailing(t)
	projectDir := t.TempDir()

	_, success, err := Check(biomeBin, projectDir, "/config", false, 0, "")
	if err == nil {
		t.Fatal("expected error for failing biome with stderr")
	}
	if success {
		t.Error("expected success=false")
	}
	if !strings.Contains(err.Error(), "biome lint failed") {
		t.Errorf("expected 'biome lint failed' in error, got: %v", err)
	}
}

func TestCheck_FailureWithDiagnostics_NoError(t *testing.T) {
	biomeBin := mockBiomeWithDiagnostics(t)
	projectDir := t.TempDir()

	report, success, err := Check(biomeBin, projectDir, "/config", false, 0, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if success {
		t.Error("expected success=false when diagnostics present")
	}
	if len(report.Diagnostics) == 0 {
		t.Error("expected diagnostics to be parsed")
	}
}

func TestCheck_InvalidBinary_ReturnsError(t *testing.T) {
	projectDir := t.TempDir()

	_, _, err := Check("/nonexistent/biome", projectDir, "/config", false, 0, "")
	if err == nil {
		t.Fatal("expected error for invalid binary path")
	}
}

func TestCheckAll_Success(t *testing.T) {
	biomeBin := mockBiome(t)
	projectDir := t.TempDir()

	report, success, err := CheckAll(biomeBin, projectDir, "/config", 0, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !success {
		t.Error("expected success=true")
	}
	if len(report.Diagnostics) != 0 {
		t.Errorf("expected 0 diagnostics, got %d", len(report.Diagnostics))
	}
}

func TestCheck_WritingPassSkipsBiomeTestRules(t *testing.T) {
	var gotArgs []string
	restore := SetExecRunForTesting(func(_ string, args []string, _ ...exec.Option) (*exec.Result, error) {
		gotArgs = args
		return &exec.Result{Success: true, ExitCode: 0, Stdout: emptyBiomeJSON}, nil
	})
	defer restore()

	if _, _, err := Check("biome", t.TempDir(), "/config", true, 0, ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{"--skip=suspicious/noSkippedTests", "--skip=suspicious/noFocusedTests"} {
		if !containsArg(gotArgs, want) {
			t.Errorf("the writing pass must not let Biome remove .skip or .only: want %s in %v", want, gotArgs)
		}
	}

	if _, _, err := Check("biome", t.TempDir(), "/config", false, 0, ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if containsArg(gotArgs, "--skip=suspicious/noSkippedTests") {
		t.Errorf("the read-only pass keeps the workspace's rules: got %v", gotArgs)
	}
}

func TestCheckAll_RunsReadOnlyCheckWithAssistEnabledButUnenforced(t *testing.T) {
	var gotArgs []string
	restore := SetExecRunForTesting(func(_ string, args []string, _ ...exec.Option) (*exec.Result, error) {
		gotArgs = args
		return &exec.Result{Success: true, ExitCode: 0, Stdout: emptyBiomeJSON}, nil
	})
	defer restore()

	if _, _, err := CheckAll("biome", t.TempDir(), "/config", 0, ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(gotArgs) == 0 || gotArgs[0] != "check" {
		t.Fatalf("expected first arg to be \"check\", got %v", gotArgs)
	}
	// Assist is explicit so singleton and batch calls cannot diverge on
	// info-level assistant diagnostics such as organizeImports.
	if !containsArg(gotArgs, "--assist-enabled=true") {
		t.Errorf("expected --assist-enabled=true, got %v", gotArgs)
	}
	// Assist enforcement is disabled so the combined pass stays iso-functional
	// with the previous format + lint phases (which never enforced assist).
	if !containsArg(gotArgs, "--enforce-assist=false") {
		t.Errorf("expected --enforce-assist=false, got %v", gotArgs)
	}
	// CheckAll is read-only: the fix (writer) path uses the distinct
	// format + lint phases instead.
	if containsArg(gotArgs, "--write") {
		t.Errorf("did not expect --write in read-only check, got %v", gotArgs)
	}
}

func TestCheckAll_FailureWithDiagnostics_NoError(t *testing.T) {
	biomeBin := mockBiomeWithDiagnostics(t)
	projectDir := t.TempDir()

	report, success, err := CheckAll(biomeBin, projectDir, "/config", 0, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if success {
		t.Error("expected success=false when diagnostics present")
	}
	if len(report.Diagnostics) == 0 {
		t.Error("expected diagnostics to be parsed")
	}
}

func TestCheckAll_FailureWithStderr_ReturnsError(t *testing.T) {
	biomeBin := mockBiomeFailing(t)
	projectDir := t.TempDir()

	_, success, err := CheckAll(biomeBin, projectDir, "/config", 0, "")
	if err == nil {
		t.Fatal("expected error for failing biome with stderr")
	}
	if success {
		t.Error("expected success=false")
	}
	if !strings.Contains(err.Error(), "biome check failed") {
		t.Errorf("expected 'biome check failed' in error, got: %v", err)
	}
}

func TestFormat_Success(t *testing.T) {
	biomeBin := mockBiome(t)
	projectDir := t.TempDir()

	report, success, err := Format(biomeBin, projectDir, "/config", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !success {
		t.Error("expected success=true")
	}
	if len(report.Diagnostics) != 0 {
		t.Errorf("expected 0 diagnostics, got %d", len(report.Diagnostics))
	}
}

func TestFormat_WithFixFlag(t *testing.T) {
	biomeBin := mockBiome(t)
	projectDir := t.TempDir()

	_, success, err := Format(biomeBin, projectDir, "/config", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !success {
		t.Error("expected success=true")
	}
}

func TestFormat_FailureWithStderr_ReturnsError(t *testing.T) {
	biomeBin := mockBiomeFailing(t)
	projectDir := t.TempDir()

	_, success, err := Format(biomeBin, projectDir, "/config", false)
	if err == nil {
		t.Fatal("expected error for failing biome with stderr")
	}
	if success {
		t.Error("expected success=false")
	}
	if !strings.Contains(err.Error(), "biome format failed") {
		t.Errorf("expected 'biome format failed' in error, got: %v", err)
	}
}

func TestFormat_InvalidBinary_ReturnsError(t *testing.T) {
	projectDir := t.TempDir()

	_, _, err := Format("/nonexistent/biome", projectDir, "/config", false)
	if err == nil {
		t.Fatal("expected error for invalid binary path")
	}
}

// TestBiomeRunsPassTheShimArgumentCheck pins that every biome command line is
// checked before it runs: a refused command line fails without starting biome.
func TestBiomeRunsPassTheShimArgumentCheck(t *testing.T) {
	refused := errors.New("refused")
	originalCheck := checkShimArgs
	t.Cleanup(func() { checkShimArgs = originalCheck })
	checked := 0
	checkShimArgs = func(path string, args []string) error {
		checked++
		if path != "biome.cmd" || len(args) == 0 {
			t.Errorf("checked %q %v, want biome.cmd and its arguments", path, args)
		}
		return refused
	}
	restore := SetExecRunForTesting(func(string, []string, ...exec.Option) (*exec.Result, error) {
		t.Error("biome ran after its command line was refused")
		return &exec.Result{Success: true, Stdout: emptyBiomeJSON}, nil
	})
	t.Cleanup(restore)

	projectDir := t.TempDir()
	runs := map[string]func() error{
		"check": func() error {
			_, _, err := Check("biome.cmd", projectDir, "/config", false, 0, "")
			return err
		},
		"check all": func() error {
			_, _, err := CheckAll("biome.cmd", projectDir, "/config", 0, "")
			return err
		},
		"format": func() error {
			_, _, err := Format("biome.cmd", projectDir, "/config", false)
			return err
		},
	}
	for name, run := range runs {
		if err := run(); !errors.Is(err, refused) {
			t.Errorf("%s: err = %v, want the refusal", name, err)
		}
	}
	if checked != len(runs) {
		t.Errorf("checked %d command lines, want %d", checked, len(runs))
	}
}

// TestBiomeRunsStartTheLauncherCommand pins that every biome run starts the
// program and the arguments biomeCommand returns, and that a biome with no
// command starts nothing.
func TestBiomeRunsStartTheLauncherCommand(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "no-node-on-the-host", "a-missing-platform-package-starts-the-launcher-with-bun")
	originalCommand := biomeCommand
	t.Cleanup(func() { biomeCommand = originalCommand })
	noBun := errors.New("bun not found")
	biomeCommand = func(bin string, args []string) (string, []string, error) {
		if bin == "launcher-without-bun" {
			return "", nil, noBun
		}
		return "/task/bun", append([]string{bin}, args...), nil
	}
	var started [][]string
	restore := SetExecRunForTesting(func(name string, args []string, _ ...exec.Option) (*exec.Result, error) {
		started = append(started, append([]string{name}, args...))
		return &exec.Result{Success: true, Stdout: emptyBiomeJSON}, nil
	})
	t.Cleanup(restore)

	projectDir := t.TempDir()
	runs := map[string]func(biome string) error{
		"lint": func(biome string) error {
			_, _, err := Check(biome, projectDir, "/config", false, 0, "")
			return err
		},
		"check": func(biome string) error {
			_, _, err := CheckAll(biome, projectDir, "/config", 0, "")
			return err
		},
		"format": func(biome string) error {
			_, _, err := Format(biome, projectDir, "/config", false)
			return err
		},
	}
	for subcommand, run := range runs {
		started = nil
		if err := run("/workspace/node_modules/.bin/biome"); err != nil {
			t.Fatalf("%s: %v", subcommand, err)
		}
		if len(started) != 1 || len(started[0]) < 3 {
			t.Fatalf("%s: started %v, want one command", subcommand, started)
		}
		if got := started[0][:3]; got[0] != "/task/bun" || got[1] != "/workspace/node_modules/.bin/biome" || got[2] != subcommand {
			t.Errorf("%s: started %v, want bun, the launcher, then the biome arguments", subcommand, started[0])
		}

		started = nil
		if err := run("launcher-without-bun"); !errors.Is(err, noBun) {
			t.Errorf("%s: err = %v, want the failed bun resolution", subcommand, err)
		}
		if len(started) != 0 {
			t.Errorf("%s: started %v with no bun to start the launcher", subcommand, started)
		}
	}
}
