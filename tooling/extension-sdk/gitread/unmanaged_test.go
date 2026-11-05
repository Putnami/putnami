package gitread

import "testing"

func TestUnmanaged_InsideRepositoryIsNil(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)
	if err := Unmanaged(dir); err != nil {
		t.Fatalf("Unmanaged(repository) = %v, want nil", err)
	}
	// A repository with no commit is still one Git manages.
	unborn := t.TempDir()
	runGit(t, unborn, "init")
	if err := Unmanaged(unborn); err != nil {
		t.Fatalf("Unmanaged(repository with no commit) = %v, want nil", err)
	}
}
