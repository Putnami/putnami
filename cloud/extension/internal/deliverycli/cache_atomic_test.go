package deliverycli

import (
	"errors"
	"os"
	"sync"
	"testing"
)

// errEmptyCache marks a read that returned a config with no URL — treated as a
// truncation-adjacent failure in the concurrency test below.
var errEmptyCache = errors.New("read a cache config with no URL")

// TestWriteCacheConfigConcurrentReadNeverTruncated pins the atomicity of the
// cache.json writer: a killed `install`/`setup` must never leave a partial
// cache.json for the cache-provider's token command to choke on. Concurrent
// writers rewrite the file while readers load it; every read must return a
// complete config, never a truncated/parse error.
func TestWriteCacheConfigConcurrentReadNeverTruncated(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	cfg := func(marker string) *CacheConfig {
		return &CacheConfig{
			Enabled: true,
			URL:     "https://cache.putnami.cloud",
			Mode:    "full-" + marker,
		}
	}

	// Seed a valid file so readers always have a complete config to observe.
	if err := WriteCacheConfig(root, cfg("seed")); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	const writers, readers, iterations = 4, 8, 250

	var writersWg, readersWg sync.WaitGroup
	stop := make(chan struct{})
	errs := make(chan error, writers+readers)

	for id := range writers {
		writersWg.Go(func() {
			for range iterations {
				if err := WriteCacheConfig(root, cfg(string(rune('a'+id)))); err != nil {
					errs <- err
					return
				}
			}
		})
	}

	for range readers {
		readersWg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				// ReadCacheConfig json.Unmarshals; a truncated read surfaces as
				// a non-nil parse error here. A complete read returns a non-nil
				// config (a complete file always exists once seeded).
				got, err := ReadCacheConfig(root)
				if err != nil {
					errs <- err
					return
				}
				if got == nil || got.URL == "" {
					errs <- errEmptyCache
					return
				}
			}
		})
	}

	writersWg.Wait()
	close(stop)
	readersWg.Wait()
	close(errs)

	for err := range errs {
		t.Fatalf("reader observed a partial/invalid cache.json: %v", err)
	}
}

// TestWriteCacheConfigPermissions confirms the atomic write keeps cache.json at
// 0644 (the temp+rename path must not widen or narrow permissions).
func TestWriteCacheConfigPermissions(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := WriteCacheConfig(root, &CacheConfig{Enabled: true, URL: "https://cache.putnami.cloud", Mode: "full"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	info, err := os.Stat(CachePath(root))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Fatalf("cache.json perm = %o, want 644", perm)
	}
}
