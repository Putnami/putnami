package jobs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	model "go.putnami.dev/cli/model/extension"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	registryproto "go.putnami.dev/protocol/registry"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
	"go.putnami.dev/tooling/cli/internal/hometest"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func TestRuntimeToolchainResolvesExactManagedCandidateWithFalseAmbientRoot(t *testing.T) {
	workspaceRoot := t.TempDir()
	home := filepath.Join(t.TempDir(), "home")
	storeRoot := filepath.Join(home, ".putnami")
	realRoot := filepath.Join(storeRoot, "toolchains", "compiler", "compiler-1.2.3", "root")
	probes := filepath.Join(t.TempDir(), "probes.jsonl")
	realBinary := fixtureproc.Write(t, filepath.Join(realRoot, "bin", "compiler"),
		fixtureproc.Program{Record: probes, Stdout: "1.2.3\n"})
	realBinary, err := filepath.EvalSymlinks(realBinary)
	if err != nil {
		t.Fatal(err)
	}
	realRoot = filepath.Dir(filepath.Dir(realBinary))
	writeRuntimeToolchainLock(t, workspaceRoot, "compiler", "1.2.3", "integrity-a")
	requirement := runtimeToolchainFixture("compiler")
	requirement.Candidates = []extensionproto.RuntimeToolchainCandidate{
		{From: extensionproto.RuntimeToolchainCandidateEnvironment, Environment: "FALSE_ROOT", Path: "bin/compiler"},
		{From: extensionproto.RuntimeToolchainCandidatePutnamiHome, Path: "toolchains/compiler/compiler-{version}/root/bin/compiler"},
	}
	requirement.Probe.Unset = []string{"FALSE_ROOT"}
	ext := runtimeToolchainExtension(requirement)
	base := []string{"HOME=" + home, "PUTNAMI_HOME=" + storeRoot, "PATH=/usr/bin:/bin", "FALSE_ROOT=/wrong/root"}
	if err := resolveRuntimeToolchains(workspaceRoot, ext, []string{"compiler"}, base); err != nil {
		t.Fatal(err)
	}
	resolved := ext.RuntimeToolchains["compiler"]
	if !resolved.Available || resolved.Executable != realBinary {
		t.Fatalf("resolution = %+v, want exact managed executable", resolved)
	}
	runs := fixtureproc.Runs(t, probes)
	if len(runs) == 0 {
		t.Fatal("the managed candidate was never probed")
	}
	for _, run := range runs {
		if _, set := run.LookupEnv("FALSE_ROOT"); set {
			t.Fatal("the probe ran with FALSE_ROOT set, want the variable probe.unset names removed")
		}
	}
	env := applyRuntimeToolchains(base, ext, []string{"compiler"})
	if got := envLastValue(env, "COMPILER_ROOT"); got != realRoot {
		t.Fatalf("derived real root = %q, want %q", got, realRoot)
	}
	if got := filepath.Clean(strings.Split(envLastValue(env, "PATH"), string(os.PathListSeparator))[0]); got != filepath.Dir(realBinary) {
		t.Fatalf("first search path = %q, want %q", got, filepath.Dir(realBinary))
	}
}

func TestRuntimeToolchainRequiredMismatchFailsClosed(t *testing.T) {
	workspaceRoot := t.TempDir()
	bin := t.TempDir()
	compiler := writeRuntimeTool(t, filepath.Join(bin, "compiler"), "1.2.2")
	writeRuntimeToolchainLock(t, workspaceRoot, "compiler", "1.2.3", "integrity-a")
	ext := runtimeToolchainExtension(runtimeToolchainFixture("compiler"))
	err := resolveRuntimeToolchains(workspaceRoot, ext, []string{"compiler"}, []string{"PATH=" + bin})
	want := `no candidate matched locked version "1.2.3": candidate 1 (path "compiler") at ` +
		compiler + `: version "1.2.2" does not match locked "1.2.3"`
	if err == nil || !strings.HasSuffix(err.Error(), want) {
		t.Fatalf("mismatched executable error = %v, want suffix %q", err, want)
	}
	spectest.Proves(t, "cli/runtime-toolchain-pinning", "exact-lock-resolution",
		"a-mismatching-ambient-binary-is-never-substituted")
}

