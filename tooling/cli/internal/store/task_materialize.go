package store

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	cache "go.putnami.dev/protocol/cache"
	proto "go.putnami.dev/protocol/extension"
)

// The one atomic materialize primitive for task-owned entries.
//
// Every restore shape — a file output, a directory output, a cold-path staging
// swap, a remote hit (B4c) — goes through publishAtomically: stage the
// replacement under a unique name, in the caller's staging root when rename(2)
// can reach the destination from there and beside the destination otherwise,
// populate it fully, then swap it in with rename(2). Nothing is ever written
// INTO a live destination path. Two failure modes this repo has already paid
// for are why:
//
//   - rename(2) CANNOT put a directory where a symlink or a file lives: it fails
//     with ENOTDIR, so "rename the new tree over the old link" is not an
//     option. The swap therefore moves the existing path out of the way
//     first — exchanging it with the staged tree, or renaming it ASIDE — and
//     only then reclaims the old inode. Both move a symlink's LINK, never its
//     target, so a stale out/ link is detached without touching the CAS blob
//     behind it.
//   - Writing over a running executable poisons the code-signing vnode cache on
//     macOS, wedging every later exec of that path. Staging plus rename gives the
//     new bytes a new inode and leaves the old one intact for whoever still holds
//     it open, which is exactly what a downstream job executing a restored tool
//     binary needs.
//
// Replacing an existing directory is ONE syscall where the host can exchange two
// paths atomically: RENAME_EXCHANGE on linux, RENAME_SWAP on darwin
// (exchangePaths). The destination entry names the old complete tree until
// the exchange and the new complete tree after it, with no instant in between.
// POSIX rename has no such operation, so where the exchange fails — an old
// kernel, a filesystem without it, an overlayfs directory it would have to copy
// up, any other platform — the swap falls back to two renames, and a lookup
// finds the old complete tree, briefly nothing, or the new complete tree.
// Neither form ever shows a half-populated destination, because population
// happens under the staging name. The "briefly nothing" is not academic: a
// reader that walks a directory by path turns it into ENOENT.
//
// A directory output that cedes subpaths is the one exception: it is merged into
// an existing destination entry by entry instead of swapped over it whole, so a
// ceded subtree is never moved at all (see MaterializeTaskOutput and mergeIn).
//
// What no form changes is what a reader already holding a directory of a
// replaced tree sees: that directory is emptied as it is reclaimed. This
// includes a pathname lookup that has traversed the replaced directory but
// not yet resolved its descendant, or a metadata operation on the retired
// inode: it can return ENOENT. An atomic exchange protects the directory entry,
// not a snapshot of a traversal through it.
// Every directory swap here has always had that property.

// materializeSwapAttempts bounds the retry loop around the directory swap. Each
// retry costs a few renames, never a re-copy: a lost race leaves the staged tree
// untouched. A path still contended after this many rounds is a real problem
// rather than a lost race (same reasoning as MaterializeDirSymlink), unless the
// last failure says another process holds a handle in the tree: Windows refuses
// to rename a directory while anything inside it is open, and the loop then
// keeps going, paced, for as long as holderWait allows since dest last took a
// new tree.
const materializeSwapAttempts = 8

// pathExchange is the one-step swap of two existing paths (exchangePaths). It is
// a variable only so a test can drive the two-rename fallback on a filesystem
// that supports the exchange.
var pathExchange = exchangePaths

// errKeptConflict marks a restored entry that holds bytes where its declaration
// cedes the path (see refuseCededBytes).
var errKeptConflict = errors.New("the restored entry holds bytes at a ceded path")

