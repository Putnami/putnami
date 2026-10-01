// Package scratch creates temporary directories that do not outlive their
// creator by more than one run.
//
// A creator that cleans up in-process only (a deferred RemoveAll, a TestMain
// that removes its directory after m.Run) leaks the directory whenever it is
// killed: a `go test -timeout` panic, a canceled session, a SIGKILL. Test
// stores hold built CLIs and extension runtimes, so each leak can be large.
//
// New gives every directory an owner lock: an exclusive file lock
// (go.putnami.dev/sdk/extension/filelock) on a file inside the directory, held
// by the creating process until Remove. The operating system releases that
// lock when the process dies, however it dies. New also starts,
// at most every ten minutes per process and prefix, a background sweep that
// removes every directory under os.TempDir() named in the Namespace whose
// owner lock is free: the next creator of any kind reclaims what a killed one
// left, and no creator waits for it. The lock descriptor is close-on-exec, so
// a child process never keeps a dead creator's directory alive. The lock file
// records its directory's name, so a copy of a scratch directory is never
// mistaken for a dead one.
//
// A directory without a valid owner lock is a creator between MkdirTemp and
// taking its lock, a directory an older build created, a copy, or one created
// where the file system refuses the lock. A sweep removes it only when its
// name starts with the sweep's prefix and it is older than UnownedGrace.
package scratch

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.putnami.dev/sdk/extension/filelock"
)

const (
	// OwnerLockName is the owner lock file inside each directory. The leading
	// dot keeps it out of listings that skip hidden files.
	OwnerLockName = ".putnami-scratch-owner"

	// UnownedGrace is how old a directory without an owner lock must be before
	// a sweep removes it.
	UnownedGrace = 24 * time.Hour

	// Namespace starts every scratch directory name. A sweep only looks
	// inside directories named in it, which bounds its cost on a temporary
	// directory full of other tools' entries.
	Namespace = "putnami-"

	stagedLockName = OwnerLockName + ".staged"

	// resweepInterval is how long a process waits before sweeping again for
	// a prefix, so a long-lived process also reclaims what creators killed
	// after its first sweep left.
	resweepInterval = 10 * time.Minute
)

var (
	sweptMu sync.Mutex
	swept   = map[string]time.Time{}
	// sweeping tracks the background sweeps New started; tests wait on it.
	sweeping sync.WaitGroup
)

// Dir is a directory New created. Its owner lock is held until Remove, and
// only while the Dir is reachable: a dropped Dir's lock descriptor is closed
// by the garbage collector, and a sweep may then remove the directory. The
// directory holds the hidden owner lock file; a caller that needs an empty
// tree uses a subdirectory.
type Dir struct {
	path string
	lock *os.File
}

// New creates a fresh directory under os.TempDir() and takes its owner lock.
// The directory name is prefix, which starts with Namespace, followed by a
// random string. It starts the background sweep for this prefix the first time
// the process uses it; a sweep failure never fails New, and a process that
// exits mid-sweep leaves the rest to the next one. Where the file system
// refuses the lock, the directory is returned without an owner lock.
func New(prefix string) (*Dir, error) {
	if !strings.HasPrefix(prefix, Namespace) || strings.ContainsAny(prefix, `/\*`) {
		return nil, fmt.Errorf("scratch prefix %q must start with %q and hold no '/', '\\' or '*'", prefix, Namespace)
	}
	startSweep(prefix)
	path, err := os.MkdirTemp("", prefix)
	if err != nil {
		return nil, err
	}
	lock, err := own(path)
	if err != nil {
		_ = os.RemoveAll(path)
		return nil, fmt.Errorf("take scratch owner lock: %w", err)
	}
	return &Dir{path: path, lock: lock}, nil
}

// Path returns the directory's absolute path.
func (d *Dir) Path() string {
	return d.path
}

// Remove deletes the directory and releases its owner lock: everything but
// the lock file goes under the lock, the lock file after its release. Safe on
// a nil Dir and after a previous Remove.
func (d *Dir) Remove() error {
	if d == nil || d.path == "" {
		return nil
	}
	path := d.path
	d.path = ""
	if d.lock == nil {
		return removeTree(path)
	}
	lock := d.lock
	d.lock = nil
	return filelock.RemoveDir(path, OwnerLockName, lock.Close)
}

