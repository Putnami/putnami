package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	runner "go.putnami.dev/protocol/runner"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/git"
)

// The ancestry snapshot tests run git in real repositories and set
// PUTNAMI_SOURCE_REVISION, so none of them is parallel.

// ancestryRepo is a repository on main with two commits, first and head, and
// an orphan commit that no ref reaches.
func ancestryRepo(t *testing.T) (root, first, head, orphan string) {
	t.Helper()
	root = t.TempDir()
	initCLISelectionGitRepo(t, root)
	first = gitLine(t, root, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(root, "second.txt"), []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCLISelectionGit(t, root, "add", "-A")
	runCLISelectionGit(t, root, "commit", "-q", "-m", "second")
	head = gitLine(t, root, "rev-parse", "HEAD")
	orphan = gitLine(t, root, "commit-tree", gitLine(t, root, "rev-parse", "HEAD^{tree}"), "-m", "orphan")
	return root, first, head, orphan
}

// gitLine runs git in dir and returns its trimmed output.
func gitLine(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return strings.TrimSpace(runCLISelectionGit(t, dir, args...))
}

// assertAncestry fails t unless snapshot is bound to revision, read whole,
// and contains exactly the commits in, none of out.
func assertAncestry(t *testing.T, snapshot *AncestrySnapshot, revision string, in, out []string) {
	t.Helper()
	if snapshot == nil || snapshot.Err() != nil {
		t.Fatalf("snapshot = %+v, err %v; want one read whole", snapshot, snapshot.Err())
	}
	if snapshot.SourceRevision() != revision {
		t.Fatalf("snapshot bound to %q, want %q", snapshot.SourceRevision(), revision)
	}
	if snapshot.Commits() != len(in) {
		t.Errorf("snapshot holds %d commits, want %d", snapshot.Commits(), len(in))
	}
	for _, commit := range in {
		if !snapshot.Contains(commit) {
			t.Errorf("snapshot does not contain %s", commit)
		}
	}
	for _, commit := range out {
		if snapshot.Contains(commit) {
			t.Errorf("snapshot contains %s, which the bound commit does not reach", commit)
		}
	}
}

// runQuietly runs req through Engine.Run with the terminal streams captured.
// The fixture roots are no workspaces, so the run stops after its first stage.
func runQuietly(t *testing.T, req Request) SessionResult {
	t.Helper()
	return runQuietlyIn(t, context.Background(), req)
}

// runQuietlyIn is runQuietly in ctx.
func runQuietlyIn(t *testing.T, ctx context.Context, req Request) SessionResult {
	t.Helper()
	var result SessionResult
	_ = captureStderr(t, func() {
		_ = captureStdout(t, func() {
			result, _ = New().Run(ctx, req, nil)
		})
	})
	return result
}

// An adapter reads the ancestry at process start, before its first-use
// bootstrap or an install runs repository code, and the run reuses that
// snapshot. Here the install grafts an orphan under HEAD and moves HEAD
// between the capture and the run: the run still answers for the commit the
// process started on, in the order git lists its history.
func TestAncestryIsCapturedBeforeTheInstallRunsRepositoryCode(t *testing.T) {
	spectest.Proves(t, "cli/provider-publication", "ancestry-is-read-before-repository-code", "the-snapshot-precedes-the-install")
	t.Setenv(git.SourceRevisionEnv, "")
	root, first, head, orphan := ancestryRepo(t)
	ctx := CaptureAncestry(context.Background(), root, []string{"build", "publish"}, nil)

	runCLISelectionGit(t, root, "replace", "--graft", "HEAD", orphan)
	runCLISelectionGit(t, root, "commit", "-q", "--allow-empty", "-m", "installed")
	installed := gitLine(t, root, "rev-parse", "HEAD")

	result := runQuietlyIn(t, ctx, Request{WorkspaceRoot: root, Config: &wsproto.Config{}, Commands: []string{"build", "publish"}})
	assertAncestry(t, result.ancestry, head, []string{head, first}, []string{orphan, installed})
	if result.ancestry != capturedAncestry(ctx, root) {
		t.Fatal("the run read its own snapshot instead of the one taken at process start")
	}
	for commit, want := range map[string]int{head: 0, first: 1} {
		if position, held := result.ancestry.Position(commit); !held || position != want {
			t.Errorf("position of %s = %d (%v), want %d", commit, position, held, want)
		}
	}
	if _, held := result.ancestry.Position(orphan); held {
		t.Error("the snapshot positions a commit the bound commit does not reach")
	}

	if capturedAncestry(ctx, t.TempDir()) != nil {
		t.Fatal("a snapshot of one workspace answers for another")
	}
	if capturedAncestry(context.Background(), root) != nil {
		t.Fatal("a context without a capture carries a snapshot")
	}
	gate := []string{"lint", "test", "build"}
	if capturedAncestry(CaptureAncestry(context.Background(), root, gate, nil), root) != nil {
		t.Fatal("a gate-only invocation read the ancestry")
	}
	bound := &runner.InvocationBlock{Commands: gate, Publication: &runner.PublicationBlock{Barrier: []string{"test"}}}
	if capturedAncestry(CaptureAncestry(context.Background(), root, gate, bound), root) == nil {
		t.Fatal("a bound request carrying invocation.publication read no ancestry")
	}
}

// A before hook is repository code: here it grafts an orphan under HEAD and
// moves HEAD. The snapshot the run read before it still answers for the
// commit the run started on, with the history its commit objects record.
func TestAncestrySnapshotIsTakenBeforeTheFirstHook(t *testing.T) {
	spectest.Proves(t, "cli/provider-publication", "ancestry-is-read-before-repository-code", "the-snapshot-precedes-the-first-hook")
	requireSh(t)
	t.Setenv(git.SourceRevisionEnv, "")
	root, first, head, orphan := ancestryRepo(t)
	hook := "git replace --graft HEAD " + orphan + " && git commit -q --allow-empty -m moved"

	result := runQuietly(t, Request{
		WorkspaceRoot: root, Config: &wsproto.Config{}, Commands: []string{"publish"},
		Hooks: &wsproto.HooksConfig{CLI: &wsproto.HookPhaseConfig{Before: []string{hook}}},
	})

	// The hook ran, and git now answers the other way.
	if moved := gitLine(t, root, "rev-parse", "HEAD"); moved == head {
		t.Fatal("the before hook did not move HEAD")
	}
	runCLISelectionGit(t, root, "merge-base", "--is-ancestor", orphan, head)
	assertAncestry(t, result.ancestry, head, []string{head, first}, []string{orphan})
}

// Replace refs and a graft file present when the snapshot is read cannot add
// a parent to it either.
func TestAncestrySnapshotIgnoresReplaceRefs(t *testing.T) {
	spectest.Proves(t, "cli/provider-publication", "ancestry-is-read-before-repository-code", "replace-refs-and-grafts-change-no-answer")
	t.Setenv(git.SourceRevisionEnv, "")
	root, first, head, orphan := ancestryRepo(t)

	runCLISelectionGit(t, root, "replace", "--graft", head, orphan)
	runCLISelectionGit(t, root, "merge-base", "--is-ancestor", orphan, head)
	assertAncestry(t, captureAncestrySnapshot(root, MaxAncestryCommits), head, []string{head, first}, []string{orphan})

	runCLISelectionGit(t, root, "replace", "-d", head)
	grafts := filepath.Join(gitLine(t, root, "rev-parse", "--absolute-git-dir"), "info", "grafts")
	if err := os.MkdirAll(filepath.Dir(grafts), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(grafts, []byte(head+" "+orphan+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(runCLISelectionGit(t, root, "rev-list", head), orphan) {
		t.Skip("this git ignores info/grafts, so the graft file grafts nothing to ignore")
	}
	assertAncestry(t, captureAncestrySnapshot(root, MaxAncestryCommits), head, []string{head, first}, []string{orphan})
}

// A shallow clone answers with the commits it has and says it is shallow.
func TestAncestrySnapshotNamesAShallowClone(t *testing.T) {
	spectest.Proves(t, "cli/provider-publication", "ancestry-is-read-before-repository-code", "a-shallow-clone-is-named")
	t.Setenv(git.SourceRevisionEnv, "")
	root, first, head, _ := ancestryRepo(t)
	if full := captureAncestrySnapshot(root, MaxAncestryCommits); full.Shallow() {
		t.Fatal("a full clone reads as shallow")
	}

	source := filepath.ToSlash(root)
	if !strings.HasPrefix(source, "/") {
		source = "/" + source
	}
	clone := filepath.Join(t.TempDir(), "clone")
	runCLISelectionGit(t, root, "clone", "-q", "--depth", "1", "file://"+source, clone)
	shallow := captureAncestrySnapshot(clone, MaxAncestryCommits)
	if !shallow.Shallow() {
		t.Fatal("a depth-1 clone does not read as shallow")
	}
	assertAncestry(t, shallow, head, []string{head}, []string{first})
}

// Above its limit the snapshot holds a named error and no commit, so every
// check against it fails closed. It binds to PUTNAMI_SOURCE_REVISION when a
// runner sets it, and a bound commit the repository lacks contains nothing.
func TestAncestrySnapshotHoldsNothingAboveTheLimit(t *testing.T) {
	spectest.Proves(t, "cli/provider-publication", "ancestry-is-read-before-repository-code", "a-history-above-the-limit-contains-nothing")
	t.Setenv(git.SourceRevisionEnv, "")
	root, first, head, _ := ancestryRepo(t)

	capped := captureAncestrySnapshot(root, 1)
	if !errors.Is(capped.Err(), ErrAncestryLimit) || capped.Commits() != 0 || capped.Contains(head) {
		t.Fatalf("capped snapshot = %d commits, contains head %v, err %v; want ErrAncestryLimit and none",
			capped.Commits(), capped.Contains(head), capped.Err())
	}
	assertAncestry(t, captureAncestrySnapshot(root, 2), head, []string{head, first}, nil)

	t.Setenv(git.SourceRevisionEnv, first)
	assertAncestry(t, captureAncestrySnapshot(root, MaxAncestryCommits), first, []string{first}, []string{head})

	t.Setenv(git.SourceRevisionEnv, unboundCommit)
	t.Setenv(git.SourceCommitTimeEnv, "1767323045")
	missing := captureAncestrySnapshot(root, MaxAncestryCommits)
	if missing.Err() == nil || missing.Contains(unboundCommit) || missing.Commits() != 0 {
		t.Fatalf("a bound commit the repository lacks = %d commits, err %v; want an error and none", missing.Commits(), missing.Err())
	}

	var none *AncestrySnapshot
	if none.Contains(head) || none.Commits() != 0 || none.Shallow() || none.SourceRevision() != "" || none.Err() != nil {
		t.Fatal("a nil snapshot answers")
	}
}

// Only a run that may publish reads the ancestry: one that names publish or
// deploy, or a bound request carrying invocation.publication.
func TestAncestrySnapshotIsNotTakenForAGateOnlyRun(t *testing.T) {
	spectest.Proves(t, "cli/provider-publication", "ancestry-is-read-before-repository-code", "a-gate-only-run-reads-no-ancestry")
	t.Setenv(git.SourceRevisionEnv, "")
	root, _, head, _ := ancestryRepo(t)
	gate := []string{"lint", "test", "build", "validate"}
	bound := func(publication *runner.PublicationBlock) *PortableExecution {
		return &PortableExecution{Request: runner.ExecutionRequest{
			Invocation: runner.InvocationBlock{Commands: gate, Publication: publication},
		}}
	}
	commit := func(publication *runner.PublicationBlock) *PortableExecution {
		return &PortableExecution{Commit: &runner.CommitRequest{
			Source:     runner.CommitSource{Commit: head},
			Invocation: runner.InvocationBlock{Commands: gate, Publication: publication},
			Selection:  runner.RequestedSelection{Mode: runner.SelectionModeAll},
		}}
	}
	for _, tc := range []struct {
		name     string
		commands []string
		portable *PortableExecution
		reads    bool
	}{
		{"gate", gate, nil, false},
		{"bound gate without the block", gate, bound(nil), false},
		{"publish", []string{"build", "publish"}, nil, true},
		{"deploy", []string{"deploy"}, nil, true},
		{"bound request with the block", gate, bound(&runner.PublicationBlock{Barrier: []string{"test"}}), true},
		{"commit request without the block", gate, commit(nil), false},
		{"commit request with the block", gate, commit(&runner.PublicationBlock{Barrier: []string{"test"}}), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := runQuietly(t, Request{WorkspaceRoot: root, Config: &wsproto.Config{}, Commands: tc.commands, Portable: tc.portable})
			if got := result.ancestry != nil; got != tc.reads {
				t.Fatalf("read the ancestry = %v, want %v", got, tc.reads)
			}
			if tc.reads && result.ancestry.SourceRevision() != head {
				t.Fatalf("snapshot bound to %q, want %q", result.ancestry.SourceRevision(), head)
			}
		})
	}
	// A watch iteration follows repository code its session already ran.
	if readsAncestry(&Request{Commands: []string{"publish"}, watchIteration: true}) {
		t.Fatal("a watch iteration reads the ancestry")
	}
}