// MaterializeTaskOutput places one of an entry's recorded outputs at dest and
// reports whether any bytes were written.
//
// An output recorded TaskOutputEmpty returns (false, nil) WITHOUT touching dest.
// That is the binding invariant of the explicit empty state: command-output
// paths are shared between the steps of one command, so treating "this entry has
// nothing for this output" as "this path should be empty" would delete a sibling
// task's artifacts. An empty output is a statement about the task that produced
// it, not about the destination.
//
// The store's SHARED lock is held for the whole materialize, so a concurrent GC
// (which needs the exclusive lock) cannot reclaim the blob mid-copy — the same
// protection Put and Materialize rely on, extended to the restore path.
//
// keep is for a directory output that CEDES subpaths (protocol ADR 0003): it
// lists them, output-relative and in slash form, and whatever the destination
// already holds at each of them is left exactly as it is.
//
// The ceding task never captured those bytes, so its entry has nothing to put
// there — but "nothing to restore" must mean "leave it alone", not "delete it".
// The subtree belongs to another task, and that task may have produced it in a
// context this restore knows nothing about: a `package` command's describe that
// executed while this `test` command's generate is served from cache, with a
// `publish` step reading the contract path in between. Swapping a tree over
// .gen that lacks the bundle deleted exactly that (a remote-cache recurrence).
// A restore that executes the task instead would leave the ceded
// subtree untouched, and a restore must be indistinguishable from an execution.
//
// So a ceding output is merged into an existing destination directory rather
// than swapped over it (mergeIn): each entry it owns is replaced in place, each
// one it no longer has is removed, and a ceded subtree is never renamed, copied
// or linked, on any host, the two-rename fallback included. Its readers and a
// concurrent writer inside it see nothing happen, and a lock file there keeps
// its inode. The cost is that the owned region flips entry by entry rather than
// all at once, which is what an execution of the ceding task does too. Moving
// the ceded subtree across a whole-tree swap instead left it missing for the
// width of the swap: a `publish` step reading the migration bundle while a
// `test` generate restored .gen hit that window as ENOENT.
//
// stagingRoot is where the replacement is staged before it is renamed to dest.
// It is used when it can be created, does not lie inside dest, and is on the
// same filesystem as dest's parent. Then the restore adds no entry to dest's
// parent besides dest itself, so a task that walks that directory while another task's output is
// restored into it meets no staging path appearing and vanishing. An empty
// stagingRoot, or one that fails those conditions, stages beside dest under a
// hidden unique `.<name>.tmp-materialize-*` name, because rename(2) cannot
// cross filesystems. A swap from stagingRoot that still fails with EXDEV (two
// mounts of one filesystem) is restaged beside dest and published again.
func (s *LocalStore) MaterializeTaskOutput(entry *TaskEntry, id, dest, stagingRoot string, keep ...string) (bool, error) {
	if entry == nil {
		return false, fmt.Errorf("materialize %q: nil entry", id)
	}
	out, ok := entry.Output(id)
	if !ok {
		return false, fmt.Errorf("materialize %q: entry for %s does not declare this output", id, entry.Key)
	}
	if !out.Present() {
		return false, nil // explicit empty: leave dest exactly as it is
	}
	if entry.FilesDir == "" {
		return false, fmt.Errorf("materialize %q: entry for %s has no payload", id, entry.Key)
	}

	release := s.lockShared()
	defer release()

	src := filepath.Join(entry.FilesDir, id)
	modes := manifestModes(entry.Manifest)
	switch out.Kind {
	case proto.OutputKindFile:
		if err := publishAtomically(dest, stagingRoot, false, nil, func(staging string) error {
			return copyFileTo(src, staging, modes, id)
		}); err != nil {
			return false, fmt.Errorf("materialize %q: %w", id, err)
		}
	case proto.OutputKindDirectory:
		if err := publishAtomically(dest, stagingRoot, true, keep, func(staging string) error {
			return copyTreeIntoStaging(src, staging, modes, id+"/")
		}); err != nil {
			return false, fmt.Errorf("materialize %q: %w", id, err)
		}
	default:
		return false, fmt.Errorf("materialize %q: unknown output kind %q", id, out.Kind)
	}
	return true, nil
}

// renameStaged is os.Rename for the renames that move a staged path to dest. It
// is a variable only so a test can fail them the way a rename between two
// mounts of one filesystem does.
var renameStaged = os.Rename

// publishAtomically is the primitive: stage, populate, swap in.
//
// The replacement is staged in stagingRoot when stagingDirFor accepts it, and
// beside dest otherwise (see MaterializeTaskOutput). A swap from stagingRoot
// that fails with EXDEV has changed nothing a restore beside dest does not
// replace anyway, so the publish is repeated from a new staging path beside
// dest.
//
// isDir selects whether the staged replacement is a directory or a file; the two
// differ only in how the swap deals with what is already at dest. populate
// receives the staging path and must leave it fully populated — it is the only
// place bytes are written, and it never sees the destination.
//
// keep names the subtrees of a directory dest that the restore leaves alone (see
// MaterializeTaskOutput); a file keeps nothing.
func publishAtomically(dest, stagingRoot string, isDir bool, keep []string, populate func(staging string) error) error {
	parent := filepath.Dir(dest)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create output parent: %w", err)
	}
	stageIn := stagingDirFor(dest, stagingRoot)
	err := publishFrom(stageIn, dest, isDir, keep, populate)
	if stageIn != parent && crossDevice(err) {
		err = publishFrom(parent, dest, isDir, keep, populate)
	}
	return err
}

