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
	got, _, err := probeRuntimeToolchainVersion(context.Background(), storeRoot, true, executable, versionProbe, "1.2.3", os.Environ())
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
		got, want, err := probeRuntimeToolchainVersion(context.Background(), storeRoot, true, tool, versionProbe, "1.2.3", os.Environ())
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
	if _, _, err := probeRuntimeToolchainVersion(context.Background(), storeRoot, true, tool, other, "1.2.3", os.Environ()); err != nil {
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
		if got, _, err := probeRuntimeToolchainVersion(context.Background(), storeRoot, true, script, versionProbe, "1.2.3", os.Environ()); err != nil || got != "1.2.3" {
			t.Fatalf("probe = %q, %v", got, err)
		}
	}
	// The shim's answer follows the environment: a run under another one
	// probes again, and nothing was recorded for the next run.
	t.Setenv("SHIM_VERSION", "1.2.4")
	if got, _, err := probeRuntimeToolchainVersion(context.Background(), storeRoot, true, script, versionProbe, "1.2.3", os.Environ()); err != nil || got != "1.2.4" {
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

// selfProbe is versionProbe with a self that answers the root of the
// toolchain on `root`, whose bin/compiler is the executable.
var selfProbe = func() extensionproto.RuntimeToolchainProbe {
	probe := versionProbe
	probe.Self = &extensionproto.RuntimeToolchainProbeSelf{Args: []string{"root"}, Path: "bin/compiler"}
	return probe
}()

// selfTool writes at path a compiled program that answers version on
// `version` and root on `root`, as a toolchain or a dispatcher names the
// root of the toolchain it runs.
func selfTool(t *testing.T, path, record, version, root string) string {
	t.Helper()
	return fixtureproc.Write(t, path, fixtureproc.Program{Record: record, On: map[string]fixtureproc.Outcome{
		"version": {Stdout: version + "\n"},
		"root":    {Stdout: root + "\n"},
	}})
}

// probeSystem probes executable with probe as a run of its own would, for a
// candidate outside the Putnami home's toolchains.
func probeSystem(t *testing.T, storeRoot, executable string, probe extensionproto.RuntimeToolchainProbe) (string, error) {
	t.Helper()
	probeAnswers.Clear()
	got, _, err := probeRuntimeToolchainVersion(context.Background(), storeRoot, false, executable, probe, "1.2.3", os.Environ())
	return got, err
}

func TestAToolchainThatNamesItselfIsRecorded(t *testing.T) {
	t.Cleanup(probeAnswers.Clear)
	storeRoot := t.TempDir()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(t.TempDir(), "runs")
	tool := selfTool(t, filepath.Join(root, "bin", "compiler"), record, "1.2.3", root)
	// A link on PATH resolves to the toolchain's own file.
	link := filepath.Join(t.TempDir(), "compiler")
	if err := os.Symlink(tool, link); err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(link)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if got, err := probeSystem(t, storeRoot, real, selfProbe); err != nil || got != "1.2.3" {
			t.Fatalf("probe = %q, %v", got, err)
		}
	}
	runs := fixtureproc.Runs(t, record)
	var args []string
	for _, run := range runs {
		args = append(args, strings.Join(run.Args, " "))
	}
	if strings.Join(args, ",") != "version,root,version" {
		t.Fatalf("three runs started %v, want the version probe, the self probe and the version probe of an empty directory, on the first run alone", args)
	}

	// Without self, the same binary outside the Putnami home is probed on
	// each run, as before the member existed.
	plain := t.TempDir()
	plainRecord := filepath.Join(t.TempDir(), "runs")
	plainTool := selfTool(t, filepath.Join(plain, "bin", "compiler"), plainRecord, "1.2.3", plain)
	for range 2 {
		if _, err := probeSystem(t, storeRoot, plainTool, versionProbe); err != nil {
			t.Fatal(err)
		}
	}
	if runs := fixtureproc.Runs(t, plainRecord); len(runs) != 2 {
		t.Fatalf("two runs probed a binary without self %d times, want 2", len(runs))
	}

	// A self without a path names the executable itself.
	direct := t.TempDir()
	directRecord := filepath.Join(t.TempDir(), "runs")
	directPath := filepath.Join(direct, "compiler")
	directTool := fixtureproc.Write(t, directPath, fixtureproc.Program{Record: directRecord, On: map[string]fixtureproc.Outcome{
		"version":   {Stdout: "1.2.3\n"},
		"exec path": {Stdout: directPath + "\n"},
	}})
	execPath := versionProbe
	execPath.Self = &extensionproto.RuntimeToolchainProbeSelf{Args: []string{"exec", "path"}}
	for range 2 {
		if _, err := probeSystem(t, storeRoot, directTool, execPath); err != nil {
			t.Fatal(err)
		}
	}
	if runs := fixtureproc.Runs(t, directRecord); len(runs) != 3 {
		t.Fatalf("two runs of a binary that names its own path started %d processes, want 3: the check of the first run", len(runs))
	}
	spectest.Proves(t, "cli/job-planning-execution", "unchanged-planning-reads-records",
		"a-toolchain-that-names-itself-is-recorded")
}

