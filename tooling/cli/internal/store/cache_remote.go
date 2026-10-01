package store

import (
	"io"

	cache "go.putnami.dev/protocol/cache"
)

// CacheTrust controls which remote cache entries may satisfy a job. It does
// not affect the local store: local entries remain eligible under every mode.
type CacheTrust string

const (
	// CacheTrustAny accepts every provider hit, including legacy entries whose
	// provider predates provenance. This is the local-development default.
	CacheTrustAny CacheTrust = "any"
	// CacheTrustCI accepts only entries on the provider-authoritative trusted
	// channel. Hint and legacy entries may warm CAS blobs but cannot satisfy jobs.
	CacheTrustCI CacheTrust = "ci"
	// CacheTrustNone disables the remote cache entirely.
	CacheTrustNone CacheTrust = "none"
)

// Valid reports whether t is a supported remote-cache trust policy.
func (t CacheTrust) Valid() bool {
	switch t {
	case CacheTrustAny, CacheTrustCI, CacheTrustNone:
		return true
	default:
		return false
	}
}

// Accepts reports whether a provider hit on channel may satisfy a job. An
// empty channel is the compatibility signal for a v1 provider: accepted by
// "any", treated as a non-authoritative hint by "ci".
func (t CacheTrust) Accepts(channel cache.Channel) bool {
	switch t {
	case CacheTrustAny:
		return true
	case CacheTrustCI:
		return channel == cache.ChannelTrusted
	default:
		return false
	}
}

// AllowsRemoteRunMarkers reports whether remote successful-run markers may
// influence project selection. The marker protocol does not carry provider
// provenance yet, so only the explicitly permissive "any" policy may use it.
// Authoritative and disabled policies fail closed to local marker state.
func (t CacheTrust) AllowsRemoteRunMarkers() bool {
	return t == CacheTrustAny
}

// Remote-cache integration surface on CacheManager. The remote build cache
// needs two capabilities beyond entry lookup and storage: read raw CAS blobs
// (the upload source) and atomically materialize a remote hit into a local
// entry (the download sink). Both live on LocalStore; exposing them through
// CacheManager keeps job-execution code talking to a single cache facade.

// OpenBlob returns a reader for the CAS blob addressed by digest, satisfying
// the remote-cache upload path's blob source.
func (cm *CacheManager) OpenBlob(digest string) (io.ReadCloser, error) {
	return cm.store.OpenBlob(digest)
}

// Materialize installs a remote cache hit as a local entry under hash, fetching
// any missing blob through fetch (see LocalStore.Materialize).
func (cm *CacheManager) Materialize(hash string, entry *Entry, fetch BlobFetcher) error {
	return cm.store.Materialize(hash, entry, fetch)
}

// PrefetchBlobs verifies and installs a remote manifest's missing blobs into
// the local CAS without publishing an action-cache entry. Authoritative runs
// use this for hint hits: a following real execution can reuse the bytes while
// the hint can never turn the job green.
func (cm *CacheManager) PrefetchBlobs(manifest *cache.Manifest, fetch BlobFetcher) error {
	return cm.store.PrefetchBlobs(manifest, fetch)
}
