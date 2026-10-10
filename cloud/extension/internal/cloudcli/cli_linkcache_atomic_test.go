package cloudcli

import (
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
)

// errPartialLink marks a read that returned a cloud-link.json with no
// workspace_id — a truncation-adjacent failure the concurrency test guards.
var errPartialLink = errors.New("read a cloud-link with no workspace_id")

// TestWriteLinkCacheConcurrentReadNeverTruncated pins the atomic
// cloud-link.json writer: a killed `install`/`setup` must never leave a partial
// cloud-link.json for the cache-provider's token command to choke on.
// Concurrent writers rewrite the cache while readers load it; every read must
// observe either the old or a complete new JSON, never a truncated one.
func TestWriteLinkCacheConcurrentReadNeverTruncated(t *testing.T) {
	t.Parallel()
	// A workspace root with NO manifest link so cloud-link.json is the only
	// state under test (writeLinkCache only touches the cache file).
	root := t.TempDir()

	link := func(marker string) map[string]any {
		return map[string]any{
			"workspace_id":      "ws-" + marker,
			"control_plane_url": "https://api.putnami.cloud",
		}
	}

	// Seed a valid cache so readers always have a complete file to observe.
	if err := writeLinkCache(root, link("seed")); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	const writers, readers, iterations = 4, 8, 250

	var writersWg, readersWg sync.WaitGroup
	stop := make(chan struct{})
	errs := make(chan error, writers+readers)

	for id := range writers {
		writersWg.Go(func() {
			for range iterations {
				if err := writeLinkCache(root, link(string(rune('a'+id)))); err != nil {
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
				data, err := os.ReadFile(linkPath(root))
				if err != nil {
					errs <- err
					return
				}
				var got map[string]any
				if err := json.Unmarshal(data, &got); err != nil {
					errs <- err
					return
				}
				if stringValue(got["workspace_id"]) == "" {
					errs <- errPartialLink
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
		t.Fatalf("reader observed a partial/invalid cloud-link.json: %v", err)
	}
}
