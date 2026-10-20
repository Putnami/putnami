package remotecache

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	cache "go.putnami.dev/protocol/cache"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/cacheprovider"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
)

// loadFakeProvider is the provider-backed remote cache of wsRoot, served by
// the fake provider with env, whose notices go to the returned buffer.
func loadFakeProvider(t *testing.T, wsRoot string, env map[string]string) (*RemoteCache, *bytes.Buffer) {
	t.Helper()
	remote, notice := LoadRemoteCache(context.Background(), wsRoot,
		[]*extension.ExtensionDescription{fakeProviderExtension(t, env)}, nil, store.CacheTrustAny, fakeProviderLaunch)
	if remote == nil || !remote.IsProviderBacked() || notice != "" {
		t.Fatalf("remote %+v, notice %q; want a provider-backed remote cache and no notice", remote, notice)
	}
	t.Cleanup(remote.Close)
	notices := &bytes.Buffer{}
	remote.provider.notices = notices
	return remote, notices
}

// readLines returns the lines of path, or none when it does not exist.
func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(data))
}

// The core sends no mode and lists provider-configuration. It materializes in
// the mode a provider returns with the echo, and in full without the echo.
func TestProviderMode_TheProviderResolvesIt(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "the-cache-provider-owns-its-configuration", "core-materializes-in-the-mode-the-provider-returns")
	useInProcessFakeProvider(t)
	for _, tc := range []struct {
		name     string
		resolved cache.Mode
		want     cache.Mode
	}{
		{name: "minimal echoed", resolved: cache.ModeMinimal, want: cache.ModeMinimal},
		{name: "toplevel echoed", resolved: cache.ModeToplevel, want: cache.ModeToplevel},
		{name: "no echo", want: cache.ModeFull},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := t.TempDir()
			capabilitiesOut, modeOut := filepath.Join(out, "capabilities"), filepath.Join(out, "mode")
			env := map[string]string{"FAKE_CAPABILITIES_OUT": capabilitiesOut, "FAKE_INIT_MODE_OUT": modeOut}
			if tc.resolved != "" {
				env["FAKE_PROVIDER_MODE"] = string(tc.resolved)
			}
			remote, notices := loadFakeProvider(t, t.TempDir(), env)
			if err := remote.StartProvider(context.Background(), nil, nil); err != nil {
				t.Fatal(err)
			}
			remote.provider.mu.Lock()
			got, ready := remote.provider.mode, remote.provider.ready
			remote.provider.mu.Unlock()
			if !ready || got != tc.want {
				t.Errorf("ready %t, mode %q; want ready and %q", ready, got, tc.want)
			}
			if capabilities := strings.Split(strings.Join(readLines(t, capabilitiesOut), ""), ","); !slices.Contains(capabilities, cache.CapabilityProviderConfiguration) {
				t.Errorf("initialize listed %v, want %q", capabilities, cache.CapabilityProviderConfiguration)
			}
			if sent := readLines(t, modeOut); len(sent) != 0 {
				t.Errorf("initialize sent mode %q, want none", sent)
			}
			if notices.Len() != 0 {
				t.Errorf("notices = %q, want none", notices.String())
			}
		})
	}
}

// A provider that answers Ready=false is the unconfigured default: the run
// builds locally with no notice, the provider process stops at once, its
// exchange directory goes, and no later operation starts it again.
func TestEnsureProvider_NotReadyStopsSilently(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "the-cache-provider-owns-its-configuration", "a-provider-that-is-not-ready-stops-silently")
	useInProcessFakeProvider(t)
	wsRoot := t.TempDir()
	opsOut := filepath.Join(t.TempDir(), "ops")
	remote, notices := loadFakeProvider(t, wsRoot, map[string]string{"FAKE_NOT_READY": "1", "FAKE_OPS_OUT": opsOut})

	if err := remote.StartProvider(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := remote.LookupRunMarker(context.Background(), "ws-id", "main", []string{"build"}, ""); ok {
		t.Error("a provider that is not ready served a run marker")
	}
	remote.PublishRunMarker(context.Background(), "ws-id", "main", []string{"build"}, "", "deadbeef", "")
	if err := remote.StartProvider(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}

	if ops := readLines(t, opsOut); !slices.Equal(ops, []string{"initialize", "shutdown"}) {
		t.Errorf("provider ops = %q, want one initialize, then the shutdown that stops it", ops)
	}
	if dir := remote.providerExchangeDir(); dir != "" {
		t.Errorf("exchange dir = %q, want none", dir)
	}
	remote.provider.sweep.Wait()
	if names := exchangeDirsUnder(t, filepath.Join(wsRoot, ".putnami")); len(names) != 0 {
		t.Errorf("exchange directories left: %v", names)
	}
	if notices.Len() != 0 {
		t.Errorf("notices = %q, want none", notices.String())
	}
}

