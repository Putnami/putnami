package remotecache

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

// AllowInsecureCacheEnv lets users explicitly accept a plaintext http:// cache
// or token URL. The default is to reject anything that isn't https so a
// misconfigured or tampered cache URL (via .putnami/cache.json or the
// PUTNAMI_CACHE_URL override) cannot silently carry the per-user bearer token
// over an unauthenticated, MITM-able channel. This mirrors the secure-by-default
// bar the extension registry already enforces for its (secretless) artifact
// channel.
const AllowInsecureCacheEnv = "PUTNAMI_ALLOW_INSECURE_CACHE"

// ValidateURL returns nil when rawURL is safe to carry the per-user bearer
// token over. https:// is always accepted. http:// is accepted only for
// loopback hosts (localhost, 127.0.0.1, ::1) — the routine dev case — or when
// the user opted in via PUTNAMI_ALLOW_INSECURE_CACHE=1. Other schemes are
// rejected outright.
func ValidateURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid cache URL %q: %w", rawURL, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return nil
	case "http":
		if isLoopbackHost(u.Hostname()) {
			return nil
		}
		if os.Getenv(AllowInsecureCacheEnv) == "1" {
			return nil
		}
		return fmt.Errorf(
			"cache URL %q uses plaintext http://; refusing to carry the cache token over an unauthenticated channel. "+
				"Switch to https://, or set %s=1 to override",
			rawURL, AllowInsecureCacheEnv,
		)
	default:
		return fmt.Errorf("cache URL %q has unsupported scheme %q (only https and http are accepted)", rawURL, u.Scheme)
	}
}

func isLoopbackHost(host string) bool {
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1", "[::1]":
		return true
	}
	return false
}
