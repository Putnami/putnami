package runtime

import (
	"os"
	"sort"
	"strings"
	"sync"

	config "go.putnami.dev/config"
)

// SourceDiscoverer discovers a cloud config source from the environment,
// returning nil when CONFIG_SERVER_URL and its companion variables do not
// enable it. It has the shape of config.SourceDiscoverer.
type SourceDiscoverer = func() config.Source

// Every remote source blocks on the network, so each one lets
// config.LoadContext cancel it.
var (
	_ config.ContextSource = (*RemoteConfigSource)(nil)
	_ config.ContextSource = (*RemoteSecretsSource)(nil)
	_ config.ContextSource = (*PreparedBootSource)(nil)
)

// Register hands this package's config-source discoverers to a registrar:
// ordinary remote config and secrets, then the protected prepared boot source.
// Discovery makes the prepared source exclusive when PUTNAMI_CONFIG_BOOT_BINDING
// is set. Priority lives on the source objects, so the order relative to the
// loader's local file (35) and CONFIG_DATA (60) sources is fixed.
//
// The activate subpackage calls it from init():
//
//	func init() { runtime.Register(config.RegisterSourceDiscoverer) }
//
// A discoverer that finds no source returns a nil interface, never a nil
// pointer boxed in a non-nil config.Source, so the loader's nil check skips it.
func Register(register func(SourceDiscoverer)) {
	// A workload may call config.Load once per schema. Discovery therefore has to
	// return one process-wide source instance: RemoteConfigSource caches the
	// first completed load of the full resolved tree, so every schema path shares
	// one fetch, one retry budget, and one snapshot fallback instead of serially
	// multiplying the startup delay.
	register(memoizeDiscovery(func() config.Source {
		s := DiscoverRemoteSource()
		if s == nil {
			return nil
		}
		return s
	}))
	register(memoizeDiscovery(func() config.Source {
		s := DiscoverRemoteSecretsSource()
		if s == nil {
			return nil
		}
		return s
	}))
	register(memoizeDiscovery(func() config.Source {
		s := DiscoverPreparedBootSource()
		if s == nil {
			return nil
		}
		return s
	}))
}

// memoizeDiscovery returns the same source while the process environment is
// unchanged. Production container env is immutable, which makes this a
// process-lifetime source; keying the cache also keeps tests and embedders that
// intentionally replace env values from observing stale discovery state.
func memoizeDiscovery(discover func() config.Source) SourceDiscoverer {
	var mu sync.Mutex
	var key string
	var source config.Source
	var initialized bool
	return func() config.Source {
		env := os.Environ()
		sort.Strings(env)
		current := strings.Join(env, "\x00")
		mu.Lock()
		defer mu.Unlock()
		if !initialized || current != key {
			source = discover()
			key = current
			initialized = true
		}
		return source
	}
}
