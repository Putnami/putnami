package filelock

import (
	"bufio"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"
)

// holderVariable makes the test binary a lock holder instead of a test run:
// it takes an exclusive lock on the path the variable names, writes
// holderRecord through the locked handle, reports "held" on its standard
// output, and exits once its standard input closes. It never releases the
// lock; its exit does.
const holderVariable = "PUTNAMI_FILELOCK_TEST_HOLDER"

const holderRecord = "held by a child process\n"

func TestMain(m *testing.M) {
	if path := os.Getenv(holderVariable); path != "" {
		os.Exit(hold(path))
	}
	os.Exit(m.Run())
}

func hold(path string) int {
	lock, err := Acquire(path, true, false)
	if err != nil {
		return 2
	}
	if _, err := lock.File().WriteAt([]byte(holderRecord), 0); err != nil {
		return 2
	}
	if err := lock.File().Truncate(int64(len(holderRecord))); err != nil {
		return 2
	}
	if _, err := os.Stdout.WriteString("held\n"); err != nil {
		return 2
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	return 0
}

// startHolder starts a child process that holds an exclusive lock on path and
// returns once it holds it. Closing the returned writer makes the child exit.
func startHolder(t *testing.T, path string) (io.WriteCloser, *exec.Cmd) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), holderVariable+"="+path)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	})
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "held\n" {
		t.Fatalf("the lock holder answered %q (%v)", line, err)
	}
	return stdin, cmd
}

// lockPath returns a fresh path under a per-test temp dir for the lock file.
func lockPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "store.lock")
}

// TestSharedLocksCoexist verifies the core writer contract: two SHARED holders
// on the same path are both granted and coexist. Two Acquire calls in one
// process open two handles and behave like independent holders.
func TestSharedLocksCoexist(t *testing.T) {
	path := lockPath(t)

	a, err := Acquire(path, false, false)
	if err != nil {
		t.Fatalf("first SHARED acquire: %v", err)
	}
	t.Cleanup(func() { _ = a.Release() })

	b, err := Acquire(path, false, false)
	if err != nil {
		t.Fatalf("second SHARED acquire must coexist with the first: %v", err)
	}
	t.Cleanup(func() { _ = b.Release() })

	if err := a.Release(); err != nil {
		t.Errorf("Release first: %v", err)
	}
	if err := b.Release(); err != nil {
		t.Errorf("Release second: %v", err)
	}
}

// TestNonBlockingExclusiveBusyUnderShared verifies that a non-blocking EXCLUSIVE
// acquire reports ErrBusy (not a hang, not a different error) while a SHARED lock
// is held on the same path.
func TestNonBlockingExclusiveBusyUnderShared(t *testing.T) {
	path := lockPath(t)

	shared, err := Acquire(path, false, false)
	if err != nil {
		t.Fatalf("SHARED acquire: %v", err)
	}
	t.Cleanup(func() { _ = shared.Release() })

	l, err := Acquire(path, true, true)
	if l != nil {
		_ = l.Release()
		t.Fatal("non-blocking EXCLUSIVE must not be granted while a SHARED lock is held")
	}
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}

	// Once the shared holder releases, the same non-blocking acquire succeeds.
	if err := shared.Release(); err != nil {
		t.Fatalf("Release shared: %v", err)
	}
	l, err = Acquire(path, true, true)
	if err != nil {
		t.Fatalf("EXCLUSIVE acquire after SHARED released: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Errorf("Release exclusive: %v", err)
	}
}

