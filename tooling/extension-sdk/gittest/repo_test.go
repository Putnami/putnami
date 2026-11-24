package gittest

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestRepoBuildIsTheSameOnAForeignMachine builds the repository Repo copies
// on this machine and on a foreign one, outside the once-per-binary template,
// so the build itself is what the comparison covers.
func TestRepoBuildIsTheSameOnAForeignMachine(t *testing.T) {
	requireGit(t)
	AssertMachineIndependent(t, func() string {
		dir := t.TempDir()
		if err := buildRepo(dir); err != nil {
			t.Fatal(err)
		}
		return dir
	})
}

// TestRepoCopyIsTheBuiltRepository compares a copy with a fresh build: the
// same branch, commit tree, identity, message, index and configuration.
func TestRepoCopyIsTheBuiltRepository(t *testing.T) {
	requireGit(t)
	t.Parallel()
	built := t.TempDir()
	if err := buildRepo(built); err != nil {
		t.Fatal(err)
	}
	copied := Repo(t)
	if got, want := Describe(t, copied), Describe(t, built); got != want {
		t.Fatalf("the copy differs from a fresh build\n--- copy ---\n%s\n--- build ---\n%s", got, want)
	}
	if got := Run(t, copied, "status", "--porcelain"); got != "" {
		t.Fatalf("a fresh copy is not clean:\n%s", got)
	}
	if got := Run(t, copied, "config", "commit.gpgsign"); got != "false" {
		t.Fatalf("commit.gpgsign = %q, want false", got)
	}
	data, err := os.ReadFile(filepath.Join(copied, RepoFile))
	if err != nil || string(data) != RepoContent {
		t.Fatalf("%s = %q, %v; want %q", RepoFile, data, err, RepoContent)
	}
}

// TestRepoCopiesAreIndependent changes one copy and checks that another copy,
// taken before or after, still holds the one commit.
func TestRepoCopiesAreIndependent(t *testing.T) {
	requireGit(t)
	t.Parallel()
	first, second := Repo(t), Repo(t)
	if first == second {
		t.Fatal("two copies share a directory")
	}
	head := Run(t, second, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(first, "second.txt"), []byte("second"), 0o644); err != nil {
		t.Fatal(err)
	}
	Run(t, first, "add", ".")
	Run(t, first, "commit", "-m", "second")
	Run(t, first, "tag", "v1")
	for _, dir := range []string{second, Repo(t)} {
		if got := Run(t, dir, "rev-parse", "HEAD"); got != head {
			t.Fatalf("HEAD of an untouched copy = %s, want %s", got, head)
		}
		if got := Run(t, dir, "tag", "--list"); got != "" {
			t.Fatalf("an untouched copy has tags %q", got)
		}
	}
}

func TestSnapshotReportsABuildFailure(t *testing.T) {
	t.Parallel()
	want := errors.New("no build")
	if _, err := snapshot(func(string) error { return want }); !errors.Is(err, want) {
		t.Fatalf("snapshot error = %v, want %v", err, want)
	}
}

func TestSnapshotRefusesASymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a symlink needs a privilege on Windows")
	}
	t.Parallel()
	_, err := snapshot(func(dir string) error { return os.Symlink("target", filepath.Join(dir, "link")) })
	if err == nil || !strings.Contains(err.Error(), "only directories and regular files") {
		t.Fatalf("snapshot error = %v, want a refusal of the symlink", err)
	}
}

// TestTemplateWritesModesAndNestedFiles copies a template with a nested
// directory, a read-only file and an executable file.
func TestTemplateWritesModesAndNestedFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows keeps no executable bit")
	}
	t.Parallel()
	var tpl template
	build := func(dir string) error {
		if err := os.MkdirAll(filepath.Join(dir, "a", "b"), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "a", "b", "read-only"), []byte("r"), 0o444); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, "a", "run"), []byte("x"), 0o755)
	}
	dir := tpl.copy(t, build)
	for name, want := range map[string]os.FileMode{"a/b/read-only": 0o444, "a/run": 0o755} {
		info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Fatalf("%s mode = %v, want %v", name, got, want)
		}
	}
	if again := tpl.copy(t, func(string) error { t.Fatal("the template was built twice"); return nil }); again == dir {
		t.Fatal("two copies share a directory")
	}
}

func TestTemplateWriteReportsAnExistingFile(t *testing.T) {
	t.Parallel()
	tpl := template{files: []templateFile{{path: "a", mode: os.ModeDir | 0o755}}}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := tpl.write(dir); err == nil {
		t.Fatal("write over an existing file succeeded")
	}
}
