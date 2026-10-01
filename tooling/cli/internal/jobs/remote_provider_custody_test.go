package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"go.putnami.dev/tooling/cli/internal/cacheprovider"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/store"
)

// The tests below change the run credential of the whole process, so none of
// them runs in parallel.

// custodyProviderEnv makes the fake provider echo the run-credential
// capability and serve a run marker.
var custodyProviderEnv = map[string]string{"FAKE_RUN_CREDENTIAL_ECHO": "1", "FAKE_MARKER_SHA": "cafef00d"}

// storeRuntimeProviderExtension is runtimeProviderExtension installed in the
// artifact store, as a hosted run requires: preparing its runtime starts no
// repository code.
func storeRuntimeProviderExtension(t *testing.T) *extension.ExtensionDescription {
	t.Helper()
	ext := runtimeProviderExtension(t, custodyProviderEnv)
	artifacts := t.TempDir()
	t.Setenv("PUTNAMI_ARTIFACT_DIR", artifacts)
	ext.Path = filepath.Join(artifacts, "extensions", "acme-cache@"+fakeProviderVersion)
	if err := os.MkdirAll(ext.Path, 0o755); err != nil {
		t.Fatal(err)
	}
	return ext
}

// nativeProviderExtension is a cache-provider extension whose command is
// {extensionRuntime}, with a native runtime that answers the handshake: the
// only provider a hosted run starts.
func nativeProviderExtension(t *testing.T) *extension.ExtensionDescription {
	t.Helper()
	ext := storeRuntimeProviderExtension(t)
	fixtureproc.Write(t, filepath.Join(ext.Path, filepath.FromSlash(providerRuntimeExecutable)),
		fixtureproc.Program{Stdout: runtimeInfoFor(t, ext)})
	return ext
}

// custodyProviderCache is a provider-backed remote cache for ext over the
// in-process fake provider, which serves a run marker, and the number of
// provider processes started so far. bearer, when set, is handed to the
// provider.
func custodyProviderCache(t *testing.T, bearer string, ext *extension.ExtensionDescription) (*RemoteCache, *atomic.Int32) {
	t.Helper()
	enableProviderRemoteCache(t)
	useInProcessFakeProvider(t)
	inProcess := spawnProviderSession
	spawns := new(atomic.Int32)
	spawnProviderSession = func(ctx context.Context, spec cacheprovider.LaunchSpec, opts ...cacheprovider.Option) (*cacheprovider.Session, error) {
		spawns.Add(1)
		return inProcess(ctx, spec, opts...)
	}
	t.Cleanup(func() { spawnProviderSession = inProcess })
	var options []RemoteCacheOption
	if bearer != "" {
		options = append(options, WithCacheRunCredential(bearer))
	}
	remote, notice := LoadRemoteCache(context.Background(), t.TempDir(), []*extension.ExtensionDescription{ext}, nil, store.CacheTrustAny, options...)
	if remote == nil || notice != "" {
		t.Fatalf("expected a provider-backed remote cache, got %+v, notice %q", remote, notice)
	}
	t.Cleanup(remote.Close)
	return remote, spawns
}

// On a hosted run the cache provider receives the run credential, so it
// starts only before repository code. After a hook ran, StartProvider refuses
// it with an error that names the provider and the hook, every later call
// refuses it again, and no operation starts it: the run cannot fall back to a
// provider started late.
func TestHostedCacheProviderAfterRepositoryCodeIsRefused(t *testing.T) {
	t.Cleanup(runcredential.SetForTest("run-bearer"))
	remote, spawns := custodyProviderCache(t, "run-bearer", nativeProviderExtension(t))
	runcredential.MarkRepositoryCodeStarted("hook hooks.commands.build.before")

	want := &runcredential.CustodyError{
		Holder: "the cache provider of " + fakeProviderExtensionName,
		Reason: "hook hooks.commands.build.before",
	}
	for attempt := range 2 {
		if err := remote.StartProvider(context.Background(), nil, nil); err == nil || err.Error() != want.Error() {
			t.Fatalf("attempt %d: StartProvider = %v, want %q", attempt, err, want.Error())
		}
	}
	if sha, ok := remote.LookupRunMarker(context.Background(), "ws-id", "main", []string{"build"}, ""); ok {
		t.Errorf("LookupRunMarker = %q after the refusal, want a miss", sha)
	}
	if n := spawns.Load(); n != 0 {
		t.Errorf("the provider was started %d times, want never", n)
	}
}

// A provider started before repository code keeps serving the run after it:
// the rule is about when a holder starts, not when it is asked.
func TestHostedCacheProviderStartedFirstServesTheRun(t *testing.T) {
	t.Cleanup(runcredential.SetForTest("run-bearer"))
	remote, spawns := custodyProviderCache(t, "run-bearer", nativeProviderExtension(t))
	if err := remote.StartProvider(context.Background(), nil, nil); err != nil {
		t.Fatalf("StartProvider before repository code = %v, want nil", err)
	}
	runcredential.MarkRepositoryCodeStarted("hook hooks.commands.build.before")

	if err := remote.StartProvider(context.Background(), nil, nil); err != nil {
		t.Errorf("StartProvider after repository code = %v, want nil for the running provider", err)
	}
	if sha, ok := remote.LookupRunMarker(context.Background(), "ws-id", "main", []string{"build"}, ""); !ok || sha != "cafef00d" {
		t.Errorf("LookupRunMarker = %q, %v; want the provider started first to serve it", sha, ok)
	}
	if n := spawns.Load(); n != 1 {
		t.Errorf("the provider was started %d times, want once", n)
	}
}

