package toolchain

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	model "go.putnami.dev/cli/model/extension"
	"go.putnami.dev/cli/model/jobs"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
	"go.putnami.dev/tooling/cli/internal/jobs/machineprobe"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// provisioningToolchainExtension declares one required toolchain every task of
// the runtime runs with, and two workspace commands that reach it: the
// provisioning verb and build.
func provisioningToolchainExtension() *model.ExtensionDescription {
	ext := runtimeToolchainExtension(runtimeToolchainFixture("compiler"))
	ext.Runtime.RunToolchains = []string{"compiler"}
	ext.Tasks = map[string]model.TaskDefinition{"install": {}, "compile": {}}
	ext.Jobs = map[string]*model.JobDefinition{
		ToolchainProvisioningCommand: {PipelineSteps: []extensionproto.PipelineStep{{ID: "install", Task: "install"}}},
		"build":                      {PipelineSteps: []extensionproto.PipelineStep{{ID: "compile", Task: "compile"}}},
	}
	return ext
}

// TestRuntimeToolchainProvisioningCommandRunsWithoutAUsablePin pins the
// provisioning exception: a lock that exists but cannot satisfy a required
// toolchain stops every command except workspace-install, which resolves the
// unavailable identity so the install that writes the pin's source can run.
func TestRuntimeToolchainProvisioningCommandRunsWithoutAUsablePin(t *testing.T) {
	bin := t.TempDir()
	writeRuntimeTool(t, filepath.Join(bin, "compiler"), "0.9.0")
	base := []string{"PATH=" + bin}
	cases := []struct {
		name      string
		lock      func(t *testing.T, root string)
		strictErr string
	}{
		{
			name:      "no pin",
			lock:      func(t *testing.T, root string) { writeRuntimeToolchainLock(t, root, "other", "1.2.3", "integrity-a") },
			strictErr: `workspace lock has no exact "compiler" pin`,
		},
		{
			name: "no host integrity",
			lock: func(t *testing.T, root string) {
				locked := lockfile.NewLockFile()
				locked.SetToolchain("compiler", lockfile.LockEntry{Version: "1.2.3", Integrities: map[string]string{"plan9/mips": "integrity-a"}})
				if err := lockfile.WriteLockFile(root, locked); err != nil {
					t.Fatal(err)
				}
			},
			strictErr: "workspace lock has no integrity for " + machineprobe.HostPlatform(),
		},
		{
			name: "no matching candidate",
			lock: func(t *testing.T, root string) {
				writeRuntimeToolchainLock(t, root, "compiler", "1.2.3", "integrity-a")
			},
			strictErr: `no candidate matched locked version "1.2.3"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			tc.lock(t, root)

			provisioning := provisioningToolchainExtension()
			if err := resolveRuntimeToolchainsForCommands(root, []*model.ExtensionDescription{provisioning},
				map[string]bool{ToolchainProvisioningCommand: true}, base); err != nil {
				t.Fatalf("%s refused a lock it exists to repair: %v", ToolchainProvisioningCommand, err)
			}
			resolved, ok := provisioning.RuntimeToolchains["compiler"]
			if !ok || resolved.Available || resolved.Identity == "" {
				t.Fatalf("provisioning resolution = %+v (present %v), want the unavailable identity", resolved, ok)
			}

			for _, active := range []map[string]bool{
				{"build": true},
				{"build": true, ToolchainProvisioningCommand: true},
			} {
				strict := provisioningToolchainExtension()
				err := resolveRuntimeToolchainsForCommands(root, []*model.ExtensionDescription{strict}, active, base)
				if err == nil || !strings.Contains(err.Error(), tc.strictErr) {
					t.Fatalf("commands %v: error = %v, want it to contain %q", active, err, tc.strictErr)
				}
			}
		})
	}
	spectest.Proves(t, "cli/runtime-toolchain-pinning", "provisioning-runs-unpinned",
		"workspace-install-resolves-an-unusable-pin-unavailable")
}

// captureRuntimeToolchainLogs routes the default logger, at its default info
// level, to a buffer for the rest of the test.
func captureRuntimeToolchainLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &logs
}

// resolveForProvisioning resolves the provisioning extension's toolchains the
// way phase 1c does for a workspace-install run: with the process environment.
func resolveForProvisioning(t *testing.T, root string) *model.ExtensionDescription {
	t.Helper()
	ext := provisioningToolchainExtension()
	if err := resolveRuntimeToolchainsForCommands(root, []*model.ExtensionDescription{ext},
		map[string]bool{ToolchainProvisioningCommand: true}, os.Environ()); err != nil {
		t.Fatal(err)
	}
	if resolved := ext.RuntimeToolchains["compiler"]; resolved.Available {
		t.Fatalf("resolution = %+v, want the unavailable identity before the install", resolved)
	}
	return ext
}

// TestRuntimeToolchainProvisioningMismatchNamesTheCommandInItsWarning pins the
// one signal a tolerated mismatch leaves once nothing installed the pinned
// toolchain: after the run, a warning naming the provisioning command, the
// toolchain and the rejected candidates. When the run starts, nothing is wrong
// yet, so nothing is logged at the default level.
func TestRuntimeToolchainProvisioningMismatchNamesTheCommandInItsWarning(t *testing.T) {
	spectest.Proves(t, "cli/runtime-toolchain-pinning", "provisioning-reports-the-installed-toolchain",
		"a-toolchain-still-missing-after-the-install-keeps-the-warning")
	fixtureproc.Prepare(t) // while go is still on PATH
	root := t.TempDir()
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	writeRuntimeTool(t, filepath.Join(bin, "compiler"), "0.9.0")
	writeRuntimeToolchainLock(t, root, "compiler", "1.2.3", "integrity-a")
	logs := captureRuntimeToolchainLogs(t)

	ext := resolveForProvisioning(t, root)
	if got := logs.String(); got != "" {
		t.Fatalf("resolution before the install logged %q, want nothing at the default level", got)
	}
	ReportProvisionedRuntimeToolchains(context.Background(), root, []*model.ExtensionDescription{ext}, nil, []string{ToolchainProvisioningCommand})
	got := logs.String()
	if strings.Count(got, "\n") != 1 || !strings.Contains(got, "level=WARN") ||
		!strings.Contains(got, "still unavailable after "+ToolchainProvisioningCommand) ||
		!strings.Contains(got, "toolchain=compiler") || !strings.Contains(got, `version \"0.9.0\" does not match locked \"1.2.3\"`) {
		t.Fatalf("provisioning mismatch warning = %q", got)
	}
}

// TestRuntimeToolchainProvisioningReportsTheToolchainItInstalled pins the first
// install of a pinned toolchain: the candidate appears during the run, so the
// report after it is one info line and no warning, and the resolution the run
// used stays the unavailable one it was keyed under.
func TestRuntimeToolchainProvisioningReportsTheToolchainItInstalled(t *testing.T) {
	spectest.Proves(t, "cli/runtime-toolchain-pinning", "provisioning-reports-the-installed-toolchain",
		"a-toolchain-the-install-provided-is-reported-at-info")
	fixtureproc.Prepare(t) // while go is still on PATH
	root := t.TempDir()
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	writeRuntimeToolchainLock(t, root, "compiler", "1.2.3", "integrity-a")
	logs := captureRuntimeToolchainLogs(t)

	ext := resolveForProvisioning(t, root)
	writeRuntimeTool(t, filepath.Join(bin, "compiler"), "1.2.3")
	// The extension reaches the report through two planned jobs, as a planned
	// copy of an extension does, and is reported once.
	planned := []*jobs.ScheduledJob{{Extension: ext}, {Extension: ext}}
	ReportProvisionedRuntimeToolchains(context.Background(), root, nil, planned, []string{ToolchainProvisioningCommand})
	got := logs.String()
	if strings.Count(got, "\n") != 1 || !strings.Contains(got, "level=INFO") ||
		!strings.Contains(got, ToolchainProvisioningCommand+" installed the pinned toolchain") ||
		!strings.Contains(got, "toolchain=compiler") || !strings.Contains(got, "version=1.2.3") {
		t.Fatalf("report after the install = %q, want one info line", got)
	}
	if resolved := ext.RuntimeToolchains["compiler"]; resolved.Available {
		t.Fatalf("the report changed the run's resolution to %+v", resolved)
	}
}

// TestRuntimeToolchainProvisioningReportSkipsOtherRuns pins the scope of the
// report: a run whose commands hold no workspace-install probes nothing and
// logs nothing, and a canceled run neither.
func TestRuntimeToolchainProvisioningReportSkipsOtherRuns(t *testing.T) {
	spectest.Proves(t, "cli/runtime-toolchain-pinning", "provisioning-reports-the-installed-toolchain",
		"a-run-without-workspace-install-reports-nothing")
	root := t.TempDir()
	t.Setenv("PATH", t.TempDir())
	writeRuntimeToolchainLock(t, root, "compiler", "1.2.3", "integrity-a")
	logs := captureRuntimeToolchainLogs(t)
	ext := resolveForProvisioning(t, root)

	ReportProvisionedRuntimeToolchains(context.Background(), root, []*model.ExtensionDescription{ext}, nil, []string{"build"})
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	ReportProvisionedRuntimeToolchains(canceled, root, []*model.ExtensionDescription{ext}, nil, []string{ToolchainProvisioningCommand})
	if got := logs.String(); got != "" {
		t.Fatalf("report outside a provisioning run logged %q", got)
	}
}

// TestRuntimeToolchainCommandSetsKeepASharedAliasStrict pins the split: an
// alias only the provisioning command reaches is lenient, and one another
// active command also reaches is required.
func TestRuntimeToolchainCommandSetsKeepASharedAliasStrict(t *testing.T) {
	ext := provisioningToolchainExtension()
	ext.Runtime.Toolchains["installer"] = runtimeToolchainFixture("installer")
	ext.Tasks["install"] = model.TaskDefinition{Toolchains: []string{"installer"}}

	required, provisioning := commandRuntimeToolchainRefSets(ext, map[string]bool{ToolchainProvisioningCommand: true})
	if len(required) != 0 || strings.Join(provisioning, ",") != "compiler,installer" {
		t.Fatalf("provisioning alone: required %v, provisioning %v", required, provisioning)
	}
	required, provisioning = commandRuntimeToolchainRefSets(ext, map[string]bool{ToolchainProvisioningCommand: true, "build": true})
	if strings.Join(required, ",") != "compiler" || strings.Join(provisioning, ",") != "installer" {
		t.Fatalf("provisioning with build: required %v, provisioning %v", required, provisioning)
	}
	if got := commandRuntimeToolchainRefs(ext, map[string]bool{ToolchainProvisioningCommand: true, "build": true}); strings.Join(got, ",") != "compiler,installer" {
		t.Fatalf("all refs = %v, want compiler,installer", got)
	}
}

// TestEnsureJobRuntimeToolchainsKeepsAProvisioningResolutionFromOtherCommands
// pins the execution backstop: a job of another command never runs on the
// unavailable identity the provisioning command tolerated while a lock exists,
// a provisioning job resolves what is missing leniently, and without a lock the
// bootstrap floor still lets every job run.
func TestEnsureJobRuntimeToolchainsKeepsAProvisioningResolutionFromOtherCommands(t *testing.T) {
	root := t.TempDir()
	writeRuntimeToolchainLock(t, root, "other", "1.2.3", "integrity-a")
	t.Setenv("PATH", t.TempDir())
	project := &workspace.Project{ID: "/app", Name: "app", Path: "app"}
	ws := workspace.NewWorkspace(root, nil, []*workspace.Project{project})
	job := func(ext *model.ExtensionDescription, name string) *jobs.ScheduledJob {
		return &jobs.ScheduledJob{Project: project, Extension: ext, JobDef: &model.JobDefinition{Name: name, Toolchains: []string{"compiler"}}}
	}

	ext := provisioningToolchainExtension()
	if err := EnsureJobRuntimeToolchains(ws, job(ext, ToolchainProvisioningCommand+"~install")); err != nil {
		t.Fatalf("provisioning job backstop: %v", err)
	}
	if resolved, ok := ext.RuntimeToolchains["compiler"]; !ok || resolved.Available {
		t.Fatalf("provisioning job resolution = %+v (present %v), want unavailable", resolved, ok)
	}
	err := EnsureJobRuntimeToolchains(ws, job(ext, "build~compile"))
	want := `resolve extension "example" runtime toolchain "compiler": workspace lock has no usable "compiler" pin; only workspace-fetch and workspace-install run without one`
	if err == nil || err.Error() != want {
		t.Fatalf("build job on a provisioning resolution: error = %v, want %q", err, want)
	}

	optional := provisioningToolchainExtension()
	requirement := optional.Runtime.Toolchains["compiler"]
	requirement.Optional = true
	optional.Runtime.Toolchains["compiler"] = requirement
	if err := EnsureJobRuntimeToolchains(ws, job(optional, "build~compile")); err != nil {
		t.Fatalf("an optional toolchain resolved unavailable must not stop the job: %v", err)
	}

	if err := os.Remove(filepath.Join(root, lockfile.LockFilename)); err != nil {
		t.Fatal(err)
	}
	if err := EnsureJobRuntimeToolchains(ws, job(ext, "build~compile")); err != nil {
		t.Fatalf("without a lock the unavailable identity is the bootstrap floor: %v", err)
	}
	spectest.Proves(t, "cli/runtime-toolchain-pinning", "provisioning-runs-unpinned",
		"another-command-never-runs-on-a-provisioning-resolution")
}

// TestFirstPartyWorkspaceInstallRunsBeforeTheLockPinsItsToolchain replays the
// fresh `putnami init` state on the shipped manifests: extensions install has
// written a lock and nothing has pinned a toolchain yet. workspace-install must
// resolve, and the commands the lock pin protects must still fail closed.
func TestFirstPartyWorkspaceInstallRunsBeforeTheLockPinsItsToolchain(t *testing.T) {
	repoRoot := findJobsRepoRoot(t)
	cases := []struct {
		dir, name, lock string
	}{
		{dir: filepath.Join("go", "extension"), name: "@putnami/go", lock: "go"},
		{dir: filepath.Join("typescript", "extension"), name: "@putnami/typescript", lock: "bun"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := lockfile.WriteLockFile(root, lockfile.NewLockFile()); err != nil {
				t.Fatal(err)
			}
			load := func() *model.ExtensionDescription {
				t.Helper()
				ext := extension.LoadExtensionFromDir(filepath.Join(repoRoot, tc.dir), tc.name)
				if ext == nil || ext.Runtime == nil || len(ext.Runtime.RunToolchains) == 0 {
					t.Fatalf("%s: the manifest no longer runs its tasks with a toolchain; this test guards nothing", tc.dir)
				}
				return ext
			}
			exts := func(ext *model.ExtensionDescription) []*model.ExtensionDescription {
				return []*model.ExtensionDescription{ext}
			}

			install := load()
			active := CommandDependencyClosure(exts(install), []string{ToolchainProvisioningCommand})
			if err := resolveRuntimeToolchainsForCommands(root, exts(install), active, os.Environ()); err != nil {
				t.Fatalf("workspace-install on a lock with no %q pin: %v", tc.lock, err)
			}

			build := load()
			active = CommandDependencyClosure(exts(build), []string{"build"})
			err := resolveRuntimeToolchainsForCommands(root, exts(build), active, os.Environ())
			if want := `workspace lock has no exact "` + tc.lock + `" pin`; err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("build on a lock with no %q pin: error = %v, want it to contain %q", tc.lock, err, want)
			}

			// A run's synchronization stage defers the missing pin: a workspace
			// with the extension and no project of its language plans no job
			// that needs it.
			deferred := load()
			if err := ResolveRuntimeToolchainsForCommandsMode(context.Background(), root, exts(deferred), active, os.Environ(), ResolveDeferred); err != nil {
				t.Fatalf("build synchronization on a lock with no %q pin: %v", tc.lock, err)
			}
			if resolved, ok := deferred.RuntimeToolchains["runtimeCompiler"]; ok {
				t.Fatalf("deferred resolution recorded %+v, want the alias left unresolved", resolved)
			}
		})
	}
}

