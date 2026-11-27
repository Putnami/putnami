// Package machineproc_test checks CLI ADR 0116 §8: a run through the machine
// process gives the plan, keys, outcomes and exit code a run in its own
// process gives. The test binary plays the CLI: TestMain answers the
// supervisor's and the worker's own commands, which the client starts from
// os.Executable.
package machineproc_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	features "go.putnami.dev/protocol/features"
	"go.putnami.dev/tooling/cli/internal/cli"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
	"go.putnami.dev/tooling/cli/internal/engine"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/fileindex"
	"go.putnami.dev/tooling/cli/internal/jobs/scheduler"
	"go.putnami.dev/tooling/cli/internal/machineproc"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func TestMain(m *testing.M) {
	clitest.AnswerRegistryTokenSeam()
	if len(os.Args) == 3 && os.Args[1] == "__machine" {
		switch os.Args[2] {
		case "supervise":
			os.Exit(machineproc.Supervise())
		case "work":
			engine.ServeHeldRuns(machineproc.RunMemory{})
			scheduler.ServeHeldRuns(machineproc.RunMemory{})
			os.Exit(machineproc.Work(func(ctx context.Context, args []string) int {
				app, err := cli.NewApp()
				if err != nil {
					return cli.ExitError
				}
				workspace.ForgetProcessMemos()
				extension.ForgetProcessMemos()
				return app.Run(ctx, args)
			}))
		}
	}
	os.Exit(clitest.Main(m))
}

// taskResult is what the comparison holds equal for one task.
type taskResult struct {
	Identity    json.RawMessage `json:"identity"`
	InputDigest string          `json:"inputDigest"`
	Status      string          `json:"status"`
	ExitCode    *int            `json:"exitCode"`
}

func (r taskResult) String() string {
	code := "-"
	if r.ExitCode != nil {
		code = fmt.Sprint(*r.ExitCode)
	}
	return fmt.Sprintf("%s %s %s %s", strings.Join(strings.Fields(string(r.Identity)), ""), r.InputDigest, r.Status, code)
}

// latestTasks reads the task results of the latest session of wsRoot, sorted.
func latestTasks(t *testing.T, wsRoot string) []string {
	t.Helper()
	id := clitest.LatestSessionID(t, wsRoot)
	data, err := os.ReadFile(filepath.Join(wsRoot, ".putnami", "sessions", id, "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	var session struct {
		Tasks []taskResult `json:"tasks"`
	}
	if err := json.Unmarshal(data, &session); err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(session.Tasks))
	for _, task := range session.Tasks {
		out = append(out, task.String())
	}
	sort.Strings(out)
	return out
}

// workerRuns is the number of runs the workers of the running supervisor
// served, once the last run's worker reported it, and the pids of the
// workers.
func workerRuns(t *testing.T) (int, []int) {
	t.Helper()
	r, err := machineproc.Query("status")
	if err != nil {
		t.Fatalf("machine status: %v", err)
	}
	runs, pids := 0, []int{}
	for _, w := range r.Status.Workers {
		runs += w.Runs
		pids = append(pids, w.PID)
	}
	return runs, pids
}

