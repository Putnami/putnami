package artifactstore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/tooling/cli/internal/flock"
)

// admitDir admits a digest derived from key whose payload is size bytes, and
// returns the digest.
func admitDir(t *testing.T, s *Store, key string, size int) string {
	t.Helper()
	d := hexDigest(key)
	if _, err := s.Admit(d, func(stage string) error {
		return os.WriteFile(filepath.Join(stage, "bin"), make([]byte, size), 0o755)
	}); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestGC_IdleReclaim(t *testing.T) {
	s := New(t.TempDir())
	keep := admitDir(t, s, "keep", 16)
	idle := admitDir(t, s, "idle", 16)
	stampUsed(s.Path(idle), time.Now().Add(-48*time.Hour)) // unused beyond MaxIdle

	res, err := s.GC(GCOptions{MaxBytes: 1 << 30, Grace: time.Hour, MaxIdle: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if res.EvictedIdle != 1 {
		t.Errorf("EvictedIdle = %d, want 1", res.EvictedIdle)
	}
	if s.Has(idle) {
		t.Error("idle entry should be evicted")
	}
	if !s.Has(keep) {
		t.Error("recent entry should be kept")
	}
}

func TestGC_GraceProtectsRecent(t *testing.T) {
	s := New(t.TempDir())
	d := admitDir(t, s, "recent", 16)
	// Idle threshold tiny and budget tiny, but a large grace window protects a
	// just-used entry from both passes.
	res, err := s.GC(GCOptions{MaxBytes: 1, Grace: time.Hour, MaxIdle: time.Nanosecond})
	if err != nil {
		t.Fatal(err)
	}
	if res.EvictedIdle != 0 || res.EvictedBudget != 0 {
		t.Errorf("entry within grace must not be evicted, got %+v", res)
	}
	if !s.Has(d) {
		t.Error("entry within grace should survive")
	}
}

func TestGC_BudgetLRU(t *testing.T) {
	s := New(t.TempDir())
	old := admitDir(t, s, "old", 1024)
	mid := admitDir(t, s, "mid", 1024)
	recent := admitDir(t, s, "recent", 1024)
	now := time.Now()
	stampUsed(s.Path(old), now.Add(-3*time.Hour))
	stampUsed(s.Path(mid), now.Add(-2*time.Hour))
	stampUsed(s.Path(recent), now.Add(-1*time.Hour))

	// ~3KB total (1024B payload each; the lastused sidecar is excluded from the
	// budget), 2KB budget (1638B watermark), grace 0 → evict oldest-first to the
	// watermark: old (3072→2048) then mid (2048→1024) cross it; recent survives.
	res, err := s.GC(GCOptions{MaxBytes: 2048, Grace: 0, MaxIdle: 0})
	if err != nil {
		t.Fatal(err)
	}
	if res.EvictedBudget != 2 {
		t.Errorf("EvictedBudget = %d, want exactly 2 (old, mid)", res.EvictedBudget)
	}
	if want := int64(2048 * 8 / 10); res.TotalBytes > want {
		t.Errorf("TotalBytes after GC = %d, want <= watermark %d", res.TotalBytes, want)
	}
	if s.Has(old) || s.Has(mid) {
		t.Error("the two oldest entries should be evicted to reach the watermark")
	}
	if !s.Has(recent) {
		t.Error("newest entry should survive")
	}
}

// TestGC_SkipsWhenLockErrors verifies the M1 fix: a destructive GC pass must NOT
// run its RemoveAll loops when the exclusive lock cannot be genuinely held — even
// in blocking mode. We force a real lock error (not contention) by replacing the
// lock file with a directory, so OpenFile(.lock, O_RDWR) fails.
func TestGC_SkipsWhenLockErrors(t *testing.T) {
	s := New(t.TempDir())
	d := admitDir(t, s, "x", 16)
	stampUsed(s.Path(d), time.Now().Add(-48*time.Hour)) // idle + over any budget

	lockPath := filepath.Join(s.root, lockFile)
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(lockPath, 0o700); err != nil { // O_RDWR on a dir → EISDIR
		t.Fatal(err)
	}

	res, err := s.GC(GCOptions{MaxBytes: 1, Grace: 0, MaxIdle: time.Nanosecond})
	if err != nil {
		t.Fatal(err)
	}
	if res.Scanned != 0 || res.EvictedIdle != 0 || res.EvictedBudget != 0 {
		t.Errorf("a blocking GC that cannot lock must skip, got %+v", res)
	}
	if !s.Has(d) {
		t.Error("entry must survive: GC must not delete without the exclusive lock")
	}
}

// TestClean_ErrorsWhenLockUnavailable verifies Clean refuses to wipe lock-free.
func TestClean_ErrorsWhenLockUnavailable(t *testing.T) {
	s := New(t.TempDir())
	d := admitDir(t, s, "x", 16)

	lockPath := filepath.Join(s.root, lockFile)
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(lockPath, 0o700); err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.Clean(0); err == nil {
		t.Error("Clean must error when it cannot hold the exclusive lock")
	}
	if !s.Has(d) {
		t.Error("Clean must not remove anything when it cannot lock")
	}
}

// TestTrashDir_MovesOutOfCanonicalTree exercises the M2 demotion primitive.
func TestTrashDir_MovesOutOfCanonicalTree(t *testing.T) {
	s := New(t.TempDir())
	d := admitDir(t, s, "corrupt", 16)
	dir := s.Path(d)
	if !s.trashDir(dir) {
		t.Fatal("trashDir should succeed")
	}
	if s.Has(d) {
		t.Error("Has must be false after the dir is trashed")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("canonical dir should no longer exist")
	}
	entries, _ := os.ReadDir(filepath.Join(s.root, tmpDirName))
	found := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "evicted-") {
			found = true
		}
	}
	if !found {
		t.Error("trashed dir should be demoted under tmp/evicted-*")
	}
}

// TestGC_TrashesUndeletableEntry verifies the M2 fix end-to-end: when RemoveAll
// fails partway, evict demotes the (now manifest-less) remnant out of the
// canonical tree so Has() stops reporting a wedged entry.
func TestGC_TrashesUndeletableEntry(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions; cannot force a RemoveAll failure")
	}
	s := New(t.TempDir())
	d := admitDir(t, s, "stuck", 16)
	dir := s.Path(d)

	// A child dir with no write bit makes RemoveAll unable to unlink its contents,
	// so the recursive delete errors and leaves the entry partially present.
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sub, 0o500); err != nil {
		t.Fatal(err)
	}
	// Restore write bits before t.TempDir's RemoveAll runs (cleanups are LIFO, so
	// this runs before the TempDir cleanup registered by New(t.TempDir())).
	t.Cleanup(func() {
		_ = filepath.WalkDir(s.root, func(p string, dd os.DirEntry, err error) error {
			if err == nil && dd.IsDir() {
				_ = os.Chmod(p, 0o700)
			}
			return nil
		})
	})
	stampUsed(dir, time.Now().Add(-48*time.Hour))

	res, err := s.GC(GCOptions{MaxBytes: 1 << 30, Grace: time.Hour, MaxIdle: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if res.EvictedIdle != 1 {
		t.Errorf("EvictedIdle = %d, want 1 (demoted)", res.EvictedIdle)
	}
	if s.Has(d) {
		t.Error("a partially-deletable entry must be demoted out of the canonical tree, not left wedged")
	}
}

// TestGC_ReclaimsIdleCLIBlob and TestClean_RemovesCLIBlobs verify the M5 fix:
// the prebuilt-CLI blobs putnamiw publishes under cli/<sha>/ are now reclaimed by
// the artifact GC and cleared by Clean, so they no longer leak unbounded.
func TestGC_ReclaimsIdleCLIBlob(t *testing.T) {
	s := New(t.TempDir())
	cliDir := filepath.Join(s.root, cliDirName, "deadbeefcafe")
	if err := os.MkdirAll(cliDir, dirPerm); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cliDir, "putnami"), make([]byte, 32), 0o755); err != nil {
		t.Fatal(err)
	}
	stampUsed(cliDir, time.Now().Add(-48*time.Hour))

	res, err := s.GC(GCOptions{MaxBytes: 1 << 30, Grace: time.Hour, MaxIdle: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if res.EvictedIdle != 1 {
		t.Errorf("EvictedIdle = %d, want 1 (idle CLI blob)", res.EvictedIdle)
	}
	if _, err := os.Stat(cliDir); !os.IsNotExist(err) {
		t.Error("an idle CLI blob should be reclaimed by GC")
	}
}

func TestClean_RemovesCLIBlobs(t *testing.T) {
	s := New(t.TempDir())
	d := admitDir(t, s, "ext", 16) // a verified sha256 entry
	cliDir := filepath.Join(s.root, cliDirName, "cabba6ef00d")
	if err := os.MkdirAll(cliDir, dirPerm); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cliDir, "putnami"), make([]byte, 16), 0o755); err != nil {
		t.Fatal(err)
	}

	dirs, _, err := s.Clean(0)
	if err != nil {
		t.Fatal(err)
	}
	if dirs < 2 {
		t.Errorf("Clean should remove the sha256 entry AND the CLI blob, got %d", dirs)
	}
	if _, err := os.Stat(cliDir); !os.IsNotExist(err) {
		t.Error("Clean --all should clear CLI blobs")
	}
	if s.Has(d) {
		t.Error("Clean should remove sha256 entries too")
	}
}

