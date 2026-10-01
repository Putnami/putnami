// Package artifactstore is the FLAT, machine-global, content-addressed store for
// binary artifacts — extension/template binaries and the prebuilt CLI — shared
// across every repo and worktree on a machine.
//
// Unlike the per-repo build store (internal/store), it has NO repo-id
// sub-level: a (name, version, os/arch) download archive is byte-identical
// everywhere, so one entry serves all checkouts. Entries are whole directories
// addressed by the BARE lowercase-hex SHA-256 of their download archive — the
// exact value the lock records in LockEntry.Integrities["os/arch"], used
// verbatim so the zero-download fast path (Has(lockDigest)) actually matches on
// disk. (Note: this is deliberately NOT the "sha256:"-prefixed form the build
// store's CAS uses — mixing the two would silently never match.)
//
// Layout:
//
//	<root>/                              (~/.putnami/artifacts, or $PUTNAMI_ARTIFACT_DIR)
//	  sha256/<digest[:2]>/<digest>/      extracted tree; manifest at the dir root
//	    lastused                         recency sidecar for GC (unix-nano)
//	  cli/<sha>/putnami                  prebuilt CLI, by binary digest
//	  cli-source/<key>/putnami           CLI built from a source workspace, by source key
//	  roots/<id> → <workspace>           GC roots: workspaces whose links pin entries
//	  tmp/                               staging (same volume → intra-device rename)
//	  locks/<digest>.lock                per-digest computation ownership
//	  .lock                              advisory flock (admit=shared, GC=exclusive)
package artifactstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.putnami.dev/tooling/cli/internal/flock"
)

const (
	lockFile    = ".lock"
	digestLocks = "locks"
	tmpDirName  = "tmp"
	shaDirName  = "sha256"
	cliDirName  = "cli" // putnamiw publishes the prebuilt CLI under <root>/cli/<sha>/
	// cliSourceDirName holds the CLIs putnamiw builds from a source workspace,
	// one per source key: <root>/cli-source/<key>/putnami.
	cliSourceDirName = "cli-source"
	lastUsedFile     = "lastused"
	// dirPerm is intentionally tighter than the build store's 0o755: these
	// directories hold executed binaries, i.e. a code-execution boundary.
	dirPerm os.FileMode = 0o700
)

// Store is an artifact store rooted at a single flat directory, resolved by
// store.ResolveArtifactStoreRoot. It carries no in-process mutex: each admit
// stages into its own unique tmp dir and publishes by atomic rename, so
// concurrent goroutines (and processes) need only the cross-process flock.
type Store struct {
	root string
}

// New returns a Store rooted at root (an absolute directory).
func New(root string) *Store { return &Store{root: root} }

// Root returns the store root directory.
func (s *Store) Root() string { return s.root }

// Path returns the on-disk directory for digest:
// <root>/sha256/<digest[:2]>/<digest>. The two-char shard bounds readdir/inode
// pressure across all extensions, templates, and CLIs on a busy machine. digest
// must be a valid hex digest (callers gate via Has or isHexDigest).
func (s *Store) Path(digest string) string {
	return filepath.Join(s.root, shaDirName, digest[:2], digest)
}

// Has reports whether a published artifact directory exists for digest. It
// returns false for an invalid digest, so an attacker-influenced path component
// never reaches the filesystem.
func (s *Store) Has(digest string) bool {
	if !isHexDigest(digest) {
		return false
	}
	info, err := os.Stat(s.Path(digest))
	return err == nil && info.IsDir()
}

// Touch stamps the artifact's last-used time so GC's recency/grace spares it.
// Best-effort and lock-free (an atomic sidecar write); a missing or invalid
// entry is a silent no-op. EnsureExtensions calls this on every digest the lock
// pins each run, so "idle" means "no worktree that pins this binary ran
// recently" WITHOUT enumerating worktrees.
func (s *Store) Touch(digest string) {
	if !isHexDigest(digest) {
		return
	}
	dir := s.Path(digest)
	if _, err := os.Stat(dir); err != nil {
		return
	}
	stampUsed(dir, time.Now())
}

// IsBookkeeping reports whether name, an entry at the root of an artifact
// directory, is store bookkeeping rather than artifact payload: the lastused
// recency sidecar and the temporary files its writers rename into place (Touch
// writes "lastused-<random>", the putnamiw wrapper writes ".lastused.<pid>").
// The store writes these names into every entry it serves, so a reader that
// copies an artifact's payload out of the store, such as rendering a template
// into a project, skips them. An artifact never ships a root file under these
// names.
func IsBookkeeping(name string) bool {
	return name == lastUsedFile ||
		strings.HasPrefix(name, lastUsedFile+"-") ||
		strings.HasPrefix(name, "."+lastUsedFile+".")
}