func startSupervisor(t *testing.T) {
	t.Helper()
	// A short home keeps the socket path within the platform's limit.
	home, err := os.MkdirTemp("", "pm")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PUTNAMI_HOME", home)
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("TMPDIR", "")
	// The test's CI does not turn the machine process off for the test, and
	// a worker, which starts without the runtime's startup variables, serves
	// a client that has none either.
	for _, name := range machineproc.CIVariables {
		t.Setenv(name, "")
	}
	for _, name := range []string{"GOMAXPROCS", "GOGC", "GOMEMLIMIT", "GODEBUG", "GOTRACEBACK", "TZ"} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	// Registered after the Setenv calls, so it runs before they are undone
	// and stops this test's supervisor, not the user's.
	t.Cleanup(func() {
		_, _ = machineproc.Query("stop")
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := machineproc.Query("status"); err != nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		_ = os.RemoveAll(home)
	})
	if err := machineproc.Start(); err != nil {
		t.Fatalf("start the supervisor: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := machineproc.Query("status"); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the supervisor did not answer in time")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestARunThroughTheMachineProcessGivesTheSameKeysAndResults runs the same
// command through a worker and in its own process after each round of writes,
// and compares every task's identity, input digest, status and exit code.
func TestARunThroughTheMachineProcessGivesTheSameKeysAndResults(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("the machine process runs on Linux and macOS only")
	}
	clitest.RequireShell(t)
	clitest.UnsetHostRunEnvironment(t)
	root := clitest.WritingCountsFixture(t, 3)
	// A run writes each project's embedded version under .gen, untracked;
	// ignoring it keeps the fixture's keys settled after a commit or a branch
	// switch, so the comparison holds two runs of one tree.
	clitest.WriteFile(t, filepath.Join(root, ".gitignore"), ".putnami\n.gen\n")
	startSupervisor(t)

	rounds := []struct {
		name  string
		write func(t *testing.T)
	}{
		{"unchanged", func(*testing.T) {}},
		{"a source edited", func(t *testing.T) {
			clitest.WriteFile(t, filepath.Join(root, "apps", "p001", "src.txt"), "p001 edited\n")
		}},
		{"a file created", func(t *testing.T) {
			clitest.WriteFile(t, filepath.Join(root, "apps", "p002", "new.txt"), "new\n")
		}},
		{"a file removed", func(t *testing.T) {
			if err := os.Remove(filepath.Join(root, "apps", "p002", "new.txt")); err != nil {
				t.Fatal(err)
			}
		}},
		{"a project's configuration edited", func(t *testing.T) {
			clitest.WriteFile(t, filepath.Join(root, "apps", "p001", "putnami.json"),
				`{"name":"p001","extensions":["@fixture/counts"],"options":{"@fixture/counts":{"level":"strict"}}}`)
		}},
		{"a branch switched", func(t *testing.T) {
			clitest.RunGit(t, root, "add", "-A")
			clitest.RunGit(t, root, "commit", "-m", "round")
			clitest.RunGit(t, root, "checkout", "-b", "other")
			clitest.WriteFile(t, filepath.Join(root, "apps", "p002", "src.txt"), "p002 on another branch\n")
			clitest.RunGit(t, root, "commit", "-am", "other")
		}},
		{"the ignore rules edited", func(t *testing.T) {
			clitest.WriteFile(t, filepath.Join(root, "apps", "p001", "scratch", "notes.txt"), "notes\n")
			clitest.WriteFile(t, filepath.Join(root, ".gitignore"), ".putnami\n.gen\napps/p001/scratch\n")
		}},
		{"a change staged", func(t *testing.T) {
			clitest.WriteFile(t, filepath.Join(root, "apps", "p000", "src.txt"), "p000 staged\n")
			clitest.RunGit(t, root, "add", "-A")
		}},
	}
	args := []string{"lint,test,build", "--all"}
	worker := 0
	for _, round := range rounds {
		round.write(t)
		// A first run after a write can itself write files its keys read
		// (the projects' embedded version), so each round first settles the
		// tree with a run in its own process.
		t.Setenv(machineproc.Env, "off")
		if code, out := clitest.RunGateArgs(t, root, args...); code != 0 {
			t.Fatalf("%s: settling run exit %d\n%s", round.name, code, out)
		}

		before, _ := workerRuns(t)
		t.Setenv(machineproc.Env, "on")
		onCode, onOut := clitest.RunGateArgs(t, root, args...)
		on := latestTasks(t, root)
		after, pids := workerRuns(t)
		if served := after - before; served != 1 {
			t.Fatalf("%s: the worker served %d runs, want 1 (last run in its own process: %+v)\n%s", round.name, served, machineproc.ReadLastLocal(), onOut)
		}
		// One warm worker serves every round.
		if len(pids) != 1 || (worker != 0 && pids[0] != worker) {
			t.Fatalf("%s: workers %v, want the one worker %d", round.name, pids, worker)
		}
		worker = pids[0]

		t.Setenv(machineproc.Env, "off")
		offCode, offOut := clitest.RunGateArgs(t, root, args...)
		off := latestTasks(t, root)
		// Control: a second run in its own process gives the same tasks.
		clitest.RunGateArgs(t, root, args...)
		if again := latestTasks(t, root); strings.Join(normalize(again), "\n") != strings.Join(normalize(off), "\n") {
			t.Errorf("%s: two runs in their own process differ: the fixture is not settled", round.name)
		}
		if offCode != onCode {
			t.Errorf("%s: exit %d in its own process, %d through the worker\n%s\n%s", round.name, offCode, onCode, offOut, onOut)
		}
		if got, want := strings.Join(normalize(on), "\n"), strings.Join(normalize(off), "\n"); got != want {
			t.Errorf("%s: tasks differ\nthrough the worker:\n%s\nin its own process:\n%s", round.name, got, want)
		}
	}
}

// normalize keeps a task's identity, input digest and whether it succeeded.
func normalize(tasks []string) []string {
	out := make([]string, len(tasks))
	for i, task := range tasks {
		fields := strings.Fields(task)
		ok := len(fields) >= 3 && (fields[2] == "success" || fields[2] == "cached")
		out[i] = fmt.Sprintf("%s %s ok=%v", fields[0], fields[1], ok)
	}
	return out
}

// TestARunWhoseWorkerEndsRunsAgainInItsOwnProcess kills the worker during a
// run: the client runs the command itself, and gets the result and the exit
// code a run in its own process gets (CLI ADR 0116 §2, a crash never fails a
// run).
func TestARunWhoseWorkerEndsRunsAgainInItsOwnProcess(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the worker's orphans are ended on Linux only")
	}
	clitest.RequireShell(t)
	clitest.UnsetHostRunEnvironment(t)
	root := clitest.WritingCountsFixture(t, 1)
	clitest.WriteFile(t, filepath.Join(root, ".gitignore"), ".putnami\n.gen\n")
	// The tasks sleep long enough for the test to kill the worker during the
	// run, and write their report as the fixture's do.
	manifestPath := filepath.Join(root, "extension", "putnami.extension.json")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	slow := strings.ReplaceAll(string(manifest), `printf '%s\\n' \"$0\" \u003e \"$1\"`, `sleep 2; printf '%s\\n' \"$0\" \u003e \"$1\"`)
	if slow == string(manifest) {
		t.Fatalf("the fixture's task command was not found in %s", manifest)
	}
	clitest.WriteFile(t, manifestPath, slow)
	clitest.RunGit(t, root, "add", "-A")
	clitest.RunGit(t, root, "commit", "-m", "slow tasks")
	startSupervisor(t)
	t.Setenv(machineproc.Env, "on")

	killed := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if r, err := machineproc.Query("status"); err == nil {
				for _, w := range r.Status.Workers {
					if w.Busy {
						time.Sleep(500 * time.Millisecond)
						killed <- syscall.Kill(w.PID, syscall.SIGKILL)
						return
					}
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		killed <- fmt.Errorf("no busy worker in time")
	}()
	code, out := clitest.RunGateArgs(t, root, "lint,test,build", "--all")
	if err := <-killed; err != nil {
		t.Fatalf("kill the worker: %v", err)
	}
	if code != 0 {
		t.Fatalf("exit %d after the worker ended, want 0\n%s", code, out)
	}
	if !strings.Contains(out, "running it again in this process") {
		t.Errorf("the client did not say it ran the command again:\n%s", out)
	}
	if last := machineproc.ReadLastLocal(); last == nil || !strings.Contains(last.Reason, "ended during the run") {
		t.Errorf("machine status does not say why the run ran in its own process: %+v", last)
	}
	tasks := latestTasks(t, root)
	if len(tasks) != 3 {
		t.Fatalf("the run ran %d tasks, want 3: %v", len(tasks), tasks)
	}
	for _, task := range normalize(tasks) {
		if !strings.HasSuffix(task, "ok=true") {
			t.Errorf("task did not succeed: %s", task)
		}
	}
}

// TestAnUnchangedRequestIsDecidedAnswerableFromMemory serves the same request
// through a worker after a settled run: once the worker holds a run of the
// unchanged worktree, the next one is answerable from memory; a write in the
// worktree, or a commit, which the worktree's journal does not see, makes it
// unanswerable (CLI ADR 0116 §6a).
func TestAnUnchangedRequestIsDecidedAnswerableFromMemory(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("a run is answered from memory only where the platform has a watcher")
	}
	clitest.RequireShell(t)
	clitest.UnsetHostRunEnvironment(t)
	root := clitest.WritingCountsFixture(t, 2)
	clitest.WriteFile(t, filepath.Join(root, ".gitignore"), ".putnami\n.gen\n")
	clitest.RunGit(t, root, "add", "-A")
	clitest.RunGit(t, root, "commit", "-m", "ignore the run's state")
	args := []string{"lint,test,build", "--all"}
	t.Setenv(machineproc.Env, "off")
	for range 2 {
		if code, out := clitest.RunGateArgs(t, root, args...); code != 0 {
			t.Fatalf("settling run exit %d\n%s", code, out)
		}
	}
	startSupervisor(t)
	t.Setenv(machineproc.Env, "on")
	decideArgs := func(step string, args ...string) *machineproc.MemoryDecision {
		t.Helper()
		if code, out := clitest.RunGateArgs(t, root, args...); code != 0 {
			t.Fatalf("%s: exit %d\n%s", step, code, out)
		}
		// The worker records its decision once the client has its exit.
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if d := machineproc.ReadLastMemory(); d != nil && d.Kept != "" {
				_ = os.Remove(machineLastMemory(t))
				return d
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("%s: the worker recorded no decision (last run in its own process: %+v)", step, machineproc.ReadLastLocal())
		return nil
	}
	decide := func(step string) *machineproc.MemoryDecision {
		t.Helper()
		return decideArgs(step, args...)
	}

	if d := decide("the first request"); d.Answerable || d.Kept != "kept" {
		t.Fatalf("the first request: %+v, want unanswerable and kept", d)
	}
	if d := decide("the unchanged request"); !d.Answerable || d.Kept != "kept" {
		t.Fatalf("the unchanged request: %+v, want answerable and kept", d)
	}
	clitest.WriteFile(t, filepath.Join(root, "apps", "p001", "src.txt"), "p001 edited\n")
	if d := decide("after a write"); d.Answerable || !strings.Contains(d.Reason, "the worktree changed") {
		t.Fatalf("after a write: %+v, want unanswerable for the worktree's change", d)
	}
	decide("the run that settles the write")
	if d := decide("the unchanged request again"); !d.Answerable {
		t.Fatalf("the unchanged request again: %+v, want answerable", d)
	}
	clitest.RunGit(t, root, "commit", "--allow-empty", "-m", "empty")
	if d := decide("after a commit"); d.Answerable || !strings.Contains(d.Reason, "outside the worktree") {
		t.Fatalf("after a commit: %+v, want unanswerable for Git's witness", d)
	}
	decide("the run that holds the commit")
	if d := decideArgs("a request with other arguments", "lint,test", "--all"); d.Answerable || !strings.Contains(d.Reason, "request differs") {
		t.Fatalf("a request with other arguments: %+v, want unanswerable", d)
	}
}

// TestAnUnchangedRunIsAnsweredFromMemory serves the same request through a
// worker until the worker holds it, then once more: the engine answers that
// run from what the held run loaded and planned, without loading or planning,
// and writes none of the state files a load and a plan maintain; the
// scheduler answers its plan from the held one; the run records its session
// and gives the tasks and the exit code a run in its own process gives on the
// same tree. Once an entry a task was served from is evicted from the store,
// after a write in the worktree, and for a request with other arguments, the
// next request is not answered from memory, runs its plan, and still gives
// what a run in its own process gives (CLI ADR 0116 §6a).
func TestAnUnchangedRunIsAnsweredFromMemory(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("a run is answered from memory only where the platform has a watcher")
	}
	clitest.RequireShell(t)
	clitest.UnsetHostRunEnvironment(t)
	root := clitest.WritingCountsFixture(t, 2)
	clitest.WriteFile(t, filepath.Join(root, ".gitignore"), ".putnami\n.gen\n")
	clitest.RunGit(t, root, "add", "-A")
	clitest.RunGit(t, root, "commit", "-m", "ignore the run's state")
	args := []string{"lint,test,build", "--all"}
	localArgs := func(step string, args ...string) (int, []string) {
		t.Helper()
		t.Setenv(machineproc.Env, "off")
		code, out := clitest.RunGateArgs(t, root, args...)
		if code != 0 {
			t.Fatalf("%s: exit %d in its own process\n%s", step, code, out)
		}
		return code, latestTasks(t, root)
	}
	local := func(step string) (int, []string) {
		t.Helper()
		return localArgs(step, args...)
	}
	local("the first settling run")
	// A task served from the local cache is held under the witness of its
	// entry, taken once the entry's files are the file index's margin old:
	// the held run reads the entries the first settling run published.
	settled := time.Now().Add(fileindex.Margin)
	local("the second settling run")
	startSupervisor(t)
	time.Sleep(time.Until(settled))
	// servedArgs runs a request through the worker, and returns its exit
	// code, the tasks of the session it recorded and the worker's decision.
	servedArgs := func(step string, args ...string) (int, []string, *machineproc.MemoryDecision) {
		t.Helper()
		t.Setenv(machineproc.Env, "on")
		before := clitest.LatestSessionID(t, root)
		code, out := clitest.RunGateArgs(t, root, args...)
		decision := awaitMemoryDecision(t, step)
		if clitest.LatestSessionID(t, root) == before {
			t.Fatalf("%s: the run recorded no session\n%s", step, out)
		}
		return code, latestTasks(t, root), decision
	}
	served := func(step string) (int, []string, *machineproc.MemoryDecision) {
		t.Helper()
		return servedArgs(step, args...)
	}

	if _, _, d := served("the first request"); d.Answerable || d.Answered || d.PlanSkipped || d.VersionsHeld || d.Kept != "kept" {
		t.Fatalf("the first request: %+v, want unanswerable, computed and kept", d)
	}
	// What a plan that executed computed is never held: the second request
	// loads and plans, and only its plan is answered.
	if _, _, d := served("the second request"); !d.Answerable || !d.Answered || d.PlanSkipped || d.VersionsHeld || d.Kept != "kept" {
		t.Fatalf("the second request: %+v, want answerable, planned, answered and kept", d)
	}
	state := stateFiles(t, root)
	code, answered, d := served("the unchanged request")
	// It took the version snapshot the held run read, without asking Git.
	if !d.Answerable || !d.Answered || !d.PlanSkipped || !d.VersionsHeld || !d.FinalizersHeld || d.Kept != "kept" {
		t.Fatalf("the unchanged request: %+v, want answerable, answered from memory without a plan, Git or finalizers, and kept", d)
	}
	if code != 0 {
		t.Fatalf("the answered run exited %d, want 0", code)
	}
	if len(answered) != 6 {
		t.Fatalf("the answered run recorded %d tasks, want 6: %v", len(answered), answered)
	}
	// It read nothing through the file index, the input record or the
	// workspace snapshot, and so wrote none of them.
	if after := stateFiles(t, root); after != state {
		t.Errorf("the answered run changed the state files a load and a plan maintain:\nbefore %s\nafter  %s", state, after)
	}
	localCode, own := local("a run in its own process on the same tree")
	if localCode != code {
		t.Errorf("exit %d in its own process, %d answered from memory", localCode, code)
	}
	if got, want := strings.Join(normalize(answered), "\n"), strings.Join(normalize(own), "\n"); got != want {
		t.Errorf("tasks differ\nanswered from memory:\n%s\nin its own process:\n%s", got, want)
	}
	if _, again, d := served("the unchanged request again"); !d.PlanSkipped || !d.Answered || !d.VersionsHeld ||
		strings.Join(normalize(again), "\n") != strings.Join(normalize(own), "\n") {
		t.Fatalf("the unchanged request again: %+v, want it answered without a plan, with the tasks of a run in its own process", d)
	}

	// An entry the store no longer holds, which a collection, a clean or
	// another worktree's run evicted, is not the one the answer would report
	// served: the next request is refused at the answer, runs its plan, which
	// executes that task again, and gives what a run in its own process gives.
	evictServedEntry(t, root, answered[0])
	code, computed, d := served("after an eviction")
	if !d.Answerable || d.Answered || !strings.Contains(d.Reason, "a served task's cache entry moved") {
		t.Fatalf("after an eviction: %+v, want answerable, refused at the answer for the moved entry and computed", d)
	}
	if code != 0 {
		t.Fatalf("after an eviction: exit %d, want 0", code)
	}
	localCode, own = local("a run in its own process after the eviction")
	if localCode != code {
		t.Errorf("after an eviction: exit %d in its own process, %d through the worker", localCode, code)
	}
	if got, want := strings.Join(normalize(computed), "\n"), strings.Join(normalize(own), "\n"); got != want || len(computed) != 6 {
		t.Errorf("after an eviction: tasks differ, or are not the 6 asked\nthrough the worker:\n%s\nin its own process:\n%s", got, want)
	}
	// The run executed a task, so the worker holds none: the next request
	// computes, and is held.
	if _, _, d := served("the request that settles the eviction"); d.Answered || d.Kept != "kept" {
		t.Fatalf("the request that settles the eviction: %+v, want computed and kept", d)
	}

	clitest.WriteFile(t, filepath.Join(root, "apps", "p001", "src.txt"), "p001 edited\n")
	code, computed, d = served("after a write")
	if d.Answerable || d.Answered || d.PlanSkipped || d.VersionsHeld || d.FinalizersHeld || !strings.Contains(d.Reason, "the worktree changed") {
		t.Fatalf("after a write: %+v, want unanswerable for the worktree's change, planned and computed", d)
	}
	localCode, own = local("a run in its own process after the write")
	if localCode != code {
		t.Errorf("after a write: exit %d in its own process, %d through the worker", localCode, code)
	}
	if got, want := strings.Join(normalize(computed), "\n"), strings.Join(normalize(own), "\n"); got != want {
		t.Errorf("after a write: tasks differ\nthrough the worker:\n%s\nin its own process:\n%s", got, want)
	}

	// The run after the write executed its tasks, so the worker holds none:
	// the next request computes, and is held.
	if _, _, d := served("the request that settles the write"); d.Answerable || d.Answered || d.PlanSkipped || d.Kept != "kept" {
		t.Fatalf("the request that settles the write: %+v, want unanswerable, computed and kept", d)
	}
	// A request that selects other projects and runs other commands is not
	// answered from the held one.
	other := []string{"lint,test", "--projects", "p000"}
	code, computed, d = servedArgs("a request with other arguments", other...)
	if d.Answerable || d.Answered || d.PlanSkipped || !strings.Contains(d.Reason, "request differs") {
		t.Fatalf("a request with other arguments: %+v, want unanswerable for the request, planned and computed", d)
	}
	localCode, own = localArgs("a run in its own process with the other arguments", other...)
	if localCode != code {
		t.Errorf("with other arguments: exit %d in its own process, %d through the worker", localCode, code)
	}
	if got, want := strings.Join(normalize(computed), "\n"), strings.Join(normalize(own), "\n"); got != want || len(own) != 2 {
		t.Errorf("with other arguments: tasks differ, or are not the 2 asked\nthrough the worker:\n%s\nin its own process:\n%s", got, want)
	}
}

