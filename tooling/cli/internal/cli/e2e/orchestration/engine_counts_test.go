package orchestration

import (
	"path/filepath"
	"testing"
	"time"

	"go.putnami.dev/tooling/cli/internal/cli/clitest"
	"go.putnami.dev/tooling/cli/internal/fileindex"
	putnamigit "go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/hometest"
)

// TestTheEngineCounts holds under their ceilings what the engine starts on a
// workspace whose jobs start nothing: at most one job process per selected
// task, the git calls of a run whatever its number of jobs, what a run of the
// unchanged workspace starts, and the processes planning it starts.
func TestTheEngineCounts(t *testing.T) {
	clitest.RequireShell(t)
	clitest.UnsetHostRunEnvironment(t)

	// Two sizes, so the git calls per job are the difference between them.
	// Each starts from an empty store, so its first run executes every task
	// and its second finds every one in the cache. The store is the default
	// one, under a home of its own, so a run resolves its location from the
	// repository as on any machine.
	hometest.Temp(t)
	cold := clitest.RunCounts(t, clitest.CountsFixture(t, 2, false), "lint,test,build", "--all")
	hometest.Temp(t)
	large := clitest.CountsFixture(t, 5, false)
	coldLarge := clitest.RunCounts(t, large, "lint,test,build", "--all")
	clitest.ForgetTaskHistory(t, large)
	unchanged := clitest.RunCounts(t, large, "lint,test,build", "--all")
	clitest.ForgetTaskHistory(t, large)
	plan := clitest.PlanCounts(t, large, "lint,test,build", "--all")

	if cold.Tasks != 6 || coldLarge.Tasks != 15 || unchanged.Tasks != 15 {
		t.Fatalf("selected tasks = %d, %d, %d, want 6, 15, 15", cold.Tasks, coldLarge.Tasks, unchanged.Tasks)
	}
	// The git calls of 6 and of 15 tasks: their difference is what 9 more jobs
	// cost the engine in git calls, held whole so that no division rounds a
	// cost per job down to 0.
	for _, check := range []struct {
		name         string
		got, ceiling int64
	}{
		{"engine git calls on 6 tasks", total(cold.EngineGit), 15},
		{"engine git calls on 15 tasks", total(coldLarge.EngineGit), 15},
		{"engine git calls added by 9 more jobs", total(coldLarge.EngineGit) - total(cold.EngineGit), 0},
		{"job processes on 15 selected tasks", coldLarge.Engine["job"], int64(coldLarge.Tasks)},
	} {
		if check.got > check.ceiling {
			t.Errorf("%s = %d, above its ceiling %d\n%s", check.name, check.got, check.ceiling, countsHint)
		}
	}
	// The candidate listings are read in-process; the one config call reads
	// Git's configuration for them (tooling/cli ADR 0112).
	holdCeiling(t, "a run of 15 trivial tasks", coldLarge.Ledger(),
		"engine job 15 · git 15 | engine git config 1 · diff 1 · for-each-ref 1 · ls-files 1 · rev-parse 6 · show 1 · status 2 · symbolic-ref 1 · tag 1 | jobs none | jobs git none")
	holdCeiling(t, "a run of the unchanged workspace, every task in the cache", unchanged.Ledger(),
		"engine git 15 | engine git config 1 · diff 1 · for-each-ref 1 · ls-files 1 · rev-parse 6 · show 1 · status 2 · symbolic-ref 1 · tag 1 | jobs none | jobs git none")
	holdCeiling(t, "planning the unchanged workspace", plan.Ledger(),
		"engine git 10 | engine git diff 1 · for-each-ref 1 · rev-parse 4 · show 1 · status 1 · symbolic-ref 1 · tag 1 | jobs none | jobs git none")
}

func total(counts map[string]int64) int64 {
	var sum int64
	for _, n := range counts {
		sum += n
	}
	return sum
}

// TestAWriteUnderAnIgnoredPutnamiListsNoCandidateAgain holds the git calls of
// a run whose tasks key on their project's Git candidates and each write a
// report under the workspace's .putnami, which Git ignores: each project's
// candidates are listed once, by its first task, whatever the tasks after it
// wrote. The run asks Git once whether .putnami is excluded, and reads the
// workspace's cut once to see that it lists nothing below it (tooling/cli ADR
// 0106). The listings are read in-process (ADR 0112), and counted so.
func TestAWriteUnderAnIgnoredPutnamiListsNoCandidateAgain(t *testing.T) {
	clitest.RequireShell(t)
	clitest.UnsetHostRunEnvironment(t)
	hometest.Temp(t)
	root := clitest.WritingCountsFixture(t, 3)
	clitest.ForgetTaskHistory(t, root)
	reads := putnamigit.InProcessCutReads()
	cold := clitest.RunCounts(t, root, "lint,test,build", "--all", "--max-parallel", "1", "--no-cache")
	// The listings are read in-process (tooling/cli ADR 0112): one per
	// project's candidates, one of the workspace's cut, one of the source
	// binding snapshot, whatever the tasks wrote under .putnami, and one for
	// the witness of the work tree's Git answers the run keeps (ADR 0136).
	if reads = putnamigit.InProcessCutReads() - reads; reads > 6 {
		t.Errorf("in-process reads of the cut = %d, above its ceiling 6\n%s", reads, countsHint)
	}
	holdCeiling(t, "a run of 9 tasks that write under the ignored .putnami", cold.Ledger(),
		"engine job 9 · git 15 | engine git check-ignore 1 · config 1 · for-each-ref 1 · ls-files 1 · rev-parse 6 · show 1 · status 2 · symbolic-ref 1 · tag 1 | jobs none | jobs git none")
}

// TestAnUnchangedRunStartsNoGitProcess holds that a run of an unchanged
// workspace starts no git process: the Git reads an earlier run made are
// answered from the input record while the files they read keep their stats
// (tooling/cli ADR 0136). The runs before it leave the margin of the record's
// clock between the fixture's writes and the run that records them.
func TestAnUnchangedRunStartsNoGitProcess(t *testing.T) {
	clitest.RequireShell(t)
	clitest.UnsetHostRunEnvironment(t)
	hometest.Temp(t)
	root := clitest.CountsFixture(t, 3, false)
	// A workspace's .putnami holds what its runs write, which Git ignores: a
	// run that wrote in the work tree would change what Git answers.
	clitest.WriteFile(t, filepath.Join(root, ".gitignore"), ".putnami/\n")
	clitest.RunGit(t, root, "add", ".gitignore")
	clitest.RunGit(t, root, "commit", "-m", "ignore .putnami")
	// RunCounts antedates HEAD before each run, which moves its stat; these
	// runs leave the repository as they find it.
	run := func() clitest.Counts {
		code, out := clitest.RunGateArgs(t, root, "lint,test,build", "--all", "--output=json")
		if code != 0 {
			t.Fatalf("putnami lint,test,build --all exit %d:\n%s", code, out)
		}
		return clitest.ParseCounts(t, out)
	}
	// The first run executes every task, and its Git calls refresh the index;
	// the next records what the margin of the record's clock left out.
	for range 3 {
		run()
		time.Sleep(fileindex.Margin + 100*time.Millisecond)
	}
	unchanged := run()
	if calls := total(unchanged.EngineGit); calls != 0 {
		t.Errorf("a run of the unchanged workspace made %d engine git call(s), want none\n%s\n%s", calls, unchanged.Ledger(), countsHint)
	}
}
