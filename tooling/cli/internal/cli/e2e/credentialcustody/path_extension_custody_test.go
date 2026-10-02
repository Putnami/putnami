package credentialcustody

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
)

// The lines the path extension of a path fixture appends to its log.
const (
	pathProviderStarted = "path extension cache provider started"
	pathTaskRan         = "path extension task ran"
	pathHookRan         = "path extension onInstall hook ran"
	pathProbeRan        = "path extension workspace probe ran"
)

// pathFetchRan is the line the workspace-fetch of a path fixture's path
// extension appends to its log: whether it received a job credential
// descriptor, and the offline signal it runs with.
func pathFetchRan(credential bool, offline string) string {
	return fmt.Sprintf("path extension fetch ran: credential=%t offline=%s", credential, offline)
}

// runPathFetchRole is the workspace-fetch of a path fixture's path extension:
// it records that it ran, with what it received.
func runPathFetchRole() int {
	_, credential := os.LookupEnv(extensionproto.JobCredentialFDEnv)
	line := pathFetchRan(credential, os.Getenv(extensionproto.OfflineDependenciesEnv))
	if err := appendLog(os.Getenv(custodyReportEnv), line); err != nil {
		return 1
	}
	return 0
}

// runPathProbeRole is the workspace probe of a path fixture's path extension:
// it records that it ran in the log custodyProbeLogEnv names, and answers
// that it knows no project.
func runPathProbeRole() int {
	if err := appendLog(os.Getenv(custodyProbeLogEnv), pathProbeRan); err != nil {
		return 1
	}
	handled, err := wsproto.ServeProbe(os.Args[1:], os.Stdin, os.Stdout, func(wsproto.ProbeRequest) (wsproto.ProbeResult, error) {
		return wsproto.ProbeResult{Extension: "@fixture/local"}, nil
	})
	if !handled || err != nil {
		return 1
	}
	return 0
}

// writePathExtensionFixture is writeBootstrapFixture with a second extension:
// a path extension at tools/local, a workspace project that the workspace
// config declares by its path. Its runtime is a copy of this binary, which
// serves its workspace-fetch and its workspace probe (runPathProbeRole), whose
// marker app holds. It also declares an onInstall hook, a cache provider and
// the build of app, which append to the log.
func writePathExtensionFixture(t *testing.T, home string) (wsRoot, log string) {
	t.Helper()
	wsRoot, log = writeBootstrapFixture(t, home)
	logStep := func(line string) string {
		return jsonString(fmt.Sprintf("printf '%%s\\n' %s >> %s", shellQuote(line), shellQuote(log)))
	}
	extRoot := filepath.Join(wsRoot, "tools", "local")
	fixtureproc.Binary(t, filepath.Join(extRoot, "compiled", "runtime"))
	clitest.WriteFile(t, filepath.Join(extRoot, "putnami.json"), `{"name":"@fixture/local"}`)
	clitest.WriteFile(t, filepath.Join(extRoot, "putnami.extension.json"), fmt.Sprintf(`{
  "name": "@fixture/local",
  "version": "0.1.0",
  "cliContract": %d,
  "runtime": { "executable": "compiled/runtime" },
  "workspace": { "markers": ["local.marker"], "inputs": ["local.marker"] },
  "hooks": {
    "onInstall": { "kind": "command", "command": "/bin/sh", "args": ["-c", %s] }
  },
  "commands": {
    "cache-provider": {
      "description": "Serve the remote cache.",
      "run": [{ "id": "serve", "task": "serve-task" }]
    },
    "workspace-fetch": {
      "description": "Fetch workspace dependencies.",
      "activation": "workspace",
      "run": [{ "id": "fetch", "task": "fetch-task" }]
    },
    "build": {
      "description": "Build.",
      "activationFiles": ["app.project"],
      "run": [{ "id": "build", "task": "build-task" }]
    }
  },
  "tasks": {
    "serve-task": { "kind": "command", "command": "/bin/sh", "args": ["-c", %s], "cache": false },
    "fetch-task": {
      "kind": "command",
      "command": "{extensionRuntime}",
      "args": ["fetch"],
      "cache": false,
      "timeoutMs": 30000,
      "env": { %s: "path-fetch", %s: %s }
    },
    "build-task": { "kind": "command", "command": "/bin/sh", "args": ["-c", %s], "cache": false, "timeoutMs": 30000 }
  }
}`,
		protocolcli.CurrentContract, logStep(pathHookRan), logStep(pathProviderStarted),
		jsonString(custodyRoleEnv), jsonString(custodyReportEnv), jsonString(log), logStep(pathTaskRan)))
	clitest.WriteFile(t, filepath.Join(wsRoot, "putnami.workspace.json"), `{
  "name": "bootstrap-ws",
  "includes": ["app", "tools/local"],
  "extensions": { "@fixture/cache": "0.1.0", "/tools/local": "" }
}`)
	clitest.WriteFile(t, filepath.Join(wsRoot, "app", "putnami.json"), `{"name":"app","extensions":["@fixture/cache","@fixture/local"]}`)
	clitest.WriteFile(t, filepath.Join(wsRoot, "app", "local.marker"), "app\n")
	return wsRoot, log
}