// TestClean_GraceSparesInUseEntry verifies the fix: a grace-aware clean
// (what `cache clean --all` runs) spares a digest dir kept warm by an active
// sibling worktree, while still reclaiming a stale one — so a concurrent clean
// can no longer strand a binary a running job is mid-exec on.
func TestClean_GraceSparesInUseEntry(t *testing.T) {
	s := New(t.TempDir())
	warm := admitDir(t, s, "warm", 16)
	stale := admitDir(t, s, "stale", 16)
	stampUsed(s.Path(warm), time.Now())                    // just used by a live run
	stampUsed(s.Path(stale), time.Now().Add(-2*time.Hour)) // not used in a while

	dirs, _, err := s.Clean(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if dirs != 1 {
		t.Errorf("Clean(grace) should remove only the stale entry, got %d", dirs)
	}
	if !s.Has(warm) {
		t.Error("an entry used within grace must survive a grace-aware clean")
	}
	if s.Has(stale) {
		t.Error("a stale entry must still be reclaimed")
	}

	// grace<=0 is the unconditional wipe: even the warm entry goes.
	if _, _, err := s.Clean(0); err != nil {
		t.Fatal(err)
	}
	if s.Has(warm) {
		t.Error("Clean(0) must wipe unconditionally, including recently-used entries")
	}
}

func TestClean_PrunesDigestOwnershipLockUnderExclusiveStoreLock(t *testing.T) {
	s := New(t.TempDir())
	digest := admitDir(t, s, "owned", 16)
	lockPath := filepath.Join(s.root, digestLocks, digest+".lock")
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("digest ownership lock missing after admit: %v", err)
	}
	if _, _, err := s.Clean(0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("orphan digest ownership lock survived clean: %v", err)
	}
}