// stagingDirFor returns the directory a replacement of dest is staged in:
// stagingRoot when it is set, can be created, does not lie inside dest, and
// shares a filesystem with dest's parent, and dest's parent otherwise.
func stagingDirFor(dest, stagingRoot string) string {
	parent := filepath.Dir(dest)
	if stagingRoot == "" {
		return parent
	}
	if rel, err := filepath.Rel(dest, stagingRoot); err != nil || filepath.IsLocal(rel) {
		return parent
	}
	if err := os.MkdirAll(stagingRoot, 0o755); err != nil {
		return parent
	}
	if !sameFilesystem(stagingRoot, parent) {
		return parent
	}
	return stagingRoot
}

// publishFrom stages dest's replacement in dir, populates it, and swaps it in.
func publishFrom(dir, dest string, isDir bool, keep []string, populate func(staging string) error) error {
	// Unique staging names (not a fixed ".tmp" suffix) so two processes
	// materializing the same destination cannot delete each other's in-flight
	// copy. The cost is that a crash mid-materialize leaves an orphan: in the
	// staging root, the scratch reaper removes it with the rest of the scratch
	// generation; beside dest, nothing reclaims it automatically — the same
	// trade MaterializeDirSymlink makes.
	pattern := "." + filepath.Base(dest) + ".tmp-materialize-"
	var staging string
	var err error
	if isDir {
		staging, err = os.MkdirTemp(dir, pattern)
	} else {
		var f *os.File
		if f, err = os.CreateTemp(dir, pattern); err == nil {
			staging = f.Name()
			err = f.Close()
		}
	}
	if err != nil {
		return fmt.Errorf("stage output: %w", err)
	}
	// A successful rename moves staging away, so this no-ops; every failure path
	// needs it gone. After an exchange or a merge, staging holds only what the
	// swap replaced or pruned — never a byte of a ceded subtree — so reclaiming
	// it is always safe.
	defer func() { _ = os.RemoveAll(staging) }()

	if err := populate(staging); err != nil {
		return fmt.Errorf("populate staged output: %w", err)
	}
	return swapIn(staging, dest, isDir, keep)
}