// evictServedEntry removes from the store the entry the task of the session
// line task (latestTasks) was served from, as a collection evicts it.
func evictServedEntry(t *testing.T, root, task string) {
	t.Helper()
	fields := strings.Fields(task)
	if len(fields) < 2 || !strings.HasPrefix(fields[1], "sha256:") {
		t.Fatalf("the task %q names no input digest", task)
	}
	entry, err := store.NewLocalStore(store.ResolveStoreRoot(root)).LookupTaskEntry(strings.TrimPrefix(fields[1], "sha256:"))
	if err != nil || entry == nil || entry.FilesDir == "" {
		t.Fatalf("the entry the task %q was served from: %v, %v", task, entry, err)
	}
	if err := os.RemoveAll(filepath.Dir(entry.FilesDir)); err != nil {
		t.Fatal(err)
	}
}

// stateFiles describes the state files a run's load and plan maintain in the
// worktree: the file index, the input record and the workspace snapshot, each
// by its size and digest, or as absent.
func stateFiles(t *testing.T, root string) string {
	t.Helper()
	var out []string
	for _, name := range []string{"file-index", "input-record", "workspace-index.json"} {
		data, err := os.ReadFile(filepath.Join(root, ".putnami", name))
		switch {
		case os.IsNotExist(err):
			out = append(out, name+" absent")
		case err != nil:
			t.Fatal(err)
		default:
			sum := sha256.Sum256(data)
			out = append(out, fmt.Sprintf("%s %d %x", name, len(data), sum[:8]))
		}
	}
	return strings.Join(out, ", ")
}

