package store

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	workspace "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/git"
)

// This file is the single source of truth for where the build store and the
// per-workspace scratch directory live.
//
// The content-addressed store (CAS: blobs/, cas/, tmp/) is machine-global and
// per-repo — every worktree of a repo shares one store, so the first build in
// any worktree warms the cache for all siblings and a remote-cache blob is
// downloaded once per machine instead of once per worktree.
//
// The arbitrary mutable scratch directory handed to extensions/jobs/hooks as
// cacheRoot stays per-workspace: it holds non-content-addressed data protected
// from generation reclamation by AcquireScratch, not from concurrent writers.
// A cache with an explicit cross-process safety
// contract may use its own machine-global root (the Go cache does).

const (
	// putnamiDir is the ".putnami" directory name, used both under $HOME
	// (~/.putnami) and inside a workspace (<ws>/.putnami).
	putnamiDir = ".putnami"
	// storeDirName is the directory under ~/.putnami that holds the per-repo
	// content-addressed stores: ~/.putnami/store/<repo-id>.
	storeDirName = "store"
	// scratchDirName is the per-workspace mutable scratch directory exposed as
	// cacheRoot. Deliberately distinct from the CAS so it is never shared.
	scratchDirName = "cache"
	// storeDirEnv overrides the resolved store location entirely. It inherits
	// into nested putnami like other PUTNAMI_* vars (subprocesses get the parent
	// environment), so a parent run and its children share one store.
	storeDirEnv = "PUTNAMI_STORE_DIR"
	// repoIDLen is the hex length of the per-repo identity suffix.
	repoIDLen = 16

	// storeMaxBytesEnv overrides the global byte budget enforced by GC.
	storeMaxBytesEnv = "PUTNAMI_STORE_MAX_BYTES"
	// defaultStoreMaxBytes is the default global budget across all per-repo
	// stores (10 GiB).
	defaultStoreMaxBytes int64 = 10 << 30
	// gcGraceEnv overrides the GC grace period.
	gcGraceEnv = "PUTNAMI_STORE_GC_GRACE"
	// defaultGCGrace is how recently an entry must have been used to be spared
	// from eviction. It doubles as the protection window for an in-flight reader
	// whose .putnami/out symlink points into a cached entry, so it must comfortably
	// exceed a single build's duration.
	defaultGCGrace = time.Hour

	// maxIdleBuildsEnv overrides the idle-reclaim threshold.
	maxIdleBuildsEnv = "PUTNAMI_STORE_MAX_IDLE_BUILDS"
	// defaultMaxIdleBuilds evicts an entry not hit in this many cache-using
	// builds (store "generations"), independent of the byte budget — so abandoned
	// entries don't linger forever in an under-budget store. Measured in builds
	// rather than wall-clock so an idle machine (vacation) doesn't reap a cache
	// that is still valued by activity. 0 disables idle reclaim.
	defaultMaxIdleBuilds int64 = 100
	// OCILayerCacheDirName is the core-owned cache shared by every image
	// extension and worktree for a repository. It lives inside the store so the
	// existing global budget and clean lifecycle own it.
	OCILayerCacheDirName = "oci"
)

// Artifact store: the FLAT, machine-global, content-addressed binary store at
// ~/.putnami/artifacts, shared across ALL repos. Unlike the build store it has
// NO per-repo sub-level — a (name, version, os/arch) archive digest is
// byte-identical on every machine, repo, and worktree, so one entry serves them
// all. The first worktree to install an extension/CLI/toolchain warms it for
// every other checkout on the machine; siblings then only re-point a symlink.
const (
	// artifactsDirName is the directory under ~/.putnami holding the artifact store.
	artifactsDirName = "artifacts"
	// artifactDirEnv overrides the artifact store location entirely, mirroring
	// storeDirEnv. It inherits into nested putnami like other PUTNAMI_* vars, and
	// when set it both relocates the store and scopes GC to that single root.
	artifactDirEnv = "PUTNAMI_ARTIFACT_DIR"
)