// TestRuntimeToolchainRequiredFailureNamesEveryCandidateInDeclarationOrder pins
// the failure report: each considered candidate appears once, in declaration
// order, with the reason it did not qualify.
func TestRuntimeToolchainRequiredFailureNamesEveryCandidateInDeclarationOrder(t *testing.T) {
	workspaceRoot := t.TempDir()
	putnamiHome := t.TempDir()
	notExecutable := writeNotExecutable(t, filepath.Join(putnamiHome, "tools", "compiler"))
	bin := t.TempDir()
	failing := fixtureproc.Write(t, filepath.Join(bin, "compiler"), fixtureproc.Program{Exit: 3})
	writeRuntimeToolchainLock(t, workspaceRoot, "compiler", "1.2.3", "integrity-a")
	requirement := runtimeToolchainFixture("compiler")
	requirement.Candidates = []extensionproto.RuntimeToolchainCandidate{
		{From: extensionproto.RuntimeToolchainCandidateEnvironment, Environment: "COMPILER_HOME", Path: "bin/compiler"},
		{From: extensionproto.RuntimeToolchainCandidatePutnamiHome, Path: "tools/compiler"},
		{From: extensionproto.RuntimeToolchainCandidatePath, Path: "compiler"},
	}
	ext := runtimeToolchainExtension(requirement)
	err := resolveRuntimeToolchains(workspaceRoot, ext, []string{"compiler"},
		[]string{"PATH=" + bin, "PUTNAMI_HOME=" + putnamiHome})
	want := `resolve extension "example" runtime toolchain "compiler": no candidate matched locked version "1.2.3": ` +
		`candidate 1 (environment COMPILER_HOME "bin/compiler"): environment variable COMPILER_HOME is not set; ` +
		`candidate 2 (putnami-home "tools/compiler") at ` + notExecutable + `: not executable; ` +
		`candidate 3 (path "compiler") at ` + failing + `: probe failed: exit status 3`
	if err == nil || err.Error() != want {
		t.Fatalf("resolution error = %v\nwant %s", err, want)
	}
	// The error carries the probe's own error, and no probe timed out.
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 {
		t.Errorf("resolution error = %v, want it to unwrap to the probe's exit status 3", err)
	}
	if errors.Is(err, ErrToolchainProbeTimeout) {
		t.Errorf("resolution error = %v, want no probe timeout", err)
	}
}

// A required toolchain whose probe timed out fails with the text of any other
// mismatch, and unwraps to ErrToolchainProbeTimeout: a caller tells a slow
// toolchain from an absent one by the error, never by its text.
func TestRuntimeToolchainMismatchUnwrapsToAProbeThatTimedOut(t *testing.T) {
	path := fixtureproc.Write(t, filepath.Join(t.TempDir(), "compiler"), fixtureproc.Program{Sleep: 5 * time.Second})
	_, probeErr := probeRuntimeToolchain(context.Background(), path, nil, []string{"PATH=/usr/bin:/bin"}, 100*time.Millisecond)
	if probeErr == nil {
		t.Fatal("a probe that sleeps 5s answered within 100ms")
	}
	absent := runtimeToolchainRejection{candidate: `candidate 1 (path "compiler")`, reason: "path missing"}
	timedOut := runtimeToolchainRejection{candidate: `candidate 2 (path "compiler") at ` + path, reason: probeErr.Error(), err: probeErr}

	err := newRuntimeToolchainMismatchError("example", "compiler", "1.2.3", []runtimeToolchainRejection{absent, timedOut})
	want := `resolve extension "example" runtime toolchain "compiler": no candidate matched locked version "1.2.3": ` +
		`candidate 1 (path "compiler"): path missing; candidate 2 (path "compiler") at ` + path + `: probe timed out after 100ms`
	if err.Error() != want {
		t.Fatalf("error = %v\nwant %s", err, want)
	}
	if !errors.Is(err, ErrToolchainProbeTimeout) {
		t.Errorf("error = %v, want it to unwrap to %v", err, ErrToolchainProbeTimeout)
	}
	if err := newRuntimeToolchainMismatchError("example", "compiler", "1.2.3", []runtimeToolchainRejection{absent}); errors.Is(err, ErrToolchainProbeTimeout) {
		t.Errorf("error of an absent toolchain = %v, want no probe timeout", err)
	}
}

