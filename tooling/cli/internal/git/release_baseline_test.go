package git

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const apiPattern = "api/v{version}"

// baselineOf reads the release baseline of dir and fails the test on an error.
func baselineOf(t *testing.T, dir, pattern string) ReleaseBaseline {
	t.Helper()
	baseline, err := ReadReleaseBaseline(dir, pattern)
	if err != nil {
		t.Fatalf("ReadReleaseBaseline(%s, %s): %v", dir, pattern, err)
	}
	return baseline
}

// releasedAPIRepo is a repository whose api line is tagged api/v0.1.0 at a
// commit that holds api/a.go, and whose HEAD is that commit.
func releasedAPIRepo(t *testing.T) (root, api string) {
	t.Helper()
	root = initGitRepo(t)
	writeCommit(t, root, "api/a.go", "feat(api): add the API")
	gitDo(t, root, "tag", "-a", "api/v0.1.0", "-m", "release")
	return root, filepath.Join(root, "api")
}

func TestReadReleaseBaselineStates(t *testing.T) {
	t.Run("no commit", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		gitDo(t, dir, "init", "-q")
		if got := baselineOf(t, dir, apiPattern); got.State != ReleaseBaselineNoCommit || got.Pattern != apiPattern {
			t.Fatalf("baseline = %+v, want state %s with the pattern", got, ReleaseBaselineNoCommit)
		}
	})
	t.Run("shallow", func(t *testing.T) {
		t.Parallel()
		root, _ := releasedAPIRepo(t)
		writeCommit(t, root, "api/b.go", "feat(api): more")
		clone := filepath.Join(t.TempDir(), "clone")
		gitDo(t, root, "clone", "-q", "--depth", "1", "file://"+root, clone)
		if got := baselineOf(t, filepath.Join(clone, "api"), apiPattern); got.State != ReleaseBaselineShallow || got.Tag != "" {
			t.Fatalf("baseline = %+v, want state %s and no tag", got, ReleaseBaselineShallow)
		}
	})
	t.Run("untagged, no tag at all", func(t *testing.T) {
		t.Parallel()
		root := initGitRepo(t)
		writeCommit(t, root, "api/a.go", "feat(api): add")
		got := baselineOf(t, filepath.Join(root, "api"), apiPattern)
		if got.State != ReleaseBaselineUntagged || got.HasTags {
			t.Fatalf("baseline = %+v, want untagged without tags", got)
		}
	})
	t.Run("untagged, another line tagged", func(t *testing.T) {
		t.Parallel()
		root := initGitRepo(t)
		writeCommit(t, root, "api/a.go", "feat(api): add")
		gitDo(t, root, "tag", "-a", "web/v1.0.0", "-m", "other line")
		got := baselineOf(t, filepath.Join(root, "api"), apiPattern)
		if got.State != ReleaseBaselineUntagged || !got.HasTags {
			t.Fatalf("baseline = %+v, want untagged with tags", got)
		}
	})
	t.Run("tagged", func(t *testing.T) {
		t.Parallel()
		_, api := releasedAPIRepo(t)
		got := baselineOf(t, api, apiPattern)
		if got.State != ReleaseBaselineTagged || got.Tag != "api/v0.1.0" || !strings.HasPrefix(got.Object, "tree ") || got.Breaking {
			t.Fatalf("baseline = %+v, want the tag and the tree it holds at api", got)
		}
	})
	t.Run("directory absent at the tag", func(t *testing.T) {
		t.Parallel()
		root, _ := releasedAPIRepo(t)
		writeCommit(t, root, "web/a.go", "feat(web): add")
		got := baselineOf(t, filepath.Join(root, "web"), apiPattern)
		if got.State != ReleaseBaselineTagged || got.Object != "" {
			t.Fatalf("baseline = %+v, want tagged with no object at web", got)
		}
	})
	t.Run("project at the root", func(t *testing.T) {
		t.Parallel()
		root, _ := releasedAPIRepo(t)
		tree := strings.TrimSpace(runGit(t, root, "rev-parse", "api/v0.1.0^{tree}"))
		if got := baselineOf(t, root, apiPattern); got.Object != "tree "+tree {
			t.Fatalf("baseline = %+v, want the tag's root tree %s", got, tree)
		}
	})
}