// ResolveStoreRoot returns the machine-global content-addressed store root for
// the repository containing workspaceRoot. Resolution order:
//
//  1. $PUTNAMI_STORE_DIR, used verbatim.
//  2. ~/.putnami/store/<repo-id>, where repo-id hashes the repo's git common
//     dir — every worktree of a repo agrees on it, so siblings share one store.
//  3. <workspaceRoot>/.putnami/store as a last resort when $HOME is unavailable.
func ResolveStoreRoot(workspaceRoot string) string {
	if dir := os.Getenv(storeDirEnv); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(workspaceRoot, putnamiDir, storeDirName)
	}
	return filepath.Join(home, putnamiDir, storeDirName, repoID(workspaceRoot))
}

// repoID derives a stable per-repo identity. It hashes the git common dir so
// every worktree of a repo maps to the same store; when workspaceRoot is not a
// git repo it falls back to hashing the workspace root itself (each non-git
// checkout then gets its own store, still under the global location).
func repoID(workspaceRoot string) string {
	key := git.CommonDir(workspaceRoot)
	if key == "" {
		key = workspaceRoot
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:repoIDLen]
}

// ResolveScratchRoot returns the per-WORKSPACE mutable scratch directory handed
// to extensions/jobs/hooks as cacheRoot. It stays per-worktree (never the shared
// CAS) so concurrent worktrees cannot corrupt each other's non-content-addressed
// scratch.
func ResolveScratchRoot(workspaceRoot string) string {
	return filepath.Join(workspaceRoot, putnamiDir, scratchDirName)
}

// GlobalStoreParent returns ~/.putnami/store — the directory that holds every
// per-repo store — and whether it could be resolved (false when $HOME is
// unavailable).
func GlobalStoreParent() (string, bool) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", false
	}
	return filepath.Join(home, putnamiDir, storeDirName), true
}

// StoreRootsForGC returns the set of per-repo store roots GC should manage.
// With $PUTNAMI_STORE_DIR set the user opted out of the standard location, so
// GC manages only that single store; otherwise it manages every per-repo store
// under ~/.putnami/store.
func StoreRootsForGC() []string {
	if dir := os.Getenv(storeDirEnv); dir != "" {
		return []string{dir}
	}
	parent, ok := GlobalStoreParent()
	if !ok {
		return nil
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		return nil
	}
	var roots []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		roots = append(roots, filepath.Join(parent, e.Name()))
	}
	return roots
}

// StoreRootsForGCIncluding returns the GC root set unioned with the store the
// given workspace actually resolves to. This guarantees the current repo's store
// is always covered — including the $HOME-unavailable fallback
// (<workspaceRoot>/.putnami/store), which lives outside ~/.putnami/store and so
// is not enumerated by StoreRootsForGC alone.
func StoreRootsForGCIncluding(workspaceRoot string) []string {
	roots := StoreRootsForGC()
	if workspaceRoot == "" {
		return roots
	}
	resolved := ResolveStoreRoot(workspaceRoot)
	if slices.Contains(roots, resolved) {
		return roots
	}
	return append(roots, resolved)
}

// ResolveArtifactStoreRoot returns the FLAT machine-global artifact store root
// shared across all repos. Resolution order:
//
//  1. $PUTNAMI_ARTIFACT_DIR, used verbatim.
//  2. ~/.putnami/artifacts — flat, with NO repo-id segment, because artifacts
//     are repo-independent so every repo and worktree shares one entry.
//  3. <workspaceRoot>/.putnami/artifacts as a last resort when $HOME is
//     unavailable (degrades to per-worktree, unshared, like the build store).
//
// workspaceRoot is consulted only for the $HOME-unavailable fallback; otherwise
// the location is workspace-independent (that is the whole point of "flat").
func ResolveArtifactStoreRoot(workspaceRoot string) string {
	if dir := os.Getenv(artifactDirEnv); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(workspaceRoot, putnamiDir, artifactsDirName)
	}
	return filepath.Join(home, putnamiDir, artifactsDirName)
}