// swapIn replaces dest with the fully populated staging path.
//
// A file swap is a single rename: rename(2) atomically replaces an existing file
// OR an existing symlink, so a reader holding dest open keeps reading the old
// inode and never sees a partial file. Only a DIRECTORY sitting at a file
// destination has to be removed first (EISDIR otherwise), which happens when a
// declaration changes an output's kind — and it is attempted only after the
// atomic rename has actually failed, so the removal never runs speculatively.
//
// A directory that keeps nothing is swapped whole. The direct rename comes first
// — it succeeds when dest is absent, which is the common cold case and leaves no
// window at all. An existing dest is then exchanged with the staged tree in one
// syscall, which leaves no window either, and the replaced tree is reclaimed
// from the staging path. Only when the host cannot exchange the two paths does
// it rename dest aside, rename the staged tree in, and reclaim the old inode,
// with dest absent in between. Losing a race to another materializer is retried
// rather than reported: the goal is that dest ends up holding a complete tree,
// not that this particular call is the one to put it there.
//
// A directory that keeps subtrees is merged into a dest that is already a
// directory (mergeIn), and swapped whole otherwise: an absent dest, a file or a
// symlink holds nothing to keep, and a symlink's target is deliberately not
// followed, for the same reason the swap moves the LINK.
//
// staging and dest need not share a parent directory, only a mount: rename(2)
// and the exchange move an entry between two directories of one mounted
// filesystem exactly as they do within one.
//
// On Windows a rename fails while another process holds a handle on the file,
// or anywhere inside the directory, that it moves or replaces: a reader of
// dest, a scanner of the freshly populated staging tree. Such a failure is
// waited out (holderWait) rather than reported. A file's first rename is not
// waited on, because Windows gives the same answer when dest is a directory.
// For the same reason a directory rename that loses to another materializer,
// which refilled dest, reads as a held handle. The wait's budget therefore
// counts only the time dest keeps one tree: each new tree another publisher
// puts there starts the wait over, so a peer that keeps publishing is
// outlasted, and a holder that keeps dest unchanged ends the wait.
func swapIn(staging, dest string, isDir bool, keep []string) error {
	if !isDir {
		if err := renameStaged(staging, dest); err == nil {
			return nil
		}
		if info, err := os.Lstat(dest); err == nil && info.IsDir() {
			if err := os.RemoveAll(dest); err != nil {
				return fmt.Errorf("remove stale output directory: %w", err)
			}
		}
		var wait holderWait
		for {
			err := renameStaged(staging, dest)
			if err == nil {
				return nil
			}
			if !wait.retry(err) {
				return fmt.Errorf("publish output file: %w", err)
			}
		}
	}

	exchange := true
	var lastErr error
	var wait holderWait
	var seen fs.FileInfo // the tree the wait last found at dest
	began, attempt, replaced := time.Now(), 0, 0
	for ; ; attempt++ {
		if attempt >= materializeSwapAttempts {
			// A new tree at dest since the last check is another publisher's
			// progress, not a held handle, so the wait starts over. SameFile
			// reads the identity now: Windows reads it by path, at the first
			// comparison.
			if current, err := os.Lstat(dest); err == nil && os.SameFile(current, current) {
				if seen != nil && !os.SameFile(seen, current) {
					wait, replaced = holderWait{}, replaced+1
				}
				seen = current
			}
			if !wait.retry(lastErr) {
				break
			}
		}
		// Direct rename: valid when dest does not exist (and, on some systems,
		// when it is an empty directory).
		err := renameStaged(staging, dest)
		if err == nil {
			return nil
		}
		if crossDevice(err) {
			// staging and dest are on two mounts, so no exchange or rename
			// below can move anything between them either. The caller restages
			// beside dest; retrying here would bury EXDEV under a later error.
			return fmt.Errorf("publish output directory: %w", err)
		}
		if len(keep) > 0 {
			if info, err := os.Lstat(dest); err == nil && info.IsDir() {
				return mergeIn(staging, dest, keep)
			}
		}

		if exchange {
			err := pathExchange(staging, dest)
			if err == nil {
				// staging names the replaced entry now. When it was a symlink
				// this unlinks the LINK and leaves the CAS blob it named intact.
				_ = os.RemoveAll(staging)
				return nil
			}
			// A failed exchange changes nothing. A vanished dest is a lost race
			// the next direct rename wins; any other failure means this host
			// cannot exchange these paths, so the rest of this call renames.
			lastErr = fmt.Errorf("exchange output directory: %w", err)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			exchange = false
		}

		// Move whatever is at dest out of the way, under a name derived from
		// staging's: in the directory staging was created in for a whole
		// output (the staging root, or dest's parent when the restore stages
		// beside dest), inside the enclosing staging tree for an entry mergeIn
		// replaces, where no reader of dest can see it. Renaming a symlink moves
		// the link itself, so the CAS blob a stale out/ link points at is
		// untouched.
		aside := staging + ".tmp-replaced-" + randomSuffix()
		if detachErr := os.Rename(dest, aside); detachErr != nil {
			if !os.IsNotExist(detachErr) {
				lastErr = fmt.Errorf("detach existing output: %w", detachErr)
				continue
			}
			// dest is absent: another materializer swapped it away, or the
			// direct rename failed with nothing at dest. Retry the direct rename
			// with the staged tree still intact; its error is the one to report.
			lastErr = fmt.Errorf("publish output directory: %w", err)
			continue
		}

		if err := renameStaged(staging, dest); err != nil {
			// Another materializer refilled dest between our two renames. Put
			// the original back so a lost race never destroys the tree we
			// detached, and retry.
			if restoreErr := os.Rename(aside, dest); restoreErr != nil {
				_ = os.RemoveAll(aside)
			}
			lastErr = fmt.Errorf("publish output directory: %w", err)
			continue
		}
		// Reclaim the replaced path. When it was a symlink this unlinks the LINK
		// and leaves the CAS blob it named intact.
		_ = os.RemoveAll(aside)
		return nil
	}
	return fmt.Errorf("publish output directory: %s stayed contended for %s over %d rounds, and other publishers replaced it %d times: %w",
		dest, time.Since(began).Round(time.Millisecond), attempt, replaced, lastErr)
}

