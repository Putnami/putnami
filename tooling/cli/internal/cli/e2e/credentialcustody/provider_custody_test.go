package credentialcustody

import (
	"bufio"
	"encoding/json"
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
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
	"go.putnami.dev/tooling/cli/internal/layout"
)

// The lines the processes of a provider fixture append to its log, in the
// order they happen.
const (
	providerStarted       = "provider started"
	providerAuthenticated = "provider authenticated with the run credential"
	tamperedStarted       = "tampered provider started"
	hookRan               = "hook ran"
	taskRan               = "task ran"
)

// runCacheProviderRole is the cache provider of a provider fixture. It serves
// the cache provider protocol on its standard streams: it records its start,
// echoes the run-credential capability, records an authenticate that carries
// the run credential, and reports itself not ready, so the build runs locally
// and sends no other operation.
func runCacheProviderRole() int {
	log := os.Getenv(custodyReportEnv)
	if err := appendLog(log, providerStarted); err != nil {
		return 1
	}
	out := bufio.NewWriter(os.Stdout)
	reply := func(req *cache.ProviderRequest, payload any, providerErr *cache.ProviderError) {
		raw, _ := cache.MarshalPayload(payload)
		data, _ := json.Marshal(&cache.ProviderResponse{
			ProtocolVersion: req.ProtocolVersion, ID: req.ID, OK: providerErr == nil, Payload: raw, Error: providerErr,
		})
		_, _ = out.Write(append(data, '\n'))
		_ = out.Flush()
	}
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		req, _ := cache.ParseAndValidateProviderRequest(in.Bytes())
		if req == nil {
			return 1
		}
		switch req.Op {
		case cache.OpInitialize:
			reply(req, &cache.InitializeResult{
				ProtocolVersion: req.ProtocolVersion,
				ProviderName:    "@fixture/cache",
				ProviderVersion: "0.1.0",
				Capabilities:    []string{cache.CapabilityRunCredential},
			}, nil)
		case cache.OpAuthenticate:
			params, _ := cache.ParseAndValidateAuthenticateParams(req.Payload)
			if params == nil || params.Credential != custodyBearer {
				reply(req, nil, &cache.ProviderError{Code: "unauthorized", Message: "not the run credential"})
				continue
			}
			if err := appendLog(log, providerAuthenticated); err != nil {
				return 1
			}
			reply(req, &cache.AuthenticateResult{}, nil)
		case cache.OpShutdown:
			reply(req, nil, nil)
			return 0
		default:
			reply(req, nil, &cache.ProviderError{Code: "unsupported", Message: string(req.Op)})
		}
	}
	return 0
}

// runTamperRole is the before-hook of a provider fixture: repository code. It
// records that it ran, then replaces the file the cache provider starts from,
// its launcher script or its runtime, with a script that records its start
// before it runs this binary. A provider started after the hook runs that
// script. The runtime-info handshake runs it too, and it records nothing
// then. The new file is renamed over the old one, because a running provider
// holds the old one open for execution.
func runTamperRole() int {
	log := os.Getenv(custodyReportEnv)
	self, err := os.Executable()
	if err != nil {
		return 1
	}
	if err := appendLog(log, hookRan); err != nil {
		return 1
	}
	target := os.Getenv(custodyTargetEnv)
	tampered := fmt.Sprintf("#!/bin/sh\ncase \"$1\" in __putnami) ;; *) printf '%%s\\n' %s >> %s ;; esac\nexec %s \"$@\"\n",
		shellQuote(tamperedStarted), shellQuote(log), shellQuote(self))
	if err := os.WriteFile(target+".tampered", []byte(tampered), 0o755); err != nil { //nolint:gosec // the hostile hook writes an executable
		return 1
	}
	if err := os.Rename(target+".tampered", target); err != nil {
		return 1
	}
	return 0
}

// appendLog appends line to the log at path.
func appendLog(path, line string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(file, line)
	return errors.Join(err, file.Close())
}

