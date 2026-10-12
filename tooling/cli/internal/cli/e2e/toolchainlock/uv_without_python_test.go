package toolchainlock

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/cli"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
	"go.putnami.dev/tooling/cli/internal/commands/lifecycle"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/hometest"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

// A workspace that never opted into Python may keep a root pyproject.toml for
// other tools. Without an extension that runs uv, its [tool.uv]
// required-version pins nothing, so a release Putnami refuses to pin fails
// neither `putnami install` nor the first-use install another command runs,
// and no uv pin is written. The chain is the real one: Install → the toolchain
// pins → DepsInstall → the provider's task → the lock refresh.
func TestInstall_PinsNoUvWithoutAnExtensionThatRunsUv(t *testing.T) {
	clitest.RequireShell(t)
	if runtime.GOOS == "windows" {
		t.Skip("shell runtime fixture")
	}
	wsRoot := t.TempDir()
	hometest.Temp(t)
	t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))
	t.Setenv("PUTNAMI_ARTIFACT_DIR", filepath.Join(t.TempDir(), "artifacts"))
	t.Setenv("PUTNAMI_WORKSPACE_BOOTSTRAPPED", "")
	t.Setenv("PUTNAMI_NO_AUTO_INSTALL", "")

	marker := writeCompiledExtensionFixture(t, wsRoot, false)
	writeFixtureFile(t, filepath.Join(wsRoot, "pyproject.toml"), "[tool.uv]\nrequired-version = \"==0.10.4\"\n", 0o644)
	if err := lockfile.WriteLockFile(wsRoot, lockfile.NewLockFile()); err != nil {
		t.Fatal(err)
	}

	for i, install := range []struct {
		name string
		ctx  context.Context
	}{
		{"putnami install", context.Background()},
		{"the first-use install", shared.WithImplicitInstall(context.Background())},
	} {
		var out strings.Builder
		var err error
		streams := clitest.CaptureStdoutStderr(t, func() {
			err = lifecycle.Install(install.ctx, wsRoot, wsproto.Load(wsRoot), nil,
				lifecycle.LifecycleEnv{Out: &out, RunJob: cli.RunWorkspaceJob})
		})
		if err != nil {
			t.Fatalf("%s with a uv 0.10.4 declaration and no extension that runs uv: %v\n%s%s", install.name, err, out.String(), streams)
		}
		if runs := markerRuns(t, marker); runs != i+1 {
			t.Fatalf("after %s the workspace-install task ran %d times, want %d", install.name, runs, i+1)
		}
		got, err := lockfile.ReadLockFile(wsRoot)
		if err != nil || got == nil {
			t.Fatalf("read lock: %v", err)
		}
		if entry, ok := got.GetToolchain("uv"); ok {
			t.Fatalf("%s pinned uv %+v in a workspace that runs no uv", install.name, entry)
		}
	}
}