// GlobalArtifactRoot returns the flat artifact store root (the env override or
// ~/.putnami/artifacts) and whether it could be resolved WITHOUT a workspace —
// false only when $HOME is unavailable and no override is set. GC, which has no
// workspace context, uses this.
func GlobalArtifactRoot() (string, bool) {
	if dir := os.Getenv(artifactDirEnv); dir != "" {
		return dir, true
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", false
	}
	return filepath.Join(home, putnamiDir, artifactsDirName), true
}

// ArtifactRootsForGC returns the set of artifact store roots GC should manage.
// The artifact store is FLAT, so this is at most ONE root (the override or the
// global ~/.putnami/artifacts) — never a per-repo enumeration like
// StoreRootsForGC. Returns nil when $HOME is unavailable and no override is set
// (the per-worktree fallback is reachable only with a workspace; see
// ArtifactRootsForGCIncluding).
func ArtifactRootsForGC() []string {
	if root, ok := GlobalArtifactRoot(); ok {
		return []string{root}
	}
	return nil
}

// ArtifactRootsForGCIncluding unions the GC root set with the artifact root the
// given workspace actually resolves to, guaranteeing the $HOME-unavailable
// per-worktree fallback (<workspaceRoot>/.putnami/artifacts) — which lives
// outside ~/.putnami and so is not covered by ArtifactRootsForGC alone — is
// swept too.
func ArtifactRootsForGCIncluding(workspaceRoot string) []string {
	roots := ArtifactRootsForGC()
	if workspaceRoot == "" {
		return roots
	}
	resolved := ResolveArtifactStoreRoot(workspaceRoot)
	if slices.Contains(roots, resolved) {
		return roots
	}
	return append(roots, resolved)
}

// Config is the config-file layer for the store's GC settings. Env vars still
// override these, and defaults fill any field left unset (a nil pointer / unset
// value). It is built from the merged workspace config via ConfigFromWorkspace.
type Config struct {
	MaxBytes      *int64
	GCGrace       *time.Duration
	MaxIdleBuilds *int64
}

// ConfigFromWorkspace extracts the store GC settings from a merged workspace
// config (global + workspace scopes), parsing the duration string. A nil config
// or absent store section yields the zero Config (all unset → env/defaults).
func ConfigFromWorkspace(wc *workspace.Config) Config {
	if wc == nil || wc.Store == nil {
		return Config{}
	}
	c := Config{MaxBytes: wc.Store.MaxBytes, MaxIdleBuilds: wc.Store.MaxIdleBuilds}
	if wc.Store.GCGrace != "" {
		if d, err := time.ParseDuration(wc.Store.GCGrace); err == nil {
			c.GCGrace = &d
		}
	}
	return c
}

// ResolveMaxBytes returns the effective global byte budget: env > config > default.
func ResolveMaxBytes(cfg Config) int64 {
	if v := os.Getenv(storeMaxBytesEnv); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	if cfg.MaxBytes != nil && *cfg.MaxBytes > 0 {
		return *cfg.MaxBytes
	}
	return defaultStoreMaxBytes
}

// ResolveGCGrace returns the effective GC grace period: env > config > default.
func ResolveGCGrace(cfg Config) time.Duration {
	if v := os.Getenv(gcGraceEnv); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			return d
		}
	}
	if cfg.GCGrace != nil && *cfg.GCGrace >= 0 {
		return *cfg.GCGrace
	}
	return defaultGCGrace
}

// ResolveMaxIdleBuilds returns the effective idle-reclaim threshold in builds:
// env > config > default. 0 (from env or config) disables idle reclaim.
func ResolveMaxIdleBuilds(cfg Config) int64 {
	if v := os.Getenv(maxIdleBuildsEnv); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			return n
		}
	}
	if cfg.MaxIdleBuilds != nil && *cfg.MaxIdleBuilds >= 0 {
		return *cfg.MaxIdleBuilds
	}
	return defaultMaxIdleBuilds
}
