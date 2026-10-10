package gitcandidate

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

// repository creates an empty Git repository with the files given, each a
// slash path relative to it mapped to its content.
func repository(t *testing.T, files map[string]string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	git(t, root, "init", "-q")
	for rel, content := range files {
		write(t, root, rel, content)
	}
	return root
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func symlink(t *testing.T, root, target, rel string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.FromSlash(target), path); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symbolic links are not available: %v", err)
		}
		t.Fatal(err)
	}
}

func open(t *testing.T, root string) *Tree {
	t.Helper()
	tree, err := Open(root)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if tree == nil {
		t.Fatal("Open returned no tree inside a repository")
	}
	return tree
}

func TestOpenReturnsNoTreeOutsideARepository(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	tree, err := Open(t.TempDir())
	if err != nil || tree != nil {
		t.Fatalf("Open outside a repository = %v, %v; want no tree and no error, so the caller reads the disk", tree, err)
	}
}

func TestOpenFailsWhenTheCutCannotBeEnumerated(t *testing.T) {
	root := repository(t, map[string]string{"README.md": "# a\n"})
	git(t, root, "add", "-A")
	if err := os.WriteFile(filepath.Join(root, ".git", "index"), []byte("not an index"), 0o644); err != nil {
		t.Fatal(err)
	}
	if tree, err := Open(root); err == nil {
		t.Fatalf("Open over a corrupt index = %v; an unreadable cut must not read as an empty one", tree.Paths())
	}
}

func TestOpenListsTheCandidateCut(t *testing.T) {
	root := repository(t, map[string]string{
		".gitignore":          "ignored.md\nbuild/\n",
		"README.md":           "# a\n",
		"doc/guide.md":        "# guide\n",
		"doc/deleted.md":      "# gone\n",
		"untracked.md":        "# new\n",
		"ignored.md":          "# ignored\n",
		"build/output.md":     "# built\n",
		"tracked/ignored.md":  "# tracked although ignored\n",
		"empty/.keep-nothing": "",
	})
	git(t, root, "add", "README.md", "doc", ".gitignore")
	git(t, root, "add", "-f", "tracked/ignored.md")
	if err := os.Remove(filepath.Join(root, "doc", "deleted.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "empty", ".keep-nothing")); err != nil {
		t.Fatal(err)
	}

	tree := open(t, root)
	want := []string{".gitignore", "README.md", "doc/guide.md", "tracked/ignored.md", "untracked.md"}
	if got := tree.Paths(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Paths() = %v, want %v: tracked and non-ignored untracked files that exist, and nothing else", got, want)
	}
	if tree.Root() != filepath.Clean(root) {
		t.Errorf("Root() = %q, want %q", tree.Root(), root)
	}
	for rel, want := range map[string]Kind{
		".":                  Dir,
		"doc":                Dir,
		"doc/guide.md":       File,
		"doc/deleted.md":     Missing,
		"ignored.md":         Missing,
		"build":              Missing,
		"build/output.md":    Missing,
		"empty":              Missing,
		"tracked/ignored.md": File,
		"Doc/guide.md":       Missing,
		"doc/guide.md/x":     Missing,
		"../outside.md":      Outside,
		"doc/../README.md":   File,
	} {
		if _, got := tree.Resolve(rel); got != want {
			t.Errorf("Resolve(%q) = %v, want %v", rel, got, want)
		}
	}
}

func TestLstatDoesNotFollowALink(t *testing.T) {
	root := repository(t, map[string]string{"doc/guide.md": "# guide\n"})
	symlink(t, root, "doc/guide.md", "link.md")
	tree := open(t, root)
	for rel, want := range map[string]fs.FileMode{
		"doc/guide.md": 0,
		"link.md":      fs.ModeSymlink,
		"doc":          fs.ModeDir,
		".":            fs.ModeDir,
	} {
		got, ok := tree.Lstat(rel)
		if !ok || got != want {
			t.Errorf("Lstat(%q) = %v, %v; want %v, true", rel, got, ok, want)
		}
	}
	for _, rel := range []string{"missing.md", "../x", "link.md/x"} {
		if _, ok := tree.Lstat(rel); ok {
			t.Errorf("Lstat(%q) found a candidate", rel)
		}
	}
}

func TestResolveFollowsLinksInsideTheCut(t *testing.T) {
	root := repository(t, map[string]string{
		".gitignore":          "private/\n",
		"shared/doc/guide.md": "# guide\n",
		"private/secret.md":   "# secret\n",
		"README.md":           "# a\n",
	})
	symlink(t, root, "shared/doc/guide.md", "file-link.md")
	symlink(t, root, "../shared/doc", "project/doc")
	symlink(t, root, "file-link.md", "chain.md")
	symlink(t, root, "/etc/hosts", "absolute.md")
	symlink(t, root, "../../outside.md", "project/escape.md")
	symlink(t, root, "missing.md", "dangling.md")
	symlink(t, root, "private/secret.md", "to-ignored.md")
	symlink(t, root, "loop-b", "loop-a")
	symlink(t, root, "loop-a", "loop-b")
	tree := open(t, root)

	for rel, want := range map[string]struct {
		resolved string
		kind     Kind
	}{
		"file-link.md":          {"shared/doc/guide.md", File},
		"chain.md":              {"shared/doc/guide.md", File},
		"project/doc":           {"shared/doc", Dir},
		"project/doc/guide.md":  {"shared/doc/guide.md", File},
		"project/doc/absent.md": {"", Missing},
		"absolute.md":           {"", Outside},
		"project/escape.md":     {"", Outside},
		"dangling.md":           {"", Missing},
		"to-ignored.md":         {"", Missing},
		"loop-a":                {"", Missing},
	} {
		resolved, kind := tree.Resolve(rel)
		if kind != want.kind || (want.resolved != "" && resolved != want.resolved) {
			t.Errorf("Resolve(%q) = %q, %v; want %q, %v", rel, resolved, kind, want.resolved, want.kind)
		}
	}

	data, err := tree.ReadFile("project/doc/guide.md")
	if err != nil || string(data) != "# guide\n" {
		t.Errorf("ReadFile through a directory link = %q, %v", data, err)
	}
	for _, rel := range []string{"to-ignored.md", "project/doc", "absolute.md", "private/secret.md"} {
		if _, err := tree.ReadFile(rel); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("ReadFile(%q) error = %v, want fs.ErrNotExist: only a regular candidate is read", rel, err)
		}
	}
}

