package store

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"time"

	cache "go.putnami.dev/protocol/cache"
	"go.putnami.dev/sdk/extension/dirlink"
)

// --- path helpers ---

func (s *LocalStore) blobDir(hash string) string {
	prefix := hash
	if len(hash) >= 2 {
		prefix = hash[:2]
	}
	return filepath.Join(s.root, "blobs", prefix, hash)
}

func (s *LocalStore) createTmpDir() (string, error) {
	tmpRoot := filepath.Join(s.root, "tmp")
	if err := os.MkdirAll(tmpRoot, 0o755); err != nil {
		return "", err
	}
	return os.MkdirTemp(tmpRoot, "staging-")
}

// --- handles another process holds ---

// holderPolicy says which errors mean another process holds a handle on an
// operation's path, and how long the operation waits for it to close.
type holderPolicy struct {
	held   func(error) bool
	budget time.Duration
}

// heldOpenPolicy is this host's holderPolicy: Windows waits up to two seconds
// on ERROR_ACCESS_DENIED and ERROR_SHARING_VIOLATION, and every other host
// never waits. It is a variable only so a test can apply a waiting policy on a
// host that never needs one.
var heldOpenPolicy = holderPolicy{held: hostHeldOpen, budget: heldOpenBudget}

// holderWait paces the retries of one operation that fails while another
// process holds a handle on its path. It sleeps a randomized, doubling backoff
// between attempts, as robustio does for a rename, and gives up once
// heldOpenPolicy's budget would run out. The zero value is ready to use.
type holderWait struct {
	start time.Time
	sleep time.Duration
}

// retry reports whether an operation that failed with err may run again, and
// sleeps before it does. It is false for nil, for an error that does not say a
// handle is held, and once the budget is spent.
func (w *holderWait) retry(err error) bool {
	policy := heldOpenPolicy
	if err == nil || !policy.held(err) {
		return false
	}
	if w.start.IsZero() {
		w.start, w.sleep = time.Now(), time.Millisecond
	}
	if time.Since(w.start)+w.sleep >= policy.budget {
		return false
	}
	time.Sleep(w.sleep)
	w.sleep += rand.N(w.sleep)
	return true
}

// readReplacedFile is os.ReadFile for a file that writers replace by rename.
// Windows refuses to open such a file for as long as another process holds it
// or a rename is replacing it, so the read waits that out (holderWait) instead
// of reporting the file unreadable.
func readReplacedFile(path string) ([]byte, error) {
	var wait holderWait
	for {
		data, err := os.ReadFile(path)
		if err == nil || !wait.retry(err) {
			return data, err
		}
	}
}

// --- filesystem utilities ---

// copyDir recursively copies a directory tree, fsyncing every file. Use it when
// dst is the only durable home for the bytes — anything published into the CAS,
// where a torn file would make a blob disagree with its manifest digest.
func copyDir(src, dst string) error {
	return copyDirFiltered(src, dst, nil)
}

// copyDirUnsynced recursively copies a directory tree without fsyncing each
// file. Use it when dst is a workspace materialization of bytes the CAS still
// holds — restoring a cached output tree, detaching a CAS symlink — because the
// fsync buys only crash durability for a tree any later run rebuilds from the
// blob, and it is expensive: on macOS os.File.Sync issues F_FULLFSYNC (a full
// device cache flush) costing ~5ms per file regardless of size, so a few hundred
// small files turn a 90ms copy into 2s. Torn-read safety is unaffected — that
// comes from copyFile's stage-then-rename, not from the fsync.
func copyDirUnsynced(src, dst string) error {
	return copyDirOpts(src, dst, nil, false)
}