// readLog returns the lines of the log at path.
func readLog(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log %s: %v", path, err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// providerFixture is a workspace whose build has a before-hook that replaces
// the file its cache provider starts from, and an extension, installed in the
// artifact store, that serves the cache provider and the build. Every process
// appends what it did to log.
type providerFixture struct {
	wsRoot string
	log    string
}

// The ways a provider fixture starts its cache provider.
const (
	// nativeProvider starts the extension's runtime, a copy of this test
	// binary: command {extensionRuntime}, args ["cache-provider"].
	nativeProvider = iota
	// launcherProvider starts /bin/sh with a launcher script in the extension
	// root that runs this test binary.
	launcherProvider
)

// writeProviderFixture builds the fixture, whose cache provider starts as
// start says. self is the absolute path of this test binary, which the hook
// runs; home is the one runEngine names. The log is outside every other root.
func writeProviderFixture(t *testing.T, self, home string, start int) providerFixture {
	t.Helper()
	fx := providerFixture{wsRoot: t.TempDir(), log: filepath.Join(t.TempDir(), "order.log")}
	extRoot := filepath.Join(artifactDir(home), "extensions", "cache@0.1.0")
	runtimePath := fixtureproc.Binary(t, filepath.Join(extRoot, "compiled", "runtime"))
	warmRuntime(t, runtimePath)
	serve := `"command": "{extensionRuntime}", "args": ["cache-provider"]`
	target := runtimePath
	if start == launcherProvider {
		target = filepath.Join(extRoot, "provider.sh")
		clitest.WriteFile(t, target, "exec "+shellQuote(self)+"\n")
		serve = `"command": "/bin/sh", "args": ["{extensionRoot}/provider.sh"]`
	}

	hookCmd := fmt.Sprintf("%s=tamper %s=%s %s=%s exec %s", custodyRoleEnv,
		custodyReportEnv, shellQuote(fx.log), custodyTargetEnv, shellQuote(target), shellQuote(self))
	clitest.WriteFile(t, filepath.Join(fx.wsRoot, "putnami.workspace.json"), fmt.Sprintf(`{
  "name": "provider-ws",
  "includes": ["app"],
  "extensions": { "@fixture/cache": "0.1.0" },
  "hooks": { "commands": { "build": { "before": [%s] } } }
}`, jsonString(hookCmd)))
	clitest.WriteFile(t, filepath.Join(fx.wsRoot, "app", "putnami.json"), `{"name":"app","extensions":["@fixture/cache"]}`)
	clitest.WriteFile(t, filepath.Join(fx.wsRoot, "app", "app.project"), "")

	logTask := fmt.Sprintf("printf '%%s\\n' %s >> %s", shellQuote(taskRan), shellQuote(fx.log))
	clitest.WriteFile(t, filepath.Join(extRoot, "putnami.extension.json"), fmt.Sprintf(`{
  "name": "@fixture/cache",
  "version": "0.1.0",
  "cliContract": %d,
  "runtime": { "executable": "compiled/runtime" },
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
      %s,
      "cache": false,
      "env": { %s: "provider", %s: %s }
    },
    "build-task": {
      "kind": "command",
      "command": "/bin/sh",
      "args": ["-c", %s],
      "cache": false,
      "timeoutMs": 30000
    }
  }
}`,
		protocolcli.CurrentContract, jsonString(cache.ProviderCommandName), serve,
		jsonString(custodyRoleEnv), jsonString(custodyReportEnv), jsonString(fx.log),
		jsonString(logTask)))
	link := layout.StableDir(fx.wsRoot, layout.Extensions, "@fixture/cache")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(extRoot, link); err != nil {
		t.Fatal(err)
	}
	return fx
}

// startProviderBuild runs `build` in a fixture whose cache provider starts as
// start says, with a remote cache configured and the build cache on, hosted or
// not. It returns the exit code, the output and the log path.
func startProviderBuild(t *testing.T, hosted bool, start int) (code int, output, log string) {
	t.Helper()
	clitest.RequireShell(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	fx := writeProviderFixture(t, self, home, start)
	code, output = runEngine(t, self, fx.wsRoot, home, hosted,
		custodyCacheEnv+"=1", "PUTNAMI_CACHE_URL=https://cache.invalid", "PUTNAMI_CACHE_TRUST=any")
	return code, output, fx.log
}

// runProviderBuild is startProviderBuild for a build that succeeds; it returns
// the log.
func runProviderBuild(t *testing.T, hosted bool, start int) []string {
	t.Helper()
	code, output, log := startProviderBuild(t, hosted, start)
	if code != 0 {
		t.Fatalf("build exit=%d, want 0\n%s", code, output)
	}
	return readLog(t, log)
}

// On a hosted run the cache provider receives the run
// credential, so it starts before the first hook, as its extension's native
// runtime. The hook, repository code, then replaces the runtime the provider
// started from, and no process ever starts from the replaced file: the
// provider that serves the run is the one started first, and it alone
// received the credential.
func TestTheCacheProviderStartsBeforeRepositoryCodeOnAHostedRun(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/credential-custody", "hostile-process-finds-nothing", "no-credential-after-repository-code")

	got := runProviderBuild(t, true, nativeProvider)
	want := []string{providerStarted, providerAuthenticated, hookRan, taskRan}
	if !slices.Equal(got, want) {
		t.Errorf("hosted build log = %q, want %q", got, want)
	}
}

// A launcher script reads more files after it starts, when
// repository code may have replaced them, so a hosted run refuses a cache
// provider that is not its extension's native runtime. The run fails before
// it starts the provider or any hook, and names the provider and the rule.
func TestAHostedRunRefusesACacheProviderThatIsNotItsNativeRuntime(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/credential-custody", "hostile-process-finds-nothing", "no-credential-after-repository-code")

	code, output, log := startProviderBuild(t, true, launcherProvider)
	if code == 0 {
		t.Fatalf("hosted build with a launcher provider exit=0, want a failure\n%s", output)
	}
	for _, want := range []string{"the cache provider of @fixture/cache cannot hold the run credential", "{extensionRuntime}"} {
		if !strings.Contains(output, want) {
			t.Errorf("output lacks %q:\n%s", want, output)
		}
	}
	if _, err := os.Stat(log); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a provider, hook or task ran after the refusal: log stat err=%v\n%s", err, readLog(t, log))
	}
}

// Without --credential-fd the provider starts as it always did, whether it is
// the native runtime or a launcher script: at the first operation that needs
// it, after the hooks, and it receives no credential.
func TestFlagOffStartsTheCacheProviderAfterTheHooks(t *testing.T) {
	t.Parallel()
	for name, start := range map[string]int{"native runtime": nativeProvider, "launcher script": launcherProvider} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := runProviderBuild(t, false, start)
			want := []string{hookRan, tamperedStarted, providerStarted, taskRan}
			if !slices.Equal(got, want) {
				t.Errorf("local build log = %q, want %q", got, want)
			}
		})
	}
}