// TestRuntimeToolchainProbeNamesWhyTheOutputIsNoVersion pins each probe
// rejection reason, including the timeout a slow machine can trip.
func TestRuntimeToolchainProbeNamesWhyTheOutputIsNoVersion(t *testing.T) {
	cases := []struct {
		name    string
		program fixtureproc.Program
		// holdOutput has a copy of the program keep its output open after the
		// program exits.
		holdOutput bool
		timeout    time.Duration
		want       string
	}{
		{"timeout", fixtureproc.Program{Sleep: 5 * time.Second}, false, 100 * time.Millisecond, "probe timed out after 100ms"},
		{"exit status", fixtureproc.Program{Exit: 3}, false, toolVersionProbeTimeout, "probe failed: exit status 3"},
		{"empty", fixtureproc.Program{}, false, toolVersionProbeTimeout, "probe output empty"},
		{"too long", fixtureproc.Program{Stdout: strings.Repeat("0", 70) + "\n"}, false, toolVersionProbeTimeout, "probe output too long (70 bytes, limit 64)"},
		{"several lines", fixtureproc.Program{Stdout: "1.2.3\nextra\n"}, false, toolVersionProbeTimeout, "probe output malformed (more than one line)"},
		{"output open after exit", fixtureproc.Program{Stdout: "1.2.3\n"}, true, toolVersionProbeTimeout, "probe output stayed open after exit (wait delay 1s)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			program := tc.program
			if tc.holdOutput {
				program.HoldOutput = filepath.Join(dir, "release")
				defer fixtureproc.Release(t, program.HoldOutput)
			}
			path := fixtureproc.Write(t, filepath.Join(dir, "compiler"), program)
			got, err := probeRuntimeToolchain(context.Background(), path, nil, []string{"PATH=/usr/bin:/bin"}, tc.timeout)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("probe = %q, %v; want error %q", got, err, tc.want)
			}
			// Only a timeout is ErrToolchainProbeTimeout.
			if timedOut := strings.HasPrefix(tc.want, "probe timed out"); errors.Is(err, ErrToolchainProbeTimeout) != timedOut {
				t.Fatalf("errors.Is(%q, ErrToolchainProbeTimeout) = %t, want %t", err, !timedOut, timedOut)
			}
		})
	}
	t.Run("never started", func(t *testing.T) {
		// A file that is no program the host can start: its interpreter does
		// not exist, and Windows reads no program image in it.
		path := withExecutableSuffix(runtime.GOOS, filepath.Join(t.TempDir(), "compiler"))
		if err := os.WriteFile(path, []byte("#!/nonexistent/interpreter\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		got, err := probeRuntimeToolchain(context.Background(), path, nil, []string{"PATH=/usr/bin:/bin"}, toolVersionProbeTimeout)
		if want := "probe failed to start: "; err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Fatalf("probe = %q, %v; want an error starting with %q", got, err, want)
		}
	})
}

func TestOptionalRuntimeToolchainPublishesUnavailableBindingWithoutAmbientFallback(t *testing.T) {
	workspaceRoot := t.TempDir()
	bin := t.TempDir()
	compiler := writeRuntimeTool(t, filepath.Join(bin, "compiler"), "0.9.0")
	writeRuntimeToolchainLock(t, workspaceRoot, "compiler", "1.2.3", "integrity-a")
	requirement := runtimeToolchainFixture("compiler")
	requirement.Optional = true
	requirement.Environment["RESOLVED_COMPILER"] = extensionproto.RuntimeToolchainEnvironment{From: extensionproto.RuntimeToolchainEnvironmentExecutable}
	ext := runtimeToolchainExtension(requirement)
	base := []string{"PATH=" + bin}
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	if err := resolveRuntimeToolchains(workspaceRoot, ext, []string{"compiler"}, base); err != nil {
		t.Fatal(err)
	}
	resolution := ext.RuntimeToolchains["compiler"]
	if resolution.Available || resolution.Identity == "" {
		t.Fatalf("optional mismatch resolution = %+v", resolution)
	}
	wantReason := fmt.Sprintf("%q", `candidate 1 (path "compiler") at `+compiler+
		`: version "0.9.0" does not match locked "1.2.3"`)
	if got := logs.String(); strings.Count(got, "\n") != 1 || !strings.Contains(got, "level=WARN") ||
		!strings.Contains(got, "toolchain=compiler") || !strings.Contains(got, "rejections="+wantReason) {
		t.Fatalf("optional unavailable warning = %q, want one WARN line with rejections=%s", got, wantReason)
	}
	env := applyRuntimeToolchains(base, ext, []string{"compiler"})
	if !envHasKey(env, "RESOLVED_COMPILER") || envLastValue(env, "RESOLVED_COMPILER") != "" {
		t.Fatalf("unavailable executable binding was not published empty: %v", env)
	}
	spectest.Proves(t, "cli/runtime-toolchain-pinning", "exact-lock-resolution",
		"an-optional-mismatch-publishes-an-empty-binding")
}