// startSweep sweeps the current temporary directory for prefix in the
// background, at most once per resweepInterval in this process. The root is
// read now: the sweep never follows a later change of TMPDIR.
func startSweep(prefix string) {
	root := os.TempDir()
	key := root + "\x00" + prefix
	now := time.Now()
	sweptMu.Lock()
	defer sweptMu.Unlock()
	if last, ok := swept[key]; ok && now.Sub(last) < resweepInterval {
		return
	}
	swept[key] = now
	sweeping.Add(1)
	go func() {
		defer sweeping.Done()
		_ = sweepIn(root, prefix)
	}()
}

// own takes the owner lock of a fresh directory. The lock is taken on a staged
// name and then renamed: a sweep that opens OwnerLockName always finds it held,
// never in the instant between its creation and the lock. Where the file
// system refuses the lock, there is no lock to take.
func own(path string) (*os.File, error) {
	staged := filepath.Join(path, stagedLockName)
	file, err := filelock.OpenFile(staged, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	// A blocking lock on a file nobody else can have opened yet fails only
	// when the file system does not support the lock.
	if err := lockFile(file, false); err != nil {
		_ = file.Close()
		return nil, os.Remove(staged)
	}
	if _, err := file.WriteString(filepath.Base(path)); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := os.Rename(staged, filepath.Join(path, OwnerLockName)); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

// Sweep removes the directories under os.TempDir() whose creator is gone:
// every directory named in the Namespace whose owner lock is free, and the
// unowned directories whose name starts with prefix once they are older than
// UnownedGrace. An unowned directory has no owner lock, or one that names
// another directory. A directory whose owner lock is held is never touched.
// It returns the errors of the directories it could not decide or remove; the
// others are still swept.
func Sweep(prefix string) error {
	return sweepIn(os.TempDir(), prefix)
}

func sweepIn(root, prefix string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	now := time.Now()
	var errs []error
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), Namespace) {
			continue
		}
		prefixed := strings.HasPrefix(entry.Name(), prefix)
		if err := sweepOne(filepath.Join(root, entry.Name()), prefixed, now); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func sweepOne(path string, prefixed bool, now time.Time) error {
	lock, err := openLock(filepath.Join(path, OwnerLockName))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return sweepUnowned(path, prefixed, now)
	case errors.Is(err, os.ErrPermission):
		return nil // another user's directory
	case err != nil:
		return err
	}
	defer func() { _ = lock.Close() }()
	if info, err := lock.Stat(); err != nil || !info.Mode().IsRegular() {
		return err // an owner lock is always a regular file
	}
	if err := lockFile(lock, true); err != nil {
		if errors.Is(err, errBusy) {
			return nil // its creator is alive
		}
		return err
	}
	owner, err := io.ReadAll(io.LimitReader(lock, 256))
	if err != nil {
		return err
	}
	if string(owner) != filepath.Base(path) {
		// A copy of another directory's lock proves nothing about this one.
		_ = lock.Close()
		return sweepUnowned(path, prefixed, now)
	}
	// The lock is released before its file goes: Windows before version 1809
	// cannot remove a directory that holds an open file. The deferred Close
	// then only reports a file already closed.
	return filelock.RemoveDir(path, OwnerLockName, lock.Close)
}

// sweepUnowned removes a directory no owner lock accounts for, once it is old
// enough that no creator can still be taking its lock.
func sweepUnowned(path string, prefixed bool, now time.Time) error {
	if !prefixed {
		return nil // not scratch, or not this sweep's to judge by age
	}
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // another sweep removed it
		}
		return err
	}
	if !info.IsDir() || now.Sub(info.ModTime()) < UnownedGrace {
		return nil
	}
	return removeTree(path)
}

// removeTree deletes the owner lock file last, and only once everything else
// is gone: a removal that fails or is interrupted by the death of its process
// leaves a directory the next sweep still recognizes.
func removeTree(path string) error {
	entries, err := os.ReadDir(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return os.RemoveAll(path)
	}
	var errs []error
	for _, entry := range entries {
		if entry.Name() != OwnerLockName {
			errs = append(errs, os.RemoveAll(filepath.Join(path, entry.Name())))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	return os.RemoveAll(path)
}
