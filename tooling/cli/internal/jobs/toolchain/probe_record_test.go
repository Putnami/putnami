//go:build linux || darwin

package toolchain

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
)

// versionProbe is the probe of a toolchain that answers its version on
// `version`, run with one variable set and one unset.
var versionProbe = extensionproto.RuntimeToolchainProbe{
	Args:        []string{"version"},
	Expect:      "{version}",
	Unset:       []string{"TOOLROOT"},
	Environment: map[string]string{"TOOLCHAIN": "local-{version}"},
}

// probeOnce probes executable as a run of its own would: the process's memo
// is emptied first, as a new run starts with none.
func probeOnce(t *testing.T, storeRoot, executable string) (string, error) {
	t.Helper()
	probeAnswers.Clear()
	got, _, err := probeRuntimeToolchainVersion(context.Background(), storeRoot, executable, versionProbe, "1.2.3", os.Environ())
	return got, err
}

func TestAProbeRunsOncePerBinaryIdentity(t *testing.T) {
	t.Cleanup(probeAnswers.Clear)
	storeRoot := t.TempDir()
	dir := t.TempDir()
	record := filepath.Join(dir, "runs")
	tool := fixtureproc.Write(t, filepath.Join(dir, "tool"), fixtureproc.Program{Record: record, Stdout: "1.2.3\n"})

	// One run probes the tool for several extensions.
	probeAnswers.Clear()
	for range 3 {
		got, want, err := probeRuntimeToolchainVersion(context.Background(), storeRoot, tool, versionProbe, "1.2.3", os.Environ())
		if err != nil || got != want {
			t.Fatalf("probe = %q, %v; want %q", got, err, want)
		}
	}
	// The next runs read the record.
	for range 2 {
		if got, err := probeOnce(t, storeRoot, tool); err != nil || got != "1.2.3" {
			t.Fatalf("recorded probe = %q, %v", got, err)
		}
	}
	if runs := fixtureproc.Runs(t, record); len(runs) != 1 {
		t.Fatalf("three runs probed one binary %d times, want 1", len(runs))
	}

	// Other arguments or variables are another identity.
	other := versionProbe
	other.Environment = map[string]string{"TOOLCHAIN": "auto"}
	if _, _, err := probeRuntimeToolchainVersion(context.Background(), storeRoot, tool, other, "1.2.3", os.Environ()); err != nil {
		t.Fatal(err)
	}
	if runs := fixtureproc.Runs(t, record); len(runs) != 2 {
		t.Fatalf("a probe with other variables left %d runs, want 2", len(runs))
	}
	spectest.Proves(t, "cli/job-planning-execution", "unchanged-planning-reads-records",
		"a-probe-runs-once-per-binary-identity")
}

func TestAChangedBinaryIsProbedAgain(t *testing.T) {
	t.Cleanup(probeAnswers.Clear)
	storeRoot := t.TempDir()
	dir := t.TempDir()
	record := filepath.Join(dir, "runs")
	path := filepath.Join(dir, "tool")
	tool := fixtureproc.Write(t, path, fixtureproc.Program{Record: record, Stdout: "1.2.3\n"})
	if got, err := probeOnce(t, storeRoot, tool); err != nil || got != "1.2.3" {
		t.Fatalf("probe = %q, %v", got, err)
	}

	// Another release installed at the same path.
	if err := os.Remove(tool); err != nil {
		t.Fatal(err)
	}
	tool = fixtureproc.Write(t, path, fixtureproc.Program{Record: record, Stdout: "1.2.4\n"})
	if got, err := probeOnce(t, storeRoot, tool); err != nil || got != "1.2.4" {
		t.Fatalf("a replaced binary answered %q, %v; want 1.2.4", got, err)
	}

	// A probe that fails is not kept: the next run probes again.
	failing := fixtureproc.Write(t, filepath.Join(dir, "failing"), fixtureproc.Program{Record: record, Exit: 3})
	for range 2 {
		if _, err := probeOnce(t, storeRoot, failing); err == nil {
			t.Fatal("a failing probe answered")
		}
	}
	if runs := fixtureproc.Runs(t, record); len(runs) != 4 {
		t.Fatalf("got %d runs, want 4: one per binary and one per failed probe", len(runs))
	}
}

