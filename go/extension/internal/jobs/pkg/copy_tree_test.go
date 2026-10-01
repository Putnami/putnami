package pkg

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

// copyTreeFixture writes a tree with every shape the packager stages: nested
// directories with loose and read-only modes, files with and without execute
// bits, and relative symbolic links to a file, to a directory, and to nothing.
func copyTreeFixture(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "src")
	for _, dir := range []string{"bin", "loose", "readonly/nested"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for path, mode := range map[string]os.FileMode{
		"README.md":                0o644,
		"bin/prepare":              0o755,
		"loose/shared.txt":         0o666,
		"loose/private.txt":        0o600,
		"readonly/nested/data.txt": 0o444,
	} {
		mustWrite(t, filepath.Join(root, path), path)
		if err := os.Chmod(filepath.Join(root, path), mode); err != nil {
			t.Fatal(err)
		}
	}
	for link, target := range map[string]string{
		"bin/readme-link": "../README.md",
		"loose-link":      "loose",
		"dangling":        "missing",
	} {
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
	}
	for dir, mode := range map[string]os.FileMode{"loose": 0o777, "readonly/nested": 0o555, "readonly": 0o555} {
		if err := os.Chmod(filepath.Join(root, dir), mode); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// allowRemoval gives every directory under root its owner permissions back, so
// the test's temporary directory can be removed.
func allowRemoval(t *testing.T, root string) {
	t.Helper()
	t.Cleanup(func() {
		_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err == nil && entry.IsDir() {
				_ = os.Chmod(path, 0o755)
			}
			return nil
		})
	})
}

type treeEntry struct {
	Mode    fs.FileMode
	Content string
}

// readTree records, for every entry under root, its type and permission bits
// and either its bytes or its link target.
func readTree(t *testing.T, root string) map[string]treeEntry {
	t.Helper()
	tree := map[string]treeEntry{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		record := treeEntry{Mode: info.Mode().Type() | info.Mode().Perm()}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			record.Content, err = os.Readlink(path)
		case info.Mode().IsRegular():
			var data []byte
			data, err = os.ReadFile(path)
			record.Content = string(data)
		}
		if err != nil {
			return err
		}
		tree[filepath.ToSlash(rel)] = record
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

// copyTree replaced `cp -R` in the packager so a Windows host can stage an
// archive. On the hosts that have cp, the staged tree, and with it the archive
// bytes, must stay the one cp made: same entries, same bytes, same link
// targets, and the same permission bits under the same umask.
func TestCopyTreeMakesTheCopyCpMakes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("cp -R is the Unix reference")
	}
	cp, err := exec.LookPath("cp")
	if err != nil {
		t.Skipf("cp not available: %v", err)
	}
	src := copyTreeFixture(t)
	allowRemoval(t, filepath.Dir(src))
	out := t.TempDir()
	allowRemoval(t, out)

	withCp := filepath.Join(out, "cp")
	if output, err := exec.Command(cp, "-R", src, withCp).CombinedOutput(); err != nil {
		t.Fatalf("cp -R: %v: %s", err, output)
	}
	withGo := filepath.Join(out, "go")
	if err := copyTree(src, withGo); err != nil {
		t.Fatalf("copyTree: %v", err)
	}

	want, got := readTree(t, withCp), readTree(t, withGo)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("copyTree tree differs from cp -R:\ncopyTree: %v\ncp -R:    %v", got, want)
	}
	if got["readonly/nested"].Mode.Perm() != 0o555 || got["readonly/nested/data.txt"].Content != "readonly/nested/data.txt" {
		t.Errorf("read-only directory copied as %v with data.txt %q; its content must be copied and its mode kept",
			got["readonly/nested"], got["readonly/nested/data.txt"])
	}
	if got["loose-link"].Mode&fs.ModeSymlink == 0 || got["loose-link"].Content != "loose" {
		t.Errorf("symbolic link copied as %v; it must stay a link to its target", got["loose-link"])
	}
}

// The destination must not exist. `cp -R src dst` into an existing directory
// copies into dst/<name of src>; copyTree fails instead of guessing.
func TestCopyTreeRefusesAnExistingDestination(t *testing.T) {
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "file.txt"), "content")
	dst := t.TempDir()
	if err := copyTree(src, dst); err == nil {
		t.Fatal("copyTree wrote into an existing destination")
	}
	if _, err := os.Stat(filepath.Join(dst, "file.txt")); !os.IsNotExist(err) {
		t.Errorf("an existing destination received content: %v", err)
	}
}
