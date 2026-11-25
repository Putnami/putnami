package scheduler

import (
	"strings"

	"go.putnami.dev/tooling/cli/internal/store"
)

// NewRunCacheManager builds the one cache manager a run uses on storeRoot, the
// store the run resolved once (store.ResolveStoreRoot), honoring a
// cache-verification store override. It is built even under --no-cache: the
// memoized file hashing it carries is what lets the execution keys and a
// release-set publish's selection keys share one pass over the tree, and the
// caller drops it before anything consumes it as a cache of results.
func NewRunCacheManager(storeRoot, storeRootOverride string) *store.CacheManager {
	if strings.TrimSpace(storeRootOverride) != "" {
		storeRoot = storeRootOverride
	}
	return store.NewCacheManager(store.NewLocalStore(storeRoot))
}