// mergeIn restores a ceding directory output into dest, an existing directory,
// without touching a ceded subtree — the way an execution of the ceding task
// leaves it (see MaterializeTaskOutput).
//
// dest keeps its own inode and takes the staged root's mode, so the result
// matches a fresh run. Each entry of the staged root replaces its namesake
// through swapIn, which merges again one level down when the entry leads to a
// kept path and swaps it whole otherwise. Each entry dest holds that the staged
// root lacks is pruned (pruneAround). staging ends up holding only what was
// replaced or pruned, for the caller to reclaim.
//
// The owned region therefore flips entry by entry. An owned file is always
// replaced by one rename. With the exchange, the entry of a directory the two
// trees share is never absent; without it, each owned directory has its own
// two-rename window. Traversing into a replaced directory can still race its
// reclamation. A ceded path has neither race, on any host.
//
// An owner that stages in a staging root has no in-flight staging inside dest.
// One that stages beside its destination (no staging root, or one it cannot
// rename from) does: a restore of .gen/migration-bundle then stages at
// .gen/.migration-bundle.tmp-materialize-*. That directory is not in the staged
// tree, so it is pruned as stale, just as the whole-tree swap moved it away, and
// that owner's restore fails over to an execution.
func mergeIn(staging, dest string, keep []string) error {
	if err := refuseCededBytes(staging, keep); err != nil {
		return fmt.Errorf("publish output directory: %w", err)
	}
	root, err := os.Lstat(staging)
	if err != nil {
		return fmt.Errorf("publish output directory: %w", err)
	}
	if err := os.Chmod(dest, root.Mode().Perm()); err != nil {
		return fmt.Errorf("publish output directory: %w", err)
	}
	entries, err := os.ReadDir(staging)
	if err != nil {
		return fmt.Errorf("publish output directory: %w", err)
	}
	replaced := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		replaced = append(replaced, name)
		if err := swapIn(filepath.Join(staging, name), filepath.Join(dest, name), e.IsDir(), keptUnder(keep, name)); err != nil {
			return err
		}
	}
	return pruneAround(dest, staging, keep, replaced)
}

// refuseCededBytes fails with errKeptConflict, before dest is touched, when the
// staged tree holds anything at a kept path or anything but a directory on the
// way to one. The ceding task's entry never captures ceded bytes
// (stageDeclaredOutput skips them and the cache key folds the carve-out), so
// finding some means the entry and the declaration disagree, and neither copy
// can be trusted over the other. A file on the way to a kept path would replace
// the directory that holds it.
func refuseCededBytes(staging string, keep []string) error {
	for _, rel := range keep {
		at := staging
		parts := strings.Split(rel, "/")
		for i, part := range parts {
			at = filepath.Join(at, part)
			info, err := os.Lstat(at)
			if errors.Is(err, fs.ErrNotExist) {
				break
			}
			if err != nil {
				return fmt.Errorf("keep ceded %s: %w", rel, err)
			}
			if i == len(parts)-1 || !info.IsDir() {
				return fmt.Errorf("keep ceded %s: %w", rel, errKeptConflict)
			}
		}
	}
	return nil
}

// pruneAround moves out of dir every entry that is not in replaced, not kept,
// and not a directory on the way to a kept path, which it prunes the same way
// one level down. A stale entry leaves dir in one rename, into trash, so a
// reader finds it whole or gone and never half-deleted, and it is reclaimed
// with trash. A symlink on the way to a kept path is stale like any other
// entry: its target is never followed.
//
// Names compare case-insensitively, like every other ownership comparison
// (isExcludedOutputPath, DecidableExcludes): the capture skipped the ceded
// subtree under whatever spelling the disk held, so the restore must recognize
// it under that spelling too, or a kept path spelled `Migration-Bundle` in the
// declaration and `migration-bundle` on a case-insensitive disk is pruned as
// stale — the other task's bytes, gone.
func pruneAround(dir, trash string, keep []string, replaced []string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("publish output directory: %w", err)
	}
	for _, e := range entries {
		name := e.Name()
		sameName := func(n string) bool { return strings.EqualFold(n, name) }
		if slices.ContainsFunc(replaced, sameName) || slices.ContainsFunc(keep, sameName) {
			continue
		}
		p := filepath.Join(dir, name)
		if under := keptUnder(keep, name); len(under) > 0 && e.IsDir() {
			if err := pruneAround(p, trash, under, nil); err != nil {
				return err
			}
			continue
		}
		if err := os.Rename(p, filepath.Join(trash, "stale-"+randomSuffix())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("publish output directory: remove stale %s: %w", p, err)
		}
	}
	return nil
}

