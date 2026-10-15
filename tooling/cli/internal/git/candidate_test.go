package git

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The index read keeps the mode the index records for each merged path, the
// executable bit included where the disk carries none, sets every unmerged
// path apart, and fails where no index can be read.
func TestReadIndexModes(t *testing.T) {
	repo := indexExecutableRepo(t)
	write(t, filepath.Join(repo, "conflict.md"), "# conflict\n")
	blob := strings.TrimSpace(runGit(t, repo, "hash-object", "-w", "conflict.md"))
	stages := exec.Command("git", "update-index", "--index-info")
	stages.Dir = repo
	stages.Stdin = strings.NewReader("100644 " + blob + " 1\tconflict.md\n100644 " + blob + " 2\tconflict.md\n")
	if out, err := stages.CombinedOutput(); err != nil {
		t.Fatalf("git update-index --index-info: %v\n%s", err, out)
	}

	index, err := ReadIndexModes(repo)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{"README.md": "100644", "plain.txt": "100644", "tool.sh": "100755"} {
		if got := index.Modes[path]; got != want {
			t.Errorf("mode of %s = %q, want %q", path, got, want)
		}
	}
	if !index.Unmerged["conflict.md"] || index.Modes["conflict.md"] != "" {
		t.Errorf("conflict.md: unmerged = %v, mode = %q; want unmerged with no mode", index.Unmerged["conflict.md"], index.Modes["conflict.md"])
	}
	if len(index.Unmerged) != 1 {
		t.Errorf("unmerged = %v, want conflict.md alone", index.Unmerged)
	}

	if _, err := ReadIndexModes(outsideRepository(t)); err == nil {
		t.Fatal("ReadIndexModes outside a repository succeeded")
	}
}

// TrackedUnder answers from the index: a tracked file deleted from the
// worktree is listed, an untracked file is not, a sibling whose name shares
// the prefix is not, and a path that does not exist lists nothing. Outside a
// repository the answer is an *UnmanagedError.
func TestTrackedUnderListsWhatTheIndexTracksBelowAPath(t *testing.T) {
	repo := initGitRepo(t)
	ws := filepath.Join(repo, "sub", "ws")
	for _, rel := range []string{"out/a.txt", "out/nested/b.txt", "outer.txt", "out[x]/c.txt"} {
		write(t, filepath.Join(ws, filepath.FromSlash(rel)), rel+"\n")
	}
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-q", "-m", "track")
	write(t, filepath.Join(ws, "out", "untracked.txt"), "new\n")
	if err := os.Remove(filepath.Join(ws, "out", "a.txt")); err != nil {
		t.Fatal(err)
	}

	for rel, want := range map[string][]string{
		"out":     {"out/a.txt", "out/nested/b.txt"},
		"out[x]":  {"out[x]/c.txt"},
		"missing": nil,
	} {
		got, err := TrackedUnder(ws, rel)
		if err != nil {
			t.Fatalf("TrackedUnder(%q): %v", rel, err)
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("TrackedUnder(%q) = %v, want %v", rel, got, want)
		}
	}

	_, err := TrackedUnder(t.TempDir(), "out")
	var unmanaged *UnmanagedError
	if !errors.As(err, &unmanaged) {
		t.Fatalf("TrackedUnder outside a repository = %v, want an *UnmanagedError", err)
	}
}