// isHexDigest reports whether d is a 64-char lowercase-hex SHA-256 — the exact
// shape lockfile.HashFile produces (hex.EncodeToString of a 32-byte sum). It is
// also a SECURITY control: it rejects "..", "/", "sha256:"-prefixed strings,
// and any other path-traversal payload a malicious lock could smuggle through a
// digest field before it ever reaches filepath.Join.
func isHexDigest(d string) bool {
	if len(d) != 64 {
		return false
	}
	for i := 0; i < len(d); i++ {
		c := d[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// stampUsed atomically writes the last-used unix-nano timestamp into dir.
func stampUsed(dir string, t time.Time) {
	tmp, err := os.CreateTemp(dir, lastUsedFile+"-") // IsBookkeeping covers this name
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	_, werr := tmp.WriteString(strconv.FormatInt(t.UnixNano(), 10))
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		os.Remove(tmpName)
		return
	}
	if err := os.Rename(tmpName, filepath.Join(dir, lastUsedFile)); err != nil {
		os.Remove(tmpName)
	}
}

// WithShared runs fn while holding the store's advisory lock in SHARED mode, so
// a concurrent GC (which takes it EXCLUSIVE) cannot evict a digest dir that fn
// resolves and then links/stamps in the window between the Has check and the
// link. This is the read-side analog of the build store's getAndTouch:
// lock-free resolution races the cross-worktree GC and can otherwise leave a
// dangling symlink (the link's target swept between Has and Symlink) or, worse,
// delete a per-worktree fallback after relinking to a target GC just evicted.
// Sibling admits also hold the lock shared, so they coexist with fn. Best-effort:
// on a lock error fn still runs (degrading to rename-atomicity only), matching
// the rest of the store.
func (s *Store) WithShared(fn func()) {
	if err := s.WithSharedContext(context.Background(), fn); err != nil {
		// Preserve the historical best-effort contract: ordinary warm paths
		// still execute their atomic callback when the advisory lock itself is
		// unavailable. Only the explicit context API below fails closed.
		fn()
	}
}

// WithSharedContext is WithShared with a bounded lock acquisition. Callers on
// latency-sensitive read paths can therefore fall back when GC holds the store
// instead of waiting past the read's deadline.
func (s *Store) WithSharedContext(ctx context.Context, fn func()) error {
	release, err := s.lockSharedContext(ctx)
	if err != nil {
		return err
	}
	defer release()
	fn()
	return nil
}

// lockSharedContext takes the store-wide advisory lock in SHARED mode. Admit
// treats a failure as fatal because per-digest ownership relies on this lock
// excluding GC while the ownership inode exists.
func (s *Store) lockSharedContext(ctx context.Context) (func(), error) {
	if err := os.MkdirAll(s.root, dirPerm); err != nil {
		return nil, err
	}
	l, err := acquireFlockContext(ctx, filepath.Join(s.root, lockFile), false)
	if err != nil {
		return nil, err
	}
	return func() { _ = l.Release() }, nil
}

// lockDigestContext takes exclusive ownership of one content digest while
// leaving admits for every other digest concurrent. Callers hold the store-wide
// shared lock first, so GC cannot remove the lock or published entry between the
// post-lock existence check and publication.
func (s *Store) lockDigestContext(ctx context.Context, digest string) (func(), error) {
	root := filepath.Join(s.root, digestLocks)
	if err := os.MkdirAll(root, dirPerm); err != nil {
		return nil, err
	}
	l, err := acquireFlockContext(ctx, filepath.Join(root, digest+".lock"), true)
	if err != nil {
		return nil, err
	}
	return func() { _ = l.Release() }, nil
}

func acquireFlockContext(ctx context.Context, path string, exclusive bool) (*flock.Lock, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// An unbounded caller has nothing to poll for. Let the kernel park the
	// process in flock(2) as it always did, instead of waking every 10ms to ask
	// again while GC holds the store.
	if ctx.Done() == nil {
		return flock.Acquire(path, exclusive, false)
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		lock, err := flock.Acquire(path, exclusive, true)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, flock.ErrBusy) {
			return nil, err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
