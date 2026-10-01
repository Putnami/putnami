package credentialcustody

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	cache "go.putnami.dev/protocol/cache"
	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/artifactstore"
	"go.putnami.dev/tooling/cli/internal/cli"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
	"go.putnami.dev/tooling/cli/internal/layout"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

// The lines the workspace-install steps of a bootstrap fixture append to its
// log.
const (
	fetchRan       = "fetch ran"
	installHookRan = "onInstall hook ran"
	installerRan   = "installer ran"
)

// runFetchRole is the workspace-fetch of a bootstrap fixture: it records that
// it ran.
func runFetchRole() int {
	if err := appendLog(os.Getenv(custodyReportEnv), fetchRan); err != nil {
		return 1
	}
	return 0
}

// writeBootstrapFixture writes a workspace that no install has restored yet,
// so `build` first runs the implicit install. Its one extension is admitted
// into the artifact store under home and pinned by the lock with that digest,
// so the install links it without a download. Its runtime is a copy of this
// binary, which serves the remote cache provider and the workspace-fetch (the
// one fetch a hosted run hands the job credential), each in its role. It also
// declares a workspace-install, an onInstall hook and the build, which its
// command group `fixture build` also runs. Every step appends what it did to
// the returned log.
func writeBootstrapFixture(t *testing.T, home string) (wsRoot, log string) {
	t.Helper()
	wsRoot, log = t.TempDir(), filepath.Join(t.TempDir(), "order.log")
	logLine := func(line string) string {
		return fmt.Sprintf("printf '%%s\\n' %s >> %s", shellQuote(line), shellQuote(log))
	}
	logStep := func(line string) string { return jsonString(logLine(line)) }
	manifest := fmt.Sprintf(`{
  "name": "@fixture/cache",
  "version": "0.1.0",
  "cliContract": %d,
  "runtime": { "executable": "compiled/runtime" },
  "hooks": {
    "onInstall": { "kind": "command", "command": "/bin/sh", "args": ["-c", %s] }
  },
  "commands": {
    %s: {
      "description": "Serve the remote cache.",
      "run": [{ "id": "serve", "task": "serve-task" }]
    },
    "workspace-fetch": {
      "description": "Fetch workspace dependencies.",
      "activation": "workspace",
      "run": [{ "id": "fetch", "task": "fetch-task" }]
    },
    "workspace-install": {
      "description": "Install workspace dependencies.",
      "activation": "workspace",
      "run": [{ "id": "install", "task": "install-task" }]
    },
    "build": {
      "description": "Build.",
      "activationFiles": ["app.project"],
      "run": [{ "id": "build", "task": "build-task" }]
    }
  },
  "commandGroups": {
    "fixture": {
      "description": "Fixture commands.",
      "subcommands": { "build": { "command": "build", "description": "Build." } }
    }
  },
  "tasks": {
    "serve-task": {
      "kind": "command",
      "command": "{extensionRuntime}",
      "args": ["cache-provider"],
      "cache": false,
      "env": { %[4]s: "provider", %[5]s: %[6]s }
    },
    "fetch-task": {
      "kind": "command",
      "command": "{extensionRuntime}",
      "args": ["fetch"],
      "cache": false,
      "timeoutMs": 30000,
      "env": { %[4]s: "fetch", %[5]s: %[6]s }
    },
    "install-task": { "kind": "command", "command": "/bin/sh", "args": ["-c", %s], "cache": false, "timeoutMs": 30000 },
    "build-task": { "kind": "command", "command": "/bin/sh", "args": ["-c", %s], "cache": false, "timeoutMs": 30000 }
  }
}`,
		protocolcli.CurrentContract, logStep(installHookRan), jsonString(cache.ProviderCommandName),
		jsonString(custodyRoleEnv), jsonString(custodyReportEnv), jsonString(log),
		logStep(installerRan), logStep(taskRan))

	digest := lockfile.HashBytes([]byte("archive of @fixture/cache 0.1.0"))
	stored, err := artifactstore.New(artifactDir(home)).Admit(digest, func(stage string) error {
		fixtureproc.Binary(t, filepath.Join(stage, "compiled", "runtime"))
		return os.WriteFile(filepath.Join(stage, "putnami.extension.json"), []byte(manifest), 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	lock := lockfile.NewLockFile()
	lock.SetExtension("@fixture/cache", lockfile.LockEntry{
		Version:      "0.1.0",
		ManifestHash: lockfile.HashBytes([]byte(manifest)),
		Integrities:  map[string]string{lockfile.PlatformKey(runtime.GOOS, runtime.GOARCH): digest},
	})
	if err := lockfile.WriteLockFile(wsRoot, lock); err != nil {
		t.Fatal(err)
	}
	clitest.WriteFile(t, filepath.Join(wsRoot, "putnami.workspace.json"), `{
  "name": "bootstrap-ws",
  "includes": ["app"],
  "extensions": { "@fixture/cache": "0.1.0" }
}`)
	clitest.WriteFile(t, filepath.Join(wsRoot, "app", "putnami.json"), `{"name":"app","extensions":["@fixture/cache"]}`)
	clitest.WriteFile(t, filepath.Join(wsRoot, "app", "app.project"), "")
	link := layout.StableDir(wsRoot, layout.Extensions, "@fixture/cache")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(stored, link); err != nil {
		t.Fatal(err)
	}
	return wsRoot, log
}

// runBootstrapBuild runs `build` in a bootstrap fixture with a remote cache
// configured, the build cache on and the implicit install on, hosted or not,
// and returns the log.
func runBootstrapBuild(t *testing.T, hosted bool) []string {
	t.Helper()
	clitest.RequireShell(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	wsRoot, log := writeBootstrapFixture(t, home)
	code, output := runEngine(t, self, wsRoot, home, hosted,
		custodyCacheEnv+"=1", "PUTNAMI_CACHE_URL=https://cache.invalid", "PUTNAMI_CACHE_TRUST=any",
		"PUTNAMI_NO_AUTO_INSTALL=")
	if code != 0 {
		t.Fatalf("build exit=%d, want 0\n%s", code, output)
	}
	return readLog(t, log)
}

// ADR 0055 part 4: a hosted `build` on a workspace no install
// has restored runs the implicit install, and succeeds with a remote cache
// configured. The install runs the workspace-fetch first; then the run starts
// its cache provider once, which authenticates with the run credential; then
// the first repository code runs: the onInstall hook, then the installer. The
// build reads the remote cache through that same provider.
func TestAHostedBuildBootstrapsWithTheCacheProviderStartedOnce(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/credential-custody", "fetch-before-repository-code", "fetch-completes-before-install")
	spectest.Proves(t, "cli/credential-custody", "hostile-process-finds-nothing", "no-credential-after-repository-code")

	got := runBootstrapBuild(t, true)
	want := []string{fetchRan, providerStarted, providerAuthenticated, installHookRan, installerRan, taskRan}
	if !slices.Equal(got, want) {
		t.Errorf("hosted build log = %q, want %q", got, want)
	}
}

// Without --credential-fd the implicit install is the one it always was: no
// workspace-fetch, the onInstall hook, then the installer; the build then
// starts the cache provider at its first operation, with no credential.
func TestFlagOffBootstrapsAsBefore(t *testing.T) {
	t.Parallel()

	got := runBootstrapBuild(t, false)
	want := []string{installHookRan, installerRan, providerStarted, taskRan}
	if !slices.Equal(got, want) {
		t.Errorf("local build log = %q, want %q", got, want)
	}
}

// ADR 0055: an extension command group resolves its remote cache only
// once its job command is known, after the implicit install ran repository
// code, so a hosted run refuses it before anything starts: exit 2, the named
// usage error, and no fetch, provider, hook or installer. Without
// --credential-fd, the same command installs and builds.
func TestAHostedRunRefusesAnExtensionCommandGroup(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/credential-custody", "hostile-process-finds-nothing", "no-credential-after-repository-code")
	clitest.RequireShell(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	run := func(hosted bool) (code int, output, log string) {
		home := t.TempDir()
		wsRoot, log := writeBootstrapFixture(t, home)
		code, output = runEngine(t, self, wsRoot, home, hosted,
			custodyArgsEnv+"=fixture\nbuild\n--projects\napp",
			custodyCacheEnv+"=1", "PUTNAMI_CACHE_URL=https://cache.invalid", "PUTNAMI_CACHE_TRUST=any",
			"PUTNAMI_NO_AUTO_INSTALL=")
		return code, output, log
	}

	code, output, log := run(true)
	if code != 2 {
		t.Errorf("hosted `fixture build` exit=%d, want 2\n%s", code, output)
	}
	if want := "putnami fixture: " + cli.ErrHostedExtensionCommand.Error(); !strings.Contains(output, want) {
		t.Errorf("hosted `fixture build` output lacks %q:\n%s", want, output)
	}
	if _, err := os.Stat(log); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("hosted `fixture build` started a process: log stat err=%v", err)
	}

	code, output, log = run(false)
	if code != 0 {
		t.Fatalf("local `fixture build` exit=%d, want 0\n%s", code, output)
	}
	if got, want := readLog(t, log), []string{installHookRan, installerRan, providerStarted, taskRan}; !slices.Equal(got, want) {
		t.Errorf("local `fixture build` log = %q, want %q", got, want)
	}
}
