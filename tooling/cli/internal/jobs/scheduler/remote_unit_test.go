package scheduler

import (
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/jobs/remotecache"
)

// newUploadTestCache builds a minimal RemoteCache for exercising the pure
// upload-eligibility logic (prepareUpload), which needs only the stats sink — no
// provider session or network.
func newUploadTestCache() *remotecache.RemoteCache {
	r := &remotecache.RemoteCache{}
	r.SetCacheStats(&cachekey.CacheStats{})
	return r
}
