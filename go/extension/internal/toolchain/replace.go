package toolchain

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.putnami.dev/sdk/extension/filelock"
)

// ReplaceExecutable moves staged, a complete executable in dest's directory, to
// dest, so a concurrent reader finds the previous file or the new one, never
// part of either. On Unix it is one rename. On Windows dest may be running,
// which forbids deleting or replacing it but not renaming it, so there the
// move goes through replaceAside.
func ReplaceExecutable(staged, dest string) error { return replaceExecutable(staged, dest) }

// fileOps is the part of the filesystem replaceAside changes. Tests substitute
// one that treats a file as a running Windows executable: it can be renamed,
// but not deleted or replaced.
type fileOps struct {
	rename func(oldpath, newpath string) error
	remove func(name string) error
}

var osFileOps = fileOps{rename: os.Rename, remove: os.Remove}

const (
	// replaceLockName is the lock file that serializes replaceAside calls in
	// a directory.
	replaceLockName = ".putnami-replace.lock"
	// asideMarker separates a moved-aside file's original name from its unique
	// suffix: .<name>.old-<pid>-<nanoseconds>.
	asideMarker = ".old-"
)

// replaceAside moves staged to dest when dest may be a running executable, as
// the CLI's version switch does. Under an exclusive lock in dest's directory
// it deletes the files earlier calls moved aside from dest, moves dest aside
// under a hidden name, moves staged in, and deletes the moved-aside file. A
// file that is still running stays, and a later call deletes it. When staged
// cannot be moved in, the file that was at dest goes back; when it cannot go
// back either, the error names the path that holds it.
func replaceAside(ops fileOps, staged, dest string) error {
	dir, base := filepath.Dir(dest), filepath.Base(dest)
	lock, err := filelock.Acquire(filepath.Join(dir, replaceLockName), true, false)
	if err != nil {
		return fmt.Errorf("lock %s: %w", dir, err)
	}
	defer func() { _ = lock.Release() }()

	reclaimAside(ops, dir, base)

	aside := ""
	if _, err := os.Lstat(dest); err == nil {
		aside = filepath.Join(dir, "."+base+asideMarker+strconv.Itoa(os.Getpid())+"-"+strconv.FormatInt(time.Now().UnixNano(), 10))
		if err := ops.rename(dest, aside); err != nil {
			return fmt.Errorf("move %s aside: %w", dest, err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	if err := ops.rename(staged, dest); err != nil {
		if aside != "" {
			if restoreErr := ops.rename(aside, dest); restoreErr != nil {
				return errors.Join(err, fmt.Errorf("restore %s from %s: %w", dest, aside, restoreErr))
			}
		}
		return err
	}
	if aside != "" {
		// A running executable refuses; a later call deletes it.
		_ = ops.remove(aside)
	}
	return nil
}

// reclaimAside deletes the files earlier calls moved aside from base in dir.
// One that is still running refuses and stays.
func reclaimAside(ops fileOps, dir, base string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	prefix := "." + base + asideMarker
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), prefix) && !entry.IsDir() {
			_ = ops.remove(filepath.Join(dir, entry.Name()))
		}
	}
}
