package jobs

import (
	"path/filepath"
	"strings"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

// A hosted run resolves its toolchains before the fetch jobs run, so a
// candidate a repository commits inside the workspace would run next to the
// credential. The run rejects it without starting it and takes the next
// candidate outside the workspace. Without the run credential, the workspace
// candidate still wins.
func TestHostedRunProbesNoToolchainInsideTheWorkspace(t *testing.T) {
	spectest.Proves(t, "cli/credential-custody", "hostile-process-finds-nothing", "hosted-run-probes-no-workspace-toolchain")
	workspaceRoot := t.TempDir()
	record := filepath.Join(t.TempDir(), "runs.jsonl")
	committed := fixtureproc.Write(t,
		filepath.Join(workspaceRoot, ".putnami", "extensions", "example", "compiler-1.2.3", "bin", "compiler"),
		fixtureproc.Program{Record: record, Stdout: "1.2.3\n"})
	bin := t.TempDir()
	host := writeRuntimeTool(t, filepath.Join(bin, "compiler"), "1.2.3")
	writeRuntimeToolchainLock(t, workspaceRoot, "compiler", "1.2.3", "integrity-a")
	requirement := runtimeToolchainFixture("compiler")
	requirement.Candidates = []extensionproto.RuntimeToolchainCandidate{
		{From: extensionproto.RuntimeToolchainCandidateEnvironment, Environment: runtimeToolchainWorkspaceRootEnv, Path: ".putnami/extensions/example/compiler-{version}/bin/compiler"},
		{From: extensionproto.RuntimeToolchainCandidatePath, Path: "compiler"},
	}
	base := []string{"PATH=" + bin}
	resolve := func(bearer string) string {
		t.Helper()
		restore := runcredential.SetForTest(bearer)
		defer restore()
		ext := runtimeToolchainExtension(requirement)
		if err := resolveRuntimeToolchains(workspaceRoot, ext, []string{"compiler"}, base); err != nil {
			t.Fatal(err)
		}
		return ext.RuntimeToolchains["compiler"].Executable
	}

	if got, want := resolve("run-bearer"), mustEvalSymlinks(t, host); got != want {
		t.Fatalf("hosted resolution = %s, want the host's %s", got, want)
	}
	if runs := fixtureproc.Runs(t, record); len(runs) != 0 {
		t.Fatalf("the hosted run started the workspace candidate %d times", len(runs))
	}
	workspaceOnly := requirement
	workspaceOnly.Candidates = requirement.Candidates[:1]
	restore := runcredential.SetForTest("run-bearer")
	_, rejections, found := resolveRuntimeToolchain(t.Context(), workspaceRoot, workspaceOnly, "1.2.3", "integrity-a", base)
	restore()
	if found || len(rejections) != 1 || !strings.Contains(rejections[0].reason, "inside the workspace") {
		t.Fatalf("rejections = %+v, want the workspace candidate rejected as inside the workspace", rejections)
	}

	if got, want := resolve(""), mustEvalSymlinks(t, committed); got != want {
		t.Fatalf("resolution without the flag = %s, want the workspace's %s", got, want)
	}
}

// The hosted guard places a relative path, such as one a relative PATH entry
// yields, against the engine's working directory, as the probe starts it.
func TestHostedWorkspaceProgramPlacesRelativePaths(t *testing.T) {
	workspaceRoot := mustEvalSymlinks(t, t.TempDir())
	outside := mustEvalSymlinks(t, t.TempDir())
	t.Chdir(workspaceRoot)
	restore := runcredential.SetForTest("run-bearer")
	defer restore()
	inside := hostedWorkspaceProgram(workspaceRoot)
	for path, want := range map[string]bool{
		filepath.Join("bin", "compiler"):                true,
		filepath.Join(workspaceRoot, "bin", "compiler"): true,
		workspaceRoot: true,
		filepath.Join(outside, "bin", "compiler"):         false,
		filepath.Join("..", filepath.Base(outside), "go"): false,
	} {
		if got := inside(path); got != want {
			t.Errorf("inside(%q) = %v, want %v", path, got, want)
		}
	}
}

func mustEvalSymlinks(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}
