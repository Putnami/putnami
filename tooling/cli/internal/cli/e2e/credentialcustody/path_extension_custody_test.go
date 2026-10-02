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
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
)

// The lines the path extension of a path fixture appends to its log.
const (
	pathProviderStarted = "path extension cache provider started"
	pathTaskRan         = "path extension task ran"
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

// writePathExtensionFixture is writeBootstrapFixture with a second extension:
// a path extension at tools/local, a workspace project that the workspace
// config declares by its path. Its runtime is a copy of this binary, which
// serves its workspace-fetch. It also declares a cache provider and the build
// of app, which append to the log.
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
		protocolcli.CurrentContract, logStep(pathProviderStarted),
		jsonString(custodyRoleEnv), jsonString(custodyReportEnv), jsonString(log), logStep(pathTaskRan)))
	clitest.WriteFile(t, filepath.Join(wsRoot, "putnami.workspace.json"), `{
  "name": "bootstrap-ws",
  "includes": ["app", "tools/local"],
  "extensions": { "@fixture/cache": "0.1.0", "/tools/local": "" }
}`)
	clitest.WriteFile(t, filepath.Join(wsRoot, "app", "putnami.json"), `{"name":"app","extensions":["@fixture/cache","@fixture/local"]}`)
	return wsRoot, log
}

// ADR 0055 parts 2 and 4: a hosted `build` on a workspace with a store
// extension and a path extension plans the path extension's tasks. The store
// extension's workspace-fetch runs first, then its cache provider starts and
// authenticates with the run credential, then the path extension's
// workspace-fetch, offline and without a credential, then the onInstall hook,
// the installer and both builds. The path extension's cache provider never
// starts.
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
		"PUTNAMI_NO_AUTO_INSTALL=")
	if code != 0 {
		t.Fatalf("hosted build exit=%d, want 0\n%s", code, output)
	}

	got := readLog(t, log)
	install := []string{fetchRan, providerStarted, providerAuthenticated, pathFetchRan(false, "1"), installHookRan, installerRan}
	if len(got) != len(install)+2 || !slices.Equal(got[:len(install)], install) {
		t.Fatalf("hosted build log = %q, want %q, then both builds\n%s", got, install, output)
	}
	builds := slices.Sorted(slices.Values(got[len(install):]))
	if want := []string{pathTaskRan, taskRan}; !slices.Equal(builds, want) {
		t.Errorf("hosted build ran %q after the install, want %q", builds, want)
	}
}
