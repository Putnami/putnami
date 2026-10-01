package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	cache "go.putnami.dev/protocol/cache"
)

// Environment overrides for the persisted remote-cache configuration. They let
// CI point at a cache without writing a config file; the credentials travel to
// the cache-provider extension through its inherited process environment, so the
// CLI only needs to know whether caching is configured and in which mode.
const (
	// cacheURLEnv overrides the configured cache server URL.
	cacheURLEnv = "PUTNAMI_CACHE_URL"
	// cacheModeEnv overrides the configured materialization mode.
	cacheModeEnv = "PUTNAMI_CACHE_MODE"
)

// remoteCacheConfig is the persisted remote-cache configuration the CLI reads to
// decide whether to delegate caching to the cloud cache-provider and in which
// materialization mode. It carries no secret: the per-user bearer is resolved by
// the provider from its own environment/recipe, never by core. The cloud
// extension writes this file via its setup command; the CLI only reads it.
//
// Any `token` field a legacy file carries is ignored here (JSON unmarshalling
// drops unknown fields): the in-core HTTP client that consumed it is gone, and
// the provider obtains its own credentials.
//
// Example (.putnami/cache.json):
//
//	{ "enabled": true, "url": "https://cache.putnami.cloud", "mode": "full" }
type remoteCacheConfig struct {
	// Enabled gates remote caching. A nil value (field omitted) means enabled,
	// so a written config is active by default; set false to disable without
	// deleting the file.
	Enabled *bool `json:"enabled,omitempty"`
	// URL is the cache server base URL. Remote caching is inactive when empty.
	URL string `json:"url,omitempty"`
	// Mode is the materialization mode (minimal|toplevel|full). Empty defers to
	// the provider default.
	Mode cache.Mode `json:"mode,omitempty"`
}

// loadRemoteCacheConfigFile reads the remote-cache config from path. A missing
// file is not an error: it returns an empty (inactive) config so callers can
// treat "no file" and "remote cache off" identically.
func loadRemoteCacheConfigFile(path string) (*remoteCacheConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &remoteCacheConfig{}, nil
		}
		return nil, fmt.Errorf("read cache config %s: %w", path, err)
	}
	var c remoteCacheConfig
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse cache config %s: %w", path, err)
	}
	return &c, nil
}

// ApplyEnv overlays the PUTNAMI_CACHE_* environment overrides onto the config,
// so CI can point at a cache without a config file.
func (c *remoteCacheConfig) ApplyEnv() {
	if v := strings.TrimSpace(os.Getenv(cacheURLEnv)); v != "" {
		c.URL = v
	}
	if v := strings.TrimSpace(os.Getenv(cacheModeEnv)); v != "" {
		c.Mode = cache.Mode(v)
	}
}

// Active reports whether remote caching is configured and enabled. The provider
// resolves credentials separately; an active config with no capable provider
// still degrades to local-only.
func (c *remoteCacheConfig) Active() bool {
	if c == nil || c.URL == "" {
		return false
	}
	return c.Enabled == nil || *c.Enabled
}

// uploadInput is the per-job data needed to share a freshly built result with the
// cache provider: the precomputed cache key, the cached outcome to record, and
// the built manifest (the file set with per-file CAS digests).
type uploadInput struct {
	Key      string
	Result   *cache.ActionResult
	Manifest *cache.Manifest
}
