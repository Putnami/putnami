package refstore

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/memory-store/internal/store"
)

const feature = "tooling/memory-store"

// isolated runs git without the person's or the machine's configuration.
func isolated(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("the Git backend's tests need git on PATH: %v", err)
	}
	global := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(global, []byte("[init]\n\tdefaultBranch = main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

func run(t *testing.T, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(), append([]string{"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid"}, env...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func record(id, content string) *store.Document {
	return &store.Document{
		Format: store.FormatVersion, ID: id, Kind: collab.MemoryKindNote, Content: content, Sequence: 1,
		Provenance: collab.Provenance{RecordedAt: "2026-09-24T08:00:00Z"}, UpdatedAt: "2026-09-24T08:00:00Z",
	}
}

func TestABranchWithRecordsAndNoIdentityIsNeverGivenANewOne(t *testing.T) {
	spectest.Proves(t, feature, "explicit-outage", "records-without-an-identity-are-never-given-a-new-one")
	isolated(t)
	path := filepath.Join(t.TempDir(), "memory.git")
	s := Open(Config{Path: path})
	if _, _, err := s.Update(context.Background(), "a", func(string, *store.Document) (*store.Document, error) {
		return record("a", "one"), nil
	}); err != nil {
		t.Fatal(err)
	}
	// Another tool commits the branch without the identity document.
	index := []string{"GIT_INDEX_FILE=" + filepath.Join(t.TempDir(), "index")}
	gitDir := "--git-dir=" + path
	blob := run(t, nil, gitDir, "rev-parse", "refs/heads/"+DefaultBranch+":"+store.RecordPath("a"))
	run(t, index, gitDir, "read-tree", "--empty")
	run(t, index, gitDir, "update-index", "--add", "--cacheinfo", "100644,"+blob+","+store.RecordPath("a"))
	tree := run(t, index, gitDir, "write-tree")
	commit := run(t, nil, gitDir, "commit-tree", "-p", "refs/heads/"+DefaultBranch, "-m", "drop the identity", tree)
	run(t, nil, gitDir, "update-ref", "refs/heads/"+DefaultBranch, commit)

	ran := false
	_, _, err := s.Update(context.Background(), "b", func(string, *store.Document) (*store.Document, error) {
		ran = true
		return record("b", "two"), nil
	})
	var failure *store.Error
	if !errors.As(err, &failure) || failure.Outcome != store.Unavailable || failure.Reason != "store.invalid" || ran {
		t.Fatalf("a write to a branch with records and no identity: %v (change ran: %v)", err, ran)
	}
	if head := run(t, nil, gitDir, "rev-parse", "refs/heads/"+DefaultBranch); head != commit {
		t.Fatalf("the refused write moved the branch to %s", head)
	}
}

func TestAWriteAnswersWithinItsDeadlines(t *testing.T) {
	spectest.Proves(t, feature, "retries-never-duplicate", "a-write-answers-before-the-tool-timeout")
	isolated(t)
	remote := filepath.Join(t.TempDir(), "remote.git")
	run(t, nil, "init", "--bare", "--quiet", remote)
	// The budgets leave the local commands of the attempt time to finish on
	// a loaded machine; without them the push alone waits 20 s, and its
	// reconciliation 20 s more.
	previous := [2]time.Duration{writeBudget, reconcileBudget}
	writeBudget, reconcileBudget = 5*time.Second, 2*time.Second
	t.Cleanup(func() { writeBudget, reconcileBudget = previous[0], previous[1] })

	// The push and every read of the remote after it hang until their
	// deadline: the write cannot land, and it cannot be reconciled.
	pushed := false
	hang := func(ctx context.Context) (Result, error) {
		<-ctx.Done()
		return Result{}, ctx.Err()
	}
	s := Open(Config{Path: filepath.Join(t.TempDir(), "local.git"), Remote: remote,
		Intercept: func(ctx context.Context, args []string, real func() (Result, error)) (Result, error) {
			switch {
			case args[0] == "push":
				pushed = true
				return hang(ctx)
			case pushed && (args[0] == "ls-remote" || args[0] == "fetch"):
				return hang(ctx)
			}
			return real()
		}})
	started := time.Now()
	_, _, err := s.Update(context.Background(), "a", func(string, *store.Document) (*store.Document, error) {
		return record("a", "one"), nil
	})
	elapsed := time.Since(started)
	var failure *store.Error
	if !errors.As(err, &failure) || failure.Outcome != store.Unresolved || !pushed {
		t.Fatalf("a write whose push and reconciliation hang: %v (pushed: %v)", err, pushed)
	}
	if elapsed > 15*time.Second {
		t.Fatalf("a write with deadlines of 5 s and 2 s answered after %s", elapsed)
	}
}
