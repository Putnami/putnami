// Package deliverycli holds the delivery-domain command implementations for the
// @putnami/cloud CLI extension: the remote build cache config surface and the
// cache-provider RPC subprocess. The extension binary registers these
// commands; the shared toolkit they build on lives in internal/clicore.
package deliverycli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// CacheFileRelative is the workspace-root-relative path to the remote build
// cache config the native CLI reads. It sits next to the workspace link so a
// single `.putnami/` directory holds all workspace-local Cloud state.
const CacheFileRelative = ".putnami/cache.json"

// DefaultCacheURL is the managed remote build cache endpoint. It matches the
// cache-server's configured JWT audience host (cache.putnami.cloud).
const DefaultCacheURL = "https://cache.putnami.cloud"

// DefaultCacheMode is the negotiate mode written when none is requested. It
// mirrors the native CLI's own default (full transfer of hits).
const DefaultCacheMode = "full"

// CacheConfig is the on-disk shape of .putnami/cache.json. It carries no
// secret — only a token *source* the native CLI runs to obtain a bearer. The
// token recipe shares the registry keys' tokenSource shape (see registries.go).
//
// This is the WRITER half of the file: `cloud setup`/`cloud cache` produce it.
// The READER is remotecache.Config, which the cache-provider subprocess loads to
// drive the client (the same model a stock CLI's in-core cache uses). The two are
// deliberately separate views — this one shares tokenSource with the registry
// commands, the other lives in the importable client package — but they describe
// the same JSON and carry no duplicated token/presence/compression *logic*
// (resolution lives only in remotecache.TokenSource, compression only in
// remotecache/compress.go, presence is core-owned). TestCacheConfigWireCompatible
// guards that the writer's output stays readable by remotecache.Config.
//
// It is exported because the retained aggregator (the extension install
// + setup flow) builds and persists it across the domain boundary.
type CacheConfig struct {
	Enabled bool                 `json:"enabled"`
	URL     string               `json:"url"`
	Mode    string               `json:"mode"`
	Token   *clicore.TokenSource `json:"token,omitempty"`
}

// cacheTokenCommand is the token source `cloud setup` records by default: the
// user's own `putnami` entrypoint minting a fresh, workspace-scoped bearer.
// Routing through `putnami` (rather than the compiled extension binary)
// keeps the source resolvable in any checkout or CI runner without the
// extension binary on PATH.
func cacheTokenCommand() []string {
	return []string{"putnami", "cloud", "token", "--for", "cache"}
}

// cacheModeOrDefault resolves and validates the negotiate mode from flags,
// environment, then the built-in default. An unknown mode is a usage error so
// a typo never silently writes an inert cache config.
func cacheModeOrDefault(params map[string]any, env map[string]string) (string, error) {
	mode := clicore.FirstString(clicore.StringParam(params, "cache-mode", "cacheMode"), clicore.EnvGet(env, "PUTNAMI_CACHE_MODE"), DefaultCacheMode)
	switch mode {
	case "minimal", "toplevel", "full":
		return mode, nil
	default:
		return "", clicore.NewError(fmt.Sprintf("invalid cache mode %q; expected one of minimal, toplevel, full", mode), clicore.ExitUsage)
	}
}

// cacheURLOrDefault resolves the cache endpoint from flags, environment, then
// the managed default, trimming a trailing slash so the stored base URL is
// canonical.
func cacheURLOrDefault(params map[string]any, env map[string]string) string {
	return clicore.TrimURL(clicore.FirstString(clicore.StringParam(params, "cache-url", "cacheUrl"), clicore.EnvGet(env, "PUTNAMI_CACHE_URL"), DefaultCacheURL))
}

// BuildCacheConfig assembles the cache config `cloud setup` persists. The
// token is always a source (never a raw bearer), so the file is safe to commit.
func BuildCacheConfig(params map[string]any, env map[string]string) (*CacheConfig, error) {
	mode, err := cacheModeOrDefault(params, env)
	if err != nil {
		return nil, err
	}
	return &CacheConfig{
		Enabled: true,
		URL:     cacheURLOrDefault(params, env),
		Mode:    mode,
		Token:   &clicore.TokenSource{Command: cacheTokenCommand()},
	}, nil
}

// CacheSummary projects a cache config to the fields safe for command output:
// the token *source* is shown, never a token.
func CacheSummary(cfg *CacheConfig, path string) map[string]any {
	out := map[string]any{
		"enabled": cfg.Enabled,
		"url":     cfg.URL,
		"mode":    cfg.Mode,
		"path":    path,
	}
	if token := cfg.tokenSummary(); token != nil {
		out["token"] = token
	}
	return out
}

