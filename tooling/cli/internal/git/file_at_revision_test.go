package git

import (
	"os"
	"path/filepath"
	"testing"
)

// FileAtRevision reads the bytes a commit records, not the working tree's,
// relative to the directory it is given: a workspace nested in a repository
// names its own files.
func TestFileAtRevision_ReadsTheCommittedBytesRelativeToTheRoot(t *testing.T) {
	dir := initGitRepo(t)
	manifest := filepath.Join(dir, "ws", "ext", "putnami.extension.json")
	write(t, manifest, "{\"name\": \"before\"}\n")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "manifest")
	write(t, manifest, "{\"name\": \"after\"}\n")

	got, ok := FileAtRevision(dir, "HEAD", "ws/ext/putnami.extension.json")
	if !ok || string(got) != "{\"name\": \"before\"}\n" {
		t.Errorf("FileAtRevision from the repository root = %q, %t; want the committed bytes", got, ok)
	}
	got, ok = FileAtRevision(filepath.Join(dir, "ws"), "HEAD", "ext/putnami.extension.json")
	if !ok || string(got) != "{\"name\": \"before\"}\n" {
		t.Errorf("FileAtRevision from a nested workspace = %q, %t; want the committed bytes", got, ok)
	}
}

// A path the commit does not record reads nothing: a file the working tree
// adds, and one that never existed.
func TestFileAtRevision_AFileTheCommitDoesNotRecordReadsNothing(t *testing.T) {
	dir := initGitRepo(t)
	write(t, filepath.Join(dir, "added.json"), "{}\n")

	for _, file := range []string{"added.json", "missing.json"} {
		if got, ok := FileAtRevision(dir, "HEAD", file); ok {
			t.Errorf("FileAtRevision(%s) = %q, want nothing", file, got)
		}
	}
}

// A path that leaves the root, or a revision that git would read as an option
// or as an object path, reads nothing.
func TestFileAtRevision_AnUnsafeArgumentReadsNothing(t *testing.T) {
	dir := initGitRepo(t)
	nested := filepath.Join(dir, "ws")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	cases := []struct{ root, revision, file string }{
		{nested, "HEAD", "../README.md"},
		{dir, "HEAD", "/README.md"},
		{dir, "HEAD", "README.md\n"},
		{dir, "HEAD", ""},
		{dir, "-p", "README.md"},
		{dir, "HEAD:README.md", "README.md"},
	}
	for _, tc := range cases {
		if got, ok := FileAtRevision(tc.root, tc.revision, tc.file); ok {
			t.Errorf("FileAtRevision(%q, %q) = %q, want nothing", tc.revision, tc.file, got)
		}
	}
	if _, ok := FileAtRevision(dir, "HEAD", "README.md"); !ok {
		t.Error("FileAtRevision(HEAD, README.md) read nothing, want the committed README")
	}
}
