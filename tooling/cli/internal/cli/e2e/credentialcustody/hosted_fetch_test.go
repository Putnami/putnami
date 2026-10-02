package credentialcustody

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

// writeCloudPathFixture writes a workspace whose one extension is a path
// extension, repository code, that serves the command group `cloud` with
// `registry-token`. Its command writes the returned marker file.
func writeCloudPathFixture(t *testing.T) (wsRoot, marker string) {
	t.Helper()
	wsRoot, marker = t.TempDir(), filepath.Join(t.TempDir(), "registry-token.ran")
	clitest.WriteFile(t, filepath.Join(wsRoot, "putnami.workspace.json"), `{
  "name": "hosted-fetch-ws",
  "includes": ["cloudext"],
  "extensions": ["/cloudext"]
}`)
	clitest.WriteFile(t, filepath.Join(wsRoot, "cloudext", "putnami.json"), `{"name":"@fixture/path-cloud"}`)
	clitest.WriteFile(t, filepath.Join(wsRoot, "cloudext", "putnami.extension.json"), fmt.Sprintf(`{
  "name": "@fixture/path-cloud",
  "version": "0.1.0",
  "cliContract": %d,
  "commandGroups": {
    "cloud": {
      "description": "A cloud group the workspace declares.",
      "subcommands": { "registry-token": { "command": "path-registry-token", "description": "Write the marker." } }
    }
  },
  "commands": {
    "path-registry-token": {
      "description": "Write the marker.",
      "activation": "workspace",
      "flags": { "host": { "type": "string", "description": "The registry host.", "default": "" } },
      "run": [{ "id": "token", "task": "token-task" }]
    }
  },
  "tasks": {
    "token-task": { "kind": "command", "command": "/bin/sh", "args": ["-c", %s], "cache": false, "timeoutMs": 30000 }
  }
}`, protocolcli.CurrentContract, jsonString("printf ran > "+shellQuote(marker))))
	return wsRoot, marker
}

// A fetch built on an older SDK starts `putnami cloud
// registry-token` beside the job credential, with the fetch's environment and
// so with runcredential.HostedFetchEnv. That CLI exits 2 with the named error
// before it discovers an extension: the path extension that serves `cloud`
// never runs. Without the marker the same command runs the path extension.
func TestACLIInsideAHostedFetchLoadsNoExtension(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/credential-custody", "hostile-process-finds-nothing", "hosted-run-runs-only-store-and-path-extensions")
	clitest.RequireShell(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	run := func(extraEnv ...string) (code int, output, marker string) {
		wsRoot, marker := writeCloudPathFixture(t)
		// --all spares the fixture, which is no git repository, the default
		// selection by branch.
		env := append([]string{custodyArgsEnv + "=cloud\nregistry-token\n--host\nregistry.example.test\n--all", custodyCacheEnv + "=1"}, extraEnv...)
		code, output = runEngine(t, self, wsRoot, t.TempDir(), false, env...)
		return code, output, marker
	}

	code, output, marker := run(runcredential.HostedFetchEnv + "=1")
	if code != protocolcli.ExitUsage {
		t.Errorf("`cloud registry-token` inside a hosted fetch exit=%d, want %d\n%s", code, protocolcli.ExitUsage, output)
	}
	if want := "putnami: " + runcredential.ErrInHostedFetch.Error(); !strings.Contains(output, want) {
		t.Errorf("`cloud registry-token` inside a hosted fetch output lacks %q:\n%s", want, output)
	}
	if _, err := os.Stat(marker); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the path extension ran inside a hosted fetch: marker stat err=%v", err)
	}

	code, output, marker = run()
	if code != 0 {
		t.Fatalf("`cloud registry-token` without %s exit=%d, want 0\n%s", runcredential.HostedFetchEnv, code, output)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("the path extension did not run without %s: %v\n%s", runcredential.HostedFetchEnv, err, output)
	}
}
