package versioncmd

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.putnami.dev/tooling/cli/internal/flock"
)

// fileOps is the part of the filesystem the rename-aside switch changes.
// Tests substitute one that treats a file as a running Windows executable:
// it can be renamed, but not deleted or replaced.
type fileOps struct {
	rename func(oldpath, newpath string) error
	remove func(name string) error
}

var osFileOps = fileOps{rename: os.Rename, remove: os.Remove}

const (
	// switchLockName is the lock file that serializes switches in a directory.
	switchLockName = ".putnami-switch.lock"
	// asideMarker separates a moved-aside file's original name from its
	// unique suffix: .<name>.old-<pid>-<nanoseconds>.
	asideMarker = ".old-"
)

// asideSwitch replaces a binary that may be running. Windows refuses to delete
// or overwrite a running executable but lets it be renamed. The switch moves
// the current file aside under a name that starts with a dot, moves the new
// file in, and deletes the moved-aside file. A file still running stays, and a
// later switch in the same directory deletes it. An exclusive lock in the
// directory keeps two switches from interleaving.
type asideSwitch struct {
	ops fileOps
}

// install copies src next to dst and switches it in.
func (s asideSwitch) install(src, dst string) error {
	stagingPath, err := stageBinary(src, dst)
	if err != nil {
		return err
	}
	defer os.Remove(stagingPath)
	return s.replace(stagingPath, dst)
}

// activate makes link a copy of targetName, a binary in the same directory.
func (s asideSwitch) activate(targetName, link string) error {
	return s.install(filepath.Join(filepath.Dir(link), targetName), link)
}

// replace moves stagingPath to dst under the directory's switch lock. When the
// move fails, the file that was at dst goes back. When that fails too, the
// file stays at its moved-aside path, and the error names that path.
func (s asideSwitch) replace(stagingPath, dst string) error {
	dir := filepath.Dir(dst)
	lock, err := flock.Acquire(filepath.Join(dir, switchLockName), true, false)
	if err != nil {
		return fmt.Errorf("lock %s: %w", dir, err)
	}
	defer func() { _ = lock.Release() }()

	s.reclaim(dir)

	aside := ""
	if _, err := os.Lstat(dst); err == nil {
		aside = filepath.Join(dir, fmt.Sprintf(".%s%s%d-%d", filepath.Base(dst), asideMarker, os.Getpid(), time.Now().UnixNano()))
		if err := s.ops.rename(dst, aside); err != nil {
			return fmt.Errorf("move %s aside: %w", dst, err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	if err := s.ops.rename(stagingPath, dst); err != nil {
		if aside != "" {
			if restoreErr := s.ops.rename(aside, dst); restoreErr != nil {
				return errors.Join(err, fmt.Errorf("restore %s: %w; the previous binary is kept at %s: move it back to %s before the next switch in %s, which deletes it", dst, restoreErr, aside, dst, dir))
			}
		}
		return err
	}
	if aside != "" {
		// A running binary refuses; the next switch deletes it.
		_ = s.ops.remove(aside)
	}
	return nil
}

// reclaim deletes the files earlier switches moved aside in dir. One that is
// still running refuses and stays for a later switch.
func (s asideSwitch) reclaim(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") && strings.Contains(name, asideMarker) && !entry.IsDir() {
			_ = s.ops.remove(filepath.Join(dir, name))
		}
	}
}

// sameContentName names the putnami-* file in dir whose content equals the
// file at path, or "" when none does.
func sameContentName(dir, path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	digest, err := hashFileSHA256(path)
	if err != nil {
		return ""
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "putnami-") {
			continue
		}
		candidate, err := entry.Info()
		if err != nil || !candidate.Mode().IsRegular() || candidate.Size() != info.Size() {
			continue
		}
		if got, err := hashFileSHA256(filepath.Join(dir, name)); err == nil && got == digest {
			return name
		}
	}
	return ""
}
