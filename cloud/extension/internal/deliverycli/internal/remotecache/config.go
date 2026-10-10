package remotecache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"

	cache "go.putnami.dev/protocol/cache"
)

// Environment overrides for the persisted remote-cache configuration. They let
// CI point at a cache and supply a token without writing a config file; the
// token override lives in TokenEnv (see client.go).
const (
	// URLEnv overrides the configured cache server URL.
	URLEnv = "PUTNAMI_CACHE_URL"
	// ModeEnv overrides the configured materialization mode.
	ModeEnv = "PUTNAMI_CACHE_MODE"
	// ReadOnlyEnv disables remote-cache writes while preserving reads. The
	// pinned CI runner uses it because its intentionally untrusted service
	// account carries cache.read but never cache.write.
	ReadOnlyEnv = "PUTNAMI_CACHE_READ_ONLY"
	// TokenSourceEnv selects metadata identity instead of a persisted human
	// token recipe. It carries no credential; TokenEnv still takes precedence.
	TokenSourceEnv = "PUTNAMI_CACHE_TOKEN_SOURCE"
)

// Config is the persisted remote-cache configuration. It carries no secret: the
// token is obtained at run time from a TokenSource (a command, URL, or GCP
// metadata audience), and a resolved bearer is never written back. The cloud
// extension writes this file via its setup command; the CLI only reads it.
//
// The canonical cache token command is `putnami cloud token --for cache`.
//
// Example (.putnami/cache.json):
//
//	{
//	  "enabled": true,
//	  "url": "https://cache.putnami.cloud",
//	  "mode": "full",
//	  "token": { "command": ["putnami", "cloud", "token", "--for", "cache"] }
//	}
type Config struct {
	// Enabled gates remote caching. A nil value (field omitted) means enabled,
	// so a written config is active by default; set false to disable without
	// deleting the file.
	Enabled *bool `json:"enabled,omitempty"`
	// URL is the cache server base URL. Remote caching is inactive when empty.
	URL string `json:"url,omitempty"`
	// Mode is the materialization mode (minimal|toplevel|full). Empty defers to
	// the client default.
	Mode cache.Mode `json:"mode,omitempty"`
	// ReadOnly preserves negotiate, restore, and marker lookup while declining
	// uploads and marker writes before they reach the network.
	ReadOnly bool `json:"readOnly,omitempty"`
	// Token describes how to obtain the per-user bearer token.
	Token TokenSource `json:"token,omitempty"`

	// metadataIdentity is an execution-local override, never persisted. The
	// native cache provider owns identity resolution, not a runner-written file.
	metadataIdentity bool
}

// ResolveToken resolves the configured bearer, defaulting the metadata-token
// audience to the cache server URL. Explicit env/command/URL token sources keep
// precedence inside TokenSource.Resolve, so this fallback only activates for a
// tokenless cache config such as the Cloud Run CI runner's.
func (c *Config) ResolveToken(ctx context.Context) (string, error) {
	b, err := c.ResolveBearer(ctx)
	return b.Token, err
}

// ResolveBearer is ResolveToken plus the class of the source that produced the
// credential. The client needs the class to decide whether a bearer the cache
// server refused can be re-minted, so this — not ResolveToken — is what the
// cache provider wires in.
func (c *Config) ResolveBearer(ctx context.Context) (Bearer, error) {
	if c == nil {
		return Bearer{Class: TokenClassNone}, nil
	}
	source := c.Token
	if c.metadataIdentity {
		source.Command = nil
		source.URL = ""
		source.Audience = ""
	}
	if strings.TrimSpace(source.Audience) == "" {
		source.Audience = strings.TrimRight(strings.TrimSpace(c.URL), "/")
	}
	return source.ResolveBearer(ctx)
}

// LoadConfig reads the remote-cache config from path. A missing file is not an
// error: it returns an empty (inactive) config so callers can treat "no file"
// and "remote cache off" identically.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &Config{}, nil
		}
		return nil, fmt.Errorf("read cache config %s: %w", path, err)
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse cache config %s: %w", path, err)
	}
	c.Token.Command = migrateLegacyTokenCommand(c.Token.Command)
	return &c, nil
}

// migrateLegacyTokenCommand rewrites the superseded cache token recipe — the
// combined `--for-cache` flag — to the canonical `--for cache` form, so an
// existing .putnami/cache.json keeps working without a re-login. The legacy flag
// is never generated; only consumed for migration. Any other command (a custom
// helper, a third-party recipe) passes through untouched.
//
//	["putnami","cloud","token","--for-cache"] → ["putnami","cloud","token","--for","cache"]
func migrateLegacyTokenCommand(cmd []string) []string {
	if len(cmd) == 0 {
		return cmd
	}
	changed := false
	out := make([]string, 0, len(cmd)+1)
	for _, arg := range cmd {
		if arg == "--for-cache" {
			out = append(out, "--for", "cache")
			changed = true
			continue
		}
		out = append(out, arg)
	}
	if !changed {
		return cmd
	}
	return out
}

// ApplyEnv overlays the PUTNAMI_CACHE_* environment overrides onto the config,
// so CI can point at a cache without a config file. A malformed read-only value
// is rejected rather than silently enabling writes on a runner that asked to
// suppress them.
func (c *Config) ApplyEnv() error {
	switch source := strings.TrimSpace(os.Getenv(TokenSourceEnv)); source {
	case "":
		c.metadataIdentity = false
	case "metadata":
		c.metadataIdentity = true
	default:
		return fmt.Errorf("%s must be metadata or unset", TokenSourceEnv)
	}
	if v := strings.TrimSpace(os.Getenv(URLEnv)); v != "" {
		c.URL = v
	}
	if v := strings.TrimSpace(os.Getenv(ModeEnv)); v != "" {
		c.Mode = cache.Mode(v)
	}
	if v := strings.TrimSpace(os.Getenv(ReadOnlyEnv)); v != "" {
		readOnly, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("%s must be true or false: %w", ReadOnlyEnv, err)
		}
		c.ReadOnly = readOnly
	}
	return nil
}

// Active reports whether remote caching is configured and enabled. A token is
// resolved separately; an active config with no resolvable token still degrades
// to local-only.
func (c *Config) Active() bool {
	if c == nil || c.URL == "" {
		return false
	}
	return c.Enabled == nil || *c.Enabled
}
