// Package machineproc_test checks CLI ADR 0116 §8: a run through the machine
// process gives the plan, keys, outcomes and exit code a run in its own
// process gives. The test binary plays the CLI: TestMain answers the
// supervisor's and the worker's own commands, which the client starts from
// os.Executable.
package machineproc_test

import (
	"context"
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
	"go.putnami.dev/tooling/cli/internal/machineproc"
)

func TestMain(m *testing.M) {
	clitest.AnswerRegistryTokenSeam()
	if len(os.Args) == 3 && os.Args[1] == "__machine" {
		switch os.Args[2] {
		case "supervise":
			os.Exit(machineproc.Supervise())
		case "work":
			os.Exit(machineproc.Work(func(ctx context.Context, args []string) int {
				app, err := cli.NewApp()
				if err != nil {
					return cli.ExitError
				}
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
