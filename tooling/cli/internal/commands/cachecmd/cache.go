package cachecmd

import (
	"context"
	"errors"
	"fmt"
	"os"

	extensionproto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/artifactstore"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/hooks"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// The cache commands after the split that moved language caches to their extensions.
//
// Core collects the caches core OWNS: the content-addressed per-repo stores and
// the machine-global artifact store. It no longer knows that Go or Bun exist —
// the language caches are collected by the extensions that create them, through
// the reserved `cache-clean` / `cache-gc` commands, and core's only knowledge of
// them is "this extension declares one".
//
// What that buys, concretely: a fourth ecosystem bounds its own cache by adding
// two commands to its manifest, with no patch to this file.

// CacheClean removes cached build artifacts. The store is now machine-global and
// per-repo (shared across all worktrees of the repo), so a plain clean clears
// the cache for every worktree of this repo. With all=true it clears every
// repo's store on the machine. The wipe runs under the store's exclusive lock,
// so it waits for in-flight sibling builds to finish and blocks new ones for the
// duration.
func CacheClean(ctx context.Context, wsRoot string, all bool, cfg *wsproto.Config, outputFormat string) error {
	// `--all` is a machine-global store operation that must work outside any
	// workspace; per-worktree extension scratch is the bare-`clean` path's job,
	// so it is not fanned out here (it would couple --all to the current cwd).
	if all {
		return cacheCleanAll(wsRoot)
	}

	if err := cleanRepoStore(wsRoot); err != nil {
		return err
	}

	// Wiping the CAS store is not a clean rebuild on its own: extension-owned
	// incremental scratch (TS .tsbuildinfo/mirror, the Go build cache, …) lives
	// in this worktree and in machine-global caches the CLI does not enumerate,
	// and survives a store wipe. Fan out to every installed extension declaring
	// `cache-clean` so each purges the caches it owns and the next build is
	// genuinely cold.
	return runExtensionCacheCommands(ctx, wsRoot, cfg, extensionproto.CommandCacheClean, outputFormat)
}

// cleanRepoStore wipes this repo's shared CAS store (the bare-`clean` path).
func cleanRepoStore(wsRoot string) error {
	storeRoot := store.ResolveStoreRoot(wsRoot)
	files, bytes, err := store.CleanStore(storeRoot)
	if err != nil {
		return fmt.Errorf("clean store: %w", err)
	}
	if files == 0 {
		iox.Fprintln(os.Stdout, "  Cache is already empty.")
		return nil
	}
	iox.Fprintf(os.Stdout, "  Cleaned this repo's shared cache: %d files, %s freed\n", files, formatSize(bytes))
	return nil
}

// cacheCleanAll wipes the cached content of every per-repo store on the machine
// (plus the current workspace's store, in case $HOME was unavailable). Each
// store is emptied under its exclusive lock by CleanStore, which preserves the
// store dir and its lock file so cross-process lock identity stays intact — so
// this deliberately does NOT os.RemoveAll the store dir (that would delete the
// lock another process may be holding and race in-flight sibling builds).
func cacheCleanAll(wsRoot string) error {
	roots := store.StoreRootsForGCIncluding(wsRoot)
	if len(roots) == 0 {
		iox.Fprintln(os.Stdout, "  No machine-global cache found.")
		return nil
	}
	var totalFiles, stores int
	var totalBytes int64
	for _, root := range roots {
		files, bytes, err := store.CleanStore(root)
		if err != nil {
			return fmt.Errorf("clean store %s: %w", root, err)
		}
		totalFiles += files
		totalBytes += bytes
		if files > 0 {
			stores++
		}
	}
	// Also clear the machine-global artifact store (binaries are shared across
	// every repo, so wiping them is an --all-scoped operation). Honor the GC grace
	// window so binaries a sibling worktree is actively running (kept warm in the
	// last grace window) are not reaped mid-exec — a non-redownloadable extension
	// like @putnami/cloud can't self-heal from that strand.
	asDirs, asBytes, _ := artifactstore.New(store.ResolveArtifactStoreRoot(wsRoot)).Clean(artifactstore.ResolveGCOptions().Grace)

	if totalFiles == 0 && asDirs == 0 {
		iox.Fprintln(os.Stdout, "  Cache is already empty.")
		return nil
	}
	if totalFiles > 0 {
		iox.Fprintf(os.Stdout, "  Cleaned %d repo store(s): %d files, %s freed\n", stores, totalFiles, formatSize(totalBytes))
	}
	if asDirs > 0 {
		iox.Fprintf(os.Stdout, "  Cleared %d shared artifact(s): %s freed\n", asDirs, formatSize(asBytes))
	}
	return nil
}

// CacheGC enforces the global byte budget across every per-repo store under
// ~/.putnami/store (plus the current workspace's store), evicting least-recently-
// used entries and sweeping unreferenced CAS blobs. It is safe to run while
// builds are in flight. wsRoot may be empty when run outside a workspace. cfg
// carries the config-file GC settings (env still overrides).
func CacheGC(ctx context.Context, wsRoot string, cfg *wsproto.Config, outputFormat string) error {
	if wsRoot != "" {
		lease, err := store.AcquireScratch(wsRoot)
		if err != nil {
			return err
		}
		defer func() { _ = lease.Close() }()
	}
	storeCfg := store.ConfigFromWorkspace(cfg)
	if roots := store.StoreRootsForGCIncluding(wsRoot); len(roots) > 0 {
		res, err := store.RunGC(roots, store.GCOptions{
			MaxBytes:           store.ResolveMaxBytes(storeCfg),
			Grace:              store.ResolveGCGrace(storeCfg),
			MaxIdleGenerations: store.ResolveMaxIdleBuilds(storeCfg),
		})
		if err != nil {
			return fmt.Errorf("garbage collect: %w", err)
		}
		iox.Fprintf(os.Stdout, "  Store: %s across %d repo store(s) (budget %s)\n",
			formatSize(res.ScannedBytes), res.StoresScanned, formatSize(store.ResolveMaxBytes(storeCfg)))
		if res.EvictedEntries == 0 && res.SweptBlobs == 0 {
			iox.Fprintln(os.Stdout, "  Within budget — nothing to collect.")
		} else {
			iox.Fprintf(os.Stdout, "  Evicted %d entr(ies) (%d idle), swept %d blob(s), %s freed\n",
				res.EvictedEntries, res.IdleEvicted, res.SweptBlobs, formatSize(res.FreedBytes))
		}
	} else {
		iox.Fprintln(os.Stdout, "  No machine-global cache found.")
	}

	gcArtifactStore(wsRoot)

	// The language caches used to be collected here by name. They are now the
	// owning extensions' `cache-gc` commands, and this is the only place core
	// mentions them at all.
	return runExtensionCacheCommands(ctx, wsRoot, cfg, extensionproto.CommandCacheGC, outputFormat)
}

// runExtensionCacheCommands fans one cache phase out to every installed
// extension declaring the reserved command, printing the bytes each reports
// freeing.
//
// EVERY extension is asked and EVERY failure is reported. The pre-C5 hook
// fan-out returned on the first error, which meant one broken extension left
// every extension sorted after it silently uncollected — a `cache clean` that
// reported an error for one extension and quietly did nothing for the rest.
// Aggregation is a deliberate behavior change: the command still fails, but it
// fails having done all the work it could.
//
// Extension caches need a workspace to be discovered from (that is where the
// manifests, the lock file and the project set are); wsRoot == "" is a no-op,
// which is what keeps `cache gc` and `cache clean --all` usable outside one.
func runExtensionCacheCommands(
	ctx context.Context,
	wsRoot string,
	cfg *wsproto.Config,
	command string,
	outputFormat string,
) error {
	// Discovery needs a workspace and its resolved config; without either there
	// are no extensions to fan out to (bare unit tests pass a nil cfg).
	if wsRoot == "" || cfg == nil {
		return nil
	}

	// Include workspace projects so cache commands work for local extensions even
	// when they are discovered from an included project rather than listed in
	// the workspace-level extensions config.
	ws, loadErr := workspace.Load(wsRoot)
	var projectPaths []string
	if ws != nil {
		projectPaths = make([]string, len(ws.Projects))
		for i, project := range ws.Projects {
			projectPaths[i] = project.Path
		}
	}

	extensions, err := extension.DiscoverExtensions(wsRoot, cfg, projectPaths)
	if err != nil {
		return fmt.Errorf("discover extensions: %w", err)
	}
	declaring := hooks.ExtensionsDeclaringCacheCommand(extensions, command)
	if len(declaring) == 0 {
		return nil
	}

	if loadErr != nil || ws == nil {
		ws = workspace.NewWorkspace(wsRoot, cfg, nil)
	}
	if err := jobs.SynchronizeExtensionRuntimes(ctx, ws, declaring, nil); err != nil {
		return fmt.Errorf("synchronize extension runtimes: %w", err)
	}

	// A structured stdout stream must stay uncorrupted, so route status chatter
	// to stderr under --output=jsonl.
	statusOut := os.Stdout
	if outputFormat == "jsonl" {
		statusOut = os.Stderr
	}

	var failures []error
	for _, outcome := range hooks.RunCacheCommands(ctx, ws, declaring, command, false) {
		if outcome.Err != nil {
			// Report the failure as it happens rather than only at the end: the
			// fan-out continues, and a user watching the command needs to know
			// which extension the error belongs to next to its own output.
			iox.Fprintf(statusOut, "  %s: %s failed: %v\n", outcome.Extension, command, outcome.Err)
			failures = append(failures, fmt.Errorf("%s %s: %w", outcome.Extension, command, outcome.Err))
			continue
		}
		if outcome.FreedBytes > 0 {
			iox.Fprintf(statusOut, "  %s: %s freed\n", outcome.Extension, formatSize(outcome.FreedBytes))
		}
	}
	return errors.Join(failures...)
}

// gcArtifactStore runs a budget/idle pass over the flat artifact store and
// reports it. Best-effort: a busy store (non-blocking lock) or any error is a
// silent skip.
func gcArtifactStore(wsRoot string) {
	opts := artifactstore.ResolveGCOptions()
	res, err := artifactstore.New(store.ResolveArtifactStoreRoot(wsRoot)).GC(opts)
	if err != nil || res == nil {
		return
	}
	iox.Fprintf(os.Stdout, "  Artifacts: %s across %d entr(ies), %d in use by a workspace (budget %s)\n",
		formatSize(res.TotalBytes), res.Scanned, res.Live, formatSize(opts.MaxBytes))
	if evicted := res.EvictedIdle + res.EvictedBudget; evicted > 0 || res.SweptStaging > 0 {
		iox.Fprintf(os.Stdout, "  Evicted %d artifact(s) (%d idle), swept %d staging, %s freed\n",
			evicted, res.EvictedIdle, res.SweptStaging, formatSize(res.FreedBytes))
	}
}

// formatSize returns a human-readable size string.
func formatSize(bytes int64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
	)

	switch {
	case bytes >= GB:
		return fmt.Sprintf("%.1f GB", float64(bytes)/float64(GB))
	case bytes >= MB:
		return fmt.Sprintf("%.1f MB", float64(bytes)/float64(MB))
	case bytes >= KB:
		return fmt.Sprintf("%.1f KB", float64(bytes)/float64(KB))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}
