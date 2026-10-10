package toolchainlock

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	extensionproto "go.putnami.dev/protocol/extension"
	runtimeproto "go.putnami.dev/protocol/runtime"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/cli"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
	"go.putnami.dev/tooling/cli/internal/commands/lifecycle"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
	"go.putnami.dev/tooling/cli/internal/hometest"
	"go.putnami.dev/tooling/cli/internal/layout"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// writeFetchingLifecycleFixture writes a workspace whose one extension fetches
// and installs. The extension is installed in the artifact store, which
// PUTNAMI_ARTIFACT_DIR names, and linked from the workspace, as a registry
// pin is: a hosted run runs no other extension. Its fetch runs through the
// extension's own runtime, the one fetch a hosted install runs, or, with
// scriptFetch, through a script of the extension. Each task appends its name
// and the offline signal it sees to the returned log; the fetch exits with
// fetchExit.
func writeFetchingLifecycleFixture(t *testing.T, wsRoot string, fetchExit int, scriptFetch bool) (log string) {
	t.Helper()
	log = filepath.Join(wsRoot, "runs.txt")
	logLine := func(name string) string {
		return "printf '" + name + " offline=%s\\n' \"$" + extensionproto.OfflineDependenciesEnv + "\" >> " + log + "\n"
	}
	task := func(name string, exit int) string {
		return "#!/bin/sh\n" + logLine(name) + "exit " + strconv.Itoa(exit) + "\n"
	}
	info := fmt.Sprintf(
		`{"extension":"@putnami/fetcher","version":"0.1.0","platform":%q,"cliContract":%d,"runtimeProtocol":%d,"runtimeABI":%d}`,
		runtime.GOOS+"/"+runtime.GOARCH, protocolcli.CurrentContract,
		runtimeproto.MaxKnownProtocolVersion, runtimeproto.RuntimeABIVersion)
	runtimeScript := "#!/bin/sh\n" +
		"if [ \"$1\" = \"__putnami\" ] && [ \"$2\" = \"runtime-info\" ]; then\n" +
		"  printf '%s\\n' '" + info + "'\n" +
		"  exit 0\n" +
		"fi\n" +
		"if [ \"$1\" = \"fetch\" ]; then\n" +
		"  " + logLine("fetch") +
		"  exit " + strconv.Itoa(fetchExit) + "\n" +
		"fi\n" +
		"exit 0\n"
	clitest.WriteFile(t, filepath.Join(wsRoot, "putnami.workspace.json"),
		`{"name":"fetch-ws","includes":["app"],"extensions":{"@putnami/fetcher":"0.1.0"}}`)
	clitest.WriteFile(t, filepath.Join(wsRoot, "app", "putnami.json"), `{"name":"app"}`)
	extRoot := filepath.Join(os.Getenv("PUTNAMI_ARTIFACT_DIR"), "extensions", "fetcher@0.1.0")
	link := layout.StableDir(wsRoot, layout.Extensions, "@putnami/fetcher")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	// The runtime is warmed for the handshake's deadline (fixtureproc.Script).
	fixtureproc.Script(t, filepath.Join(extRoot, "bin", "runtime"), runtimeScript, "__putnami", "runtime-info")
	writeFixtureFile(t, filepath.Join(extRoot, "install.sh"), task("install", 0), 0o755)
	fetchTask := `{ "kind": "command", "command": "{extensionRuntime}", "args": ["fetch"], "cache": false, "timeoutMs": 10000 }`
	if scriptFetch {
		writeFixtureFile(t, filepath.Join(extRoot, "fetch.sh"), task("fetch", fetchExit), 0o755)
		fetchTask = `{ "kind": "command", "command": "{extensionRoot}/fetch.sh", "cache": false, "timeoutMs": 10000 }`
	}
	if err := os.Symlink(extRoot, link); err != nil {
		t.Fatal(err)
	}
	clitest.WriteFile(t, filepath.Join(extRoot, "putnami.extension.json"), `{
  "name": "@putnami/fetcher",
  "version": "0.1.0",
  "cliContract": 4,
  "runtime": { "executable": "bin/runtime" },
  "commands": {
    "workspace-fetch": {
      "description": "Fetch workspace dependencies.",
      "activation": "workspace",
      "run": [{ "id": "fetch", "task": "fetch-task" }]
    },
    "workspace-install": {
      "description": "Install workspace dependencies.",
      "activation": "workspace",
      "run": [{ "id": "install", "task": "install-task" }]
    }
  },
  "tasks": {
    "fetch-task": `+fetchTask+`,
    "install-task": { "kind": "command", "command": "{extensionRoot}/install.sh", "cache": false, "timeoutMs": 10000 }
  }
}`)
	workspace.InvalidateLoadCache(wsRoot)
	return log
}