func TestRuntimeToolchainLockChangeInvalidatesPortableIdentity(t *testing.T) {
	workspaceRoot := t.TempDir()
	requirement := runtimeToolchainFixture("compiler")
	requirement.Probe.Environment = map[string]string{"EXPECTED_VERSION": "{version}"}
	// Each pin gets a compiler that prints it. The identity hashes no path, so
	// the two directories cannot tell the identities apart.
	resolve := func(version, integrity string) string {
		t.Helper()
		bin := t.TempDir()
		probes := filepath.Join(bin, "probes.jsonl")
		fixtureproc.Write(t, filepath.Join(bin, "compiler"), fixtureproc.Program{Record: probes, Stdout: version + "\n"})
		writeRuntimeToolchainLock(t, workspaceRoot, "compiler", version, integrity)
		ext := runtimeToolchainExtension(requirement)
		if err := resolveRuntimeToolchains(workspaceRoot, ext, []string{"compiler"}, []string{"PATH=" + bin}); err != nil {
			t.Fatal(err)
		}
		runs := fixtureproc.Runs(t, probes)
		if len(runs) == 0 {
			t.Fatalf("the %s compiler was never probed", version)
		}
		for _, run := range runs {
			if got, _ := run.LookupEnv("EXPECTED_VERSION"); got != version {
				t.Fatalf("probe EXPECTED_VERSION = %q, want the pinned %q", got, version)
			}
		}
		return ext.RuntimeToolchains["compiler"].Identity
	}
	first := resolve("1.2.3", "integrity-a")
	if second := resolve("1.2.4", "integrity-b"); first == second {
		t.Fatalf("runtime identity did not change with workspace pin: %q", first)
	}
}

// TestRuntimeToolchainCommandScanResolvesOnlyActiveCommandRequirements pins the
// narrowing: an extension resolves the tools of the commands this run selected
// and nothing else, so a run that plans none of its tasks pays no probe.
func TestRuntimeToolchainCommandScanResolvesOnlyActiveCommandRequirements(t *testing.T) {
	workspaceRoot := t.TempDir()
	writeRuntimeToolchainLock(t, workspaceRoot, "optional", "1.0.0", "integrity-a")
	requirement := runtimeToolchainFixture("optional")
	requirement.Optional = true
	ext := runtimeToolchainExtension(requirement)
	ext.Tasks = map[string]model.TaskDefinition{
		"generate": {Toolchains: []string{"compiler"}},
		"publish":  {Toolchains: []string{"compiler"}},
	}
	ext.Jobs = map[string]*model.JobDefinition{
		"build":   {PipelineSteps: []extensionproto.PipelineStep{{ID: "generate", Task: "generate"}}},
		"deploy":  {PipelineSteps: []extensionproto.PipelineStep{{ID: "publish", Task: "publish"}}},
		"inspect": {},
	}

	if got := commandRuntimeToolchainRefs(ext, map[string]bool{"inspect": true}); len(got) != 0 {
		t.Fatalf("inactive-task command refs = %v, want none", got)
	}
	if err := resolveRuntimeToolchainsForCommands(workspaceRoot, []*model.ExtensionDescription{ext},
		map[string]bool{"build": true}, os.Environ()); err != nil {
		t.Fatal(err)
	}
	if _, ok := ext.RuntimeToolchains["compiler"]; !ok {
		t.Fatal("active command requirement was not resolved")
	}
}

// TestRuntimeToolchainCommandScanMergesRunAndTaskRequirementsOnce pins the
// de-duplication: a task that restates a run-wide requirement names it once, so
// the tool is probed once, published once, and named once in the identity.
func TestRuntimeToolchainCommandScanMergesRunAndTaskRequirementsOnce(t *testing.T) {
	ext := runtimeToolchainExtension(runtimeToolchainFixture("compiler"))
	ext.Runtime.RunToolchains = []string{"compiler"}
	ext.Tasks = map[string]model.TaskDefinition{"generate": {Toolchains: []string{"compiler"}}}
	ext.Jobs = map[string]*model.JobDefinition{
		"build": {
			Toolchains:    []string{"compiler", "compiler"},
			PipelineSteps: []extensionproto.PipelineStep{{ID: "generate", Task: "generate"}},
		},
	}
	if got := commandRuntimeToolchainRefs(ext, map[string]bool{"build": true}); len(got) != 1 || got[0] != "compiler" {
		t.Fatalf("merged refs = %v, want [compiler]", got)
	}
}