func TestANestedRepositoryIsADirectoryWithoutCandidates(t *testing.T) {
	root := repository(t, map[string]string{"README.md": "# a\n"})
	nested := filepath.Join(root, "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, nested, "init", "-q")
	write(t, root, "nested/inner.md", "# inner\n")
	tree := open(t, root)
	if _, kind := tree.Resolve("nested"); kind != Dir {
		t.Errorf("Resolve(nested) = %v, want Dir", kind)
	}
	if _, kind := tree.Resolve("nested/inner.md"); kind != Missing {
		t.Errorf("Resolve(nested/inner.md) = %v, want Missing: Git does not list a nested repository's files", kind)
	}
}

func TestOpenAtASubdirectoryListsItsPathsRelativeToIt(t *testing.T) {
	root := repository(t, map[string]string{
		"README.md":         "# a\n",
		"project/README.md": "# p\n",
		"project/doc/x.md":  "# x\n",
	})
	tree := open(t, filepath.Join(root, "project"))
	if got, want := tree.Paths(), []string{"README.md", "doc/x.md"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Paths() = %v, want %v", got, want)
	}
}

func TestNewTreeRefusesAPathOutsideItsRoot(t *testing.T) {
	for _, listed := range []string{"../x", "/abs", "."} {
		if _, err := newTree(t.TempDir(), []string{listed}); err == nil {
			t.Errorf("newTree accepted the listed path %q", listed)
		}
	}
}

func TestFingerprintMovesWithWhatAGitKeyReads(t *testing.T) {
	root := repository(t, map[string]string{
		".gitignore": "ignored/\n",
		"README.md":  "# a\n",
		"doc/x.md":   "# x\n",
	})
	symlink(t, root, "doc/x.md", "link.md")
	fingerprint := func() string {
		t.Helper()
		value, err := open(t, root).Fingerprint()
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	before := fingerprint()
	for _, step := range []struct {
		name  string
		apply func()
		moves bool
	}{
		{"an ignored file appears", func() { write(t, root, "ignored/x.md", "# ignored\n") }, false},
		{"an empty directory appears", func() {
			if err := os.MkdirAll(filepath.Join(root, "empty"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"the cut is staged", func() { git(t, root, "add", "-A") }, false},
		{"a candidate changes", func() { write(t, root, "doc/x.md", "# y\n") }, true},
		{"a candidate appears", func() { write(t, root, "doc/new.md", "# new\n") }, true},
		{"a link changes its target", func() {
			if err := os.Remove(filepath.Join(root, "link.md")); err != nil {
				t.Fatal(err)
			}
			symlink(t, root, "README.md", "link.md")
		}, true},
		{"a tracked candidate is deleted", func() {
			if err := os.Remove(filepath.Join(root, "doc", "new.md")); err != nil {
				t.Fatal(err)
			}
		}, true},
	} {
		step.apply()
		after := fingerprint()
		if moved := after != before; moved != step.moves {
			t.Errorf("%s: fingerprint moved = %v, want %v", step.name, moved, step.moves)
		}
		before = after
	}
}