// Without --credential-fd the record does nothing: the provider starts at the
// first operation that needs it, after a hook as before.
func TestCacheProviderWithoutARunCredentialIgnoresRepositoryCode(t *testing.T) {
	t.Cleanup(runcredential.SetForTest(""))
	remote, spawns := custodyProviderCache(t, "", fakeProviderExtension(t, custodyProviderEnv))
	runcredential.MarkRepositoryCodeStarted("hook hooks.commands.build.before")

	if sha, ok := remote.LookupRunMarker(context.Background(), "ws-id", "main", []string{"build"}, ""); !ok || sha != "cafef00d" {
		t.Errorf("LookupRunMarker = %q, %v; want the provider to start and serve it", sha, ok)
	}
	if err := remote.StartProvider(context.Background(), nil, nil); err != nil {
		t.Errorf("StartProvider = %v, want nil", err)
	}
	if n := spawns.Load(); n != 1 {
		t.Errorf("the provider was started %d times, want once", n)
	}
}

// launcherProviderExtension is nativeProviderExtension whose command is a
// shell launcher in the extension root instead of {extensionRuntime}.
func launcherProviderExtension(t *testing.T) *extension.ExtensionDescription {
	t.Helper()
	ext := nativeProviderExtension(t)
	launcher := filepath.Join(ext.Path, "bin", "provider")
	if err := os.MkdirAll(filepath.Dir(launcher), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\nexec \"$0.bin\" \"$@\"\n"), 0o755); err != nil { //nolint:gosec // an executable test fixture
		t.Fatal(err)
	}
	for _, job := range ext.Jobs {
		job.Command = "{extensionRoot}/bin/provider"
	}
	return ext
}

// scriptRuntimeProviderExtension is a cache-provider extension whose command
// is {extensionRuntime} and whose runtime is a shell script that answers the
// handshake.
func scriptRuntimeProviderExtension(t *testing.T) *extension.ExtensionDescription {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("a #! runtime cannot answer the handshake on Windows")
	}
	ext := storeRuntimeProviderExtension(t)
	path := filepath.Join(ext.Path, filepath.FromSlash(providerRuntimeExecutable))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s\\n' '" + runtimeInfoFor(t, ext) + "'\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil { //nolint:gosec // an executable test fixture
		t.Fatal(err)
	}
	return ext
}

// On a hosted run the cache provider starts only as its extension's native
// runtime, command {extensionRuntime}. A command that is not the runtime, such
// as a shell launcher, or a runtime that is a script reads more store files
// after it starts, so StartProvider refuses it with an error that names the
// provider and the reason, and never starts it. Without --credential-fd each
// of them starts as before.
func TestHostedCacheProviderStartsOnlyAsItsNativeRuntime(t *testing.T) {
	for _, tc := range []struct {
		name string
		ext  func(t *testing.T) *extension.ExtensionDescription
		// want is a fragment of the refusal, or "" when the provider starts.
		want string
	}{
		{name: "the native runtime", ext: nativeProviderExtension},
		{name: "a command that is not the runtime", ext: func(t *testing.T) *extension.ExtensionDescription {
			return fakeProviderExtension(t, custodyProviderEnv)
		}, want: "is not its extension's runtime executable"},
		{name: "a launcher beside a native runtime", ext: launcherProviderExtension, want: "is not its extension's runtime executable"},
		{name: "a runtime that is a script", ext: scriptRuntimeProviderExtension, want: "is a script (#!)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ext := tc.ext(t)
			t.Cleanup(runcredential.SetForTest("run-bearer"))
			remote, spawns := custodyProviderCache(t, "run-bearer", ext)

			err := remote.StartProvider(context.Background(), nil, nil)
			if tc.want == "" {
				if err != nil || spawns.Load() != 1 {
					t.Fatalf("StartProvider = %v after %d starts; want the native runtime started once", err, spawns.Load())
				}
				return
			}
			var refusal *runcredential.NativeHolderError
			if !errors.As(err, &refusal) || refusal.Holder != "the cache provider of "+fakeProviderExtensionName ||
				!strings.Contains(refusal.Reason, tc.want) {
				t.Fatalf("StartProvider = %v, want a refusal that names the provider and %q", err, tc.want)
			}
			if again := remote.StartProvider(context.Background(), nil, nil); again == nil || again.Error() != err.Error() {
				t.Errorf("a second StartProvider = %v, want the same refusal", again)
			}
			if sha, ok := remote.LookupRunMarker(context.Background(), "ws-id", "main", []string{"build"}, ""); ok {
				t.Errorf("LookupRunMarker = %q after the refusal, want a miss", sha)
			}
			if n := spawns.Load(); n != 0 {
				t.Errorf("the provider was started %d times, want never", n)
			}

			// Without --credential-fd the same kind of provider starts.
			t.Cleanup(runcredential.SetForTest(""))
			local, localSpawns := custodyProviderCache(t, "", tc.ext(t))
			if err := local.StartProvider(context.Background(), nil, nil); err != nil || localSpawns.Load() != 1 {
				t.Errorf("without a run credential: StartProvider = %v after %d starts, want it started once", err, localSpawns.Load())
			}
		})
	}
}
