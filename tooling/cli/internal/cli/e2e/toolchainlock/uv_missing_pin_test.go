package toolchainlock

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/uvtoolchain"
	"go.putnami.dev/tooling/cli/internal/cli"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
	"go.putnami.dev/tooling/cli/internal/commands/lifecycle"
	"go.putnami.dev/tooling/cli/internal/hometest"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// In a workspace whose extension runs uv, a root pyproject.toml whose
// [tool.uv] required-version names no release Putnami can pin leaves the lock
// without a uv pin after `putnami install`. A task that needs uv then fails
// before it runs, naming the declaration to write and then `putnami install`:
// an install alone would pin nothing. Without a [tool.uv] declaration, the
// missing pin gets the error every missing pin gets. The chain is the real
// one: Install → the toolchain pins → DepsInstall → the lock refresh, then
// RunWorkspaceJob → Engine.Run → runtime synchronization → the toolchain check.
func TestBuild_AMissingUvPinNamesTheDeclarationToWrite(t *testing.T) {
	spectest.Proves(t, "cli/runtime-toolchain-pinning", "missing-uv-pin-names-the-declaration",
		"a-missing-uv-pin-names-the-declaration-to-write")
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

	markers := writeRunToolchainExtensionFixture(t, wsRoot)
	manifest := filepath.Join(wsRoot, "compiled-extension", "putnami.extension.json")
	data, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	// The runtime toolchain resolves the uv lock identity, as the
	// @putnami/python runtime does.
	goLock := `"lock": "go",`
	if !strings.Contains(string(data), goLock) {
		t.Fatal("the fixture manifest no longer locks its toolchain on go; this test guards nothing")
	}
	writeFixtureFile(t, manifest, strings.Replace(string(data), goLock, `"lock": "uv",`, 1), 0o644)
	pyproject := filepath.Join(wsRoot, uvtoolchain.Pyproject)
	writeFixtureFile(t, pyproject, "[tool.uv]\nrequired-version = \">=0.5\"\n", 0o644)
	if err := lockfile.WriteLockFile(wsRoot, lockfile.NewLockFile()); err != nil {
		t.Fatal(err)
	}
	workspace.InvalidateLoadCache(wsRoot)

	var out strings.Builder
	streams := clitest.CaptureStdoutStderr(t, func() {
		err = lifecycle.Install(context.Background(), wsRoot, wsproto.Load(wsRoot), nil,
			lifecycle.LifecycleEnv{Out: &out, RunJob: cli.RunWorkspaceJob})
	})
	if err != nil {
		t.Fatalf("putnami install with a uv range: %v\n%s%s", err, out.String(), streams)
	}
	if runs := markerRuns(t, markers.install); runs != 1 {
		t.Fatalf("the workspace-install task ran %d times, want 1\n%s%s", runs, out.String(), streams)
	}
	locked, err := lockfile.ReadLockFile(wsRoot)
	if err != nil || locked == nil {
		t.Fatalf("read lock: %v", err)
	}
	if entry, ok := locked.GetToolchain("uv"); ok {
		t.Fatalf("putnami install pinned uv %+v from a range", entry)
	}

	build := func() (lifecycle.WorkspaceJobResult, string) {
		t.Helper()
		workspace.InvalidateLoadCache(wsRoot)
		var result lifecycle.WorkspaceJobResult
		var runErr error
		var buildOut strings.Builder
		streams := clitest.CaptureStdoutStderr(t, func() {
			result, runErr = cli.RunWorkspaceJob(context.Background(), lifecycle.WorkspaceJobRequest{
				WorkspaceRoot: wsRoot, Config: wsproto.Load(wsRoot), Job: "build", Out: &buildOut,
			})
		})
		if runErr != nil {
			t.Fatalf("build: %v\n%s%s", runErr, buildOut.String(), streams)
		}
		return result, buildOut.String() + streams
	}
	const missing = `workspace lock has no exact "uv" pin: `
	const install = ", then run `putnami install`"
	for _, tc := range []struct {
		declaration, want string
	}{
		{"[tool.uv]\nrequired-version = \">=0.5\"\n",
			missing + `pyproject.toml [tool.uv] required-version is ">=0.5", which names no single uv release: ` +
				`declare one exact release, such as required-version = "==` + uvtoolchain.DefaultVersion + `", so Putnami can pin it` + install},
		{"[tool.uv]\nrequired-version = \"==0.10.4\"\n",
			missing + `pyproject.toml [tool.uv] required-version declares a release Putnami cannot run: `},
		{"",
			missing + "add a project that declares it, or run `putnami install` to record the declared one"},
	} {
		if tc.declaration == "" {
			if err := os.Remove(pyproject); err != nil {
				t.Fatal(err)
			}
		} else {
			writeFixtureFile(t, pyproject, tc.declaration, 0o644)
		}
		result, transcript := build()
		if result.Outcome != lifecycle.WorkspaceJobFailed || !strings.Contains(transcript, tc.want) {
			t.Fatalf("build without a uv pin, declaring %q: outcome %v, want a failure containing %q\n%s",
				tc.declaration, result.Outcome, tc.want, transcript)
		}
		if tc.declaration != "" && !strings.Contains(transcript, install) {
			t.Fatalf("build without a uv pin, declaring %q: the failure does not name `putnami install`\n%s", tc.declaration, transcript)
		}
		if runs := markerRuns(t, markers.build); runs != 0 {
			t.Fatalf("the build task ran %d times without a uv pin, want 0", runs)
		}
	}
}
