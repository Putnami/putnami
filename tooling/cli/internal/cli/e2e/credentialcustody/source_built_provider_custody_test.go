package credentialcustody

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	cache "go.putnami.dev/protocol/cache"
	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

// prepareRan is the line the runtime.prepare of a source-built fixture
// appends to its log.
const prepareRan = "runtime prepare ran"

// writeSourceBuiltFixture writes a source workspace: its lock records the
// workspace as the source of its CLI. Its one extension is the path extension
// /tools/cloud, which serves the cache provider and the build of app. Its
// runtime.prepare places a copy of self, this test binary, as the runtime.
// The build has a before-hook, repository code. The prepare, the provider, the
// hook and the build append what they did to log.
func writeSourceBuiltFixture(t *testing.T, self string) providerFixture {
	t.Helper()
	fx := providerFixture{wsRoot: t.TempDir(), log: filepath.Join(t.TempDir(), "order.log")}
	logLine := func(line string) string {
		return fmt.Sprintf("printf '%%s\\n' %s >> %s", shellQuote(line), shellQuote(fx.log))
	}
	extRoot := filepath.Join(fx.wsRoot, "tools", "cloud")
	// The prepare runs the runtime it placed once, so the handshake that
	// follows within its deadline does not also pay for the first launch of the
	// new file.
	clitest.WriteFile(t, filepath.Join(extRoot, "bin", "prepare"), fmt.Sprintf(`#!/bin/sh
set -eu
%s
mkdir -p "$2/compiled"
cp %s "$2/compiled/runtime"
chmod +x "$2/compiled/runtime"
"$2/compiled/runtime" __putnami runtime-info >/dev/null
`, logLine(prepareRan), shellQuote(self)))
	if err := os.Chmod(filepath.Join(extRoot, "bin", "prepare"), 0o755); err != nil {
		t.Fatal(err)
	}
	clitest.WriteFile(t, filepath.Join(extRoot, "cmd", "source"), "source\n")
	clitest.WriteFile(t, filepath.Join(extRoot, "putnami.extension.json"), fmt.Sprintf(`{
  "name": "@fixture/cache",
  "version": "0.1.0",
  "cliContract": %d,
  "runtime": {
    "executable": "compiled/runtime",
    "prepare": {
      "command": "{extensionRoot}/bin/prepare",
      "args": ["--output", "{runtimeOutput}"],
      "inputs": ["bin/**", "cmd/**"]
    }
  },
  "commands": {
    %s: {
      "description": "Serve the remote cache.",
      "run": [{ "id": "serve", "task": "serve-task" }]
    },
    "build": {
      "description": "Build.",
      "activationFiles": ["app.project"],
      "run": [{ "id": "build", "task": "build-task" }]
    }
  },
  "tasks": {
    "serve-task": {
      "kind": "command",
      "command": "{extensionRuntime}",
      "args": ["cache-provider"],
      "cache": false,
      "env": { %s: "provider", %s: %s }
    },
    "build-task": { "kind": "command", "command": "/bin/sh", "args": ["-c", %s], "cache": false, "timeoutMs": -1 }
  }
}`,
		protocolcli.CurrentContract, jsonString(cache.ProviderCommandName),
		jsonString(custodyRoleEnv), jsonString(custodyReportEnv), jsonString(fx.log),
		jsonString(logLine(taskRan))))
	clitest.WriteFile(t, filepath.Join(fx.wsRoot, "putnami.workspace.json"), fmt.Sprintf(`{
  "name": "source-ws",
  "includes": ["app"],
  "extensions": { "/tools/cloud": "" },
  "hooks": { "commands": { "build": { "before": [%s] } } }
}`, jsonString(logLine(hookRan))))
	clitest.WriteFile(t, filepath.Join(fx.wsRoot, "app", "putnami.json"), `{"name":"app","extensions":["@fixture/cache"]}`)
	clitest.WriteFile(t, filepath.Join(fx.wsRoot, "app", "app.project"), "")
	lock := lockfile.NewLockFile()
	lock.SetCLI(lockfile.LockEntry{Source: lockfile.SourceWorkspace})
	if err := lockfile.WriteLockFile(fx.wsRoot, lock); err != nil {
		t.Fatal(err)
	}
	return fx
}

// runSourceBuiltBuild runs, in a new source-built fixture, `putnami extensions
// prepare` without a run credential when prepare is set, then a hosted `build`
// with a remote cache configured and the build cache on. It returns the exit
// code and output of the build and the fixture's log path.
func runSourceBuiltBuild(t *testing.T, prepare bool) (code int, output, log string) {
	t.Helper()
	clitest.RequireShell(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	fx := writeSourceBuiltFixture(t, self)
	if prepare {
		code, output := runEngine(t, self, fx.wsRoot, home, false,
			custodyArgsEnv+"=extensions\nprepare", custodyCacheEnv+"=1")
		if code != 0 {
			t.Fatalf("extensions prepare exit=%d, want 0\n%s", code, output)
		}
	}
	code, output = runEngine(t, self, fx.wsRoot, home, true,
		custodyCacheEnv+"=1", "PUTNAMI_CACHE_URL=https://cache.invalid", "PUTNAMI_CACHE_TRUST=any")
	return code, output, fx.log
}

// ADR 0055: in a source workspace, a hosted `build` starts the cache provider
// of a path extension from the runtime `putnami extensions prepare` prepared
// before the run, which `./putnamiw --print-engine` runs. The provider starts
// before the first hook and authenticates with the run credential; the run
// prepares nothing itself.
func TestAHostedRunOfASourceWorkspaceStartsThePreparedProviderBeforeRepositoryCode(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/credential-custody", "source-built-providers",
		"a-hosted-source-workspace-run-authenticates-the-prepared-provider-before-repository-code")

	code, output, log := runSourceBuiltBuild(t, true)
	if code != 0 {
		t.Fatalf("hosted build exit=%d, want 0\n%s", code, output)
	}
	want := []string{prepareRan, providerStarted, providerAuthenticated, hookRan, taskRan}
	if got := readLog(t, log); !slices.Equal(got, want) {
		t.Errorf("log = %q, want %q\n%s", got, want, output)
	}
}

// ADR 0055: in a source workspace whose provider runtime was not prepared
// before the run, a hosted `build` neither prepares it nor starts the provider:
// the failure names `./putnamiw --print-engine`, and no prepare or provider
// runs. The fixture's build does not run that runtime, so the remote cache
// degrades to a local build and the build still runs.
func TestAHostedRunOfASourceWorkspacePreparesNoProviderRuntime(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/credential-custody", "source-built-providers",
		"a-source-built-runtime-the-store-lacks-fails-the-launch")

	code, output, log := runSourceBuiltBuild(t, false)
	if code != 0 {
		t.Fatalf("hosted build exit=%d, want 0 with the remote cache degraded\n%s", code, output)
	}
	if !strings.Contains(output, "./putnamiw --print-engine") {
		t.Errorf("output does not name the step that prepares the provider runtime:\n%s", output)
	}
	got, err := os.ReadFile(log)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	for _, line := range []string{prepareRan, providerStarted, providerAuthenticated} {
		if strings.Contains(string(got), line) {
			t.Errorf("a hosted run without a prepared runtime logged %q:\n%s\n%s", line, got, output)
		}
	}
	if !strings.Contains(string(got), taskRan) {
		t.Errorf("the build did not run with the remote cache degraded:\n%s\n%s", got, output)
	}
}
