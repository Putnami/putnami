package docslinks

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestSelectedDocumentsAreWhatThePatternsSelectInTheCut pins the selection a
// workspace configures: every candidate file the patterns select, with their
// exclusions, and nothing a name rule or a project boundary would add or
// remove. A file Git ignores is not selected, as it is absent from a clone.
func TestSelectedDocumentsAreWhatThePatternsSelectInTheCut(t *testing.T) {
	root := workTree(t)
	writeFile(t, root, "README.md", "# root\n")
	writeFile(t, root, "pkg/README.md", "# pkg\n")
	writeFile(t, root, "pkg/putnami.json", "{}\n")
	writeFile(t, root, "pkg/doc/guide.md", "# guide\n")
	writeFile(t, root, "pkg/notes.md", "# notes\n")
	writeFile(t, root, "vendor/lib/README.md", "# vendored\n")
	writeFile(t, root, "build/README.md", "# ignored\n")
	writeFile(t, root, ".gitignore", "build/\n")

	reader, err := NewReader(root)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reader.SelectedDocuments([]string{"**/README.md", "**/doc/**/*.md", "!vendor/**"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		filepath.Join(root, "README.md"),
		filepath.Join(root, "pkg", "README.md"),
		filepath.Join(root, "pkg", "doc", "guide.md"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("selected documents = %v, want %v", got, want)
	}
}

// TestSelectedDocumentsWalkTheDiskOutsideGit pins the fallback: outside a Git
// work tree the disk is walked, less the .git directory.
func TestSelectedDocumentsWalkTheDiskOutsideGit(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "README.md", "# root\n")
	writeFile(t, root, "a/doc/x.md", "# x\n")
	writeFile(t, root, "a/other.md", "# other\n")

	reader, err := NewReader(root)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reader.SelectedDocuments([]string{"**/README.md", "**/doc/**/*.md"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(root, "README.md"), filepath.Join(root, "a", "doc", "x.md")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("selected documents = %v, want %v", got, want)
	}
}

// TestSelectedDocumentsPruneExcludedTreesOutsideGit pins that the disk walk
// never enters a tree an exclusion removes whole, so an unreadable directory
// there cannot fail the walk, and that an unreadable directory elsewhere is
// skipped rather than failing it.
func TestSelectedDocumentsPruneExcludedTreesOutsideGit(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "README.md", "# root\n")
	writeFile(t, root, "node_modules/pkg/README.md", "# installed\n")
	writeFile(t, root, "locked/README.md", "# unreadable\n")
	if err := os.Chmod(filepath.Join(root, "locked"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(root, "locked"), 0o755) })

	reader, err := NewReader(root)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reader.SelectedDocuments([]string{"**/README.md", "!**/node_modules/**"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(root, "README.md")}
	if os.Geteuid() == 0 {
		// root reads a 0o000 directory, so its README is selected.
		want = append(want, filepath.Join(root, "locked", "README.md"))
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("selected documents = %v, want %v", got, want)
	}
}
