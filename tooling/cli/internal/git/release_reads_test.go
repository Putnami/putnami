package git

import (
	"os"
	"path/filepath"
	"testing"
)

// The run's reader answers every directory of its work tree with the
// baseline ReadReleaseBaseline reads there, and a directory it cannot place
// in that work tree, a nested repository, a link or a path outside the root,
// is read there as ReadReleaseBaseline reads it (tooling/cli ADR 0110).
func TestTheReleaseBaselineReaderReadsWhatEachDirectoryReads(t *testing.T) {
	root, _ := releasedAPIRepo(t)
	writeCommit(t, root, "web/a.go", "feat(web)!: break the web")
	writeCommit(t, root, "api/b.go", "feat(api)!: break the API")
	gitDo(t, root, "tag", "-a", "web/v1.0.0", "-m", "web release")
	writeCommit(t, root, "workspace/lib/c.go", "fix(lib): a fix")
	nested := filepath.Join(root, "workspace", "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	gitDo(t, nested, "init", "-q")
	writeCommit(t, nested, "n.go", "feat: nested")
	if err := os.Symlink(filepath.Join(root, "api"), filepath.Join(root, "workspace", "linked")); err != nil {
		t.Skip("symbolic links unavailable:", err)
	}
	outside := t.TempDir()

	for _, readerRoot := range []string{root, filepath.Join(root, "workspace")} {
		reader := NewReleaseBaselineReader(readerRoot)
		dirs := []string{root, filepath.Join(root, "api"), filepath.Join(root, "web"), filepath.Join(root, "absent"),
			filepath.Join(root, "workspace"), filepath.Join(root, "workspace", "lib"), nested,
			filepath.Join(root, "workspace", "linked"), outside}
		for _, dir := range dirs {
			for _, pattern := range []string{apiPattern, "web/v{version}", "lib/v{version}"} {
				want, wantErr := ReadReleaseBaseline(dir, pattern)
				got, err := reader.Read(dir, pattern)
				if (err == nil) != (wantErr == nil) || got != want {
					t.Errorf("reader at %s, %s with %s: got %+v (%v), want %+v (%v)", readerRoot, dir, pattern, got, err, want, wantErr)
				}
			}
		}
	}
}

// A reader whose root no work tree holds reads each directory as
// ReadReleaseBaseline does.
func TestAReleaseBaselineReaderOutsideAWorkTreeReadsEachDirectory(t *testing.T) {
	dir := t.TempDir()
	reader := NewReleaseBaselineReader(dir)
	got, err := reader.Read(dir, apiPattern)
	want, wantErr := ReadReleaseBaseline(dir, apiPattern)
	if (err == nil) != (wantErr == nil) || got != want {
		t.Fatalf("got %+v (%v), want %+v (%v)", got, err, want, wantErr)
	}
}
