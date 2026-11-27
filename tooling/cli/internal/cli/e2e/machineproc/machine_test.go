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

	"go.putnami.dev/tooling/cli/internal/cli"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
	"go.putnami.dev/tooling/cli/internal/engine"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/scheduler"
	"go.putnami.dev/tooling/cli/internal/machineproc"
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
// same tree. After a write in the worktree, and for a request with other
// arguments, the next request loads, plans and runs its plan, and still gives
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
	local("the second settling run")
	startSupervisor(t)
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

	if _, _, d := served("the first request"); d.Answerable || d.Answered || d.PlanSkipped || d.Kept != "kept" {
		t.Fatalf("the first request: %+v, want unanswerable, computed and kept", d)
	}
	// What a plan that executed computed is never held: the second request
	// loads and plans, and only its plan is answered.
	if _, _, d := served("the second request"); !d.Answerable || !d.Answered || d.PlanSkipped || d.Kept != "kept" {
		t.Fatalf("the second request: %+v, want answerable, planned, answered and kept", d)
	}
	state := stateFiles(t, root)
	code, answered, d := served("the unchanged request")
	if !d.Answerable || !d.Answered || !d.PlanSkipped || d.Kept != "kept" {
		t.Fatalf("the unchanged request: %+v, want answerable, answered from memory without a plan and kept", d)
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
	if _, again, d := served("the unchanged request again"); !d.PlanSkipped || !d.Answered ||
		strings.Join(normalize(again), "\n") != strings.Join(normalize(own), "\n") {
		t.Fatalf("the unchanged request again: %+v, want it answered without a plan, with the tasks of a run in its own process", d)
	}

	clitest.WriteFile(t, filepath.Join(root, "apps", "p001", "src.txt"), "p001 edited\n")
	code, computed, d := served("after a write")
	if d.Answerable || d.Answered || d.PlanSkipped || !strings.Contains(d.Reason, "the worktree changed") {
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