func TestADispatcherWhoseTargetChangesIsProbedAgain(t *testing.T) {
	t.Cleanup(probeAnswers.Clear)
	storeRoot := t.TempDir()
	record := filepath.Join(t.TempDir(), "runs")
	// The dispatcher, a compiled stub, answers for the toolchain its
	// configuration names: another file, under another root.
	first, second := t.TempDir(), t.TempDir()
	dispatcher := selfTool(t, filepath.Join(t.TempDir(), "compiler"), record, "1.2.3", first)
	selfTool(t, filepath.Join(first, "bin", "compiler"), filepath.Join(t.TempDir(), "unused"), "1.2.3", first)
	selfTool(t, filepath.Join(second, "bin", "compiler"), filepath.Join(t.TempDir(), "unused"), "1.2.4", second)
	if got, err := probeSystem(t, storeRoot, dispatcher, selfProbe); err != nil || got != "1.2.3" {
		t.Fatalf("first run = %q, %v", got, err)
	}
	// Its target changes; its own file does not.
	data, err := json.Marshal(fixtureproc.Program{Record: record, On: map[string]fixtureproc.Outcome{
		"version": {Stdout: "1.2.4\n"},
		"root":    {Stdout: second + "\n"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dispatcher+".fixture.json", data, 0o644); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if got, err := probeSystem(t, storeRoot, dispatcher, selfProbe); err != nil || got != "1.2.4" {
			t.Fatalf("a run after the switch = %q, %v; want 1.2.4", got, err)
		}
	}
	var args []string
	for _, run := range fixtureproc.Runs(t, record) {
		args = append(args, strings.Join(run.Args, " "))
	}
	if strings.Join(args, ",") != "version,root,version,version" {
		t.Fatalf("three runs started %v, want the version probe on each and the self check once", args)
	}
	spectest.Proves(t, "cli/job-planning-execution", "unchanged-planning-reads-records",
		"a-dispatcher-whose-target-changes-is-probed-again")
}

func TestABinaryThatCannotNameItselfIsCheckedOnce(t *testing.T) {
	t.Cleanup(probeAnswers.Clear)
	storeRoot := t.TempDir()
	root := t.TempDir()
	record := filepath.Join(t.TempDir(), "runs")
	for _, outcome := range []fixtureproc.Outcome{{Exit: 2}, {Stdout: "relative/root\n"}, {Stdout: filepath.Join(root, "missing") + "\n"}} {
		path := filepath.Join(root, "bin", "compiler")
		_ = os.Remove(path)
		tool := fixtureproc.Write(t, path, fixtureproc.Program{Record: record, On: map[string]fixtureproc.Outcome{
			"version": {Stdout: "1.2.3\n"},
			"root":    outcome,
		}})
		for range 3 {
			if got, err := probeSystem(t, storeRoot, tool, selfProbe); err != nil || got != "1.2.3" {
				t.Fatalf("probe = %q, %v", got, err)
			}
		}
	}
	// Each binary: the version probe on each of three runs, the check once.
	if runs := fixtureproc.Runs(t, record); len(runs) != 12 {
		t.Fatalf("got %d runs, want 12: a self check that names no file is not tried again", len(runs))
	}
}

func TestTheSelfProbeRunsInAnEmptyDirectory(t *testing.T) {
	t.Cleanup(probeAnswers.Clear)
	// A configuration file of the working directory is code the repository
	// controls: the self probe does not run where it sits.
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "bunfig.toml"), []byte("preload = [\"./x.ts\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(work)
	root := t.TempDir()
	record := filepath.Join(t.TempDir(), "runs")
	tool := selfTool(t, filepath.Join(root, "bin", "compiler"), record, "1.2.3", root)
	if _, err := probeSystem(t, t.TempDir(), tool, selfProbe); err != nil {
		t.Fatal(err)
	}
	runs := fixtureproc.Runs(t, record)
	if len(runs) != 3 {
		t.Fatalf("runs = %d, want the version probe and the check's two probes", len(runs))
	}
	// The version probe that decides runs in the working directory; the
	// check's probes run in one empty directory of their own.
	if runs[0].Dir != work || runs[1].Dir == work || runs[1].Dir == "" || runs[1].Dir != runs[2].Dir {
		t.Fatalf("the probes ran in %q, %q and %q; want %q, then one empty directory", runs[0].Dir, runs[1].Dir, runs[2].Dir, work)
	}
	if entries, err := os.ReadDir(runs[1].Dir); err == nil {
		t.Fatalf("the self probe's directory outlived it with %d entries", len(entries))
	}
}
