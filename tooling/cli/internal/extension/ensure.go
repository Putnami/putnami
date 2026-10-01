package extension

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"go.putnami.dev/tooling/cli/internal/layout"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

// EnsureArtifact makes sure the named artifact is materialized and its
// per-worktree stable symlink resolves, downloading ONLY when the machine-global
// store lacks the pinned digest. It is the per-command warm path behind implicit
// (lock-driven) installs: a resolvable symlink — whether it points into the
// shared store or a per-worktree dir — is confirmed with a single stat, plus a
// keep-warm stamp so the shared store's GC knows a worktree still pins it. A
// missing or dangling link is healed via InstallArtifact (a sibling-warmed store
// makes that a symlink swap, not a download).
func (inst *Installer) EnsureArtifact(ctx context.Context, name, constraint string, lockEntry *lockfile.LockEntry, spec ArtifactSpec) error {
	stable := layout.StableDir(inst.WorkspaceRoot, spec.Kind, name)
	if manifestResolves(stable, spec.ManifestFilename) {
		if lockEntry != nil {
			if lockEntry.ManifestHash != "" {
				got, err := HashFile(filepath.Join(stable, spec.ManifestFilename))
				if err != nil || got != lockEntry.ManifestHash {
					_, installErr := inst.InstallArtifact(ctx, name, constraint, lockEntry, spec)
					if installErr != nil {
						_ = layout.UnlinkArtifact(inst.WorkspaceRoot, spec.Kind, name)
					}
					return installErr
				}
			}
			if digest := lockEntry.IntegrityFor(spec.OS, spec.Arch); digest != "" {
				inst.migrateAndTouch(ctx, stable, name, lockEntry.Version, digest, spec)
			}
		}
		return nil
	}
	if _, err := inst.InstallArtifact(ctx, name, constraint, lockEntry, spec); err != nil {
		// The stable symlink did not resolve (missing or dangling) and the heal
		// failed — e.g. a non-redownloadable extension whose machine-global store
		// entry a concurrent `cache clean` reaped. Drop the dangling link so the
		// extension reads as cleanly absent (caught by discovery / the missing-
		// extension guard) rather than resolving to a binary that only fails at
		// fork/exec time with an opaque ENOENT. Mirrors the manifest-hash
		// mismatch branch above.
		_ = layout.UnlinkArtifact(inst.WorkspaceRoot, spec.Kind, name)
		return err
	}
	return nil
}

// EnsureArtifactLocked strictly materializes the artifact selected by lockEntry.
// It is intentionally separate from EnsureArtifact: normal command discovery is
// a best-effort warm path, while callers that write committed generated files
// must not depend on a usable-but-ahead stable symlink.
func (inst *Installer) EnsureArtifactLocked(ctx context.Context, name, constraint string, lockEntry *lockfile.LockEntry, spec ArtifactSpec) error {
	if lockEntry == nil || !isPinnableVersion(lockEntry.Version) {
		return fmt.Errorf("lock does not record an exact version")
	}

	stable := layout.StableDir(inst.WorkspaceRoot, spec.Kind, name)
	if lockedArtifactMatchesContext(ctx, stable, lockEntry, spec) {
		if digest := lockEntry.IntegrityFor(spec.OS, spec.Arch); digest != "" {
			inst.migrateAndTouch(ctx, stable, name, lockEntry.Version, digest, spec)
		}
		return nil
	}

	if _, err := inst.InstallArtifact(ctx, name, constraint, lockEntry, spec); err != nil {
		return fmt.Errorf("materialize locked %s@%s: %w", spec.Label, lockEntry.Version, err)
	}
	if !lockedArtifactMatchesContext(ctx, stable, lockEntry, spec) {
		// Do not leave an artifact that failed strict validation available via the
		// stable path. This can happen only with an invalid/corrupt lock or store.
		_ = layout.UnlinkArtifact(inst.WorkspaceRoot, spec.Kind, name)
		return fmt.Errorf("materialized %s does not match locked version %s", spec.Label, lockEntry.Version)
	}
	return nil
}

