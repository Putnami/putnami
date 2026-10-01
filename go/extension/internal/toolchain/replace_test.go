package toolchain

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeReplaceFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func readReplaceFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// asideFiles lists the files replaceAside moved aside from base in dir.
func asideFiles(t *testing.T, dir, base string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "."+base+asideMarker) {
			names = append(names, entry.Name())
		}
	}
	return names
}

// runningOps treats the files in running as running Windows executables: a
// running file can be renamed, and the process keeps it under its new name,
// but it cannot be deleted or replaced.
type runningOps struct {
	running map[string]bool
}

var errRunning = errors.New("the file is in use by a running program")

func (r *runningOps) ops() fileOps {
	return fileOps{
		rename: func(oldpath, newpath string) error {
			if r.running[newpath] {
				return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: errRunning}
			}
			if err := os.Rename(oldpath, newpath); err != nil {
				return err
			}
			if r.running[oldpath] {
				delete(r.running, oldpath)
				r.running[newpath] = true
			}
			return nil
		},
		remove: func(name string) error {
			if r.running[name] {
				return &os.PathError{Op: "remove", Path: name, Err: errRunning}
			}
			return os.Remove(name)
		},
	}
}

func TestReplaceExecutableMovesTheStagedFileOverDest(t *testing.T) {
	for _, withDest := range []bool{true, false} {
		dir := t.TempDir()
		dest := filepath.Join(dir, "tool")
		staged := filepath.Join(dir, ".tool.tmp")
		if withDest {
			writeReplaceFile(t, dest, "old")
		}
		writeReplaceFile(t, staged, "new")

		if err := ReplaceExecutable(staged, dest); err != nil {
			t.Fatalf("withDest=%v: ReplaceExecutable: %v", withDest, err)
		}
		if got := readReplaceFile(t, dest); got != "new" {
			t.Errorf("withDest=%v: dest = %q, want the staged content", withDest, got)
		}
		if _, err := os.Lstat(staged); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("withDest=%v: the staged file is still there: %v", withDest, err)
		}
		if aside := asideFiles(t, dir, "tool"); len(aside) != 0 {
			t.Errorf("withDest=%v: files left aside: %v", withDest, aside)
		}
	}
}

func TestReplaceAsideReplacesARunningExecutable(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "tool.exe")
	staged := filepath.Join(dir, ".tool.exe.tmp1")
	writeReplaceFile(t, dest, "old")
	writeReplaceFile(t, staged, "new")
	fake := &runningOps{running: map[string]bool{dest: true}}
	ops := fake.ops()

	// A plain rename over the running file is what Windows refuses.
	if err := ops.rename(staged, dest); !errors.Is(err, errRunning) {
		t.Fatalf("a rename over the running file = %v, want it refused", err)
	}

	if err := replaceAside(ops, staged, dest); err != nil {
		t.Fatalf("replaceAside over a running executable: %v", err)
	}
	if got := readReplaceFile(t, dest); got != "new" {
		t.Fatalf("dest = %q, want the staged content", got)
	}
	aside := asideFiles(t, dir, "tool.exe")
	if len(aside) != 1 {
		t.Fatalf("files moved aside = %v, want the running one kept", aside)
	}
	if got := readReplaceFile(t, filepath.Join(dir, aside[0])); got != "old" {
		t.Fatalf("the moved-aside file holds %q, want the previous executable", got)
	}

	// Once the old program exits, the next replacement deletes what it left.
	fake.running = map[string]bool{}
	next := filepath.Join(dir, ".tool.exe.tmp2")
	writeReplaceFile(t, next, "newer")
	if err := replaceAside(ops, next, dest); err != nil {
		t.Fatalf("second replaceAside: %v", err)
	}
	if got := readReplaceFile(t, dest); got != "newer" {
		t.Fatalf("dest = %q, want the second staged content", got)
	}
	if left := asideFiles(t, dir, "tool.exe"); len(left) != 0 {
		t.Fatalf("files still aside after the program exited: %v", left)
	}
}

func TestReplaceAsidePutsDestBackWhenTheStagedFileCannotMoveIn(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "tool.exe")
	staged := filepath.Join(dir, ".tool.exe.tmp")
	writeReplaceFile(t, dest, "old")
	writeReplaceFile(t, staged, "new")
	errRefused := errors.New("refused")
	ops := fileOps{
		rename: func(oldpath, newpath string) error {
			if oldpath == staged {
				return errRefused
			}
			return os.Rename(oldpath, newpath)
		},
		remove: os.Remove,
	}

	if err := replaceAside(ops, staged, dest); !errors.Is(err, errRefused) {
		t.Fatalf("replaceAside = %v, want the refused move", err)
	}
	if got := readReplaceFile(t, dest); got != "old" {
		t.Fatalf("dest = %q, want the previous file back", got)
	}
	if aside := asideFiles(t, dir, "tool.exe"); len(aside) != 0 {
		t.Fatalf("files left aside after the restore: %v", aside)
	}
}

func TestReplaceAsideNamesThePreviousFileWhenItCannotGoBack(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "tool.exe")
	staged := filepath.Join(dir, ".tool.exe.tmp")
	writeReplaceFile(t, dest, "old")
	writeReplaceFile(t, staged, "new")
	errRefused := errors.New("refused")
	ops := fileOps{
		rename: func(oldpath, newpath string) error {
			if oldpath == staged || newpath == dest {
				return errRefused
			}
			return os.Rename(oldpath, newpath)
		},
		remove: os.Remove,
	}

	err := replaceAside(ops, staged, dest)
	if !errors.Is(err, errRefused) {
		t.Fatalf("replaceAside = %v, want the refused move", err)
	}
	aside := asideFiles(t, dir, "tool.exe")
	if len(aside) != 1 {
		t.Fatalf("files moved aside = %v, want the previous file kept", aside)
	}
	kept := filepath.Join(dir, aside[0])
	if got := readReplaceFile(t, kept); got != "old" {
		t.Fatalf("the kept file holds %q, want the previous executable", got)
	}
	if !strings.Contains(err.Error(), kept) {
		t.Fatalf("error %q does not name %s, which holds the previous executable", err, kept)
	}
}

func TestReplaceAsideLeavesOtherFilesAlone(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "tool.exe")
	staged := filepath.Join(dir, ".tool.exe.tmp")
	other := filepath.Join(dir, ".other.exe"+asideMarker+"1-1")
	writeReplaceFile(t, dest, "old")
	writeReplaceFile(t, staged, "new")
	writeReplaceFile(t, other, "other")

	if err := replaceAside(osFileOps, staged, dest); err != nil {
		t.Fatalf("replaceAside: %v", err)
	}
	if got := readReplaceFile(t, other); got != "other" {
		t.Fatalf("another tool's moved-aside file = %q, want it untouched", got)
	}
}