// On a hosted run, the install runs the fetch to completion through the
// engine first, and every installer then runs offline. A failed fetch runs no
// installer, and neither does a fetch that is not the extension's own
// runtime: it would run beside the fetches that hold the job credential. A
// local run installs as it always did.
// The chain is the real one: DepsInstall → RunWorkspaceJob → Engine.Run → the
// extension's task.
func TestDepsInstallFetchesThroughTheEngineOnAHostedRun(t *testing.T) {
	clitest.RequireShell(t)
	if runtime.GOOS == "windows" {
		t.Skip("shell task fixture")
	}
	const (
		fetched = "Workspace dependencies fetched (@putnami/fetcher)"
		setup   = "Workspace setup completed (@putnami/fetcher)"
	)
	cases := []struct {
		name        string
		bearer      string
		fetchExit   int
		scriptFetch bool
		wantLog     string
		wantErr     string
		wantDone    []string
	}{
		{"a local run", "", 0, false, "install offline=\n", "", []string{setup}},
		{"a hosted run", "run-bearer", 0, false, "fetch offline=\ninstall offline=1\n", "", []string{fetched, setup}},
		{"a hosted run whose fetch fails", "run-bearer", 1, false, "fetch offline=\n", "deps fetch failed", nil},
		{"a hosted run whose fetch is a script", "run-bearer", 0, true, "", "deps fetch failed", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wsRoot := t.TempDir()
			hometest.Temp(t)
			t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))
			t.Setenv("PUTNAMI_ARTIFACT_DIR", filepath.Join(t.TempDir(), "artifacts"))
			log := writeFetchingLifecycleFixture(t, wsRoot, tc.fetchExit, tc.scriptFetch)
			t.Cleanup(runcredential.SetForTest(tc.bearer))
			if tc.bearer != "" {
				// What runcredential.Capture leaves in a hosted run's environment.
				t.Setenv(extensionproto.OfflineDependenciesEnv, "1")
			}

			var done []string
			env := lifecycle.LifecycleEnv{Out: io.Discard, RunJob: cli.RunWorkspaceJob, OnAction: func(action lifecycle.LifecycleAction) {
				if action.Kind != "workspace-index" {
					done = append(done, action.Description)
				}
			}}
			var err error
			streams := clitest.CaptureStdoutStderr(t, func() {
				err = lifecycle.DepsInstall(context.Background(), wsRoot, wsproto.Load(wsRoot), "", "", env)
			})
			if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || err.Error() != tc.wantErr) {
				t.Fatalf("DepsInstall = %v, want %q\n%s", err, tc.wantErr, streams)
			}
			data, _ := os.ReadFile(log)
			if string(data) != tc.wantLog {
				t.Errorf("the tasks ran as\n%s\nwant\n%s\n%s", data, tc.wantLog, streams)
			}
			if !reflect.DeepEqual(done, tc.wantDone) {
				t.Errorf("completed actions %q, want %q", done, tc.wantDone)
			}
		})
	}
}