// TestWithShared_HoldsLockAgainstGC verifies the M3/M4 fix: WithShared genuinely
// holds the store's shared lock for the duration of fn, so a concurrent GC (which
// needs the exclusive lock) cannot evict an entry mid-resolution.
func TestWithShared_HoldsLockAgainstGC(t *testing.T) {
	s := New(t.TempDir())
	d := admitDir(t, s, "warm", 16)
	stampUsed(s.Path(d), time.Now().Add(-48*time.Hour)) // idle → GC would evict

	entered := make(chan struct{})
	releaseFn := make(chan struct{})
	done := make(chan struct{})
	go func() {
		s.WithShared(func() {
			close(entered)
			<-releaseFn
		})
		close(done)
	}()
	<-entered

	res, err := s.GC(GCOptions{MaxBytes: 1, Grace: 0, MaxIdle: time.Nanosecond, NonBlocking: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Scanned != 0 || res.EvictedIdle != 0 {
		t.Errorf("GC must skip while a shared lock is held, got %+v", res)
	}
	if !s.Has(d) {
		t.Error("entry must survive: GC could not evict under the held shared lock")
	}

	close(releaseFn)
	<-done
}

func TestGC_EvictThenHeal(t *testing.T) {
	s := New(t.TempDir())
	d := admitDir(t, s, "heal", 16)
	stampUsed(s.Path(d), time.Now().Add(-48*time.Hour))
	if _, err := s.GC(GCOptions{MaxBytes: 1 << 30, Grace: time.Hour, MaxIdle: 24 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	if s.Has(d) {
		t.Fatal("entry should be evicted")
	}
	// Self-heal: a re-admit re-materializes the evicted digest.
	if _, err := s.Admit(d, func(stage string) error {
		return os.WriteFile(filepath.Join(stage, "bin"), []byte("x"), 0o755)
	}); err != nil {
		t.Fatal(err)
	}
	if !s.Has(d) {
		t.Error("re-admit should re-materialize the evicted digest")
	}
}

func TestGC_SweepsStaleStaging(t *testing.T) {
	s := New(t.TempDir())
	tmpRoot := filepath.Join(s.root, tmpDirName)
	stale := filepath.Join(tmpRoot, "staging-stale")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	_ = os.Chtimes(stale, old, old)

	res, err := s.GC(GCOptions{MaxBytes: 1 << 30, Grace: time.Hour, MaxIdle: 0})
	if err != nil {
		t.Fatal(err)
	}
	if res.SweptStaging != 1 {
		t.Errorf("SweptStaging = %d, want 1", res.SweptStaging)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("stale staging dir should be swept")
	}
}

func TestGC_NonBlockingSkipsBusyStore(t *testing.T) {
	s := New(t.TempDir())
	admitDir(t, s, "x", 16)

	// Hold the store lock exclusively from another file descriptor.
	if err := os.MkdirAll(s.root, dirPerm); err != nil {
		t.Fatal(err)
	}
	held, err := flock.Acquire(filepath.Join(s.root, lockFile), true, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Release() }()

	res, err := s.GC(GCOptions{MaxBytes: 1, Grace: 0, MaxIdle: time.Nanosecond, NonBlocking: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Scanned != 0 || res.EvictedIdle != 0 || res.EvictedBudget != 0 {
		t.Errorf("non-blocking GC should skip a busy store, got %+v", res)
	}
}

func TestResolveGCOptions_EnvOverride(t *testing.T) {
	t.Setenv(artifactMaxBytesEnv, "12345")
	t.Setenv(artifactGCGraceEnv, "30m")
	t.Setenv(artifactMaxIdleEnv, "240h")
	o := ResolveGCOptions()
	if o.MaxBytes != 12345 {
		t.Errorf("MaxBytes = %d, want 12345", o.MaxBytes)
	}
	if o.Grace != 30*time.Minute {
		t.Errorf("Grace = %v, want 30m", o.Grace)
	}
	if o.MaxIdle != 240*time.Hour {
		t.Errorf("MaxIdle = %v, want 240h", o.MaxIdle)
	}
}