// TestRuntimeToolchainLocksCoverEveryLockTheResolversRead ties the lock
// refresh to the resolvers: every lock identity the preparation, command and
// job resolvers read for an extension is one extension.RuntimeToolchainLocks
// reports, so the refresh keeps every pin the next command reads.
func TestRuntimeToolchainLocksCoverEveryLockTheResolversRead(t *testing.T) {
	t.Parallel()
	ext := runtimeToolchainExtension(runtimeToolchainFixture("compiler-lock"))
	ext.LocalSource = true
	ext.Runtime.Toolchains["runner"] = runtimeToolchainFixture("runner-lock")
	ext.Runtime.Toolchains["generator"] = runtimeToolchainFixture("generator-lock")
	ext.Runtime.Toolchains["packer"] = runtimeToolchainFixture("packer-lock")
	ext.Runtime.RunToolchains = []string{"runner"}
	ext.Runtime.Prepare = &extensionproto.RuntimePrepare{Command: "{extensionRoot}/bin/prepare", Toolchains: []string{"compiler"}}
	ext.Tasks = map[string]model.TaskDefinition{"generate": {Toolchains: []string{"generator"}}}
	ext.Jobs = map[string]*model.JobDefinition{
		"build":   {PipelineSteps: []extensionproto.PipelineStep{{ID: "generate", Task: "generate"}}},
		"package": {Toolchains: []string{"packer"}},
	}

	read := map[string]bool{}
	lockOf := func(refs []string) {
		for _, ref := range refs {
			read[ext.Runtime.Toolchains[ref].Lock] = true
		}
	}
	commands := map[string]bool{}
	for name, def := range ext.Jobs {
		commands[name] = true
		lockOf(def.Toolchains)
	}
	lockOf(commandRuntimeToolchainRefs(ext, commands))
	lockOf(ext.Runtime.Prepare.Toolchains)

	kept := extension.RuntimeToolchainLocks(ext)
	for lock := range read {
		if !slices.Contains(kept, lock) {
			t.Errorf("the resolvers read the %q pin, but the lock refresh would drop it (kept %v)", lock, kept)
		}
	}
	if len(read) != 4 {
		t.Fatalf("the resolvers read %d lock identities, want 4: the fixture no longer exercises every reference kind", len(read))
	}
}

// TestRuntimeToolchainHomeCandidateResolvesWithoutHomeInTheChildEnvironment
// pins the portable home: a stripped environment names no HOME, and reading it
// alone would skip the candidate and report the tool missing.
// An extension that installs its toolchain inside the workspace, as the Go
// extension installs the locked Go release on a host without one, declares it
// as an environment candidate below PUTNAMI_WORKSPACE_ROOT. The CLI expands it
// against this workspace's root, not a value inherited from a parent job.
func TestRuntimeToolchainEnvironmentCandidateFindsAToolInsideTheWorkspace(t *testing.T) {
	workspaceRoot := t.TempDir()
	binary := writeRuntimeTool(t,
		filepath.Join(workspaceRoot, ".putnami", "extensions", "example", "compiler-1.2.3", "root", "bin", "compiler"), "1.2.3")
	binary, err := filepath.EvalSymlinks(binary)
	if err != nil {
		t.Fatal(err)
	}
	writeRuntimeToolchainLock(t, workspaceRoot, "compiler", "1.2.3", "integrity-a")
	requirement := runtimeToolchainFixture("compiler")
	requirement.Candidates = []extensionproto.RuntimeToolchainCandidate{{
		From:        extensionproto.RuntimeToolchainCandidateEnvironment,
		Environment: runtimeToolchainWorkspaceRootEnv,
		Path:        ".putnami/extensions/example/compiler-{version}/root/bin/compiler",
	}}
	ext := runtimeToolchainExtension(requirement)
	inherited := runtimeToolchainWorkspaceRootEnv + "=" + t.TempDir()
	if err := resolveRuntimeToolchains(workspaceRoot, ext, []string{"compiler"}, []string{"PATH=/usr/bin:/bin", inherited}); err != nil {
		t.Fatal(err)
	}
	if resolved := ext.RuntimeToolchains["compiler"]; !resolved.Available || resolved.Executable != binary {
		t.Fatalf("resolution = %+v, want the workspace's %s", resolved, binary)
	}
	if env := runtimeToolchainCandidateEnv("", []string{inherited}); envLastValue(env, runtimeToolchainWorkspaceRootEnv) != "" {
		t.Fatalf("candidate environment without a workspace = %v, want the inherited root unset", env)
	}
	spectest.Proves(t, "cli/runtime-toolchain-pinning", "workspace-installed-candidates",
		"an-environment-candidate-finds-a-tool-inside-the-workspace")
}

