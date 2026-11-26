package orchestration

import (
	"testing"

	"go.putnami.dev/tooling/cli/internal/cli/clitest"
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
		{"engine git calls on 6 tasks", total(cold.EngineGit), 16},
		{"engine git calls on 15 tasks", total(coldLarge.EngineGit), 16},
		{"engine git calls added by 9 more jobs", total(coldLarge.EngineGit) - total(cold.EngineGit), 0},
		{"job processes on 15 selected tasks", coldLarge.Engine["job"], int64(coldLarge.Tasks)},
	} {
		if check.got > check.ceiling {
			t.Errorf("%s = %d, above its ceiling %d\n%s", check.name, check.got, check.ceiling, countsHint)
		}
	}
	holdCeiling(t, "a run of 15 trivial tasks", coldLarge.Ledger(),
		"engine job 15 · git 16 | engine git diff 1 · for-each-ref 1 · ls-files 3 · rev-parse 6 · show 1 · status 2 · symbolic-ref 1 · tag 1 | jobs none | jobs git none")
	holdCeiling(t, "a run of the unchanged workspace, every task in the cache", unchanged.Ledger(),
		"engine git 16 | engine git diff 1 · for-each-ref 1 · ls-files 3 · rev-parse 6 · show 1 · status 2 · symbolic-ref 1 · tag 1 | jobs none | jobs git none")
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
// 0106).
func TestAWriteUnderAnIgnoredPutnamiListsNoCandidateAgain(t *testing.T) {
	clitest.RequireShell(t)
	clitest.UnsetHostRunEnvironment(t)
	hometest.Temp(t)
	root := clitest.WritingCountsFixture(t, 3)
	clitest.ForgetTaskHistory(t, root)
	cold := clitest.RunCounts(t, root, "lint,test,build", "--all", "--max-parallel", "1", "--no-cache")
	holdCeiling(t, "a run of 9 tasks that write under the ignored .putnami", cold.Ledger(),
		"engine job 9 · git 26 | engine git check-ignore 1 · for-each-ref 1 · ls-files 10 · rev-parse 9 · show 1 · status 2 · symbolic-ref 1 · tag 1 | jobs none | jobs git none")
}