// MaterializeDirSymlink detaches a symlinked directory into a writable real
// directory holding the same contents, so the caller can write into it without
// mutating the link's target.
//
// Command output directories are shared by every step of a pipeline. An
// all-cache-hit run leaves that directory as a symlink into the immutable CAS;
// when one step executes on a later run it must detach the tree before writing.
// Deleting the symlink and starting from an empty directory loses the outputs
// owned by sibling steps and can make a tool binary vanish from under a
// concurrently executing job.
//
// The copy is staged beside target and swapped in by removing the symlink and
// renaming. That swap is NOT atomic, and cannot be: rename(2) refuses to replace
// a symlink with a directory (ENOTDIR), so there is no single call that puts a
// directory where a link was. The window is two syscalls wide instead of the
// whole job, and the original link is restored if the rename fails, but a caller
// that needs the path to be continuously live must hold its own reference.
//
// Conditions that are not failures, because the caller's next step is to create
// the directory anyway: target missing, target already a real directory, target
// already detached by another process. A dangling link — including one whose
// blob is reclaimed part-way through the copy, which GC can do at any point
// since this reader holds no lease — is removed so the caller recreates it
// empty.
//
// A crash between staging and the swap leaves a `.<name>.tmp-materialize-*`
// directory behind. Staging names are unique rather than fixed so that two
// concurrent processes cannot delete each other's in-flight copy; the cost of
// that choice is that nothing reclaims an orphan automatically.
//
// Every step here races another process detaching the same path, so a lost race
// is retried from a fresh read rather than reported: the goal is that the path
// ends up a real directory, not that this particular call is the one to make it
// so. Retries are bounded — a path still contended after that many rounds is a
// real problem, not a lost race.
func MaterializeDirSymlink(target string) error {
	const attempts = 8
	for attempt := range attempts {
		done, err := materializeDirSymlinkOnce(target, attempt == attempts-1)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
	return fmt.Errorf("materialize output: %s stayed contended", target)
}

// materializeDirSymlinkOnce makes one detach attempt. done reports that target
// reached the wanted state — a real directory, or removed because the link had
// nothing behind it. (false, nil) means the path changed under this attempt and
// the caller should re-read and retry; last converts those into errors so a
// genuinely stuck path surfaces instead of spinning.
func materializeDirSymlinkOnce(target string, last bool) (bool, error) {
	// A lost race and a real failure are the same syscall error; only the state
	// of the path distinguishes them, so re-read rather than guess. Returns nil
	// (retry) until the final attempt, where the error is the honest answer.
	lost := func(err error) error {
		if last {
			return err
		}
		return nil
	}

	info, err := lstatSettled(target)
	if err != nil {
		if os.IsNotExist(err) {
			return true, nil
		}
		return false, err
	}
	if !dirlink.IsLink(target, info) {
		return detachedDirectory(info, lost)
	}

	// Readlink for the rollback value (the literal link text), Resolve for a
	// path the copy can read through.
	linkText, err := os.Readlink(target)
	if err != nil {
		return false, lost(fmt.Errorf("read output symlink: %w", err))
	}
	src, err := dirlink.Resolve(target)
	if err != nil {
		if os.IsNotExist(err) {
			return true, removeStaleLink(target)
		}
		// Resolving is not atomic with the Lstat above: a sibling that completes
		// its own detach in that window turns target into a real directory, and
		// readlink then reports EINVAL. That is this function's most common lost
		// race, not a broken path, so it re-reads like every other step.
		return false, lost(fmt.Errorf("resolve output symlink: %w", err))
	}
	srcInfo, err := os.Stat(src)
	if err != nil {
		if os.IsNotExist(err) {
			return true, removeStaleLink(target)
		}
		return false, fmt.Errorf("stat output symlink target: %w", err)
	}
	if !srcInfo.IsDir() {
		return false, fmt.Errorf("output symlink target is not a directory")
	}

	parent := filepath.Dir(target)
	staging, err := os.MkdirTemp(parent, "."+filepath.Base(target)+".tmp-materialize-")
	if err != nil {
		return false, err
	}
	// A successful rename moves staging away, so RemoveAll then no-ops; every
	// other exit needs it gone.
	defer func() { _ = os.RemoveAll(staging) }()

	// Force owner-write so the copy can populate a tree whose source is
	// read-only, and chmod after the copy rather than before for the same reason.
	if err := os.Chmod(staging, srcInfo.Mode().Perm()|0o700); err != nil {
		return false, err
	}
	if err := copyDirUnsynced(src, staging); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// The blob was reclaimed mid-copy (GC, or `putnami cache clean` in a
			// sibling worktree of the same machine-global store). Identical
			// condition to a dangling link, so give it identical handling rather
			// than failing a build over a cache entry nobody promised to keep.
			return true, removeStaleLink(target)
		}
		return false, fmt.Errorf("copy output symlink target: %w", err)
	}

	switch current, err := lstatSettled(target); {
	case err == nil && !dirlink.IsLink(target, current):
		return detachedDirectory(current, lost) // another process detached it first
	case err == nil:
		// ENOENT means someone else just removed the link, leaving exactly the
		// state the rename below wants.
		if rmErr := os.Remove(target); rmErr != nil && !os.IsNotExist(rmErr) {
			return false, lost(fmt.Errorf("remove output symlink: %w", rmErr))
		}
	case !os.IsNotExist(err):
		return false, err
	}

	if err := os.Rename(staging, target); err != nil {
		if !last {
			return false, nil
		}
		// Out of attempts: restore the link so a failed detach does not also
		// destroy the sibling outputs this function exists to preserve.
		_ = dirlink.Create(linkText, target)
		return false, fmt.Errorf("publish materialized output: %w", err)
	}
	return true, nil
}