// lockedArtifactMatchesContext checks the durable, platform-independent
// identity of a materialized artifact while honoring a bounded read. The
// archive digest is verified before a global-store entry is admitted; version
// and manifest hash bind the stable link to this workspace's lock.
func lockedArtifactMatchesContext(ctx context.Context, stableDir string, lockEntry *lockfile.LockEntry, spec ArtifactSpec) bool {
	if ctx == nil || ctx.Err() != nil {
		return false
	}
	if lockEntry == nil || !isPinnableVersion(lockEntry.Version) {
		return false
	}
	manifestPath := filepath.Join(stableDir, spec.ManifestFilename)
	if !manifestResolves(stableDir, spec.ManifestFilename) || spec.ManifestVersion(manifestPath) != lockEntry.Version {
		return false
	}
	if lockEntry.ManifestHash == "" {
		return true
	}
	got, err := HashFileContext(ctx, manifestPath)
	return err == nil && got == lockEntry.ManifestHash
}

// migrateAndTouch keeps a resolvable artifact warm and, when it is still a legacy
// per-worktree install but the shared store has since been warmed (by a sibling
// worktree) with this digest, migrates it: relink to the shared copy, then drop
// the legacy tree. The ordering is strict — the store is confirmed to hold the
// digest, then the symlink is swapped, and only then are the legacy bytes
// deleted — so a crash can never leave a dangling link with no local bytes. The
// archive is long gone, so migration is by relink-from-store, never by re-hash.
//
// The whole Has → relink → delete-legacy sequence runs under the store's shared
// lock so a concurrent GC (exclusive) cannot evict the digest between the Has
// check and the relink — which would otherwise leave a dangling link AND, having
// already deleted the legacy tree, no local fallback bytes either.
func (inst *Installer) migrateAndTouch(ctx context.Context, stable, name, version, digest string, spec ArtifactSpec) {
	as := inst.artifactStore()
	withArtifactStoreShared(ctx, as, func() {
		if !as.Has(digest) {
			return // nothing in the shared store yet; leave the per-worktree install as-is
		}
		want := as.Path(digest)
		if cur, _ := os.Readlink(stable); cur != want {
			if err := layout.LinkArtifactGlobal(inst.WorkspaceRoot, spec.Kind, name, want); err == nil {
				if legacy := layout.ArtifactDir(inst.WorkspaceRoot, spec.Kind, name, version); legacy != want {
					_ = os.RemoveAll(legacy)
				}
			}
		}
		as.Touch(digest) // keep-warm: this worktree still pins it
	})
}

// EnsureExtension is EnsureArtifact specialized to the extension kind.
func (inst *Installer) EnsureExtension(ctx context.Context, name, constraint string, lockEntry *lockfile.LockEntry) error {
	return inst.EnsureArtifact(ctx, name, constraint, lockEntry, inst.targetSpec())
}

// EnsureExtensionLocked is EnsureArtifactLocked specialized to extensions.
// Context generation uses it so its generated guidance has lock-resolved input.
func (inst *Installer) EnsureExtensionLocked(ctx context.Context, name, constraint string, lockEntry *lockfile.LockEntry) error {
	return inst.EnsureArtifactLocked(ctx, name, constraint, lockEntry, inst.targetSpec())
}

// manifestResolves reports whether stableDir (a symlink) resolves to a directory
// that actually contains the manifest — i.e. the artifact is installed and the
// link is not dangling. os.Stat follows the symlink, so a dangling link (whose
// target a GC swept away) reports false and triggers a heal.
func manifestResolves(stableDir, manifestFile string) bool {
	info, err := os.Stat(filepath.Join(stableDir, manifestFile))
	return err == nil && !info.IsDir()
}
