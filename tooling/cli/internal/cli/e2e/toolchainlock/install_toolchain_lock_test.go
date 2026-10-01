package toolchainlock

import (
	"context"
	"fmt"
	"maps"
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

// A workspace with no Go project still pins Go when a declared extension
// prepares its runtime with the pinned Go. An explicit install drives the whole
// chain — Install → DepsInstall → RunWorkspaceJob → Engine.Run → runtime
// preparation → the provider's task → the lock refresh — and must leave the pin
// the next command resolves. Once no declared extension resolves the pin, the
// refresh drops it.
func TestInstall_KeepsTheToolchainPinADeclaredExtensionPreparesWith(t *testing.T) {
	spectest.Proves(t, "cli/toolchain-lock", "extension-resolved-pins-survive", "install-keeps-a-pin-a-declared-extension-prepares-with")
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

	marker := writeCompiledExtensionFixture(t, wsRoot, true)
	pinned := lockfile.LockEntry{
		Version: goVersion,
		Integrities: map[string]string{
			lockfile.PlatformKey(runtime.GOOS, runtime.GOARCH): strings.Repeat("a", 64),
			"linux/amd64": strings.Repeat("b", 64),
		},
		Source: "https://go.dev/dl/",
	}
	locked := lockfile.NewLockFile()
	locked.SetToolchain("go", pinned)
	if err := lockfile.WriteLockFile(wsRoot, locked); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(wsRoot, "go.work")); !os.IsNotExist(err) {
		t.Fatalf("the fixture must declare no Go workspace: %v", err)
	}

	install := func(step string) {
		t.Helper()
		var out strings.Builder
		var err error
		streams := clitest.CaptureStdoutStderr(t, func() {
			err = lifecycle.Install(context.Background(), wsRoot, wsproto.Load(wsRoot), nil,
				lifecycle.LifecycleEnv{Out: &out, RunJob: cli.RunWorkspaceJob})
		})
		if err != nil {
			t.Fatalf("%s: putnami install: %v\n%s%s", step, err, out.String(), streams)
		}
	}
	goPin := func() (lockfile.LockEntry, bool) {
		t.Helper()
		got, err := lockfile.ReadLockFile(wsRoot)
		if err != nil || got == nil {
			t.Fatalf("read lock: %v", err)
		}
		return got.GetToolchain("go")
	}

	install("first install")
	entry, ok := goPin()
	if !ok || entry.Version != pinned.Version || entry.Source != pinned.Source || !maps.Equal(entry.Integrities, pinned.Integrities) {
		t.Fatalf("toolchains.go after install = %+v (present %v), want the committed pin %+v kept verbatim", entry, ok, pinned)
	}

	// The next command prepares the path-declared runtime again, which resolves
	// the go pin before anything runs.
	install("second install")
	if runs := markerRuns(t, marker); runs != 2 {
		t.Fatalf("the provider task ran %d times over two installs, want 2", runs)
	}

	// Without an extension that resolves it, the pin is a stale entry.
	writeCompiledExtensionFixture(t, wsRoot, false)
	install("install after the extension stopped resolving go")
	if entry, ok := goPin(); ok {
		t.Fatalf("toolchains.go = %+v survived a refresh that no declaration or extension resolves", entry)
	}
}

// writeCompiledExtensionFixture writes a workspace whose only extension is a
// local source that prepares its runtime and runs its workspace-install task
// through it. With withToolchain, the prepare step declares a toolchain locked
// as "go". It returns the marker the task appends to.
func writeCompiledExtensionFixture(t *testing.T, wsRoot string, withToolchain bool) string {
	t.Helper()
	marker := filepath.Join(wsRoot, "install-runs.txt")
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
		"  printf 'ran\\n' >> '"+marker+"'\n"+
		"fi\n"+
		"exit 0\n", 0o755)
	writeFixtureFile(t, filepath.Join(extRoot, "bin", "prepare"), "#!/bin/sh\nset -eu\n"+
		"output=\"$2\"\n"+
		"mkdir -p \"$output/bin\"\n"+
		"cp \"$(dirname \"$0\")/runtime-template\" \"$output/bin/runtime\"\n"+
		"chmod +x \"$output/bin/runtime\"\n", 0o755)

	toolchains, prepareToolchains := "", ""
	if withToolchain {
		toolchains = `,
    "toolchains": {
      "compiler": {
        "lock": "go",
        "candidates": [{"from": "path", "path": "fixture-go"}],
        "probe": {"args": ["env", "GOVERSION"], "expect": "go{version}"}
      }
    }`
		prepareToolchains = `,
      "toolchains": ["compiler"]`
	}
	writeFixtureFile(t, filepath.Join(extRoot, "putnami.extension.json"), `{
  "name": "@putnami/compiled",
  "version": "0.1.0",
  "cliContract": `+fmt.Sprint(protocolcli.CurrentContract)+`,
  "runtime": {
    "executable": "bin/runtime"`+toolchains+`,
    "prepare": {
      "command": "{extensionRoot}/bin/prepare",
      "args": ["--output", "{runtimeOutput}"],
      "inputs": ["bin/prepare", "bin/runtime-template"]`+prepareToolchains+`
    }
  },
  "commands": {
    "workspace-install": {
      "description": "Install workspace dependencies.",
      "activation": "workspace",
      "run": [{ "id": "install", "task": "install-task" }]
    }
  },
  "tasks": {
    "install-task": {
      "kind": "command",
      "command": "{extensionRuntime}",
      "args": ["install"],
      "cache": false,
      "timeoutMs": 10000
    }
  }
}`, 0o644)
	workspace.InvalidateLoadCache(wsRoot)
	return marker
}

// markerRuns counts how many times the fixture's install task executed.
func markerRuns(t *testing.T, marker string) int {
	t.Helper()
	data, err := os.ReadFile(marker)
	if err != nil {
		return 0
	}
	return strings.Count(string(data), "ran")
}

func writeFixtureFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}
