package toolchainlock

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	runtimeproto "go.putnami.dev/protocol/runtime"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/cli"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
	"go.putnami.dev/tooling/cli/internal/commands/lifecycle"
	"go.putnami.dev/tooling/cli/internal/hometest"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// A fresh `putnami init` leaves a lock that the extensions install wrote and
// nothing has pinned a toolchain in yet: the pin derives from files the
// workspace-install job writes, and the refresh that records it runs after that
// job. `putnami install` must get through workspace-install on that lock, while
// every other command stays stopped until the pin exists, and runs once it does.
// The chain is the real one: Install → DepsInstall → RunWorkspaceJob →
// Engine.Run → runtime synchronization → the provider's task.
func TestInstall_RunsWorkspaceInstallBeforeTheLockPinsItsToolchain(t *testing.T) {
	spectest.Proves(t, "cli/runtime-toolchain-pinning", "provisioning-runs-unpinned",
		"install-runs-on-a-lock-that-does-not-pin-the-toolchain-yet")
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

	const goVersion = "1.26.1"
	tools := t.TempDir()
	writeFixtureFile(t, filepath.Join(tools, "fixture-go"),
		"#!/bin/sh\n[ \"$1\" = env ] && [ \"$2\" = GOVERSION ] && { printf 'go%s\\n' '"+goVersion+"'; exit 0; }\nexit 1\n", 0o755)
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))

	markers := writeRunToolchainExtensionFixture(t, wsRoot)
	if err := lockfile.WriteLockFile(wsRoot, lockfile.NewLockFile()); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	var err error
	streams := clitest.CaptureStdoutStderr(t, func() {
		err = lifecycle.Install(context.Background(), wsRoot, wsproto.Load(wsRoot), nil,
			lifecycle.LifecycleEnv{Out: &out, RunJob: cli.RunWorkspaceJob})
	})
	if err != nil {
		t.Fatalf("putnami install on a lock with no go pin: %v\n%s%s", err, out.String(), streams)
	}
	if runs := markerRuns(t, markers.install); runs != 1 {
		t.Fatalf("the workspace-install task ran %d times, want 1\n%s%s", runs, out.String(), streams)
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

	result, transcript := build()
	if result.Outcome != lifecycle.WorkspaceJobFailed || !strings.Contains(transcript, `workspace lock has no exact "go" pin`) {
		t.Fatalf("build without a go pin: outcome %v, want a failure naming the missing pin\n%s", result.Outcome, transcript)
	}
	if runs := markerRuns(t, markers.build); runs != 0 {
		t.Fatalf("the build task ran %d times without a go pin, want 0", runs)
	}

	pinned := lockfile.NewLockFile()
	pinned.SetToolchain("go", lockfile.LockEntry{
		Version:     goVersion,
		Integrities: map[string]string{lockfile.PlatformKey(runtime.GOOS, runtime.GOARCH): strings.Repeat("a", 64)},
		Source:      "https://go.dev/dl/",
	})
	if err := lockfile.WriteLockFile(wsRoot, pinned); err != nil {
		t.Fatal(err)
	}
	result, transcript = build()
	if result.Outcome != lifecycle.WorkspaceJobOK || markerRuns(t, markers.build) != 1 {
		t.Fatalf("build with the go pin: outcome %v, build runs %d, want one successful run\n%s",
			result.Outcome, markerRuns(t, markers.build), transcript)
	}
}