// A declared provider that fails to start, or whose initialize handshake
// fails, says so once, names its extension, and leaves the run building
// locally: no later operation starts it again or repeats the line.
func TestEnsureProvider_ADeclaredProviderThatCannotServeSaysSoOnce(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "a-declared-provider-that-cannot-serve-says-so-once", "a-provider-that-fails-to-start-or-to-initialize-says-so-once")
	for _, tc := range []struct {
		name   string
		env    map[string]string
		spawn  func(context.Context, cacheprovider.LaunchSpec, ...cacheprovider.Option) (*cacheprovider.Session, error)
		reason string
	}{
		{
			name: "start",
			spawn: func(context.Context, cacheprovider.LaunchSpec, ...cacheprovider.Option) (*cacheprovider.Session, error) {
				return nil, errors.New("provider binary missing")
			},
			reason: "failed to start (provider binary missing)",
		},
		{
			name:   "handshake",
			env:    map[string]string{"FAKE_INITIALIZE_FAIL": "1"},
			reason: "failed its initialize handshake",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useInProcessFakeProvider(t)
			if tc.spawn != nil {
				SpawnProviderSession = tc.spawn
			}
			remote, notices := loadFakeProvider(t, t.TempDir(), tc.env)
			for range 2 {
				if err := remote.StartProvider(context.Background(), nil, nil); err != nil {
					t.Fatal(err)
				}
				if _, ok := remote.LookupRunMarker(context.Background(), "ws-id", "main", []string{"build"}, ""); ok {
					t.Fatal("a provider that cannot serve served a run marker")
				}
			}
			lines := strings.Split(strings.TrimSuffix(notices.String(), "\n"), "\n")
			if len(lines) != 1 {
				t.Fatalf("notices = %q, want one line", notices.String())
			}
			for _, want := range []string{"putnami: the cache provider of " + fakeProviderExtensionName, tc.reason, "building locally"} {
				if !strings.Contains(lines[0], want) {
					t.Errorf("notice %q lacks %q", lines[0], want)
				}
			}
		})
	}
}

// A provider whose run ended before it answered says nothing: the failure is
// the cancellation, not the provider.
func TestEnsureProvider_ACanceledStartSaysNothing(t *testing.T) {
	useInProcessFakeProvider(t)
	SpawnProviderSession = func(context.Context, cacheprovider.LaunchSpec, ...cacheprovider.Option) (*cacheprovider.Session, error) {
		return nil, context.Canceled
	}
	remote, notices := loadFakeProvider(t, t.TempDir(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := remote.StartProvider(ctx, nil, nil); err != nil {
		t.Fatal(err)
	}
	if notices.Len() != 0 {
		t.Errorf("notices = %q, want none", notices.String())
	}
}

// A run loads its remote cache once: every Load of one RunRemoteCache returns
// the first load's cache and notice, so the run's stages share one provider,
// and Close stops it. A nil RunRemoteCache loads a new one each time.
func TestRunRemoteCache_LoadsOnce(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "the-cache-provider-owns-its-configuration", "one-provider-per-run")
	useInProcessFakeProvider(t)
	opsOut := filepath.Join(t.TempDir(), "ops")
	exts := []*extension.ExtensionDescription{fakeProviderExtension(t, map[string]string{"FAKE_OPS_OUT": opsOut})}
	wsRoot := t.TempDir()

	run := &RunRemoteCache{}
	first, _ := run.Load(context.Background(), wsRoot, exts, nil, store.CacheTrustAny, fakeProviderLaunch)
	second, _ := run.Load(context.Background(), wsRoot, nil, nil, store.CacheTrustCI, nil)
	if first == nil || second != first {
		t.Fatalf("loads = %p, %p; want one remote cache", first, second)
	}
	if err := first.StartProvider(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := second.StartProvider(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	first.Close()
	run.Close()
	if ops := readLines(t, opsOut); !slices.Equal(ops, []string{"initialize", "shutdown"}) {
		t.Errorf("provider ops = %q, want one provider started and stopped once", ops)
	}

	var none *RunRemoteCache
	a, _ := none.Load(context.Background(), wsRoot, exts, nil, store.CacheTrustAny, fakeProviderLaunch)
	b, _ := none.Load(context.Background(), wsRoot, exts, nil, store.CacheTrustAny, fakeProviderLaunch)
	defer a.Close()
	defer b.Close()
	if a == nil || a == b {
		t.Errorf("nil holder loads = %p, %p; want two remote caches", a, b)
	}
	none.Close()
}