// lstatMaterialized is os.Lstat for MaterializeDirSymlink. It is a variable
// only so a test can replay the answers Windows gives while another process
// detaches the same path. Not for parallel tests.
var lstatMaterialized = os.Lstat

// lstatSettled reads a path another process may be detaching. Windows answers
// ERROR_ACCESS_DENIED for a path whose delete is pending, until the last handle
// on it closes: a sibling removed the link while someone still read it. The
// read waits that out (holderWait) instead of reporting a lost race as a
// failure; after the wait the path is gone or is the sibling's directory.
func lstatSettled(path string) (fs.FileInfo, error) {
	var wait holderWait
	for {
		info, err := lstatMaterialized(path)
		if err == nil || !wait.retry(err) {
			return info, err
		}
	}
}

// detachedDirectory classifies a path that dirlink.IsLink rejected: a real
// directory is the state MaterializeDirSymlink wants. Anything else is an
// error, except that an fs.ModeIrregular answer is re-read (lost): a Windows
// junction reports that mode, and IsLink confirms it with a Readlink, which
// fails when another process replaces the junction with its directory between
// the Lstat and the Readlink. The mode then describes a link that no longer
// exists. Unix reports a link as fs.ModeSymlink, so no link reaches here there.
func detachedDirectory(info fs.FileInfo, lost func(error) error) (bool, error) {
	if info.IsDir() {
		return true, nil
	}
	err := fmt.Errorf("output path is not a directory")
	if info.Mode()&fs.ModeIrregular != 0 {
		return false, lost(err)
	}
	return false, err
}

// removeStaleLink drops target when it is still a symlink with nothing usable
// behind it. It is deliberately a no-op once another process has replaced the
// path with a real directory, so losing that race is not an error.
func removeStaleLink(target string) error {
	info, err := os.Lstat(target)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !dirlink.IsLink(target, info) {
		return nil
	}
	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// replaceDirPreserving stages a fresh copy of src beside dst and atomically
// swaps it into place. Selected post-capture files from the existing target are
// carried forward; preserve receives slash-normalized paths relative to dst.
// This keeps scheduler-owned sidecars outside a task's cache snapshot without
// coupling their lifetime to a later regeneration phase. A nil predicate makes
// dst byte-identical to src.
//
// dst is always a workspace path being rebuilt from a blob the CAS still holds,
// so the copies skip the per-file fsync (see copyDirUnsynced).
func replaceDirPreserving(src, dst string, preserve func(rel string) bool) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	staging := dst + ".tmp-restore"
	if err := os.RemoveAll(staging); err != nil {
		return err
	}
	if err := copyDirUnsynced(src, staging); err != nil {
		os.RemoveAll(staging)
		return err
	}
	if preserve != nil {
		if info, err := os.Stat(dst); err == nil && info.IsDir() {
			if err := copyDirOpts(dst, staging, func(rel string) bool {
				return !preserve(filepath.ToSlash(rel))
			}, false); err != nil {
				os.RemoveAll(staging)
				return err
			}
		} else if err != nil && !os.IsNotExist(err) {
			os.RemoveAll(staging)
			return err
		}
	}
	if err := os.RemoveAll(dst); err != nil {
		os.RemoveAll(staging)
		return err
	}
	if err := os.Rename(staging, dst); err != nil {
		os.RemoveAll(staging)
		return err
	}
	return nil
}