func TestRuntimeToolchainHomeCandidateResolvesWithoutHomeInTheChildEnvironment(t *testing.T) {
	// The fallback under test is os.UserHomeDir, which reads this process's
	// HOME. Point it at a scratch directory so the test never writes into the
	// real home: the native CI lane runs the DAG below root with a read-only
	// /root as HOME, and a shared host home must not collect fixtures either.
	// The fixture program is built first, under the real home.
	fixtureproc.Prepare(t)
	hometest.Temp(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("user home: %v", err)
	}
	relative := filepath.Join(".putnami-runtime-toolchain-test", fmt.Sprintf("compiler-%d", os.Getpid()))
	writeRuntimeTool(t, filepath.Join(home, relative, "bin", "compiler"), "1.2.3")

	workspaceRoot := t.TempDir()
	writeRuntimeToolchainLock(t, workspaceRoot, "compiler", "1.2.3", "integrity-a")
	requirement := runtimeToolchainFixture("compiler")
	requirement.Candidates = []extensionproto.RuntimeToolchainCandidate{{
		From: extensionproto.RuntimeToolchainCandidateHome,
		Path: filepath.ToSlash(filepath.Join(relative, "bin", "compiler")),
	}}
	ext := runtimeToolchainExtension(requirement)
	if err := resolveRuntimeToolchains(workspaceRoot, ext, []string{"compiler"}, []string{"PATH=/usr/bin:/bin"}); err != nil {
		t.Fatal(err)
	}
	if !ext.RuntimeToolchains["compiler"].Available {
		t.Fatal("home candidate was not resolved without HOME in the child environment")
	}
	spectest.Proves(t, "cli/runtime-toolchain-pinning", "minimal-child-environment",
		"a-home-candidate-resolves-without-home-in-the-environment")
}

// TestMinimalRuntimePathOrdersResolvedFirstAndKeepsEveryDirectoryOnce pins the
// PATH the child receives: the pinned tool wins every bare-name lookup, each
// inherited directory survives exactly once, and a directory that also carries
// an ambient copy of the pinned tool is NOT removed — a shared bin directory
// holds unrelated tools a build still needs.
func TestMinimalRuntimePathOrdersResolvedFirstAndKeepsEveryDirectoryOnce(t *testing.T) {
	resolvedDir := filepath.FromSlash("/pinned/bin")
	ambientDir := filepath.FromSlash("/shared/bin")
	neutralDir := filepath.FromSlash("/usr/bin")

	inherited := strings.Join(
		[]string{ambientDir, neutralDir, "", ambientDir, resolvedDir},
		string(filepath.ListSeparator))
	got := filepath.SplitList(minimalRuntimePath(inherited, []string{resolvedDir}))
	want := []string{resolvedDir, ambientDir, neutralDir}
	if !slices.Equal(got, want) {
		t.Fatalf("minimal PATH = %v, want %v", got, want)
	}
}

