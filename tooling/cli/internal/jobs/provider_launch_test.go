package jobs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	cache "go.putnami.dev/protocol/cache"
	protocolcli "go.putnami.dev/protocol/cli"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	registryproto "go.putnami.dev/protocol/registry"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
	"go.putnami.dev/tooling/cli/internal/store"
)

// fakeRuntimeInfoEnv carries the runtime-info document this test binary
// answers the CLI's runtime handshake with when it runs as an extension
// runtime (see TestMain).
const fakeRuntimeInfoEnv = "PUTNAMI_JOBS_FAKE_RUNTIME_INFO"

// providerRuntimeExecutable is the runtime.executable the provider fixtures
// declare. On Windows the CLI runs it as compiled/putnami-acme.exe.
const providerRuntimeExecutable = "compiled/putnami-acme"

// runtimeInfoFor is the handshake answer that verifies ext on this machine.
func runtimeInfoFor(t *testing.T, ext *extension.ExtensionDescription) string {
	t.Helper()
	data, err := json.Marshal(runtimeproto.Info{
		Extension:       ext.Name,
		Version:         ext.Version,
		Platform:        runtime.GOOS + "/" + runtime.GOARCH,
		CLIContract:     protocolcli.CurrentContract,
		RuntimeProtocol: runtimeproto.MaxKnownProtocolVersion,
		RuntimeABI:      runtimeproto.RuntimeABIVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// runtimeProviderExtension is an installed cache-provider extension whose
// provider command runs its declared runtime, the way an archive that ships
// compiled/<name> declares it. No executable is placed.
func runtimeProviderExtension(t *testing.T, env map[string]string) *extension.ExtensionDescription {
	t.Helper()
	ext := fakeProviderExtension(t, env)
	ext.Runtime = &extension.RuntimeDefinition{Executable: providerRuntimeExecutable}
	job := ext.Jobs[cache.ProviderCommandName]
	job.Command = "{extensionRuntime}"
	job.Args = []string{"cache-provider", "--runtime", "{extensionRuntime}"}
	job.Env["FAKE_RUNTIME_SEEN"] = "{extensionRuntime}"
	return ext
}

// A cache provider declared as {extensionRuntime} launches the runtime the CLI
// verified with the runtime-info handshake: compiled/<name>, and
// compiled/<name>.exe on Windows. Every template the command carries names the
// same executable.
func TestLoadRemoteCacheLaunchesThePreparedProviderRuntime(t *testing.T) {
	spectest.Proves(t, "cli/windows-consumers", "providers-run-the-prepared-runtime", "a-cache-provider-launches-the-prepared-runtime")
	enableProviderRemoteCache(t)
	ext := runtimeProviderExtension(t, nil)
	want := fixtureproc.Write(t, filepath.Join(ext.Path, filepath.FromSlash(providerRuntimeExecutable)),
		fixtureproc.Program{Stdout: runtimeInfoFor(t, ext)})
	if runtime.GOOS == "windows" && !strings.HasSuffix(want, ".exe") {
		t.Fatalf("the Windows runtime fixture is %s, want the .exe name", want)
	}

	remote, notice := LoadRemoteCache(context.Background(), t.TempDir(), []*extension.ExtensionDescription{ext}, nil, store.CacheTrustAny)
	if remote == nil || !remote.isProviderBacked() {
		t.Fatalf("remote cache is not provider-backed (nil %v), notice %q", remote == nil, notice)
	}
	defer remote.Close()
	if notice != "" {
		t.Fatalf("a provider whose runtime prepares must load without a notice, got %q", notice)
	}
	launch := remote.provider.launch
	if launch.Command != want {
		t.Fatalf("provider command = %q, want the prepared runtime %q", launch.Command, want)
	}
	if got := strings.Join(launch.Args, " "); got != "cache-provider --runtime "+want {
		t.Fatalf("provider args = %q, want the runtime expanded", got)
	}
	// The message never prints launch.Env: it carries this process's environment.
	if !slices.Contains(launch.Env, "FAKE_RUNTIME_SEEN="+want) {
		t.Fatalf("provider env does not carry FAKE_RUNTIME_SEEN=%s", want)
	}
	if ext.RuntimeExecutable != want {
		t.Fatalf("extension runtime = %q, want %q recorded by the preparation", ext.RuntimeExecutable, want)
	}
}

// The prepared runtime is a real provider: started from its compiled/<name>
// path, it completes the initialize handshake, on every OS.
func TestProviderRemoteCacheStartsThePreparedRuntime(t *testing.T) {
	spectest.Proves(t, "cli/windows-consumers", "providers-run-the-prepared-runtime", "a-cache-provider-launches-the-prepared-runtime")
	enableProviderRemoteCache(t)
	// A race-instrumented binary otherwise sleeps a second before it exits.
	// The handshake inherits this process's environment; the provider gets
	// its own from the manifest.
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	ext := runtimeProviderExtension(t, map[string]string{"GORACE": "atexit_sleep_ms=0"})
	want := fixtureproc.Binary(t, filepath.Join(ext.Path, filepath.FromSlash(providerRuntimeExecutable)))
	t.Setenv(fakeRuntimeInfoEnv, runtimeInfoFor(t, ext))
	// The handshake has a deadline, so the copy is warmed first.
	fixtureproc.Warm(t, want, "__putnami", "runtime-info")

	wsRoot := t.TempDir()
	remote, notice := LoadRemoteCache(context.Background(), wsRoot, []*extension.ExtensionDescription{ext}, nil, store.CacheTrustAny)
	if remote == nil || !remote.isProviderBacked() {
		t.Fatalf("remote cache is not provider-backed (nil %v), notice %q", remote == nil, notice)
	}
	defer remote.Close()
	if got := remote.provider.launch.Command; got != want {
		t.Fatalf("provider command = %q, want the prepared runtime %q", got, want)
	}
	if sess, ready := remote.ensureProvider(context.Background(), nil, nil); sess == nil || !ready {
		t.Fatalf("the prepared runtime did not start as a ready provider: session started %v, ready %v", sess != nil, ready)
	}
}

// A provider runtime the CLI cannot prepare leaves the build local, with the
// one-line notice that names the extension and the runtime failure.
func TestLoadRemoteCacheBuildsLocallyWhenTheProviderRuntimeIsMissing(t *testing.T) {
	spectest.Proves(t, "cli/windows-consumers", "providers-run-the-prepared-runtime", "an-unprepared-cache-provider-runtime-builds-locally-with-a-notice")
	enableProviderRemoteCache(t)
	ext := runtimeProviderExtension(t, nil)

	remote, notice := LoadRemoteCache(context.Background(), t.TempDir(), []*extension.ExtensionDescription{ext}, nil, store.CacheTrustAny)
	if remote != nil {
		remote.Close()
		t.Fatal("a provider whose runtime is missing must build locally, got a remote cache")
	}
	for _, want := range []string{fakeProviderExtensionName, extensionproto.FailureRuntimeExecutableMissing, "building locally"} {
		if !strings.Contains(notice, want) {
			t.Fatalf("notice %q does not name %q", notice, want)
		}
	}
}

// loadRuntimeProvider loads the provider-backed remote cache for ext and closes
// it, failing the test unless the provider resolved without a notice.
func loadRuntimeProvider(t *testing.T, ext *extension.ExtensionDescription) {
	t.Helper()
	remote, notice := LoadRemoteCache(context.Background(), t.TempDir(), []*extension.ExtensionDescription{ext}, nil, store.CacheTrustAny)
	if remote == nil || !remote.isProviderBacked() {
		t.Fatalf("remote cache is not provider-backed (nil %v), notice %q", remote == nil, notice)
	}
	remote.Close()
	if notice != "" {
		t.Fatalf("a provider whose runtime prepares must load without a notice, got %q", notice)
	}
}

// A provider runtime is verified once per process for each runtime identity.
// Loading the cache again, from the same description or from a second
// discovery of the same installed extension, runs no second handshake. A
// failed handshake is not remembered, and a runtime file that changed is
// verified again.
func TestProviderRuntimeIsVerifiedOncePerRuntimeIdentity(t *testing.T) {
	enableProviderRemoteCache(t)
	ext := runtimeProviderExtension(t, nil)
	path := filepath.Join(ext.Path, filepath.FromSlash(providerRuntimeExecutable))
	handshakes := filepath.Join(t.TempDir(), "handshakes.jsonl")
	runs := func() int { return len(fixtureproc.Runs(t, handshakes)) }

	failing := fixtureproc.Write(t, path, fixtureproc.Program{Record: handshakes, Stderr: "not a runtime", Exit: 3})
	remote, notice := LoadRemoteCache(context.Background(), t.TempDir(), []*extension.ExtensionDescription{ext}, nil, store.CacheTrustAny)
	if remote != nil {
		remote.Close()
		t.Fatal("a runtime that fails its handshake must leave the build local")
	}
	if !strings.Contains(notice, extensionproto.FailureRuntimeHandshakeFailed) {
		t.Fatalf("notice %q does not name %s", notice, extensionproto.FailureRuntimeHandshakeFailed)
	}

	if err := os.Remove(failing); err != nil {
		t.Fatal(err)
	}
	want := fixtureproc.Write(t, path, fixtureproc.Program{Record: handshakes, Stdout: runtimeInfoFor(t, ext)})
	loadRuntimeProvider(t, ext)
	if got := runs(); got != 2 {
		t.Fatalf("handshakes = %d after a failed and a good runtime, want 2: a failure is not remembered", got)
	}
	loadRuntimeProvider(t, ext)
	rediscovered := *ext
	rediscovered.RuntimeExecutable, rediscovered.RuntimeDigest = "", ""
	loadRuntimeProvider(t, &rediscovered)
	if got := runs(); got != 2 {
		t.Fatalf("handshakes = %d after two more loads of the same runtime, want still 2", got)
	}
	if rediscovered.RuntimeExecutable != want {
		t.Fatalf("a second discovery runs %q, want the verified runtime %q", rediscovered.RuntimeExecutable, want)
	}

	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(want, later, later); err != nil {
		t.Fatal(err)
	}
	loadRuntimeProvider(t, ext)
	if got := runs(); got != 3 {
		t.Fatalf("handshakes = %d after the runtime file changed, want 3", got)
	}
}

// A provider that runs a bare `putnami`, as the cache provider's token command
// does, reaches the CLI that launched it: a source workspace refuses every
// other binary, which silently turned the remote cache off.
func TestProviderLaunchFindsThisCLIFirst(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	launch, err := PrepareProviderLaunch(context.Background(), t.TempDir(), fakeProviderExtension(t, nil), cache.ProviderCommandName)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Split(envLastValue(launch.Env, "PATH"), string(os.PathListSeparator))[0]; got != filepath.Dir(self) {
		t.Fatalf("provider PATH starts with %q, want this CLI's directory %q", got, filepath.Dir(self))
	}
	if got := envLastValue(launch.Env, registryproto.CLIExecutableEnv); got != self {
		t.Fatalf("provider %s = %q, want %q", registryproto.CLIExecutableEnv, got, self)
	}
}