// awaitMemoryDecision waits for the worker's decision on the run it just
// served, which it records once the client has its exit, and removes it so
// the next run's is not mistaken for it.
func awaitMemoryDecision(t *testing.T, step string) *machineproc.MemoryDecision {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if d := machineproc.ReadLastMemory(); d != nil && d.Kept != "" {
			_ = os.Remove(machineLastMemory(t))
			return d
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s: the worker recorded no decision (last run in its own process: %+v)", step, machineproc.ReadLastLocal())
	return nil
}

// machineLastMemory is the path of the worker's last decision.
func machineLastMemory(t *testing.T) string {
	t.Helper()
	dir, err := machineproc.Dir(os.Getenv, false)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "last-memory.json")
}

// TestEachWorktreeIsServedByAWorkerOfItsOwn serves two worktrees in turn:
// each gets a worker of its own, which serves it again on its next request,
// so the two can progress at once instead of one refusing the other (CLI ADR
// 0116 §1).
func TestEachWorktreeIsServedByAWorkerOfItsOwn(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("the machine process runs on Linux and macOS only")
	}
	clitest.RequireShell(t)
	clitest.UnsetHostRunEnvironment(t)
	roots := []string{clitest.WritingCountsFixture(t, 1), clitest.WritingCountsFixture(t, 1)}
	startSupervisor(t)
	t.Setenv(machineproc.Env, "on")
	// runsByWorker is each worker's count of served runs, by pid.
	runsByWorker := func() map[int]int {
		t.Helper()
		workerRuns(t) // waits for the last run's worker to report it
		r, err := machineproc.Query("status")
		if err != nil {
			t.Fatal(err)
		}
		out := map[int]int{}
		for _, w := range r.Status.Workers {
			out[w.PID] = w.Runs
		}
		return out
	}
	serve := func(root string) int {
		t.Helper()
		before := runsByWorker()
		if code, out := clitest.RunGateArgs(t, root, "lint", "--all"); code != 0 {
			t.Fatalf("%s: exit %d\n%s", root, code, out)
		}
		after := runsByWorker()
		servedBy := 0
		for pid, runs := range after {
			if runs > before[pid] {
				if servedBy != 0 {
					t.Fatalf("%s: two workers served one request: %v then %v", root, before, after)
				}
				servedBy = pid
			}
		}
		if servedBy == 0 {
			t.Fatalf("%s: no worker served the request (last run in its own process: %+v)", root, machineproc.ReadLastLocal())
		}
		return servedBy
	}
	first := []int{serve(roots[0]), serve(roots[1])}
	if first[0] == first[1] {
		t.Fatalf("two worktrees were served by one worker %d, want a worker each", first[0])
	}
	if again := serve(roots[0]); again != first[0] {
		t.Errorf("the first worktree was served again by worker %d, want its own %d", again, first[0])
	}
	if again := serve(roots[1]); again != first[1] {
		t.Errorf("the second worktree was served again by worker %d, want its own %d", again, first[1])
	}
}

