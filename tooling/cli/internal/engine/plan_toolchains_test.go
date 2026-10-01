package engine

import (
	"strings"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// TestBuildPlan_FailsOnlyAPlannedJobWithAnUnresolvedToolchain pins where the
// planned-toolchain check runs: planning itself fails when a planned job requires a
// toolchain the lock does not pin, before the plan reaches any key or job, and
// a plan with no such job is returned. Not parallel: captureStderr swaps the
// process-wide os.Stderr.
func TestBuildPlan_FailsOnlyAPlannedJobWithAnUnresolvedToolchain(t *testing.T) {
	root := t.TempDir()
	locked := lockfile.NewLockFile()
	locked.SetToolchain("other", lockfile.LockEntry{Version: "1.2.3"})
	if err := lockfile.WriteLockFile(root, locked); err != nil {
		t.Fatal(err)
	}
	goExt := &extension.ExtensionDescription{
		Name: "@putnami/go",
		Runtime: &extensionproto.RuntimeDefinition{
			Executable: "compiled/go",
			Toolchains: map[string]extensionproto.RuntimeToolchain{"compiler": {
				Lock:       "go",
				Candidates: []extensionproto.RuntimeToolchainCandidate{{From: extensionproto.RuntimeToolchainCandidatePath, Path: "go"}},
				Probe:      extensionproto.RuntimeToolchainProbe{Args: []string{"version"}, Expect: "{version}"},
			}},
			RunToolchains: []string{"compiler"},
		},
		Jobs: map[string]*extension.JobDefinition{
			"build": {Name: "build", ExtensionName: "@putnami/go", Command: "/bin/true", Toolchains: []string{"compiler"}},
		},
	}
	plain := &extension.ExtensionDescription{
		Name: "@putnami/plain",
		Jobs: map[string]*extension.JobDefinition{
			"build": {Name: "build", ExtensionName: "@putnami/plain", Command: "/bin/true"},
		},
	}
	exts := []*extension.ExtensionDescription{goExt, plain}
	plan := func(uses string) ([]*jobs.ScheduledJob, int, string) {
		t.Helper()
		proj := &workspace.Project{ID: "/app", Name: "app", Path: "app", Extensions: []string{uses}}
		ws := workspace.NewWorkspace(root, nil, []*workspace.Project{proj})
		req := &Request{WorkspaceRoot: root, Commands: []string{"build"}}
		var planned []*jobs.ScheduledJob
		var code int
		stderr := captureStderr(t, func() {
			planned, code = buildPlan(req, ws, []*workspace.Project{proj}, exts, &extension.DiscoveryResult{Extensions: exts})
		})
		return planned, code, stderr
	}

	planned, code, stderr := plan("@putnami/plain")
	if code != ExitSuccess || len(planned) != 1 || planned[0].Extension != plain {
		t.Fatalf("a plan with no job of @putnami/go: code %d, %d job(s)\n%s", code, len(planned), stderr)
	}
	if _, resolved := goExt.RuntimeToolchains["compiler"]; resolved {
		t.Fatal("planning resolved a toolchain no planned job requires")
	}

	planned, code, stderr = plan("@putnami/go")
	want := `resolve extension "@putnami/go" runtime toolchain "compiler": workspace lock has no exact "go" pin: ` +
		"add a project that declares it, or run `putnami install` to record the declared one"
	if code != ExitError || planned != nil || !strings.Contains(stderr, want) {
		t.Fatalf("a plan with a @putnami/go job: code %d, %d job(s), stderr %q; want ExitError naming %q", code, len(planned), stderr, want)
	}
	spectest.Proves(t, "cli/runtime-toolchain-pinning", "unplanned-toolchains-do-not-stop-the-run",
		"planning-fails-before-any-key-when-a-planned-job-needs-an-unresolved-toolchain")
}