// keptUnder returns the kept paths strictly inside name, relative to it. The
// containment folds case, as pruneAround does.
func keptUnder(keep []string, name string) []string {
	var under []string
	for _, rel := range keep {
		if len(rel) > len(name)+1 && rel[len(name)] == '/' && strings.EqualFold(rel[:len(name)], name) {
			under = append(under, rel[len(name)+1:])
		}
	}
	return under
}

// copyTreeIntoStaging copies a blob subtree into a freshly created, still
// PRIVATE staging directory.
//
// It differs from copyDirUnsynced in dropping the per-file stage-then-rename.
// That idiom exists so a concurrent reader of a live destination never sees a
// half-written file, and nothing can reach this tree: it was created under a
// unique name a moment ago and becomes visible only when swapIn renames the
// whole directory. Paying for it here doubled the syscall count per file and
// dominated the cost of retiring the CAS-symlink fast path (see
// BenchmarkTaskOutputMaterializeVsSymlink). Durability is likewise skipped —
// these are workspace bytes the CAS still holds, so a crash reproduces them
// from the blob (see copyDirUnsynced).
//
// Each file takes the mode modes records for prefix plus its slash-form path
// relative to src; directories keep the blob tree's modes.
func copyTreeIntoStaging(src, dst string, modes recordedModes, prefix string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if info.IsDir() {
			if rel == "." {
				// The staging root already exists, but os.MkdirTemp created it
				// 0700. Its mode has to become the SOURCE's, or the restored
				// directory ends up owner-only where a fresh run leaves it
				// world-readable — a difference that outlives the build (another
				// uid reading .putnami/out, a docker context, a published tree).
				return os.Chmod(dst, info.Mode().Perm())
			}
			return os.MkdirAll(filepath.Join(dst, rel), info.Mode().Perm())
		}
		return writeFileInto(p, filepath.Join(dst, rel), modes.of(prefix+filepath.ToSlash(rel), info.Mode()))
	})
}

// recordedModes maps each manifest path to the permission bits the capture
// recorded for it.
//
// A restore applies these rather than the mode of the file in the entry's
// files/ tree. That file is a hardlink to a CAS blob every entry holding the
// same bytes shares, and each capture chmods the blob to its own mode, so the
// file carries the mode of whichever entry linked that blob last. The manifest
// is also the only record of the executable bit on a host whose filesystem has
// none.
type recordedModes map[string]os.FileMode

// manifestModes indexes a manifest's modes by path. A nil manifest yields an
// empty index, and every file then keeps the mode it has on disk.
func manifestModes(manifest *cache.Manifest) recordedModes {
	modes := recordedModes{}
	if manifest == nil {
		return modes
	}
	for _, file := range manifest.Files {
		modes[file.Path] = os.FileMode(file.Mode).Perm()
	}
	return modes
}

// of returns the permission bits recorded for path, or disk's when the manifest
// does not list it.
func (modes recordedModes) of(path string, disk os.FileMode) os.FileMode {
	if mode, ok := modes[path]; ok {
		return mode
	}
	return disk.Perm()
}

// writeFileInto copies src to a path nothing else can observe yet, applying the
// source mode explicitly so umask cannot drop the exec bit.
func writeFileInto(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Chmod(mode); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// copyFileTo copies src onto the already-created staging path, applying the
// mode modes records for path (including the exec bit), or src's own when the
// manifest does not list it. Unlike copyFile it does NOT stage and rename of
// its own accord: the caller's staging path IS the stage, and swapIn owns the
// rename, so there is exactly one publish step in the primitive.
func copyFileTo(src, staging string, modes recordedModes, path string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(staging, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	// os.CreateTemp opened the stage 0600; restore the recorded mode before the
	// swap so the destination never appears with the wrong permissions.
	if err := out.Chmod(modes.of(path, info.Mode())); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// randomSuffix returns a short unique token for a staging sibling name. It
// reuses the lease owner nonce rather than adding a second source of randomness.
func randomSuffix() string {
	return newLeaseOwner()
}