// gateFixture writes a workspace whose validate command projects one
// executable requirement for each of two projects, alpha and beta, and whose
// test command, of the verification kind, reports the observation its
// project states: alpha's passes, beta states none. The spec gate collects a
// record of two groups, alpha verified and beta missing. Every task is
// cached, so a run of an unchanged tree serves each from the cache.
func gateFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	clitest.WriteFile(t, filepath.Join(root, "putnami.workspace.json"),
		`{"name":"gate-fixture","includes":["gate-extension","alpha","beta"]}`)
	ext := filepath.Join(root, "gate-extension")
	clitest.WriteFile(t, filepath.Join(ext, "putnami.json"), `{"name":"@fixture/gate"}`)
	clitest.WriteFile(t, filepath.Join(ext, "emit.sh"), `#!/bin/sh
source="$PUTNAMI_PROJECT_ROOT/$1.src.json"
if [ -f "$source" ]; then
  mkdir -p "$PUTNAMI_OUTPUT_PATH"
  cp "$source" "$PUTNAMI_OUTPUT_PATH/$1"
  printf '%s\n' "{\"v\":2,\"type\":\"artifact\",\"id\":\"$2\",\"name\":\"$2\",\"kind\":\"report\",\"path\":\"$PUTNAMI_OUTPUT_PATH/$1\"}"
else
  rm -f "$PUTNAMI_OUTPUT_PATH/$1"
fi
`)
	if err := os.Chmod(filepath.Join(ext, "emit.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	task := func(file, artifact, activation string) string {
		return `{"kind": "command", "command": "{extensionRoot}/emit.sh", "args": ["` + file + `", "` + artifact + `"],
      "cwd": "{projectRoot}", "timeoutMs": -1,
      "inputs": {"sources": {"from": "project", "files": ["*.json", "*.txt", "` + activation + `"]}},
      "cache": {"enabled": true},
      "declares": {"outputs": {"out": {"kind": "file", "root": "command-output", "path": "` + file + `", "optionalEmpty": true}}}}`
	}
	clitest.WriteFile(t, filepath.Join(ext, "putnami.extension.json"), `{
  "name": "@fixture/gate", "version": "0.1.0", "cliContract": 11,
  "commands": {
    "validate": {"activationFiles": ["gate.specs"], "traits": {"kind": "criteria"}, "run": [{"id": "specs", "task": "criteria"}]},
    "test": {"activationFiles": ["gate.project"], "traits": {"kind": "verification"}, "run": [{"id": "test", "task": "report"}]}
  },
  "tasks": {
    "criteria": `+task("spec-criteria.json", "putnami-spec-criteria", "gate.specs")+`,
    "report": `+task("putnami-feature-verification.json", "putnami-feature-verification", "gate.project")+`
  }
}`)
	for _, project := range []string{"alpha", "beta"} {
		dir := filepath.Join(root, project)
		clitest.WriteFile(t, filepath.Join(dir, "putnami.json"), `{"name":"`+project+`","extensions":["@fixture/gate"]}`)
		clitest.WriteFile(t, filepath.Join(dir, "gate.project"), "")
		clitest.WriteFile(t, filepath.Join(dir, "gate.specs"), "")
		clitest.WriteFile(t, filepath.Join(dir, project+"_check.txt"), "fixture declaration\n")
		clitest.WriteFile(t, filepath.Join(dir, "spec-criteria.json.src.json"), `{"protocolVersion": 1, "groups": [{
  "feature": "gate/`+project+`", "spec": "`+project+`/specs/`+project+`.json", "specRequirements": ["holds"],
  "requirements": [{"id": "holds", "stage": "coded", "evidenceKinds": ["attestation"],
    "verification": {"kind": "acceptance", "checks": ["`+project+`-check"]}}]}]}`)
	}
	clitest.WriteFile(t, filepath.Join(root, "alpha", "putnami-feature-verification.json.src.json"), `{"protocolVersion": 1, "observations": [{
  "feature": "gate/alpha", "requirement": "holds", "check": "alpha-check", "status": "passed",
  "provenance": {"path": "alpha_check.txt", "symbol": "FixtureCheck"}}]}`)
	clitest.WriteFile(t, filepath.Join(root, ".gitignore"), ".putnami\n")
	clitest.InitGitRepo(t, root)
	return root
}

