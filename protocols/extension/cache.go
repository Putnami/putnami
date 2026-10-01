// Extension-owned machine caches: the reserved cache-lifecycle commands and the
// per-extension machine cache root.
//
// Before this slice core KNEW the language caches. It shipped a Go collector and
// a Bun collector, exported PUTNAMI_GO_CACHE_DIR / PUTNAMI_BUN_CACHE_DIR /
// BUN_INSTALL_CACHE_DIR into EVERY job regardless of which extension ran it, and
// ran both collectors on the way out of every command. A third ecosystem had no
// way in that did not start with a patch to the CLI.
//
// Two declarations replace all of it:
//
//   - extension.cacheRoot (protocols/job). Core hands each extension ONE stable
//     machine-global directory it owns. Core never looks inside it — the layout
//     is the extension's business — but it is a real, absolute, per-extension
//     path, so the extension has somewhere durable to put a cache without
//     inventing a location per host and without core exporting a variable for
//     each ecosystem.
//
//   - The reserved cache commands below. `putnami cache clean` and `putnami
//     cache gc` fan out to every installed extension that declares them. They
//     are ordinary typed tasks behind hidden commands — the same shape as the
//     workspace-sync command slice C3b introduced — so an extension's cache
//     policy is declared, digested and validated like the rest of its manifest
//     instead of living in a bespoke hook vocabulary.
//
// The names are RESERVED rather than configured: core has to be able to ask
// "does this extension collect its caches?" without a per-extension registry,
// and a manifest-declared indirection would buy nothing but a second spelling.

package extension

import (
	"os"
	"path/filepath"
	"strings"
)

// Reserved command names for the cache lifecycle. An extension that owns a
// machine cache declares one or both as internal (hidden) commands whose single
// step runs its own typed task.
const (
	// CommandCacheClean is the cold-reset command: `putnami cache clean` fans
	// out to it so the extension can purge the caches it owns and the next
	// build is genuinely cold.
	CommandCacheClean = "cache-clean"
	// CommandCacheGC is the budget/eviction command: `putnami cache gc` fans
	// out to it so the extension can bound its own caches. It is also what
	// core's throttled opportunistic collection invokes.
	CommandCacheGC = "cache-gc"
)

// ReservedCacheCommands is the closed set of cache-lifecycle command names, in
// canonical (sorted) order. Adding a third is a protocol change, not a manifest
// one: core dispatches on these names.
var ReservedCacheCommands = []string{CommandCacheClean, CommandCacheGC}

// IsCacheCommand reports whether a command name is one of the reserved cache
// lifecycle commands.
func IsCacheCommand(name string) bool {
	return name == CommandCacheClean || name == CommandCacheGC
}

// MachineCacheDirEnv overrides the PARENT directory every extension's machine
// cache root is created under. It exists for tests and for hosts that place
// caches outside $HOME; it is not a per-extension override, because the
// per-extension segment is what makes the roots disjoint.
const MachineCacheDirEnv = "PUTNAMI_EXTENSION_CACHE_DIR"

// MachineCacheRootEnv carries the resolved per-extension machine cache root to
// a subprocess that does not receive a job context document — the reserved
// cache commands, which are workspace-level and have no project.
const MachineCacheRootEnv = "PUTNAMI_EXTENSION_CACHE_ROOT"

// SharedOCILayerCacheRootEnv carries core's resolved, per-repository OCI layer
// cache root. Unlike MachineCacheRootEnv, this cache is deliberately shared by
// every image-producing extension and by every worktree of the repository.
// Core owns its lifecycle through `putnami cache gc` and `putnami cache clean`.
const SharedOCILayerCacheRootEnv = "PUTNAMI_OCI_CACHE_ROOT"

// MachineCacheRoot returns the machine-global cache directory the named
// extension owns, or "" when no root can be resolved at all.
//
// Resolution, in order:
//
//  1. $PUTNAMI_EXTENSION_CACHE_DIR/<segment>, used verbatim as the parent.
//  2. ~/.putnami/cache/extensions/<segment> — machine-global and shared by every
//     repository and worktree, because a compiler or package cache is
//     content-addressed and its whole value is being reused across checkouts.
//  3. <workspaceRoot>/.putnami/cache/extensions/<segment> when no home directory
//     is available, so locked-down environments keep a usable (if unshared)
//     root instead of silently caching nothing.
//
// The path is a FUNCTION of the extension name, never of the command, the
// project or the run: an extension that finds a different directory on its
// second invocation has no cache at all.
func MachineCacheRoot(extensionName, workspaceRoot string) string {
	segment := CacheSegment(extensionName)
	if segment == "" {
		return ""
	}
	if dir := strings.TrimSpace(os.Getenv(MachineCacheDirEnv)); dir != "" {
		return filepath.Join(dir, segment)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".putnami", "cache", "extensions", segment)
	}
	if workspaceRoot == "" {
		return ""
	}
	return filepath.Join(workspaceRoot, ".putnami", "cache", "extensions", segment)
}

// CacheSegment maps an extension name to the single path segment its machine
// cache root is named with.
//
// Scoped names carry a separator ("@putnami/go"), which would otherwise turn one
// segment into two directories and let a name like "a/../b" reach outside the
// parent. Everything outside [A-Za-z0-9._@-] collapses to "-", so "@putnami/go"
// becomes "@putnami-go" — the spelling the pre-C5 per-worktree fallback already
// used, which keeps the mapping recognizable to anyone reading a cache
// directory listing.
func CacheSegment(extensionName string) string {
	trimmed := strings.TrimSpace(extensionName)
	if trimmed == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range trimmed {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.' || r == '_' || r == '@' || r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	segment := b.String()
	// A segment made entirely of dots would name the parent or the current
	// directory rather than a child of it.
	if strings.Trim(segment, ".") == "" {
		return ""
	}
	return segment
}
