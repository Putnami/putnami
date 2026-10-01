package store

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// With '/' as the separator the order is plain string order, so no POSIX fold
// order moves.
func TestSlashPathLess_IsStringOrderOnSlashHosts(t *testing.T) {
	paths := []string{"/ws/a/b", "/ws/a1", "/ws/a", "/ws/a/", "/ws/z.go", "/ws/sub/x.go", "/sibling/p.json", ""}
	for _, a := range paths {
		for _, b := range paths {
			if got, want := slashPathLess(a, b, '/'), a < b; got != want {
				t.Errorf("slashPathLess(%q, %q, '/') = %v, want %v", a, b, got, want)
			}
		}
	}
}

// A backslash host orders a tree exactly as a slash host orders the slash
// spelling of the same tree. Byte order would put `a\b` after `a1`, since '\'
// sorts after '1' and '/' before it.
func TestSlashPathLess_BackslashHostFoldsInSlashOrder(t *testing.T) {
	if !slashPathLess(`C:\ws\proj\a\b`, `C:\ws\proj\a1`, '\\') {
		t.Errorf(`a\b must sort before a1, as a/b does`)
	}
	if slashPathLess(`C:\ws\proj\a1`, `C:\ws\proj\a\b`, '\\') {
		t.Errorf(`a1 must not sort before a\b`)
	}

	posix := []string{
		"/ws/proj/z.go", "/ws/proj/a1", "/ws/sibling/putnami.json", "/ws/proj/a/b",
		"/ws/proj/sub/x.go", "/ws/proj/a", "/ws/proj-b/x", "/ws/proj/a.b",
	}
	windows := make([]string, len(posix))
	for i, p := range posix {
		windows[i] = `C:` + strings.ReplaceAll(p, "/", `\`)
	}
	slices.SortFunc(posix, func(a, b string) int { return compareWith(a, b, '/') })
	slices.SortFunc(windows, func(a, b string) int { return compareWith(a, b, '\\') })
	for i := range posix {
		if got := strings.ReplaceAll(strings.TrimPrefix(windows[i], "C:"), `\`, "/"); got != posix[i] {
			t.Fatalf("position %d: the backslash host folds %s, the slash host %s\nwindows %v\nposix %v", i, got, posix[i], windows, posix)
		}
	}
}

// Two spellings of one slash form still order, by their raw bytes, so the order
// is total and collectFiles' dedupe sees equal paths side by side.
func TestSlashPathLess_OrdersEqualSlashFormsByRawBytes(t *testing.T) {
	if !slashPathLess(`a/b`, `a\b`, '\\') || slashPathLess(`a\b`, `a/b`, '\\') {
		t.Errorf("two spellings of a/b must order by their raw bytes")
	}
	for _, p := range []string{`a\b`, `a/b`, ``} {
		if slashPathLess(p, p, '\\') {
			t.Errorf("slashPathLess(%q, %q) = true, want false", p, p)
		}
	}
}

func compareWith(a, b string, separator byte) int {
	switch {
	case slashPathLess(a, b, separator):
		return -1
	case slashPathLess(b, a, separator):
		return 1
	}
	return 0
}

// hashFiles folds each file under its slash-form relative path, in the slash
// order of its ABSOLUTE path. The tree mixes a nested path (a/b) with a sibling
// name that byte order splits differently per separator (a1), and a "../" input
// outside the hashed directory, which absolute order puts last and relative
// order would put first. The golden is computed outside Go, so any host that
// reorders the tree or writes a native separator moves it.
func TestHashFiles_FoldsSlashPathsInAbsoluteSlashOrder(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "proj")
	files := []struct{ path, content string }{
		{"proj/a1", "a1"},
		{"proj/a/b", "ab"},
		{"proj/sub/x.go", "package sub"},
		{"proj/z.go", "package z"},
		{"sibling/shared.txt", "shared"},
	}
	for _, f := range files {
		p := filepath.Join(root, filepath.FromSlash(f.path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f.content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	expected := sha256.New()
	for _, f := range []struct{ rel, content string }{
		{"a/b", "ab"},
		{"a1", "a1"},
		{"sub/x.go", "package sub"},
		{"z.go", "package z"},
		{"../sibling/shared.txt", "shared"},
	} {
		expected.Write([]byte(f.rel))
		expected.Write([]byte{0})
		digest := sha256.Sum256([]byte(f.content))
		expected.Write(digest[:])
		expected.Write([]byte{0})
	}
	structural := hex.EncodeToString(expected.Sum(nil))

	const golden = "583aa08533f1d61a332a1a233b45fe0658397989a0f90019cab69642f4b38a0a"
	if structural != golden {
		t.Fatalf("the structural expectation drifted from the golden: %s != %s", structural, golden)
	}

	got, err := hashFiles(dir, []string{"z.go", "a1", "sub/*.go", "../sibling/shared.txt", "a/b"}, ProjectConfigScope{})
	if err != nil {
		t.Fatal(err)
	}
	if got != golden {
		t.Fatalf("hashFiles folded the tree differently: got %s, want %s", got, golden)
	}
}

// A declared asset directory folds its files in the same slash order, each
// under its slash-form path relative to the directory.
func TestHashPathContent_FoldsADirectoryInSlashOrder(t *testing.T) {
	dir := t.TempDir()
	for rel, content := range map[string]string{"a1": "one", "a/b": "nested"} {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got := sha256.New()
	hashPathContent(got, dir)

	want := sha256.New()
	want.Write([]byte("a/b\x00nested\x00a1\x00one\x00"))
	if hex.EncodeToString(got.Sum(nil)) != hex.EncodeToString(want.Sum(nil)) {
		t.Fatal("hashPathContent did not fold a/b before a1 under slash-form paths")
	}
}