// latestGateRecord is the spec gate's record of the latest session of root,
// without the instant it was evaluated at and the session it names, which it
// checks is that session, or "" when the session has none.
func latestGateRecord(t *testing.T, root string) string {
	t.Helper()
	id := clitest.LatestSessionID(t, root)
	path := filepath.Join(root, ".putnami", "sessions", id, features.SpecVerificationRecordFilename)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]json.RawMessage
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	var at, session string
	if json.Unmarshal(record["generatedAt"], &at) != nil || json.Unmarshal(record["sessionId"], &session) != nil {
		t.Fatalf("the record of session %s names no instant or session: %s", id, data)
	}
	if _, err := time.Parse(time.RFC3339, at); err != nil {
		t.Fatalf("the record's evaluation instant %q: %v", at, err)
	}
	if session != id {
		t.Fatalf("the record of session %s names the session %s", id, session)
	}
	delete(record, "generatedAt")
	delete(record, "sessionId")
	out, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// gateWarnings are the spec gate's warning lines of a run's output.
func gateWarnings(out string) string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "spec verification") {
			lines = append(lines, strings.TrimSpace(line))
		}
	}
	return strings.Join(lines, "\n")
}

// TestAnUnchangedGateRunReplaysTheHeldSpecGate serves a run whose session
// carries the spec gate through a worker until the worker holds it, then
// once more: that run replays what the held run's finalizers did instead of
// collecting the gate again, and records the session, the gate's record and
// its warnings, the tasks and the exit code a run in its own process gives on
// the same tree. After a write in the worktree, nothing is held (CLI ADR 0116
// §6a).
func TestAnUnchangedGateRunReplaysTheHeldSpecGate(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("a run is answered from memory only where the platform has a watcher")
	}
	clitest.RequireShell(t)
	clitest.UnsetHostRunEnvironment(t)
	root := gateFixture(t)
	args := []string{"validate,test", "--all"}
	type run struct {
		code     int
		out      string
		tasks    []string
		record   string
		decision *machineproc.MemoryDecision
	}
	local := func(step string) run {
		t.Helper()
		t.Setenv(machineproc.Env, "off")
		code, out := clitest.RunGateArgs(t, root, args...)
		if code != 0 {
			t.Fatalf("%s: exit %d in its own process\n%s", step, code, out)
		}
		return run{code: code, out: out, tasks: latestTasks(t, root), record: latestGateRecord(t, root)}
	}
	local("the first settling run")
	settled := time.Now().Add(fileindex.Margin)
	own := local("the second settling run")
	if own.record == "" || len(own.tasks) != 4 || gateWarnings(own.out) == "" {
		t.Fatalf("a run in its own process recorded %d tasks and the record %q, and warned %q, want 4 tasks, the gate's record and its warning",
			len(own.tasks), own.record, gateWarnings(own.out))
	}
	startSupervisor(t)
	time.Sleep(time.Until(settled))
	served := func(step string) run {
		t.Helper()
		t.Setenv(machineproc.Env, "on")
		before := clitest.LatestSessionID(t, root)
		code, out := clitest.RunGateArgs(t, root, args...)
		decision := awaitMemoryDecision(t, step)
		if clitest.LatestSessionID(t, root) == before {
			t.Fatalf("%s: the run recorded no session\n%s", step, out)
		}
		return run{code: code, out: out, tasks: latestTasks(t, root), record: latestGateRecord(t, root), decision: decision}
	}
	same := func(step string, got, want run) {
		t.Helper()
		if got.code != want.code {
			t.Errorf("%s: exit %d, %d in its own process", step, got.code, want.code)
		}
		if g, w := strings.Join(normalize(got.tasks), "\n"), strings.Join(normalize(want.tasks), "\n"); g != w {
			t.Errorf("%s: tasks differ\nthrough the worker:\n%s\nin its own process:\n%s", step, g, w)
		}
		if got.record != want.record {
			t.Errorf("%s: the gate's record differs\nthrough the worker:\n%s\nin its own process:\n%s", step, got.record, want.record)
		}
		if g, w := gateWarnings(got.out), gateWarnings(want.out); g != w {
			t.Errorf("%s: the gate's warnings differ\nthrough the worker:\n%s\nin its own process:\n%s", step, g, w)
		}
	}

	if d := served("the first request").decision; d.Answerable || d.FinalizersHeld || d.Kept != "kept" {
		t.Fatalf("the first request: %+v, want unanswerable, computed and kept", d)
	}
	// The second request plans, so its finalizers run on the answered plan,
	// and what they did is held with what it planned.
	if d := served("the second request").decision; !d.Answered || d.PlanSkipped || d.FinalizersHeld || d.Kept != "kept" {
		t.Fatalf("the second request: %+v, want answered, planned, its finalizers run, and kept", d)
	}
	answered := served("the unchanged request")
	if d := answered.decision; !d.Answered || !d.PlanSkipped || !d.VersionsHeld || !d.FinalizersHeld || d.Kept != "kept" {
		t.Fatalf("the unchanged request: %+v, want answered without a plan, Git or the finalizers, and kept", d)
	}
	same("the unchanged request", answered, local("a run in its own process on the same tree"))
	if again := served("the unchanged request again"); !again.decision.FinalizersHeld {
		t.Fatalf("the unchanged request again: %+v, want its finalizers replayed", again.decision)
	}

	clitest.WriteFile(t, filepath.Join(root, "beta", "putnami-feature-verification.json.src.json"), `{"protocolVersion": 1, "observations": [{
  "feature": "gate/beta", "requirement": "holds", "check": "beta-check", "status": "failed",
  "provenance": {"path": "beta_check.txt", "symbol": "FixtureCheck"}}]}`)
	written := served("after a write")
	if d := written.decision; d.Answerable || d.PlanSkipped || d.VersionsHeld || d.FinalizersHeld {
		t.Fatalf("after a write: %+v, want nothing held", d)
	}
	after := local("a run in its own process after the write")
	same("after a write", written, after)
	if after.record == own.record {
		t.Errorf("the write did not change the gate's record: the comparison proves nothing")
	}
}