func TestPrepareJobInvocationResolvesToolchainBeforeBuildingEnvironment(t *testing.T) {
	workspaceRoot := t.TempDir()
	putnamiHome := filepath.Join(t.TempDir(), "putnami-home")
	realRoot := filepath.Join(putnamiHome, "tools", "compiler-1.2.3", "root")
	realBinary := writeRuntimeTool(t, filepath.Join(realRoot, "bin", "compiler"), "1.2.3")
	realBinary, err := filepath.EvalSymlinks(realBinary)
	if err != nil {
		t.Fatal(err)
	}
	realRoot = filepath.Dir(filepath.Dir(realBinary))
	wrongBin := t.TempDir()
	writeRuntimeTool(t, filepath.Join(wrongBin, "compiler"), "9.9.9")
	writeRuntimeToolchainLock(t, workspaceRoot, "compiler", "1.2.3", "integrity-a")
	t.Setenv("PUTNAMI_HOME", putnamiHome)
	t.Setenv("PATH", wrongBin)

	requirement := runtimeToolchainFixture("compiler")
	requirement.Candidates = []extensionproto.RuntimeToolchainCandidate{{
		From: extensionproto.RuntimeToolchainCandidatePutnamiHome,
		Path: "tools/compiler-{version}/root/bin/compiler",
	}}
	ext := runtimeToolchainExtension(requirement)
	project := &workspace.Project{ID: "/provider", Name: "provider", Path: "."}
	ws := workspace.NewWorkspace(workspaceRoot, nil, []*workspace.Project{project})
	job := &ScheduledJob{
		Project:   project,
		Extension: ext,
		JobDef: &model.JobDefinition{
			Name: "generate", Kind: "command", Command: "/bin/true", Toolchains: []string{"compiler"},
		},
	}

	_, cancel, invocation, err := prepareJobInvocation(context.Background(), ws, job, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	defer func() { _ = os.Remove(invocation.contextFile) }()
	if got := envLastValue(invocation.env, "COMPILER_ROOT"); got != realRoot {
		t.Fatalf("invocation compiler root = %q, want %q", got, realRoot)
	}
	paths := filepath.SplitList(envLastValue(invocation.env, "PATH"))
	if len(paths) == 0 || filepath.Clean(paths[0]) != filepath.Dir(realBinary) {
		t.Fatalf("invocation PATH = %q, want the resolved compiler first", envLastValue(invocation.env, "PATH"))
	}
	// The mismatching ambient directory stays reachable — it carries other
	// tools — but it can no longer answer the bare name.
	if !slices.Contains(paths, wrongBin) {
		t.Fatalf("invocation PATH = %q, dropped the ambient directory entirely", envLastValue(invocation.env, "PATH"))
	}
	spectest.Proves(t, "cli/runtime-toolchain-pinning", "minimal-child-environment",
		"the-pinned-tool-answers-every-bare-name-lookup")
}

func TestPrepareJobInvocationResolvedToolchainPrecedesMismatchedCLISibling(t *testing.T) {
	workspaceRoot := t.TempDir()
	putnamiHome := filepath.Join(t.TempDir(), "putnami-home")
	toolName := fmt.Sprintf("putnami-runtime-tool-%d", os.Getpid())
	resolvedBinary := writeRuntimeTool(t, filepath.Join(putnamiHome, "tools", "compiler-1.2.3", "bin", toolName), "1.2.3")
	resolvedBinary, err := filepath.EvalSymlinks(resolvedBinary)
	if err != nil {
		t.Fatal(err)
	}
	cliExecutable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// A file named like the pinned tool beside the CLI. Nothing starts it: the
	// one candidate is below PUTNAMI_HOME.
	cliSibling := withExecutableSuffix(runtime.GOOS, filepath.Join(filepath.Dir(cliExecutable), toolName))
	if err := os.WriteFile(cliSibling, []byte("9.9.9\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(cliSibling) })
	writeRuntimeToolchainLock(t, workspaceRoot, "compiler", "1.2.3", "integrity-a")
	t.Setenv("PUTNAMI_HOME", putnamiHome)

	requirement := runtimeToolchainFixture("compiler")
	requirement.Candidates = []extensionproto.RuntimeToolchainCandidate{{
		From: extensionproto.RuntimeToolchainCandidatePutnamiHome,
		Path: "tools/compiler-{version}/bin/" + toolName,
	}}
	ext := runtimeToolchainExtension(requirement)
	project := &workspace.Project{ID: "/provider", Name: "provider", Path: "."}
	ws := workspace.NewWorkspace(workspaceRoot, nil, []*workspace.Project{project})
	job := &ScheduledJob{
		Project:   project,
		Extension: ext,
		JobDef: &model.JobDefinition{
			Name: "generate", Kind: "command", Command: "/bin/true", Toolchains: []string{"compiler"},
		},
	}

	_, cancel, invocation, err := prepareJobInvocation(context.Background(), ws, job, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	defer func() { _ = os.Remove(invocation.contextFile) }()
	paths := filepath.SplitList(envLastValue(invocation.env, "PATH"))
	if len(paths) == 0 || filepath.Clean(paths[0]) != filepath.Dir(resolvedBinary) {
		t.Fatalf("invocation PATH = %q, want the resolved compiler first", envLastValue(invocation.env, "PATH"))
	}
	// The directory beside the CLI carries a mismatching copy of the pinned
	// name. It stays on PATH — the CLI lives there — but the resolved compiler
	// precedes it, so the bare name never reaches the sibling. The exact CLI
	// process stays addressable through its explicit variable either way.
	resolvedIndex := slices.Index(paths, filepath.Dir(resolvedBinary))
	cliIndex := slices.Index(paths, filepath.Dir(cliExecutable))
	if resolvedIndex < 0 || cliIndex < 0 || resolvedIndex >= cliIndex {
		t.Fatalf("invocation PATH = %q, want the resolved compiler before the CLI directory",
			envLastValue(invocation.env, "PATH"))
	}
	if got := envLastValue(invocation.env, registryproto.CLIExecutableEnv); got != cliExecutable {
		t.Fatalf("CLI executable variable = %q, want %q", got, cliExecutable)
	}
}

func TestPrepareJobInvocationFailsBeforeEnvironmentForWrongRequiredPin(t *testing.T) {
	workspaceRoot := t.TempDir()
	wrongBin := t.TempDir()
	writeRuntimeTool(t, filepath.Join(wrongBin, "compiler"), "9.9.9")
	writeRuntimeToolchainLock(t, workspaceRoot, "compiler", "1.2.3", "integrity-a")
	t.Setenv("PATH", wrongBin)

	ext := runtimeToolchainExtension(runtimeToolchainFixture("compiler"))
	project := &workspace.Project{ID: "/provider", Name: "provider", Path: "."}
	ws := workspace.NewWorkspace(workspaceRoot, nil, []*workspace.Project{project})
	job := &ScheduledJob{
		Project:   project,
		Extension: ext,
		JobDef: &model.JobDefinition{
			Name: "generate", Kind: "command", Command: "/bin/true", Toolchains: []string{"compiler"},
		},
	}

	_, _, _, err := prepareJobInvocation(context.Background(), ws, job, nil, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), `no candidate matched locked version "1.2.3"`) {
		t.Fatalf("wrong-pin invocation error = %v", err)
	}
}

// TestRuntimeToolchainWithoutALockResolvesUnavailableAndKeysApart pins the
// bootstrap floor: the commands that WRITE the lock must run in a workspace that
// has none. The reference still gets an identity, and it differs from the one a
// pinned run gets, so the artifact a bootstrap produces is never served to a
// pinned run.
func TestRuntimeToolchainWithoutALockResolvesUnavailableAndKeysApart(t *testing.T) {
	workspaceRoot := t.TempDir()
	bin := t.TempDir()
	writeRuntimeTool(t, filepath.Join(bin, "compiler"), "1.2.3")
	base := []string{"PATH=" + bin}

	bootstrap := runtimeToolchainExtension(runtimeToolchainFixture("compiler"))
	if err := resolveRuntimeToolchains(workspaceRoot, bootstrap, []string{"compiler"}, base); err != nil {
		t.Fatalf("a workspace with no lock refused to bootstrap: %v", err)
	}
	unpinned := bootstrap.RuntimeToolchains["compiler"]
	if unpinned.Available || unpinned.Identity == "" {
		t.Fatalf("unpinned resolution = %+v, want an unavailable identity", unpinned)
	}

	writeRuntimeToolchainLock(t, workspaceRoot, "compiler", "1.2.3", "integrity-a")
	pinnedExt := runtimeToolchainExtension(runtimeToolchainFixture("compiler"))
	if err := resolveRuntimeToolchains(workspaceRoot, pinnedExt, []string{"compiler"}, base); err != nil {
		t.Fatal(err)
	}
	if pinned := pinnedExt.RuntimeToolchains["compiler"]; !pinned.Available || pinned.Identity == unpinned.Identity {
		t.Fatalf("pinned resolution = %+v, want an available identity distinct from %q", pinned, unpinned.Identity)
	}
}

// TestRuntimeToolchainWithALockButNoPinFailsClosed keeps the floor narrow: a
// lock that exists and does not pin a required alias is a workspace error, not
// permission to use whatever is installed.
func TestRuntimeToolchainWithALockButNoPinFailsClosed(t *testing.T) {
	workspaceRoot := t.TempDir()
	writeRuntimeToolchainLock(t, workspaceRoot, "other", "1.2.3", "integrity-a")
	ext := runtimeToolchainExtension(runtimeToolchainFixture("compiler"))
	err := resolveRuntimeToolchains(workspaceRoot, ext, []string{"compiler"}, []string{"PATH=" + t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), `workspace lock has no exact "compiler" pin`) {
		t.Fatalf("missing-pin error = %v", err)
	}
}

func runtimeToolchainFixture(lock string) extensionproto.RuntimeToolchain {
	return extensionproto.RuntimeToolchain{
		Lock:       lock,
		Candidates: []extensionproto.RuntimeToolchainCandidate{{From: extensionproto.RuntimeToolchainCandidatePath, Path: "compiler"}},
		Probe:      extensionproto.RuntimeToolchainProbe{Args: []string{"--version"}, Expect: "{version}"},
		Environment: map[string]extensionproto.RuntimeToolchainEnvironment{
			"COMPILER_ROOT": {From: extensionproto.RuntimeToolchainEnvironmentAncestor, Levels: 2},
		},
		PrependPath: true,
	}
}

func runtimeToolchainExtension(requirement extensionproto.RuntimeToolchain) *model.ExtensionDescription {
	return &model.ExtensionDescription{
		Name: "example",
		Runtime: &extensionproto.RuntimeDefinition{
			Executable: "compiled/example",
			Toolchains: map[string]extensionproto.RuntimeToolchain{"compiler": requirement},
		},
	}
}

func writeRuntimeToolchainLock(t *testing.T, root, name, version, integrity string) {
	t.Helper()
	locked := lockfile.NewLockFile()
	locked.SetToolchain(name, lockfile.LockEntry{
		Version:     version,
		Integrities: map[string]string{lockfile.PlatformKey(runtime.GOOS, runtime.GOARCH): integrity},
	})
	if err := lockfile.WriteLockFile(root, locked); err != nil {
		t.Fatal(err)
	}
}

// writeRuntimeTool places, at path, a program that prints version, and
// returns its path (fixtureproc.Write).
func writeRuntimeTool(t *testing.T, path, version string) string {
	t.Helper()
	return fixtureproc.Write(t, path, fixtureproc.Program{Stdout: version + "\n"})
}

// writeNotExecutable places, at path, an entry a candidate finds and cannot
// start, and returns its path: a file without an executable bit, or on
// Windows, which has no such bit, a directory named like the program.
func writeNotExecutable(t *testing.T, path string) string {
	t.Helper()
	path = withExecutableSuffix(runtime.GOOS, path)
	if runtime.GOOS == "windows" {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("1.2.3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
