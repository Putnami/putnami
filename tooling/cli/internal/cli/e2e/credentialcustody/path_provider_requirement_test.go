package credentialcustody

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cache "go.putnami.dev/protocol/cache"
	ciproto "go.putnami.dev/protocol/ci"
	protocolcli "go.putnami.dev/protocol/cli"
	distribution "go.putnami.dev/protocol/distribution"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
)

// pathProviderBuildRan is the line the build of a path provider fixture
// appends to its log.
const pathProviderBuildRan = "path provider build ran"

// writePathProviderFixture writes a consumer workspace whose one extension is
// a path extension at cloudext that declares the release-set provider, the
// cache provider and the build of app. With publish, the workspace's CI
// document declares a rule that publishes main to canary. The build appends
// pathProviderBuildRan to the returned log.
func writePathProviderFixture(t *testing.T, publish bool) (wsRoot, log string) {
	t.Helper()
	wsRoot, log = t.TempDir(), filepath.Join(t.TempDir(), "build.log")
	clitest.WriteFile(t, filepath.Join(wsRoot, "putnami.workspace.json"), `{
  "name": "path-provider-ws",
  "includes": ["app", "cloudext"],
  "extensions": ["/cloudext"]
}`)
	clitest.WriteFile(t, filepath.Join(wsRoot, "cloudext", "putnami.json"), `{"name":"@fixture/path-provider"}`)
	clitest.WriteFile(t, filepath.Join(wsRoot, "cloudext", "putnami.extension.json"), fmt.Sprintf(`{
  "name": "@fixture/path-provider",
  "version": "0.1.0",
  "cliContract": %d,
  "commands": {
    %s: { "description": "Serve release sets.", "run": [{ "id": "serve", "task": "noop-task" }] },
    %s: { "description": "Serve the remote cache.", "run": [{ "id": "serve", "task": "noop-task" }] },
    "build": {
      "description": "Build.",
      "activationFiles": ["app.project"],
      "run": [{ "id": "build", "task": "build-task" }]
    }
  },
  "tasks": {
    "noop-task": { "kind": "command", "command": "/bin/sh", "args": ["-c", "exit 0"], "cache": false },
    "build-task": { "kind": "command", "command": "/bin/sh", "args": ["-c", %s], "cache": false, "timeoutMs": -1 }
  }
}`, protocolcli.CurrentContract, jsonString(distribution.ProviderCommandName), jsonString(cache.ProviderCommandName),
		jsonString(fmt.Sprintf("printf '%%s\\n' %s >> %s", shellQuote(pathProviderBuildRan), shellQuote(log)))))
	clitest.WriteFile(t, filepath.Join(wsRoot, "app", "putnami.json"), `{"name":"app","extensions":["@fixture/path-provider"]}`)
	clitest.WriteFile(t, filepath.Join(wsRoot, "app", "app.project"), "")
	if publish {
		clitest.WriteFile(t, filepath.Join(wsRoot, ciproto.Filename), `{
  "version": 3,
  "commands": ["build"],
  "rules": [{ "branches": "main", "publish": ["canary"] }]
}`)
	}
	return wsRoot, log
}

// A consumer workspace that serves a provider capability only from a path
// extension fails a hosted run when its CI configuration needs that
// capability: the release-set provider for a rule that publishes, the cache
// provider for a configured remote cache. The run fails before any job, with
// the capability, the path extension and the fix in its message. A hosted run
// that needs neither builds, and so does a run without --credential-fd.
func TestAHostedRunFailsWhenTheCIConfigurationNeedsAPathExtensionProvider(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/credential-custody", "needed-provider-only-on-a-path-extension", "a-hosted-run-fails-when-the-ci-needs-a-path-extension-provider")
	clitest.RequireShell(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cacheEnv := []string{custodyCacheEnv + "=1", "PUTNAMI_CACHE_URL=https://cache.invalid", "PUTNAMI_CACHE_TRUST=any"}
	for _, tc := range []struct {
		name    string
		publish bool
		hosted  bool
		env     []string
		missing string
	}{
		{name: "publish rule", publish: true, hosted: true, missing: distribution.ProviderCommandName},
		{name: "remote cache", hosted: true, env: cacheEnv, missing: cache.ProviderCommandName},
		{name: "neither", hosted: true},
		{name: "not hosted", publish: true, env: cacheEnv},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			wsRoot, log := writePathProviderFixture(t, tc.publish)
			code, output := runEngine(t, self, wsRoot, t.TempDir(), tc.hosted, tc.env...)
			_, statErr := os.Stat(log)
			built := statErr == nil
			if tc.missing == "" {
				if code != 0 || !built {
					t.Fatalf("build exit=%d built=%t, want 0 and true\n%s", code, built, output)
				}
				return
			}
			if code != protocolcli.ExitUsage {
				t.Errorf("hosted build exit=%d, want %d\n%s", code, protocolcli.ExitUsage, output)
			}
			if built {
				t.Errorf("hosted build ran the build of app, want it to fail before any job\n%s", output)
			}
			for _, want := range []string{tc.missing, "@fixture/path-provider", "pin"} {
				if !strings.Contains(output, want) {
					t.Errorf("hosted build output lacks %q:\n%s", want, output)
				}
			}
		})
	}
}
