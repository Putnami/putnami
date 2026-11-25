package orchestration

import (
	"runtime"
	"testing"
	"time"

	"go.putnami.dev/tooling/cli/internal/cli/clitest"
)

// engineCPUGrowth is the largest ratio of the engine's CPU per task on a plan
// four times as large to that on the smaller plan. An engine whose work per
// task is the same whatever the plan reads below 1.4 on a 4-core Linux
// container, most often below 1.0; work repeated for each pending candidate
// grows with the plan and reads above 1.6.
const engineCPUGrowth = 1.5

// engineThreads is the GOMAXPROCS the test runs the engine with. A Putnami run
// sets GOMAXPROCS to a job's CPU budget, and the test binary inherits it; the
// engine's CPU and its growth depend on it.
const engineThreads = 4

// engineCPUPerTask is the engine's CPU per task recorded on the larger plan,
// with coverage on a 4-core Linux container. Hosts differ, so it is only a
// backstop: the test fails at four times it.
const engineCPUPerTask = 900 * time.Microsecond

// TestTheEngineCPUPerTask holds the CPU the engine process spends per task
// when the tasks themselves cost nothing: planning, dispatch, batching and the
// results of trivial batchable tasks, on plans of 300 and 1,200 tasks run in
// batch processes on four workers. It runs with --no-cache, since writing cache
// entries costs file system calls whose CPU depends on the disk, not on the
// engine. Each plan reads the least of three runs after a first one, with the
// engine on engineThreads threads.
//
// Work the scheduler repeats for each pending batchable candidate, such as
// building its whole job context to compute its batch key, adds no process and
// no git call: this test is what notices it, as CPU per task that grows with
// the plan.
func TestTheEngineCPUPerTask(t *testing.T) {
	clitest.RequireShell(t)
	clitest.UnsetHostRunEnvironment(t)
	if raceEnabled {
		t.Skip("the race detector multiplies the engine's CPU")
	}
	previous := runtime.GOMAXPROCS(engineThreads)
	t.Cleanup(func() { runtime.GOMAXPROCS(previous) })
	small := leastCPUPerTask(t, 100)
	large := leastCPUPerTask(t, 400)
	growth := float64(large) / float64(small)
	t.Logf("engine CPU per task: %s on 300 tasks, %s on 1,200 tasks, growth %.2f (recorded %s)",
		small, large, growth, engineCPUPerTask)
	if growth >= engineCPUGrowth {
		t.Errorf("engine CPU per task grows %.2f times from 300 to 1,200 tasks, at least %.1f: the engine repeats work for each pending task; %s",
			growth, engineCPUGrowth, countsHint)
	}
	if large >= 4*engineCPUPerTask {
		t.Errorf("engine CPU per task = %s on 1,200 tasks, at least four times the recorded %s; %s",
			large, engineCPUPerTask, countsHint)
	}
}

// leastCPUPerTask is the least engine CPU per task of three runs of a plan of
// projects projects, each with three batchable tasks, after a first run that
// writes the workspace's index and first state.
func leastCPUPerTask(t *testing.T, projects int) time.Duration {
	t.Helper()
	t.Setenv("PUTNAMI_STORE_DIR", t.TempDir())
	root := clitest.CountsFixture(t, projects, true)
	args := []string{"lint,test,build", "--all", "--no-cache", "--max-parallel", "4"}
	clitest.RunCounts(t, root, args...)
	least := time.Duration(0)
	for range 3 {
		run := clitest.RunCounts(t, root, args...)
		if !run.CPUMeasured {
			t.Skip("this platform measures no process CPU")
		}
		if run.Tasks != 3*projects {
			t.Fatalf("selected tasks = %d, want %d", run.Tasks, 3*projects)
		}
		if perTask := run.EngineCPU / time.Duration(run.Tasks); least == 0 || perTask < least {
			least = perTask
		}
	}
	return least
}
