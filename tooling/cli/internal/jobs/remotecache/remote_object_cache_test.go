package remotecache

import (
	"testing"

	cache "go.putnami.dev/protocol/cache"
	"go.putnami.dev/tooling/cli/internal/cacheprovider"
	"go.putnami.dev/tooling/cli/internal/store"
)

// TestObjectCacheJobEnvFailsClosed covers, at the accessor, every case where
// the two variables must not be exported at all.
func TestObjectCacheJobEnvFailsClosed(t *testing.T) {
	var nilCache *RemoteCache
	if got := nilCache.ObjectCacheJobEnv(); got != nil {
		t.Fatalf("nil cache env = %v, want none", got)
	}
	if got := nilCache.ObjectCacheSocket(); got != "" {
		t.Fatalf("nil cache socket = %q, want empty", got)
	}

	// A provider-backed cache that has not negotiated a socket exports nothing.
	local := newProviderRemoteCache(t.TempDir(), cacheprovider.LaunchSpec{Command: "/bin/true"}, store.CacheTrustAny)
	if got := local.ObjectCacheJobEnv(); got != nil {
		t.Fatalf("un-negotiated env = %v, want none", got)
	}

	// A socket with a disabled trust policy stays unexported: the object cache
	// follows the same fail-closed rule the task cache does.
	local.provider.ready = true
	local.provider.objectCacheSocket = "/tmp/objects.sock"
	local.Trust = store.CacheTrustNone
	if got := local.ObjectCacheJobEnv(); got != nil {
		t.Fatalf("trust none env = %v, want none", got)
	}
	local.Trust = store.CacheTrustCI
	want := []string{
		cache.ObjectCacheSocketEnv + "=/tmp/objects.sock",
		cache.CacheTrustEnv + "=ci",
	}
	got := local.ObjectCacheJobEnv()
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("env = %v, want %v", got, want)
	}
}
