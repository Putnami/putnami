package features

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"testing"

	putnamigit "go.putnami.dev/tooling/sdd/extension/internal/gitread"
)

func cutSymlink(t *testing.T, repo, target, relative string) {
	t.Helper()
	path := filepath.Join(repo, filepath.FromSlash(relative))
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

// The task reader reads the candidate cut its `git:**` key holds and nothing
// else, with the containment and error kinds the disk reader reports.
func TestCutReaderReadsOnlyTheCandidateCut(t *testing.T) {
	repo := initFeatureGitRepo(t)
	writeRevisionFile(t, repo, ".gitignore", "billing/schema/feature-evidence/ignored.json\n")
	writeRevisionFile(t, repo, "outside.json", "{}")
	writeRevisionFile(t, repo, "billing/putnami.features.json", "{\"manifest\":true}")
	writeRevisionFile(t, repo, "billing/schema/feature-evidence/a.json", "{}")
	writeRevisionFile(t, repo, "billing/schema/feature-evidence/sub/b.json", "{}")
	writeRevisionFile(t, repo, "billing/schema/feature-evidence/ignored.json", "{}")
	if err := os.MkdirAll(filepath.Join(repo, "billing", "schema", "feature-evidence", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	cutSymlink(t, repo, "putnami.features.json", "billing/link.json")
	cutSymlink(t, repo, "missing.json", "billing/dangling.json")
	cutSymlink(t, repo, "schema/feature-evidence/ignored.json", "billing/to-ignored.json")
	cutSymlink(t, repo, "../outside.json", "billing/escape.json")
	cutSymlink(t, repo, filepath.Join(t.TempDir(), "absolute.json"), "billing/absolute.json")
	cutSymlink(t, repo, "billing", "linked-billing")
	cutSymlink(t, repo, filepath.Dir(repo), "away")

	opened, err := NewTaskReader(repo)
	if err != nil {
		t.Fatal(err)
	}
	reader, ok := opened.(*CutReader)
	if !ok {
		t.Fatalf("NewTaskReader in a work tree = %T, want the candidate cut", opened)
	}

	if data, err := reader.ReadFile("billing", "putnami.features.json"); err != nil || string(data) != "{\"manifest\":true}" {
		t.Errorf("ReadFile(manifest) = %q, %v", data, err)
	}
	if data, err := reader.ReadFile("billing", "link.json"); err != nil || string(data) != "{\"manifest\":true}" {
		t.Errorf("ReadFile through a candidate link = %q, %v", data, err)
	}
	if data, err := reader.ReadFile("linked-billing", "putnami.features.json"); err != nil || string(data) != "{\"manifest\":true}" {
		t.Errorf("ReadFile under a linked root = %q, %v", data, err)
	}
	for _, missing := range []struct{ root, relative string }{
		{"billing", "schema/feature-evidence/ignored.json"},
		{"billing", "absent.json"},
		{"absent", "putnami.features.json"},
	} {
		if _, err := reader.ReadFile(missing.root, missing.relative); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("ReadFile(%q, %q) error = %v, want fs.ErrNotExist", missing.root, missing.relative, err)
		}
	}
	for _, refused := range []struct {
		root, relative string
		kind           ReaderErrorKind
	}{
		{"billing", "dangling.json", ReaderErrorUnsupportedFile},
		{"billing", "to-ignored.json", ReaderErrorUnsupportedFile},
		{"billing", "schema", ReaderErrorUnsupportedFile},
		{"billing", "escape.json", ReaderErrorSymlinkEscape},
		{"billing", "absolute.json", ReaderErrorSymlinkEscape},
		{"billing", "../outside.json", ReaderErrorPathEscape},
		{"../billing", "putnami.features.json", ReaderErrorPathEscape},
		{"away", "x.json", ReaderErrorOutsideWorkspace},
	} {
		if _, err := reader.ReadFile(refused.root, refused.relative); kindOfReaderError(err) != refused.kind {
			t.Errorf("ReadFile(%q, %q) error = %v, want %s", refused.root, refused.relative, err, refused.kind)
		}
	}
	if _, err := reader.ReadFile("billing/putnami.features.json", "x.json"); err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Errorf("ReadFile under a file root error = %v, want a structural error", err)
	}

	entries, err := reader.ReadDir("billing", "schema/feature-evidence")
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	if want := []DirEntry{{Name: "a.json"}, {Name: "sub", IsDirectory: true}}; !reflect.DeepEqual(entries, want) {
		t.Errorf("ReadDir = %+v, want %+v: an ignored file and an empty directory are not in the cut", entries, want)
	}
	if root, err := reader.ReadDir("", "billing"); err != nil || len(root) == 0 {
		t.Errorf("ReadDir(billing) = %+v, %v", root, err)
	}
	if _, err := reader.ReadDir("billing", "putnami.features.json"); err == nil {
		t.Error("ReadDir of a file succeeded")
	}
	if _, err := reader.ReadDir("billing", "schema/feature-evidence/empty"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("ReadDir of an empty directory error = %v, want fs.ErrNotExist", err)
	}

	want, err := putnamigit.ProjectSourceBinding(reader.root, filepath.Join(reader.root, "billing"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := reader.SourceBinding("billing"); err != nil || got != want {
		t.Errorf("SourceBinding(billing) = %q, %v; want %q", got, err, want)
	}
	if _, err := reader.SourceBinding(""); err != nil {
		t.Errorf("SourceBinding of the workspace: %v", err)
	}
	for _, root := range []string{"billing/putnami.features.json", "absent", "../billing"} {
		if _, err := reader.SourceBinding(root); err == nil {
			t.Errorf("SourceBinding(%q) succeeded", root)
		}
	}
}

func TestNewTaskReaderReadsTheDiskOutsideAWorkTree(t *testing.T) {
	reader, err := NewTaskReader(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, disk := reader.(*OSReader); !disk {
		t.Fatalf("NewTaskReader outside a work tree = %T, want the disk reader", reader)
	}
	if _, err := NewTaskReader(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("NewTaskReader of an absent root succeeded")
	}
}