// TestBlockingExclusiveWaitsForShared verifies the GC contract: a BLOCKING
// EXCLUSIVE acquire does not proceed until the SHARED holder releases. The wait
// is synchronized with channels; the bounded timeouts are only failure guards,
// never the synchronization mechanism.
func TestBlockingExclusiveWaitsForShared(t *testing.T) {
	path := lockPath(t)

	shared, err := Acquire(path, false, false)
	if err != nil {
		t.Fatalf("SHARED acquire: %v", err)
	}
	sharedReleased := false
	t.Cleanup(func() {
		if !sharedReleased {
			_ = shared.Release()
		}
	})

	acquired := make(chan *Lock, 1)
	acqErr := make(chan error, 1)
	go func() {
		l, err := Acquire(path, true, false) // blocking EXCLUSIVE
		if err != nil {
			acqErr <- err
			return
		}
		acquired <- l
	}()

	// The exclusive acquire must be blocked while the shared lock is held. Give
	// the goroutine a bounded window to (incorrectly) complete; if it stays
	// blocked, that is the expected outcome.
	select {
	case <-acquired:
		t.Fatal("blocking EXCLUSIVE acquired while a SHARED lock was still held")
	case err := <-acqErr:
		t.Fatalf("blocking EXCLUSIVE acquire errored: %v", err)
	case <-time.After(100 * time.Millisecond):
		// Still blocked, as required.
	}

	// Release the shared lock; the exclusive acquire must now complete.
	if err := shared.Release(); err != nil {
		t.Fatalf("Release shared: %v", err)
	}
	sharedReleased = true

	select {
	case l := <-acquired:
		if err := l.Release(); err != nil {
			t.Errorf("Release exclusive: %v", err)
		}
	case err := <-acqErr:
		t.Fatalf("blocking EXCLUSIVE acquire errored after SHARED released: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("blocking EXCLUSIVE did not proceed after the SHARED lock was released")
	}
}

// TestReleaseNilAndIdempotent verifies Release and Close are safe on a nil
// *Lock and idempotent: a second call is a no-op with no panic.
func TestReleaseNilAndIdempotent(t *testing.T) {
	var nilLock *Lock
	if err := nilLock.Release(); err != nil {
		t.Errorf("Release on nil *Lock: %v", err)
	}
	if err := nilLock.Close(); err != nil {
		t.Errorf("Close on nil *Lock: %v", err)
	}

	l, err := Acquire(lockPath(t), false, false)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Errorf("first Release: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Errorf("second Release must be a no-op: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Errorf("Close after Release must be a no-op: %v", err)
	}
}

// TestCloseEndsALockNothingInherited verifies that Close without a child
// holding a copy of the descriptor frees the lock at once, on every platform.
func TestCloseEndsALockNothingInherited(t *testing.T) {
	path := lockPath(t)
	l, err := Acquire(path, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := Acquire(path, true, true)
	if err != nil {
		t.Fatalf("the lock outlived Close: %v", err)
	}
	_ = again.Release()
}

// TestAnotherProcessExcludesAndItsExitWakesAWaiter verifies the cross-process
// contract: a lock another process holds answers ErrBusy to both modes, a
// blocking request waits for it, and the holder's exit, without any release,
// wakes that request.
func TestAnotherProcessExcludesAndItsExitWakesAWaiter(t *testing.T) {
	path := lockPath(t)
	release, holder := startHolder(t, path)

	for _, exclusive := range []bool{true, false} {
		if l, err := Acquire(path, exclusive, true); !errors.Is(err, ErrBusy) {
			_ = l.Release()
			t.Fatalf("non-blocking acquire (exclusive=%v) under another process's lock = %v, want ErrBusy", exclusive, err)
		}
	}

	acquired := make(chan error, 1)
	go func() {
		l, err := Acquire(path, true, false)
		if err == nil {
			err = l.Release()
		}
		acquired <- err
	}()
	select {
	case err := <-acquired:
		t.Fatalf("a blocking acquire returned while another process held the lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	_ = release.Close()
	select {
	case err := <-acquired:
		if err != nil {
			t.Fatalf("blocking acquire after the holder exited: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the holder's exit never released its lock")
	}
	if err := holder.Wait(); err != nil {
		t.Fatalf("the lock holder: %v", err)
	}
}

// TestALockNeverCoversContent verifies that the file under a lock another
// process holds stays readable and writable through other handles: a waiter
// reads the holder's record, and a rewrite truncates and writes it again.
func TestALockNeverCoversContent(t *testing.T) {
	path := lockPath(t)
	release, holder := startHolder(t, path)

	data, err := os.ReadFile(path)
	if err != nil || string(data) != holderRecord {
		t.Fatalf("read of a locked file = %q (%v), want the holder's record", data, err)
	}
	if err := os.WriteFile(path, []byte("rewritten\n"), 0o644); err != nil {
		t.Fatalf("write of a locked file: %v", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(0); err != nil {
		_ = f.Close()
		t.Fatalf("truncate of a locked file: %v", err)
	}
	if _, err := f.Write([]byte("second\n")); err != nil {
		_ = f.Close()
		t.Fatalf("write after truncate of a locked file: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "second\n" {
		t.Fatalf("read after the rewrite = %q (%v)", data, err)
	}
	if l, err := Acquire(path, true, true); !errors.Is(err, ErrBusy) {
		_ = l.Release()
		t.Fatalf("the rewrite released the other process's lock: %v", err)
	}

	_ = release.Close()
	if err := holder.Wait(); err != nil {
		t.Fatalf("the lock holder: %v", err)
	}
}

// TestAHolderRenamesAndRemovesItsLockFile verifies the owner-lock pattern: a
// lock taken on a staged file stays held under the name it is renamed to, and
// its holder removes the directory that contains it with RemoveDir, which
// releases the lock before it deletes the lock file.
func TestAHolderRenamesAndRemovesItsLockFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "owned")
	if err := os.MkdirAll(filepath.Join(dir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nested", "payload"), []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(dir, "owner.staged")
	f, err := OpenFile(staged, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := LockFile(f, true, false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("owned"); err != nil {
		t.Fatal(err)
	}
	owner := filepath.Join(dir, "owner.lock")
	if err := os.Rename(staged, owner); err != nil {
		t.Fatalf("rename of a held lock file: %v", err)
	}
	if l, err := Acquire(owner, true, true); !errors.Is(err, ErrBusy) {
		_ = l.Release()
		t.Fatalf("the renamed lock file is not the held one: %v", err)
	}
	released := false
	release := func() error {
		released = true
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("the directory went before the release: %v", err)
		}
		if len(entries) != 1 || entries[0].Name() != "owner.lock" {
			t.Fatalf("the release came before the other entries went: %v", entries)
		}
		if l, err := Acquire(owner, true, true); !errors.Is(err, ErrBusy) {
			_ = l.Release()
			t.Fatalf("the lock was free while the other entries went: %v", err)
		}
		return errors.Join(UnlockFile(f), f.Close())
	}
	if err := RemoveDir(dir, "owner.lock", release); err != nil {
		t.Fatalf("RemoveDir: %v", err)
	}
	if !released {
		t.Fatal("RemoveDir never released the lock")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the directory survived its removal: %v", err)
	}
}

// TestRemoveDirLocksKeepsEveryHeldLockUntilTheRelease verifies that
// RemoveDirLocks removes the other entries first, keeps every held lock file
// until the release, and then removes the directory.
func TestRemoveDirLocksKeepsEveryHeldLockUntilTheRelease(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	if err := os.MkdirAll(filepath.Join(dir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	names := []string{"a.lock", "b.lock"}
	held := make([]*Lock, 0, len(names))
	for _, name := range names {
		l, err := Acquire(filepath.Join(dir, name), true, true)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, l)
	}
	released := false
	release := func() error {
		released = true
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("the directory went before the release: %v", err)
		}
		left := make([]string, 0, len(entries))
		for _, entry := range entries {
			left = append(left, entry.Name())
		}
		if !slices.Equal(left, names) {
			t.Fatalf("entries at the release = %v, want only the lock files %v", left, names)
		}
		errs := make([]error, 0, len(held))
		for _, l := range held {
			errs = append(errs, l.Release())
		}
		return errors.Join(errs...)
	}
	if err := RemoveDirLocks(dir, names, release); err != nil {
		t.Fatalf("RemoveDirLocks: %v", err)
	}
	if !released {
		t.Fatal("RemoveDirLocks never released the locks")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the directory survived its removal: %v", err)
	}
}

// TestRemoveDirEndsTheHoldOnEveryPath verifies the edges of RemoveDir: a
// directory already gone still ends the hold, a release error reaches the
// caller, and an entry that cannot be removed keeps the lock file and its
// directory for whoever reclaims them later.
func TestRemoveDirEndsTheHoldOnEveryPath(t *testing.T) {
	calls := 0
	missing := filepath.Join(t.TempDir(), "gone")
	if err := RemoveDir(missing, "owner.lock", func() error { calls++; return nil }); err != nil {
		t.Fatalf("RemoveDir of a missing directory: %v", err)
	}
	if calls != 1 {
		t.Fatalf("release ran %d times for a missing directory, want 1", calls)
	}

	dir := filepath.Join(t.TempDir(), "owned")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "owner.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	failed := errors.New("release failed")
	if err := RemoveDir(dir, "owner.lock", func() error { return failed }); !errors.Is(err, failed) {
		t.Fatalf("RemoveDir hid the release error: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("a release error kept the directory: %v", err)
	}

	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		return // a read-only directory stops neither Windows nor root
	}
	stuck := filepath.Join(t.TempDir(), "owned")
	sealed := filepath.Join(stuck, "sealed")
	if err := os.MkdirAll(sealed, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sealed, "payload"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stuck, "owner.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sealed, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sealed, 0o755) })
	calls = 0
	if err := RemoveDir(stuck, "owner.lock", func() error { calls++; return nil }); err == nil {
		t.Fatal("RemoveDir reported no error for an entry it could not remove")
	}
	if calls != 1 {
		t.Fatalf("release ran %d times after a failed removal, want 1", calls)
	}
	if _, err := os.Stat(filepath.Join(stuck, "owner.lock")); err != nil {
		t.Fatalf("a failed removal deleted the lock file: %v", err)
	}
}

// TestLockFileRefusesADirectory verifies that a directory answers ErrDirectory
// instead of a lock only some platforms can take.
// TestSyncDirAfterAReplacingRename pins SyncDir on every platform: after a
// file is renamed over a name held under a lock, syncing its directory
// succeeds and the name holds the new file.
func TestSyncDirAfterAReplacingRename(t *testing.T) {
	dir := t.TempDir()
	lock, err := Acquire(filepath.Join(dir, "state.lock"), true, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Release() }()
	target := filepath.Join(dir, "state.json")
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(dir, ".state-staged")
	if err := os.WriteFile(staged, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(staged, target); err != nil {
		t.Fatal(err)
	}
	if err := SyncDir(dir); err != nil {
		t.Fatalf("sync the directory after the rename: %v", err)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "new" {
		t.Fatalf("after the rename the name holds %q, %v", data, err)
	}
}

func TestLockFileRefusesADirectory(t *testing.T) {
	d, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	for _, nonBlocking := range []bool{true, false} {
		if err := LockFile(d, true, nonBlocking); !errors.Is(err, ErrDirectory) {
			t.Fatalf("LockFile on a directory (nonBlocking=%v) = %v, want ErrDirectory", nonBlocking, err)
		}
	}
}

// TestOpenFileRefusesAnUnsupportedFlag verifies that a flag outside the
// supported set fails on every platform instead of meaning something else on
// one of them.
func TestOpenFileRefusesAnUnsupportedFlag(t *testing.T) {
	path := lockPath(t)
	if f, err := OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644); !errors.Is(err, errors.ErrUnsupported) {
		if f != nil {
			_ = f.Close()
		}
		t.Fatalf("OpenFile with O_APPEND = %v, want errors.ErrUnsupported", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a refused open created the file: %v", err)
	}
}

// TestNoFollowRefusesASymlink verifies that NoFollow never opens the target of
// a link planted at the path.
func TestNoFollowRefusesASymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("elsewhere"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("this host cannot create a symbolic link: %v", err)
	}
	if f, err := OpenFile(link, os.O_RDWR|NoFollow, 0); !errors.Is(err, os.ErrPermission) {
		if f != nil {
			_ = f.Close()
		}
		t.Fatalf("OpenFile with NoFollow on a symlink = %v, want os.ErrPermission", err)
	}
	f, err := OpenFile(target, os.O_RDWR|NoFollow, 0)
	if err != nil {
		t.Fatalf("OpenFile with NoFollow on a regular file: %v", err)
	}
	_ = f.Close()
}