// dirMatchesManifest reports whether target already contains exactly the files
// described by manifest. prefix selects a manifest subtree (for example
// "gen/") and is stripped before comparing paths. Empty directories are not
// represented in cache manifests and therefore do not affect the result.
//
// This content check is substantially cheaper than replacing a small generated
// tree: replacement fsyncs every copied file, while a consecutive warm build
// usually only needs to read a handful of already-cached bytes. Digests—not
// mtimes—keep the fast path correct when a file was edited without changing its
// size, and the final count rejects extra stale files.
func dirMatchesManifest(target string, manifest *cache.Manifest, prefix string, ignore func(rel string) bool) bool {
	if manifest == nil {
		return false
	}
	prefix = filepath.ToSlash(prefix)
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	expected := make(map[string]cache.FileEntry)
	for _, file := range manifest.Files {
		path := filepath.ToSlash(file.Path)
		if prefix != "" {
			if !strings.HasPrefix(path, prefix) {
				continue
			}
			path = strings.TrimPrefix(path, prefix)
		}
		if path != "" && !manifestMatchIgnored(path, ignore) {
			expected[path] = file
		}
	}

	matched := 0
	err := filepath.Walk(target, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(target, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if manifestMatchIgnored(rel, ignore) {
			return nil
		}
		if !info.Mode().IsRegular() {
			return os.ErrInvalid
		}
		file, ok := expected[rel]
		if !ok || info.Size() != file.Size || uint32(info.Mode().Perm()) != file.Mode {
			return os.ErrInvalid
		}
		digest, _, err := digestFile(path)
		if err != nil || digest != file.Digest {
			return os.ErrInvalid
		}
		matched++
		return nil
	})
	return err == nil && matched == len(expected)
}

func manifestMatchIgnored(rel string, ignore func(rel string) bool) bool {
	return uncacheableArtifact(rel) || (ignore != nil && ignore(rel))
}

// copyDirFiltered recursively copies a directory tree, skipping any file whose
// path relative to src satisfies skip. Directories are always created so the
// tree shape is preserved even when all of a directory's files are skipped.
func copyDirFiltered(src, dst string, skip func(rel string) bool) error {
	return copyDirOpts(src, dst, skip, true)
}

// copyDirOpts backs copyDir/copyDirFiltered/copyDirUnsynced. sync selects
// whether each copied file is fsynced; see copyDirUnsynced for when to drop it.
func copyDirOpts(src, dst string, skip func(rel string) bool, sync bool) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)

		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}

		if skip != nil && skip(rel) {
			return nil
		}

		return copyFileOpts(path, target, sync)
	})
}

// uncacheableArtifact reports whether a captured output file is a throwaway
// intermediate that must not be stored in the cache. Coverage runs emit
// per-shard lcov temp files (lcov.info.<n>.tmp) that dominate the test task's
// cached output (the bulk of its multi-megabyte payload) without being a real
// deliverable, so they are excluded from the stored artifact set.
func uncacheableArtifact(rel string) bool {
	base := filepath.Base(rel)
	return strings.HasPrefix(base, "lcov.info.") && strings.HasSuffix(base, ".tmp")
}

// copyFile copies a single file to dst atomically. Rather than truncating the
// live dst in place (open+O_TRUNC), it stages the bytes into a unique temp file
// in dst's own directory, fsyncs and chmods it to the source mode, then renames
// it over dst. Because the temp is a sibling of dst the rename stays intra-
// filesystem and is atomic: a concurrent reader/exec of dst (a downstream run
// launching its task binary straight from the restored bin/ tree) sees either
// the old inode or the fully-written new one, never torn bytes. Mirrors the
// replaceDir stage-then-rename idiom above. os.CreateTemp creates the temp 0600,
// so the explicit chmod-to-source-mode is required to preserve the exec bit.
func copyFile(src, dst string) error {
	return copyFileOpts(src, dst, true)
}

// copyFileOpts is copyFile with the fsync made optional. The fsync is what makes
// the copy durable across power loss; it is NOT what makes the copy safe against
// a concurrent reader (the rename does that), so callers materializing bytes the
// CAS still holds can pass sync=false. See copyDirUnsynced.
func copyFileOpts(src, dst string, sync bool) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	info, err := srcFile.Stat()
	if err != nil {
		return err
	}

	dstDir := filepath.Dir(dst)
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return err
	}

	tmpFile, err := os.CreateTemp(dstDir, filepath.Base(dst)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()
	renamed := false
	defer func() {
		tmpFile.Close() // no-op if already closed below
		// Best-effort cleanup; no-ops once rename has consumed tmpPath.
		if !renamed {
			os.Remove(tmpPath)
		}
	}()

	if _, err := io.Copy(tmpFile, srcFile); err != nil {
		return err
	}
	if sync {
		if err := tmpFile.Sync(); err != nil {
			return err
		}
	}
	// os.CreateTemp opens 0600; restore the source mode (including the exec bit).
	if err := tmpFile.Chmod(info.Mode()); err != nil {
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		return err
	}
	renamed = true
	return nil
}
