package memory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/memory-store/internal/refstore"
)

// Git-specific behavior: what a remote write does when its answer is lost,
// and what it never touches.

const remoteBranch = "agents/memory"

// remoteHarness binds the Git backend to a bare remote in a fresh workspace.
func remoteHarness(t *testing.T) (*harness, string) {
	t.Helper()
	isolateGit(t)
	remote := filepath.Join(t.TempDir(), "memory.git")
	gitCommand(t, filepath.Dir(remote), "init", "--bare", "--quiet", remote)
	return remoteHarnessOn(t, remote), remote
}

func remoteHarnessOn(t *testing.T, remote string) *harness {
	t.Helper()
	return newHarness(t, t.TempDir(), `{"backend":"git","remote":`+mustJSON(t, remote)+`,"branch":"`+remoteBranch+`"}`)
}

// commits counts the commits of the memory branch in a repository.
func commits(t *testing.T, gitDir string) int {
	t.Helper()
	n, err := strconv.Atoi(gitCommand(t, gitDir, "--git-dir="+gitDir, "rev-list", "--count", "refs/heads/"+remoteBranch))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// losePushAnswers makes every push of h run and then fail as if the
// connection dropped before the remote's answer arrived. after runs between
// the push and the failure.
func losePushAnswers(h *harness, after func()) {
	h.provider.gitIntercept = func(_ context.Context, args []string, run func() (refstore.Result, error)) (refstore.Result, error) {
		if args[0] != "push" {
			return run()
		}
		if _, err := run(); err != nil {
			return refstore.Result{}, err
		}
		if after != nil {
			after()
		}
		return refstore.Result{}, errors.New("connection reset by peer")
	}
}

// TestAWriterThatLostTheRaceIsRebuiltOrRefused lets another writer land
// between a checkpoint's read and its write: the ref update or push is
// conditional on the head the checkpoint read, so it is rebuilt on the new
// head when the other writer changed another mission, and refused when it
// changed the same one. An unconditional update would overwrite either.
func TestAWriterThatLostTheRaceIsRebuiltOrRefused(t *testing.T) {
	spectest.Proves(t, feature, "compare-and-set-checkpoints", "concurrent-checkpoints-at-one-revision-land-once")
	for _, mode := range []struct{ name, write string }{{"git", "update-ref"}, {"git-remote", "push"}} {
		t.Run(mode.name, func(t *testing.T) {
			var h *harness
			if mode.name == "git" {
				isolateGit(t)
				h = newHarness(t, t.TempDir(), `{"backend":"git"}`)
			} else {
				h, _ = remoteHarness(t)
			}
			other := h.fresh()
			interleave := func(write func()) {
				done := false
				h.provider.gitIntercept = func(_ context.Context, args []string, run func() (refstore.Result, error)) (refstore.Result, error) {
					if args[0] == mode.write && !done {
						done = true
						write()
					}
					return run()
				}
			}
			base := h.save(checkpoint("m", "k1", "one", "")).Record

			interleave(func() { other.save(checkpoint("bystander", "b1", "another mission", "")) })
			rebuilt := h.save(checkpoint("m", "k2", "two", base.Revision)).Record
			if sequenceOf(t, rebuilt.Revision) != 2 {
				t.Fatalf("the rebuilt write answered %+v", rebuilt)
			}
			if bystander := other.mission("bystander"); bystander.Content != "another mission" {
				t.Fatalf("the other writer's checkpoint was overwritten: %+v", bystander)
			}

			var theirs collab.MemoryRecord
			interleave(func() { theirs = other.save(checkpoint("m", "k3", "three", rebuilt.Revision)).Record })
			failure := h.fails(collab.OperationCheckpoint, checkpoint("m", "k4", "four", rebuilt.Revision), collab.OutcomeConflict)
			if failure.Current != theirs.Revision {
				t.Fatalf("the loser saw %+v, the winner wrote %s", failure, theirs.Revision)
			}
			h.provider.gitIntercept = nil
			if current := h.mission("m"); current.Content != "three" || other.mission("bystander").Content != "another mission" {
				t.Fatalf("the store holds %+v", current)
			}
		})
	}
}

func TestAnUncertainPushThatLandedIsReconciled(t *testing.T) {
	spectest.Proves(t, feature, "retries-never-duplicate", "an-uncertain-push-is-reconciled")
	h, remote := remoteHarness(t)
	first := h.save(checkpoint("m", "k1", "one", "")).Record
	losePushAnswers(h, nil)
	landed := h.save(checkpoint("m", "k2", "two", first.Revision))
	if landed.Replayed || sequenceOf(t, landed.Record.Revision) != 2 {
		t.Fatalf("the reconciled write answered %+v", landed)
	}
	if n := commits(t, remote); n != 2 {
		t.Fatalf("the remote holds %d commits, want 2", n)
	}
	if current := remoteHarnessOn(t, remote).mission("m"); current.Revision != landed.Record.Revision {
		t.Fatalf("another clone reads %+v", current)
	}
}

func TestAnUnreconcilablePushIsUnresolvedAndItsRepeatReplays(t *testing.T) {
	spectest.Proves(t, feature, "retries-never-duplicate", "an-unreconcilable-push-is-unresolved")
	h, remote := remoteHarness(t)
	first := h.save(checkpoint("m", "k1", "one", "")).Record
	aside := remote + ".aside"
	losePushAnswers(h, func() { mustDo(t, os.Rename(remote, aside)) })
	request := checkpoint("m", "k2", "two", first.Revision)
	failure := h.fails(collab.OperationCheckpoint, request, collab.OutcomeUnresolved)
	if failure.Retryable || failure.Reason != "store.unreconciled" || !strings.Contains(failure.Reconcile, "memory.mission") {
		t.Fatalf("unresolved %+v", failure)
	}
	mustDo(t, os.Rename(aside, remote))
	h.provider.gitIntercept = nil

	// The write had landed: the repeat the reconciliation names replays it.
	replay := h.save(request)
	if !replay.Replayed || sequenceOf(t, replay.Record.Revision) != 2 || replay.Record.Content != "two" {
		t.Fatalf("the repeat answered %+v", replay)
	}
	if n := commits(t, remote); n != 2 {
		t.Fatalf("the remote holds %d commits, want 2: the repeat wrote again", n)
	}
}

func TestAPushThatDidNotLandIsUnavailable(t *testing.T) {
	h, remote := remoteHarness(t)
	first := h.save(checkpoint("m", "k1", "one", "")).Record
	h.provider.gitIntercept = func(_ context.Context, args []string, run func() (refstore.Result, error)) (refstore.Result, error) {
		if args[0] == "push" {
			return refstore.Result{}, errors.New("connection refused")
		}
		return run()
	}
	failure := h.fails(collab.OperationCheckpoint, checkpoint("m", "k2", "two", first.Revision), collab.OutcomeUnavailable)
	if !failure.Retryable || !strings.Contains(failure.Message, "did not land") {
		t.Fatalf("a push that never reached the remote answered %+v", failure)
	}
	h.provider.gitIntercept = nil
	if after := h.save(checkpoint("m", "k2", "two", first.Revision)); after.Replayed || sequenceOf(t, after.Record.Revision) != 2 {
		t.Fatalf("the retry answered %+v", after)
	}
	if n := commits(t, remote); n != 2 {
		t.Fatalf("the remote holds %d commits, want 2", n)
	}
}

func TestARemoteThatRefusesTheWriteIsDenied(t *testing.T) {
	h, remote := remoteHarness(t)
	first := h.save(checkpoint("m", "k1", "one", "")).Record
	hook := filepath.Join(remote, "hooks", "pre-receive")
	mustDo(t, os.WriteFile(hook, []byte("#!/bin/sh\necho 'memory is read-only' >&2\nexit 1\n"), 0o755))
	failure := h.fails(collab.OperationCheckpoint, checkpoint("m", "k2", "two", first.Revision), collab.OutcomeDenied)
	if failure.Retryable || failure.Reason != "store.refused" {
		t.Fatalf("a refused push answered %+v", failure)
	}
	if n := commits(t, remote); n != 1 {
		t.Fatalf("the remote holds %d commits, want 1", n)
	}
}

func TestClonesShareOneRemoteStore(t *testing.T) {
	spectest.Proves(t, feature, "one-contract-every-backend", "read-checkpoint-resume")
	one, remote := remoteHarness(t)
	two := remoteHarnessOn(t, remote)
	written := one.save(checkpoint("m", "k1", "from one", "")).Record
	read := two.mission("m")
	if read.Revision != written.Revision || read.Ref != written.Ref {
		t.Fatalf("the second clone reads %+v, the first wrote %+v", read, written)
	}
	next := two.save(checkpoint("m", "k2", "from two", read.Revision)).Record
	failure := one.fails(collab.OperationCheckpoint, checkpoint("m", "k3", "stale", written.Revision), collab.OutcomeConflict)
	if failure.Current != next.Revision {
		t.Fatalf("the first clone's stale write answered %+v", failure)
	}
}

func TestTheGitBackendTouchesNoHookCheckoutOrWorkingTree(t *testing.T) {
	spectest.Proves(t, feature, "portable-git-writes", "hooks-checkouts-and-working-trees-are-never-touched")
	isolateGit(t)
	repository := t.TempDir()
	gitCommand(t, repository, "init", "--quiet")
	mustDo(t, os.WriteFile(filepath.Join(repository, "README.md"), []byte("code\n"), 0o644))
	gitCommand(t, repository, "add", "README.md")
	gitCommand(t, repository, "commit", "--quiet", "-m", "code")
	for _, name := range []string{"reference-transaction", "pre-commit", "post-commit", "pre-push"} {
		mustDo(t, os.WriteFile(filepath.Join(repository, ".git", "hooks", name), []byte("#!/bin/sh\nexit 1\n"), 0o755))
	}
	head := gitCommand(t, repository, "rev-parse", "HEAD")

	h := newHarness(t, t.TempDir(), `{"backend":"git","path":`+mustJSON(t, repository)+`}`)
	record := h.save(checkpoint("m", "k1", "one", "")).Record
	if got := gitCommand(t, repository, "rev-parse", "HEAD"); got != head {
		t.Fatalf("HEAD moved from %s to %s", head, got)
	}
	if status := gitCommand(t, repository, "status", "--porcelain"); status != "" {
		t.Fatalf("the working tree changed:\n%s", status)
	}
	stored := gitCommand(t, repository, "show", refstore.DefaultBranch+":records/"+record.Ref.ID+".json")
	if !strings.Contains(stored, `"content": "one"`) {
		t.Fatalf("the branch holds %s", stored)
	}

	checkedOut := newHarness(t, t.TempDir(), `{"backend":"git","path":`+mustJSON(t, repository)+`,"branch":"main"}`)
	failure := checkedOut.fails(collab.OperationCheckpoint, checkpoint("m", "k1", "one", ""), collab.OutcomeInvalid)
	if !strings.Contains(failure.Message, "checked out") {
		t.Fatalf("a checked-out branch answered %+v", failure)
	}
}

func TestAPathThatIsNotARepositoryIsRefused(t *testing.T) {
	isolateGit(t)
	path := t.TempDir()
	mustDo(t, os.WriteFile(filepath.Join(path, "notes.txt"), []byte("mine"), 0o644))
	h := newHarness(t, t.TempDir(), `{"backend":"git","path":`+mustJSON(t, path)+`}`)
	h.fails(collab.OperationContext, map[string]any{}, collab.OutcomeInvalid)
	h.fails(collab.OperationCheckpoint, checkpoint("m", "k", "c", ""), collab.OutcomeInvalid)

	file := filepath.Join(t.TempDir(), "file")
	mustDo(t, os.WriteFile(file, nil, 0o644))
	h = newHarness(t, t.TempDir(), `{"backend":"git","path":`+mustJSON(t, file)+`}`)
	h.fails(collab.OperationMission, map[string]any{"mission": "m"}, collab.OutcomeInvalid)

	// An empty directory becomes the repository.
	empty := t.TempDir()
	h = newHarness(t, t.TempDir(), `{"backend":"git","path":`+mustJSON(t, empty)+`}`)
	var listed collab.MemoryListResult
	h.ok(collab.OperationContext, map[string]any{}, &listed)
	h.save(checkpoint("m", "k", "c", ""))
	if _, err := os.Stat(filepath.Join(empty, "HEAD")); err != nil {
		t.Fatalf("the empty directory did not become a repository: %v", err)
	}
}