func TestAScriptIsProbedOnEachRun(t *testing.T) {
	t.Cleanup(probeAnswers.Clear)
	storeRoot := t.TempDir()
	dir := t.TempDir()
	counter := filepath.Join(dir, "runs")
	script := filepath.Join(dir, "shim")
	writeExecutable(t, script, "#!/bin/sh\necho run >> "+counter+"\necho \"$SHIM_VERSION\"\n")

	t.Setenv("SHIM_VERSION", "1.2.3")
	probeAnswers.Clear()
	for range 2 {
		if got, _, err := probeRuntimeToolchainVersion(context.Background(), storeRoot, script, versionProbe, "1.2.3", os.Environ()); err != nil || got != "1.2.3" {
			t.Fatalf("probe = %q, %v", got, err)
		}
	}
	// The shim's answer follows the environment: a run under another one
	// probes again, and nothing was recorded for the next run.
	t.Setenv("SHIM_VERSION", "1.2.4")
	if got, _, err := probeRuntimeToolchainVersion(context.Background(), storeRoot, script, versionProbe, "1.2.3", os.Environ()); err != nil || got != "1.2.4" {
		t.Fatalf("probe under another environment = %q, %v; want 1.2.4", got, err)
	}
	if got, err := probeOnce(t, storeRoot, script); err != nil || got != "1.2.4" {
		t.Fatalf("probe of the next run = %q, %v", got, err)
	}
	data, err := os.ReadFile(counter)
	if err != nil {
		t.Fatal(err)
	}
	if runs := len(data) / len("run\n"); runs != 3 {
		t.Fatalf("the shim ran %d times, want 3", runs)
	}
	if entries, _ := os.ReadDir(filepath.Join(storeRoot, toolchainProbeDir)); len(entries) != 0 {
		t.Fatalf("a script's answer was recorded: %v", entries)
	}
}

func TestAMalformedProbeRecordIsNotRead(t *testing.T) {
	t.Cleanup(probeAnswers.Clear)
	storeRoot := t.TempDir()
	dir := t.TempDir()
	record := filepath.Join(dir, "runs")
	tool := fixtureproc.Write(t, filepath.Join(dir, "tool"), fixtureproc.Program{Record: record, Stdout: "1.2.3\n"})
	if _, err := probeOnce(t, storeRoot, tool); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(storeRoot, toolchainProbeDir))
	if err != nil || len(entries) != 1 {
		t.Fatalf("records = %v, %v; want one", entries, err)
	}
	recordPath := filepath.Join(storeRoot, toolchainProbeDir, entries[0].Name())
	for _, content := range []string{"", "{", `{"schema":1,"key":"other","answer":"1.2.3"}`, `{"schema":1,"key":"` + entries[0].Name() + `","answer":"two\nlines"}`} {
		if err := os.WriteFile(recordPath, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := probeOnce(t, storeRoot, tool); err != nil || got != "1.2.3" {
			t.Fatalf("probe after record %q = %q, %v", content, got, err)
		}
	}
	if runs := fixtureproc.Runs(t, record); len(runs) != 5 {
		t.Fatalf("got %d runs, want 5: each malformed record probes again", len(runs))
	}
}