// Outside a work tree is a state; a git that cannot run is an error, because
// a state read from a failure would name a baseline the check never saw.
func TestReadReleaseBaselineOutsideARepositoryAndWithoutGit(t *testing.T) {
	if got := baselineOf(t, outsideRepository(t), apiPattern); got.State != ReleaseBaselineUnmanaged {
		t.Fatalf("baseline = %+v, want %s", got, ReleaseBaselineUnmanaged)
	}
	if _, err := ReadReleaseBaseline(filepath.Join(t.TempDir(), "missing"), apiPattern); err == nil {
		t.Fatal("a directory git cannot run in read as a baseline, want an error")
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := ReadReleaseBaseline(t.TempDir(), apiPattern); err == nil {
		t.Fatal("no git on PATH read as a baseline, want an error")
	}
}

// A branch and its squash merge hold the same tree and declare the same
// break, so they give the same baseline, whatever their HEAD commits are.
func TestReadReleaseBaselineIsTheSameForABranchAndItsSquash(t *testing.T) {
	t.Parallel()
	root, api := releasedAPIRepo(t)
	gitDo(t, root, "checkout", "-q", "-b", "feature")
	writeCommit(t, root, "api/a.go", "feat(api)!: drop the old call")
	writeCommit(t, root, "api/b.go", "fix(api): follow up")
	branch := baselineOf(t, api, apiPattern)
	branchHead := strings.TrimSpace(runGit(t, root, "rev-parse", "HEAD"))

	gitDo(t, root, "checkout", "-q", "main")
	gitDo(t, root, "merge", "-q", "--squash", "feature")
	gitDo(t, root, "commit", "-q", "-m", "feat(api)!: drop the old call (#12)")
	squash := baselineOf(t, api, apiPattern)
	squashHead := strings.TrimSpace(runGit(t, root, "rev-parse", "HEAD"))

	if branchHead == squashHead {
		t.Fatal("the branch and its squash share a HEAD commit; the test proves nothing")
	}
	if branch.Value() != squash.Value() {
		t.Fatalf("branch baseline %s != squash baseline %s", branch.Value(), squash.Value())
	}
	if !branch.Breaking {
		t.Fatalf("baseline = %+v, want the declared break", branch)
	}
	for _, head := range []string{branchHead, squashHead} {
		if strings.Contains(branch.Value(), head) || strings.Contains(branch.Value(), head[:12]) {
			t.Fatalf("baseline %s names HEAD %s", branch.Value(), head)
		}
	}
}

// Each input of the check's verdict moves the baseline: a new tag, other
// content at the tag, a newly declared break and a shallow clone. A break
// declared outside the project directory does not.
func TestReadReleaseBaselineMovesWithEachInput(t *testing.T) {
	t.Parallel()
	root, api := releasedAPIRepo(t)
	writeCommit(t, root, "api/b.go", "feat(api): add b")
	before := baselineOf(t, api, apiPattern)

	writeCommit(t, root, "web/a.go", "feat(web)!: break the web line")
	if got := baselineOf(t, api, apiPattern); got != before {
		t.Fatalf("a break outside api moved its baseline: %+v -> %+v", before, got)
	}

	writeCommitWithBody(t, root, "api/c.go", "refactor(api): rename", "BREAKING CHANGE: the old name is gone")
	declared := baselineOf(t, api, apiPattern)
	if !declared.Breaking || declared.Tag != before.Tag || declared.Object != before.Object {
		t.Fatalf("a declared break = %+v, want only Breaking to move from %+v", declared, before)
	}

	gitDo(t, root, "tag", "-a", "api/v0.2.0", "-m", "release")
	tagged := baselineOf(t, api, apiPattern)
	if tagged.Tag != "api/v0.2.0" || tagged.Object == before.Object || tagged.Breaking {
		t.Fatalf("a new tag = %+v, want the new tag, its tree and no break since it", tagged)
	}

	// The same tag name over other content at api.
	writeCommit(t, root, "api/d.go", "feat(api): add d")
	gitDo(t, root, "tag", "-f", "-a", "api/v0.2.0", "-m", "re-release")
	retagged := baselineOf(t, api, apiPattern)
	if retagged.Tag != tagged.Tag || retagged.Object == tagged.Object {
		t.Fatalf("a re-pointed tag = %+v, want the same name over another tree than %+v", retagged, tagged)
	}

	clone := filepath.Join(t.TempDir(), "clone")
	gitDo(t, root, "clone", "-q", "--depth", "1", "file://"+root, clone)
	if got := baselineOf(t, filepath.Join(clone, "api"), apiPattern); got.Value() == retagged.Value() {
		t.Fatalf("a shallow clone gave the full clone's baseline %s", got.Value())
	}
}

// A literal pathspec: a project directory whose name git would read as a
// pattern is listed as itself.
func TestReadReleaseBaselineReadsTheProjectPathLiterally(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("a directory name with * is not valid on Windows")
	}
	root := initGitRepo(t)
	writeCommit(t, root, "a*/x.go", "feat: star")
	writeCommit(t, root, "ab/x.go", "feat: plain")
	gitDo(t, root, "tag", "-a", "v0.1.0", "-m", "release")
	want := strings.TrimSpace(runGit(t, root, "rev-parse", "v0.1.0:a*"))
	if got := baselineOf(t, filepath.Join(root, "a*"), "v{version}"); got.Object != "tree "+want {
		t.Fatalf("baseline = %+v, want tree %s", got, want)
	}
}