// TestPrepareToolchainsResolveUnpinnedOnlyInAProvisioningRun pins how a
// local-source extension is prepared on a lock that does not pin its compiler:
// a run of only workspace-install prepares it with the unavailable identity,
// and a run with any other active command refuses the missing pin.
func TestPrepareToolchainsResolveUnpinnedOnlyInAProvisioningRun(t *testing.T) {
	root := t.TempDir()
	writeRuntimeToolchainLock(t, root, "other", "1.2.3", "integrity-a")
	prepared := func() *model.ExtensionDescription {
		ext := runtimeToolchainExtension(runtimeToolchainFixture("compiler"))
		ext.LocalSource = true
		ext.Runtime.Prepare = &extensionproto.RuntimePrepare{Toolchains: []string{"compiler"}}
		return ext
	}

	provisioning := map[string]bool{ToolchainProvisioningCommand: true}
	if !ProvisioningOnlyRun(provisioning) {
		t.Fatalf("a run of only %s is not a provisioning run", ToolchainProvisioningCommand)
	}
	ext := prepared()
	if err := ResolveRuntimeToolchainsForPreparation(context.Background(), root,
		[]*model.ExtensionDescription{ext}, nil, ProvisioningOnlyRun(provisioning)); err != nil {
		t.Fatalf("a provisioning run refused to prepare without a pin: %v", err)
	}
	if resolved, ok := ext.RuntimeToolchains["compiler"]; !ok || resolved.Available || resolved.Identity == "" {
		t.Fatalf("prepare resolution = %+v (present %v), want the unavailable identity", resolved, ok)
	}

	for _, active := range []map[string]bool{
		{},
		{"build": true},
		{"build": true, ToolchainProvisioningCommand: true},
	} {
		if ProvisioningOnlyRun(active) {
			t.Fatalf("commands %v: counted as a provisioning run", active)
		}
		err := ResolveRuntimeToolchainsForPreparation(context.Background(), root,
			[]*model.ExtensionDescription{prepared()}, nil, ProvisioningOnlyRun(active))
		if want := `workspace lock has no exact "compiler" pin`; err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("commands %v: error = %v, want it to contain %q", active, err, want)
		}
	}
	spectest.Proves(t, "cli/runtime-toolchain-pinning", "provisioning-runs-unpinned",
		"a-local-extension-prepares-unpinned-only-in-a-provisioning-run")
}