func TestAHardLinkedDispatcherIsProbedOnEachRun(t *testing.T) {
	t.Cleanup(probeAnswers.Clear)
	storeRoot := t.TempDir()
	dir := t.TempDir()
	record := filepath.Join(dir, "runs")
	dispatcher := fixtureproc.Write(t, filepath.Join(dir, "dispatcher"), fixtureproc.Program{Record: record, Stdout: "1.2.3\n"})
	// A dispatcher answers under each name it is linked as, such as a rustup
	// proxy, and picks its toolchain from configuration outside its file.
	tool := filepath.Join(dir, "tool")
	if err := os.Link(dispatcher, tool); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if err := os.WriteFile(tool+".fixture.json", mustRead(t, dispatcher+".fixture.json"), 0o644); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if got, err := probeOnce(t, storeRoot, tool); err != nil || got != "1.2.3" {
			t.Fatalf("probe = %q, %v", got, err)
		}
	}
	if runs := fixtureproc.Runs(t, record); len(runs) != 2 {
		t.Fatalf("two runs probed a hard-linked dispatcher %d times, want 2", len(runs))
	}
	if entries, _ := os.ReadDir(filepath.Join(storeRoot, toolchainProbeDir)); len(entries) != 0 {
		t.Fatalf("a hard-linked dispatcher's answer was recorded: %v", entries)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// configuredTool places, in dir, a native program that answers what its
// configuration file says, as a dispatcher answers for the toolchain its
// configuration names, and returns its path and a function that switches the
// configuration to answer version.
func configuredTool(t *testing.T, dir, record, version string) (string, func(version string)) {
	t.Helper()
	path := fixtureproc.Write(t, filepath.Join(dir, "compiler"), fixtureproc.Program{Record: record, Stdout: version + "\n"})
	return path, func(version string) {
		data, err := json.Marshal(fixtureproc.Program{Record: record, Stdout: version + "\n"})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path+".fixture.json", data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// resolveCompiler resolves the compiler toolchain pinned at 1.2.3 as a run of
// its own does, with bin first on PATH.
func resolveCompiler(t *testing.T, workspaceRoot, bin string) error {
	t.Helper()
	probeAnswers.Clear()
	ext := runtimeToolchainExtension(runtimeToolchainFixture("compiler"))
	return resolveRuntimeToolchainsContext(context.Background(), workspaceRoot, ext, []string{"compiler"},
		[]string{"PATH=" + bin, "PUTNAMI_HOME=" + os.Getenv("PUTNAMI_HOME")})
}

func TestADispatcherOutsideThePutnamiHomeAnswersItsConfigurationOnEachRun(t *testing.T) {
	t.Cleanup(probeAnswers.Clear)
	t.Setenv("PUTNAMI_HOME", t.TempDir())
	workspaceRoot := t.TempDir()
	writeRuntimeToolchainLock(t, workspaceRoot, "compiler", "1.2.3", "integrity-a")
	bin := t.TempDir()
	record := filepath.Join(t.TempDir(), "runs")
	_, configure := configuredTool(t, bin, record, "1.2.3")
	if err := resolveCompiler(t, workspaceRoot, bin); err != nil {
		t.Fatalf("first run: %v", err)
	}
	// The configuration switches to another release; the file is the same.
	configure("1.2.4")
	err := resolveCompiler(t, workspaceRoot, bin)
	if err == nil || !strings.Contains(err.Error(), `version "1.2.4" does not match locked "1.2.3"`) {
		t.Fatalf("a run after the switch resolved %v; want the 1.2.4 answer refused", err)
	}
	if runs := fixtureproc.Runs(t, record); len(runs) != 2 {
		t.Fatalf("two runs probed the dispatcher %d times, want 2", len(runs))
	}
	spectest.Proves(t, "cli/job-planning-execution", "unchanged-planning-reads-records",
		"a-changed-binary-is-probed-again")
}

func TestAPutnamiHomeReleaseAnswersFromItsRecord(t *testing.T) {
	t.Cleanup(probeAnswers.Clear)
	home := t.TempDir()
	t.Setenv("PUTNAMI_HOME", home)
	workspaceRoot := t.TempDir()
	writeRuntimeToolchainLock(t, workspaceRoot, "compiler", "1.2.3", "integrity-a")
	bin := filepath.Join(home, "toolchains", "compiler", "compiler-1.2.3", "bin")
	record := filepath.Join(t.TempDir(), "runs")
	configuredTool(t, bin, record, "1.2.3")
	for range 3 {
		if err := resolveCompiler(t, workspaceRoot, bin); err != nil {
			t.Fatal(err)
		}
	}
	if runs := fixtureproc.Runs(t, record); len(runs) != 1 {
		t.Fatalf("three runs probed a Putnami home release %d times, want 1", len(runs))
	}
}

func TestALinkFromThePutnamiHomeToADispatcherAnswersItsConfigurationOnEachRun(t *testing.T) {
	t.Cleanup(probeAnswers.Clear)
	home := t.TempDir()
	t.Setenv("PUTNAMI_HOME", home)
	workspaceRoot := t.TempDir()
	writeRuntimeToolchainLock(t, workspaceRoot, "compiler", "1.2.3", "integrity-a")
	record := filepath.Join(t.TempDir(), "runs")
	tool, configure := configuredTool(t, t.TempDir(), record, "1.2.3")
	bin := filepath.Join(home, "toolchains", "compiler", "compiler-1.2.3", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(tool, filepath.Join(bin, "compiler")); err != nil {
		t.Fatal(err)
	}
	if err := resolveCompiler(t, workspaceRoot, bin); err != nil {
		t.Fatalf("first run: %v", err)
	}
	configure("1.2.4")
	err := resolveCompiler(t, workspaceRoot, bin)
	if err == nil || !strings.Contains(err.Error(), `version "1.2.4" does not match locked "1.2.3"`) {
		t.Fatalf("a run after the switch resolved %v; want the 1.2.4 answer refused", err)
	}
}
