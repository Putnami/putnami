package toolchain

import (
	"path/filepath"
	"strings"
)

// Machine cache resolution: the Go extension's own policy since an earlier migration
// this extension.
//
// Before it, the CLI exported PUTNAMI_GO_CACHE_DIR into EVERY job of EVERY
// extension and this package simply read it. Core no longer knows that Go has a
// cache — so the extension that owns the cache is the one that decides where it
// lives.
//
// The default is UNCHANGED on purpose. ~/.putnami/cache/go is where every
// existing checkout's compiled packages already are, and moving the root as
// part of an ownership change would hand every user a machine-wide cold Go
// build plus an orphaned multi-gigabyte tree nothing ever collects. Ownership
// moved; the bytes did not.
const (
	// GoCacheDirEnv is the explicit user/CI override. Unchanged, and still
	// first: an environment that pins the Go cache root keeps pinning it.
	GoCacheDirEnv = "PUTNAMI_GO_CACHE_DIR"
	// PutnamiHomeEnv relocates the whole ~/.putnami tree.
	PutnamiHomeEnv = "PUTNAMI_HOME"
	// ExtensionCacheRootEnv is the generic per-extension machine root the C5
	// contract hands this extension (protocols/extension). It is the LAST
	// resort — the home-unavailable fallback — because taking it earlier would
	// relocate every existing Go cache.
	ExtensionCacheRootEnv = "PUTNAMI_EXTENSION_CACHE_ROOT"
)

// ResolveGoCacheRoot returns the machine-global Go cache root, or "" when no
// root can be resolved at all.
//
// Order: explicit override, relocated putnami home, the historical
// ~/.putnami/cache/go default, then the contract-provided extension cache root.
// GOCACHE is <root>/build and GOMODCACHE is <root>/mod.
//
// lookup reads one variable. It is a function rather than an []string so both
// callers can use it: GoCommandEnv resolves against the environment it is
// BUILDING (not the process's), while the cache commands resolve against
// os.Getenv.
func ResolveGoCacheRoot(lookup func(string) string) string {
	if lookup == nil {
		return ""
	}
	if dir := strings.TrimSpace(lookup(GoCacheDirEnv)); dir != "" {
		return dir
	}
	if putnamiHome := ResolvePutnamiHome(lookup); putnamiHome != "" {
		return filepath.Join(putnamiHome, "cache", "go")
	}
	if extensionCache := strings.TrimSpace(lookup(ExtensionCacheRootEnv)); extensionCache != "" {
		return filepath.Join(extensionCache, "go")
	}
	return ""
}

// GoBuildCacheDir is the go command's own build cache directory under a
// resolved cache root: the GOCACHE value GoCommandEnv sets.
//
// It is one function because three callers must agree on it: GoCommandEnv;
// the GOCACHEPROG helper (internal/gocacheprog), which writes the compiled
// objects it stores into this same directory so that a go command running
// without the helper finds them instead of storing a second copy; and the
// collector (internal/jobs/cachepolicy), which bounds and cleans it.
func GoBuildCacheDir(root string) string {
	return filepath.Join(root, "build")
}

// EnvLookup adapts an environment slice to the lookup function
// ResolveGoCacheRoot takes. The LAST matching entry wins, matching os/exec.
func EnvLookup(env []string) func(string) string {
	return func(key string) string { return envValue(env, key) }
}
