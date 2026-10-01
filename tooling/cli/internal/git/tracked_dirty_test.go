package git

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The two snapshots name exactly what a step rewrote: a committed file it
// changed, a committed file it deleted — and not a file that was already
// dirty and left alone, nor an untracked file it created.
func TestTrackedDirtyDigests_TwoSnapshotsNameWhatAStepRewrote(t *testing.T) {
	dir := initGitRepo(t)
	for _, name := range []string{"lock.json", "already-dirty.txt", "gone.txt"} {
		commitNewFile(t, dir, name)
	}
	if err := os.WriteFile(filepath.Join(dir, "already-dirty.txt"), []byte("edited before\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	before, err := TrackedDirtyDigests(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := RewrittenTrackedFiles(before, before); len(got) != 0 {
		t.Fatalf("a step that changed nothing rewrote %v", got)
	}

	// The step: rewrite one committed file, delete another, add an untracked
	// one, leave the already-dirty one alone.
	if err := os.WriteFile(filepath.Join(dir, "lock.json"), []byte("{\"rewritten\":true}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := TrackedDirtyDigests(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := RewrittenTrackedFiles(before, after), []string{"gone.txt", "lock.json"}; !slices.Equal(got, want) {
		t.Fatalf("rewritten = %v, want %v", got, want)
	}
	if after["gone.txt"] != deletedTrackedDigest {
		t.Errorf("deleted file digest = %q, want %q", after["gone.txt"], deletedTrackedDigest)
	}

	// A second edit of the already-dirty file IS the step's doing.
	if err := os.WriteFile(filepath.Join(dir, "already-dirty.txt"), []byte("edited again\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	again, err := TrackedDirtyDigests(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := RewrittenTrackedFiles(after, again); !slices.Equal(got, []string{"already-dirty.txt"}) {
		t.Fatalf("rewritten after the second edit = %v, want [already-dirty.txt]", got)
	}
}

// Outside a repository there is nothing to compare against, and nothing to
// report: nil, nil rather than an error the caller would have to classify.
func TestTrackedDirtyDigests_OutsideARepositoryIsEmpty(t *testing.T) {
	digests, err := TrackedDirtyDigests(t.TempDir())
	if err != nil || digests != nil {
		t.Fatalf("outside a repository = %v, %v; want nil, nil", digests, err)
	}
}
