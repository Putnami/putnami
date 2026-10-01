package treearchive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"go.putnami.dev/sdk/extension/dirlink"
)

// writeTree lays out files under root with the given permission bits and
// modification time.
func writeTree(t *testing.T, root string, files map[string]os.FileMode, mtime time.Time) {
	t.Helper()
	for rel, perm := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("content of "+rel+"\n"), perm); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, perm); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
}

type entry struct {
	name     string
	typeflag byte
	mode     int64
}

func readArchive(t *testing.T, data []byte) []entry {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if !gz.ModTime.IsZero() || gz.Name != "" || gz.OS != 255 {
		t.Errorf("gzip header = mtime %v, name %q, OS %d; want no time, no name, OS 255", gz.ModTime, gz.Name, gz.OS)
	}
	tr := tar.NewReader(gz)
	var entries []entry
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return entries
		}
		if err != nil {
			t.Fatal(err)
		}
		if !h.ModTime.Equal(time.Unix(0, 0)) || h.Uid != 0 || h.Gid != 0 || h.Uname != "" || h.Gname != "" {
			t.Errorf("%s: mtime %v uid %d gid %d uname %q gname %q; want the epoch, 0, 0 and no names",
				h.Name, h.ModTime, h.Uid, h.Gid, h.Uname, h.Gname)
		}
		entries = append(entries, entry{name: h.Name, typeflag: h.Typeflag, mode: h.Mode})
	}
}

// Two trees with the same paths, contents and execute bits pack into the same
// bytes, whatever their timestamps and other permission bits.
func TestWriteTarGzIsReproducible(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	writeTree(t, first, map[string]os.FileMode{
		"b.txt": 0o644, "a/z.txt": 0o644, "a/run.sh": 0o755, "a.txt": 0o644,
	}, time.Date(2020, 1, 2, 3, 4, 5, 6, time.UTC))
	writeTree(t, second, map[string]os.FileMode{
		"b.txt": 0o600, "a/z.txt": 0o664, "a/run.sh": 0o700, "a.txt": 0o640,
	}, time.Date(2026, 9, 27, 1, 2, 3, 4, time.UTC))

	archive := func(root string) []byte {
		path := filepath.Join(t.TempDir(), "out.tar.gz")
		if err := WriteTarGz(path, root); err != nil {
			t.Fatalf("WriteTarGz: %v", err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	a, b := archive(first), archive(second)
	if !bytes.Equal(a, b) {
		t.Fatal("two trees with the same paths, contents and execute bits packed into different bytes")
	}

	exec := int64(0o755)
	if runtime.GOOS == "windows" {
		exec = 0o644 // Windows records no execute bit
	}
	// filepath.WalkDir order: names sorted within each directory, a directory
	// before its content, so "a/" and its files come before "a.txt".
	want := []entry{
		{"a/", tar.TypeDir, 0o755},
		{"a/run.sh", tar.TypeReg, exec},
		{"a/z.txt", tar.TypeReg, 0o644},
		{"a.txt", tar.TypeReg, 0o644},
		{"b.txt", tar.TypeReg, 0o644},
	}
	if got := readArchive(t, a); !slices.Equal(got, want) {
		t.Fatalf("entries = %v, want %v", got, want)
	}
}

func TestCopyTreeSkipsWhatSkipAnswersAndKeepsTheRest(t *testing.T) {
	src := t.TempDir()
	writeTree(t, src, map[string]os.FileMode{
		"keep.txt": 0o644, "nested/keep.txt": 0o644, "nested/drop.txt": 0o644,
		"dropped/inner.txt": 0o644, ".hidden": 0o644,
	}, time.Now())
	dst := filepath.Join(t.TempDir(), "stage")
	var asked []string
	err := CopyTree(src, dst, func(rel string) bool {
		asked = append(asked, rel)
		return rel == "dropped" || rel == ".hidden" || rel == "nested/drop.txt"
	})
	if err != nil {
		t.Fatalf("CopyTree: %v", err)
	}
	var got []string
	if err := filepath.WalkDir(dst, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dst, path)
		got = append(got, filepath.ToSlash(rel))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"keep.txt", "nested/keep.txt"}; !slices.Equal(got, want) {
		t.Fatalf("copied %v, want %v", got, want)
	}
	if slices.Contains(asked, "dropped/inner.txt") {
		t.Fatalf("skip was asked about an entry under a skipped directory: %v", asked)
	}
	data, err := os.ReadFile(filepath.Join(dst, "nested", "keep.txt"))
	if err != nil || string(data) != "content of nested/keep.txt\n" {
		t.Fatalf("copied content = %q, %v", data, err)
	}
}

// A directory link, a symbolic link on Unix and a junction on Windows, fails
// both the copy and the archive with a *LinkError that names it.
func TestADirectoryLinkIsRefused(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]os.FileMode{"config/defaults/a.json": 0o644}, time.Now())
	if err := dirlink.Create(filepath.Join(root, "config", "defaults"), filepath.Join(root, "config", "current")); err != nil {
		t.Fatalf("create the link: %v", err)
	}

	assertLinkError := func(what string, err error) {
		t.Helper()
		var link *LinkError
		if !errors.As(err, &link) {
			t.Fatalf("%s error = %v, want a *LinkError", what, err)
		}
		if link.Path != "config/current" || link.Target == "" {
			t.Fatalf("%s refused %+v, want config/current and its target", what, link)
		}
	}
	assertLinkError("CopyTree", CopyTree(root, filepath.Join(t.TempDir(), "stage"), nil))

	archivePath := filepath.Join(t.TempDir(), "out.tar.gz")
	assertLinkError("WriteTarGz", WriteTarGz(archivePath, root))
	if _, err := os.Stat(archivePath); !os.IsNotExist(err) {
		t.Fatalf("a refused archive was left behind (stat error %v)", err)
	}
}