// TestTheFetchProvisionsLikeTheInstall pins the fetch's share of the
// provisioning exception: a hosted run fetches before the installers run, so
// the fetch resolves an unusable pin unavailable and prepares its runtime
// without one, as the install does. A fetch alone installs no toolchain, so
// its run reports nothing about one.
func TestTheFetchProvisionsLikeTheInstall(t *testing.T) {
	spectest.Proves(t, "cli/runtime-toolchain-pinning", "workspace-installed-candidates", "the-fetch-provisions-like-the-install")
	fetch := extensionproto.WorkspaceFetchCommand
	root := t.TempDir()
	writeRuntimeToolchainLock(t, root, "compiler", "1.2.3", "integrity-a")
	t.Setenv("PATH", t.TempDir())
	ext := provisioningToolchainExtension()
	ext.Tasks["fetch"] = model.TaskDefinition{}
	ext.Jobs[fetch] = &model.JobDefinition{PipelineSteps: []extensionproto.PipelineStep{{ID: "fetch", Task: "fetch"}}}

	if err := resolveRuntimeToolchainsForCommands(root, []*model.ExtensionDescription{ext},
		map[string]bool{fetch: true}, os.Environ()); err != nil {
		t.Fatalf("%s refused a lock the install after it repairs: %v", fetch, err)
	}
	if resolved, ok := ext.RuntimeToolchains["compiler"]; !ok || resolved.Available || resolved.Identity == "" {
		t.Fatalf("fetch resolution = %+v (present %v), want the unavailable identity", resolved, ok)
	}
	for _, active := range []map[string]bool{
		{fetch: true},
		{fetch: true, ToolchainProvisioningCommand: true},
	} {
		if !ProvisioningOnlyRun(active) {
			t.Errorf("commands %v: not a provisioning run", active)
		}
	}
	if ProvisioningOnlyRun(map[string]bool{fetch: true, "build": true}) {
		t.Error("a run with build counted as a provisioning run")
	}

	logs := captureRuntimeToolchainLogs(t)
	ReportProvisionedRuntimeToolchains(context.Background(), root, []*model.ExtensionDescription{ext}, nil, []string{fetch})
	if got := logs.String(); got != "" {
		t.Fatalf("a fetch run reported %q", got)
	}
}