// ADR 0055 parts 2 and 4: a hosted `build` on a workspace with a store
// extension and a path extension plans the path extension's tasks. The store
// extension's workspace-fetch runs first, then its cache provider starts and
// authenticates with the run credential. Every process of the path extension
// starts after that: its workspace-fetch, offline and without a credential,
// then both onInstall hooks, the installer and both builds, and its workspace
// probe. The path extension's cache provider never starts.
func TestAHostedBuildRunsThePathExtensionAfterCustody(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/credential-custody", "fetch-before-repository-code", "path-extension-fetch-runs-after-custody")
	spectest.Proves(t, "cli/credential-custody", "hostile-process-finds-nothing", "path-extension-serves-no-provider")
	clitest.RequireShell(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	wsRoot, log := writePathExtensionFixture(t, home)
	code, output := runEngine(t, self, wsRoot, home, true,
		custodyCacheEnv+"=1", "PUTNAMI_CACHE_URL=https://cache.invalid", "PUTNAMI_CACHE_TRUST=any",
		"PUTNAMI_NO_AUTO_INSTALL=", custodyProbeLogEnv+"="+log)
	if code != 0 {
		t.Fatalf("hosted build exit=%d, want 0\n%s", code, output)
	}

	got := readLog(t, log)
	custody := []string{fetchRan, providerStarted, providerAuthenticated}
	if len(got) < len(custody) || !slices.Equal(got[:len(custody)], custody) {
		t.Fatalf("hosted build log = %q, want it to start with %q\n%s", got, custody, output)
	}
	after := got[len(custody):]
	// The workspace probe answers whenever the engine reads the workspace, so
	// it may run more than once; every other step runs once.
	steps := slices.DeleteFunc(slices.Clone(after), func(line string) bool { return line == pathProbeRan })
	want := []string{pathFetchRan(false, "1"), installHookRan, pathHookRan, installerRan, pathTaskRan, taskRan}
	if probes := len(after) - len(steps); probes == 0 || !slices.Equal(slices.Sorted(slices.Values(steps)), slices.Sorted(slices.Values(want))) {
		t.Fatalf("hosted build log after custody = %q, want %q and at least one %q\n%s", after, want, pathProbeRan, output)
	}
	at := func(line string) int { return slices.Index(steps, line) }
	for _, order := range [][2]string{
		{pathFetchRan(false, "1"), installHookRan}, {pathFetchRan(false, "1"), pathHookRan},
		{installHookRan, installerRan}, {pathHookRan, installerRan},
		{installerRan, pathTaskRan}, {installerRan, taskRan},
	} {
		if at(order[0]) > at(order[1]) {
			t.Errorf("hosted build ran %q before %q: %q", order[1], order[0], after)
		}
	}
}