func (c *CacheConfig) tokenSummary() map[string]any {
	if c == nil || c.Token == nil {
		return nil
	}
	out := map[string]any{}
	if len(c.Token.Command) > 0 {
		out["command"] = c.Token.Command
	}
	if c.Token.URL != "" {
		out["url"] = c.Token.URL
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// tokenSourceText renders the token source for human output.
func (c *CacheConfig) tokenSourceText() string {
	if c == nil || c.Token == nil {
		return ""
	}
	if len(c.Token.Command) > 0 {
		return strings.Join(c.Token.Command, " ")
	}
	return c.Token.URL
}

// CachePath resolves the workspace-local cache config path. Exported because the
// retained aggregator summarizes the persisted config across the domain boundary.
func CachePath(workspaceRoot string) string {
	return filepath.Join(workspaceRoot, CacheFileRelative)
}

// ReadCacheConfig loads .putnami/cache.json. A missing file is not an error —
// it returns (nil, nil) so callers can distinguish "not configured" cleanly.
func ReadCacheConfig(workspaceRoot string) (*CacheConfig, error) {
	file := CachePath(workspaceRoot)
	data, err := os.ReadFile(file)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var cfg CacheConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", file, err)
	}
	return &cfg, nil
}

// WriteCacheConfig persists the cache config with 0644 perms — it holds no
// secret, unlike the 0600 auth credential file. The write is atomic (temp file
// + fsync + rename) so a killed `install`/`setup` can't leave a partial
// cache.json for the cache provider's token command to choke on.
func WriteCacheConfig(workspaceRoot string, cfg *CacheConfig) error {
	file := CachePath(workspaceRoot)
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return clicore.WriteFileAtomic(file, append(data, '\n'), 0o644)
}

func removeCacheConfig(workspaceRoot string) error {
	err := os.Remove(CachePath(workspaceRoot))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Cache exposes the read/teardown half of the build-cache surface.
// Provisioning lives on `cloud setup`; this group only introspects and
// disables, mirroring the lightweight `cloud registries status/logout` split.
//
//	putnami cloud cache [status]   the cache status: config, identity, last run reuse
//	putnami cloud cache disable    set enabled:false (or --remove to delete)
func Cache(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	if err := clicore.RejectAppFlag(args, "cloud cache"); err != nil {
		return err
	}
	sub := clicore.FirstPositional(args)
	switch sub {
	case "", "status":
		return cacheRunStatus(params, args, workspaceRoot, env, ioctx)
	case "disable":
		return cacheRunDisable(params, workspaceRoot, ioctx)
	case "help":
		return cacheHelp(params, ioctx)
	default:
		return clicore.NewError("unknown cache subcommand: "+sub, clicore.ExitUsage)
	}
}

func cacheHelp(params map[string]any, ioctx clicore.IO) error {
	commands := []map[string]string{
		{"command": "cloud cache status", "description": "cache config and identity checks, and the reuse of the last CI run (also: cloud cache)"},
		{"command": "cloud cache disable", "description": "disable the build cache (enabled:false; --remove deletes the file)"},
	}
	if clicore.StructuredOutput(params) {
		clicore.WriteResult(map[string]any{"commands": commands}, params, ioctx, "")
		return nil
	}
	ioctx.Stdout("@putnami/cloud cache commands:")
	for _, c := range commands {
		ioctx.Stdout(fmt.Sprintf("  putnami %-22s %s", c["command"], c["description"]))
	}
	ioctx.Stdout("")
	ioctx.Stdout("Provision the build cache with `putnami cloud setup` (use --no-cache to skip it).")
	return nil
}

// cacheRunStatus prints the cache status node: the config and
// identity checks, and the reuse of the last CI run when it can be read.
func cacheRunStatus(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	if positionals := clicore.Positionals(args); len(positionals) > 1 {
		return clicore.NewError("cloud cache status takes no name; got "+strings.Join(positionals[1:], " "), clicore.ExitUsage)
	}
	return clicore.WriteStatus(params, ioctx, CacheStatusNode(params, workspaceRoot, env, ioctx))
}

func cacheRunDisable(params map[string]any, workspaceRoot string, ioctx clicore.IO) error {
	if clicore.Truthy(clicore.Param(params, "remove")) {
		if err := removeCacheConfig(workspaceRoot); err != nil {
			return err
		}
		clicore.WriteResult(map[string]any{"cache_disabled": true, "removed": true}, params, ioctx, "Removed build cache config.")
		return nil
	}
	cfg, err := ReadCacheConfig(workspaceRoot)
	if err != nil {
		return err
	}
	if cfg == nil {
		clicore.WriteResult(map[string]any{"cache_disabled": false}, params, ioctx, "Build cache is not configured; nothing to disable.")
		return nil
	}
	cfg.Enabled = false
	if err := WriteCacheConfig(workspaceRoot, cfg); err != nil {
		return err
	}
	clicore.WriteResult(map[string]any{"cache_disabled": true, "removed": false}, params, ioctx, "Disabled build cache (enabled:false).")
	return nil
}

// localIdentity reads the active identity from the stored credential file
// without a network round-trip, so `cloud cache status` works offline. Returns
// nil when not authenticated.
func localIdentity(env map[string]string) map[string]any {
	auth, err := clicore.ReadAuth(env, false)
	if err != nil || auth == nil || auth.AccessToken == "" {
		return nil
	}
	claims := clicore.DecodeJWT(auth.AccessToken)
	out := map[string]any{}
	if sub := clicore.StringValue(claims["sub"]); sub != "" {
		out["sub"] = sub
	}
	if email := clicore.StringValue(claims["email"]); email != "" {
		out["email"] = email
	}
	if ws := clicore.ClaimedWorkspaceID(claims); ws != "" {
		out["workspace_id"] = ws
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func identityText(identity map[string]any) string {
	if identity == nil {
		return ""
	}
	return clicore.FirstString(clicore.StringValue(identity["email"]), clicore.StringValue(identity["sub"]))
}