// A workspace that declares an extension and has no project of its language,
// such as `putnami init --extension go` before its first Go project, has no
// pin for the extension's toolchain: the pin derives from the files that
// project brings. A build plans no job of the extension, so the missing pin
// must not stop it. Once a project uses the extension, the build fails
// naming the missing pin and the command that writes it, before the task runs.
// The chain is the real one: RunWorkspaceJob → Engine.Run → runtime
// synchronization → planning → the planned-toolchain check.
func TestBuild_ALockWithoutAPinStopsOnlyAPlannedJob(t *testing.T) {
	spectest.Proves(t, "cli/runtime-toolchain-pinning", "unplanned-toolchains-do-not-stop-the-run",
		"a-build-with-no-project-of-the-extension-runs-without-its-pin")
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
	// The build activates for a project that uses the extension, as a
	// language extension's build does, not for every project.
	activation := `"description": "Build the workspace.",
      "activation": "workspace",`
	if !strings.Contains(string(data), activation) {
		t.Fatal("the fixture manifest no longer activates its build for the workspace; this test guards nothing")
	}
	writeFixtureFile(t, manifest, strings.Replace(string(data), activation, `"description": "Build the workspace.",`, 1), 0o644)
	if err := lockfile.WriteLockFile(wsRoot, lockfile.NewLockFile()); err != nil {
		t.Fatal(err)
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

	result, transcript := build()
	if result.Outcome != lifecycle.WorkspaceJobNoMatches || strings.Contains(transcript, "no exact") {
		t.Fatalf("build with no project of the extension: outcome %v, want no matched project and no pin failure\n%s",
			result.Outcome, transcript)
	}

	writeFixtureFile(t, filepath.Join(wsRoot, "app", "putnami.json"), `{"name":"app","extensions":["@putnami/compiled"]}`, 0o644)
	result, transcript = build()
	want := `workspace lock has no exact "go" pin: add a project that declares it, or run ` + "`putnami install`" + ` to record the declared one`
	if result.Outcome != lifecycle.WorkspaceJobFailed || !strings.Contains(transcript, want) {
		t.Fatalf("build of a project that uses the extension, without a go pin: outcome %v, want a failure containing %q\n%s",
			result.Outcome, want, transcript)
	}
	if runs := markerRuns(t, markers.build); runs != 0 {
		t.Fatalf("the build task ran %d times without a go pin, want 0", runs)
	}
}

// A local-source extension whose prepare step compiles with the locked
// toolchain is prepared by `putnami install` before the lock pins it: the run's
// only command is workspace-install. A build on the same lock still fails on
// the missing pin before any task runs.
func TestInstall_PreparesALocalExtensionBeforeTheLockPinsItsCompiler(t *testing.T) {
	spectest.Proves(t, "cli/runtime-toolchain-pinning", "provisioning-runs-unpinned",
		"a-local-extension-prepares-unpinned-only-in-a-provisioning-run")
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
	inputs := `"inputs": ["bin/prepare", "bin/runtime-template"]`
	if !strings.Contains(string(data), inputs) {
		t.Fatal("the fixture manifest no longer declares its prepare inputs; this test guards nothing")
	}
	data = []byte(strings.Replace(string(data), inputs, inputs+`,
      "toolchains": ["compiler"]`, 1))
	writeFixtureFile(t, manifest, string(data), 0o644)
	if err := lockfile.WriteLockFile(wsRoot, lockfile.NewLockFile()); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	streams := clitest.CaptureStdoutStderr(t, func() {
		err = lifecycle.Install(context.Background(), wsRoot, wsproto.Load(wsRoot), nil,
			lifecycle.LifecycleEnv{Out: &out, RunJob: cli.RunWorkspaceJob})
	})
	if err != nil {
		t.Fatalf("putnami install of a local extension on a lock with no go pin: %v\n%s%s", err, out.String(), streams)
	}
	if runs := markerRuns(t, markers.install); runs != 1 {
		t.Fatalf("the workspace-install task ran %d times, want 1\n%s%s", runs, out.String(), streams)
	}

	workspace.InvalidateLoadCache(wsRoot)
	var result lifecycle.WorkspaceJobResult
	var buildOut strings.Builder
	streams = clitest.CaptureStdoutStderr(t, func() {
		result, err = cli.RunWorkspaceJob(context.Background(), lifecycle.WorkspaceJobRequest{
			WorkspaceRoot: wsRoot, Config: wsproto.Load(wsRoot), Job: "build", Out: &buildOut,
		})
	})
	transcript := buildOut.String() + streams
	if err == nil && (result.Outcome != lifecycle.WorkspaceJobFailed || !strings.Contains(transcript, `workspace lock has no exact "go" pin`)) {
		t.Fatalf("build without a go pin: outcome %v, want a failure naming the missing pin\n%s", result.Outcome, transcript)
	}
	if err != nil && !strings.Contains(err.Error()+transcript, `workspace lock has no exact "go" pin`) {
		t.Fatalf("build without a go pin: %v, want a failure naming the missing pin\n%s", err, transcript)
	}
	if runs := markerRuns(t, markers.build); runs != 0 {
		t.Fatalf("the build task ran %d times without a go pin, want 0", runs)
	}
}

type runToolchainMarkers struct {
	install, build string
}

// writeRunToolchainExtensionFixture writes a workspace whose only extension is
// a local source that runs every task with a required toolchain locked as
// "go", and offers workspace-install and build. Its prepare step declares no
// toolchain, as an installed extension's runtime needs none.
func writeRunToolchainExtensionFixture(t *testing.T, wsRoot string) runToolchainMarkers {
	t.Helper()
	markers := runToolchainMarkers{
		install: filepath.Join(wsRoot, "install-runs.txt"),
		build:   filepath.Join(wsRoot, "build-runs.txt"),
	}
	extRoot := filepath.Join(wsRoot, "compiled-extension")
	writeFixtureFile(t, filepath.Join(wsRoot, wsproto.WorkspaceConfigFilename),
		`{"name":"toolchain-ws","includes":["compiled-extension","app"]}`, 0o644)
	writeFixtureFile(t, filepath.Join(wsRoot, "app", "putnami.json"), `{"name":"app"}`, 0o644)
	writeFixtureFile(t, filepath.Join(extRoot, "putnami.json"), `{"name":"@putnami/compiled"}`, 0o644)

	info := fmt.Sprintf(
		`{"extension":"@putnami/compiled","version":"0.1.0","platform":%q,"cliContract":%d,"runtimeProtocol":%d,"runtimeABI":%d}`,
		runtime.GOOS+"/"+runtime.GOARCH, protocolcli.CurrentContract,
		runtimeproto.MaxKnownProtocolVersion, runtimeproto.RuntimeABIVersion)
	writeFixtureFile(t, filepath.Join(extRoot, "bin", "runtime-template"), "#!/bin/sh\n"+
		"if [ \"$1\" = \"__putnami\" ] && [ \"$2\" = \"runtime-info\" ]; then\n"+
		"  printf '%s\\n' '"+info+"'\n"+
		"  exit 0\n"+
		"fi\n"+
		"if [ \"$1\" = \"install\" ]; then\n"+
		"  printf 'ran\\n' >> '"+markers.install+"'\n"+
		"fi\n"+
		"if [ \"$1\" = \"build\" ]; then\n"+
		"  printf 'ran\\n' >> '"+markers.build+"'\n"+
		"fi\n"+
		"exit 0\n", 0o755)
	writeFixtureFile(t, filepath.Join(extRoot, "bin", "prepare"), "#!/bin/sh\nset -eu\n"+
		"output=\"$2\"\n"+
		"mkdir -p \"$output/bin\"\n"+
		"cp \"$(dirname \"$0\")/runtime-template\" \"$output/bin/runtime\"\n"+
		"chmod +x \"$output/bin/runtime\"\n", 0o755)

	writeFixtureFile(t, filepath.Join(extRoot, "putnami.extension.json"), `{
  "name": "@putnami/compiled",
  "version": "0.1.0",
  "cliContract": `+fmt.Sprint(protocolcli.CurrentContract)+`,
  "runtime": {
    "executable": "bin/runtime",
    "toolchains": {
      "compiler": {
        "lock": "go",
        "candidates": [{"from": "path", "path": "fixture-go"}],
        "probe": {"args": ["env", "GOVERSION"], "expect": "go{version}"}
      }
    },
    "runToolchains": ["compiler"],
    "prepare": {
      "command": "{extensionRoot}/bin/prepare",
      "args": ["--output", "{runtimeOutput}"],
      "inputs": ["bin/prepare", "bin/runtime-template"]
    }
  },
  "commands": {
    "workspace-install": {
      "description": "Install workspace dependencies.",
      "activation": "workspace",
      "run": [{ "id": "install", "task": "install-task" }]
    },
    "build": {
      "description": "Build the workspace.",
      "activation": "workspace",
      "run": [{ "id": "compile", "task": "build-task" }]
    }
  },
  "tasks": {
    "install-task": {
      "kind": "command",
      "command": "{extensionRuntime}",
      "args": ["install"],
      "cache": false,
      "timeoutMs": 10000
    },
    "build-task": {
      "kind": "command",
      "command": "{extensionRuntime}",
      "args": ["build"],
      "cache": false,
      "timeoutMs": 10000
    }
  }
}`, 0o644)
	workspace.InvalidateLoadCache(wsRoot)
	return markers
}
