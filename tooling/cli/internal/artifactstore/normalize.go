package artifactstore

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// NormalizedTime is the modification time stamped on every file and directory
// of a normalized artifact tree.
//
// Extraction otherwise leaves the extracting host's wall clock on disk, so the
// same archive materializes differently on every run. That is fatal for the
// packaging use case this exists for: a `type: "image"` layer that
// pre-warms this store is hashed into the image's content key, so a wall-clock
// mtime would re-key the image on every build.
//
// The value is arbitrary but must stay FIXED — changing it re-keys every image
// built from a materialization. 1980-01-01 is used rather than the Unix epoch
// because it is the earliest timestamp the zip container can represent, so a
// tree normalized here survives whatever archive format a packager picks.
var NormalizedTime = time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)

const (
	normalizedDirMode  os.FileMode = 0o755
	normalizedFileMode os.FileMode = 0o644
	normalizedExecMode os.FileMode = 0o755
)

// NormalizeTree makes an extracted artifact tree a pure function of the archive
// it came from, so the same lock plus the same platform materialize
// byte-identically across machines and runs.
//
// Two host-dependent properties extraction leaves behind are erased:
//
//   - modification times, which are simply the wall clock at extraction;
//   - permission bits, which are the tar header's mode masked by the extracting
//     process's umask (an archive's 0o755 lands as 0o750 under umask 027).
//     Modes are canonicalized to ONE executable/non-executable pair, preserving
//     the single distinction an artifact actually depends on: whether the file
//     may be exec'd.
//
// File CONTENT is never touched. That is load-bearing: the manifest's SHA-256
// is the lock's platform-independent binding (LockEntry.ManifestHash) and is
// re-checked on every cached link, so normalizing a byte of it would turn every
// materialized entry into a permanent re-download.
//
// Two things it deliberately does NOT normalize:
//
//   - symbolic links, because the standard library offers no portable way to
//     set a link's OWN timestamps or mode: os.Chtimes and os.Chmod both follow
//     the link, so "normalizing" one would silently rewrite its target instead;
//   - readdir order, which the filesystem owns. Entries are created in archive
//     order, which is as reproducible as the archive digest itself; a packager
//     that needs a total order sorts, as every deterministic archiver does.
//
// root's own permission bits are left to the caller — inside the store they are
// the staging directory's, which the store deliberately keeps tight — while its
// mtime is normalized like everything else.
func NormalizeTree(root string) error {
	type node struct {
		path  string
		isDir bool
	}
	var nodes []node
	if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		nodes = append(nodes, node{path: path, isDir: d.IsDir()})
		return nil
	}); err != nil {
		return fmt.Errorf("normalize: walk %s: %w", root, err)
	}

	for _, n := range nodes {
		if n.path == root {
			continue // root's mode is the caller's business; see the doc comment
		}
		mode := normalizedDirMode
		if !n.isDir {
			info, err := os.Lstat(n.path)
			if err != nil {
				return fmt.Errorf("normalize: stat %s: %w", n.path, err)
			}
			mode = normalizedFileMode
			if info.Mode().Perm()&0o111 != 0 {
				mode = normalizedExecMode
			}
		}
		if err := os.Chmod(n.path, mode); err != nil {
			return fmt.Errorf("normalize: chmod %s: %w", n.path, err)
		}
	}

	// Stamp bottom-up. Touching a child updates its parent's mtime, so parents
	// must come last; WalkDir is pre-order, so walking the collected nodes in
	// reverse visits every child before its parent. (chmod moves ctime, not
	// mtime, which is why the mode pass above can run in any order.)
	for i := len(nodes) - 1; i >= 0; i-- {
		if err := os.Chtimes(nodes[i].path, NormalizedTime, NormalizedTime); err != nil {
			return fmt.Errorf("normalize: set times on %s: %w", nodes[i].path, err)
		}
	}
	return nil
}

// Normalize turns a store root that was built as a PACKAGING DESTINATION into a
// byte-reproducible tree: it strips the host-local bookkeeping a live store
// needs and then normalizes every remaining mode and mtime via NormalizeTree.
//
// Stripped:
//
//   - tmp/ and locks/, which are staging and per-digest ownership scaffolding
//     with host-unique names;
//   - .lock, the advisory flock file whose whole purpose is cross-PROCESS
//     identity on one machine;
//   - the per-entry `lastused` recency sidecars, whose unix-nano wall clock is
//     the one field that can never be reproduced. GC falls back to the entry's
//     directory mtime when the sidecar is absent, and the consuming machine
//     re-stamps every lock-pinned digest through Touch on its next run;
//   - the per-entry `implementationdigest` records, which exist only where a
//     run computed a cache key, so their presence is host history. The
//     consuming machine derives them again from the payload.
//
// NEVER call this on a live store: deleting .lock while other processes hold it
// breaks the lock identity that keeps GC from reaping an in-flight admit, and
// the relaxed 0o755 modes NormalizeTree writes would widen a namespace the
// store deliberately keeps at 0o700. Callers gate on the destination not being
// the machine-global root.
func (s *Store) Normalize() error {
	if _, err := os.Stat(s.root); err != nil {
		if os.IsNotExist(err) {
			// Nothing was materialized (e.g. every configured artifact is a
			// workspace-local reference), so there is nothing to normalize.
			return nil
		}
		return fmt.Errorf("normalize: stat %s: %w", s.root, err)
	}
	for _, name := range []string{tmpDirName, digestLocks, lockFile} {
		if err := os.RemoveAll(filepath.Join(s.root, name)); err != nil {
			return fmt.Errorf("normalize: drop artifact-store %s: %w", name, err)
		}
	}
	if err := filepath.WalkDir(s.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || (d.Name() != lastUsedFile && d.Name() != implementationFile) {
			return nil
		}
		return os.Remove(path)
	}); err != nil {
		return fmt.Errorf("normalize: drop artifact-store bookkeeping sidecars: %w", err)
	}
	if err := NormalizeTree(s.root); err != nil {
		return err
	}
	// NormalizeTree leaves a tree root's mode to its caller; here the caller IS
	// the store, and the root is the packaging output's own directory, so give it
	// the same relaxed, host-independent mode as everything under it.
	if err := os.Chmod(s.root, normalizedDirMode); err != nil {
		return fmt.Errorf("normalize: chmod %s: %w", s.root, err)
	}
	return os.Chtimes(s.root, NormalizedTime, NormalizedTime)
}
