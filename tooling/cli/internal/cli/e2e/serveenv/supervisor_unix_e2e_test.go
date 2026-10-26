//go:build unix

package serveenv

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/cli"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
)

const (
	// supervisedRootEnv names the fixture workspace the helper serves; set
	// only for the helper process.
	supervisedRootEnv = "PUTNAMI_SUPERVISED_HELPER_ROOT"
	// supervisedWatchEnv, set to true, makes the helper's run watch, as compose
	// runs its target.
	supervisedWatchEnv = "PUTNAMI_SUPERVISED_HELPER_WATCH"
	// startBound is how long the helper may take to start its serve step.
	startBound = 60 * time.Second
	// stopBound is how long the run may take to see its supervisor's loss and
	// stop its serve step.
	stopBound = 20 * time.Second
)

// longServeFixture writes a workspace whose one project, example, has a serve
// command of one step that runs until SIGTERM. The step records its pid, claims
// readiness, then writes a log line every 50 ms; it ignores SIGPIPE, so the
// run's loss alone does not end it. On SIGTERM it records that it stopped.
func longServeFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	clitest.WriteFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name":"supervisor-fixture","includes":["extension","example"]}`)
	clitest.WriteFile(t, filepath.Join(root, "example", "putnami.json"), `{"name":"example","extensions":["@fixture/serve"]}`)
	clitest.WriteFile(t, filepath.Join(root, "example", "serve.project"), "")
	clitest.WriteFile(t, filepath.Join(root, "extension", "putnami.json"), `{"name":"@fixture/serve"}`)
	clitest.WriteFile(t, filepath.Join(root, "extension", "serve.sh"), `trap '' PIPE
trap 'printf stopped > "$PUTNAMI_PROJECT_ROOT/serve.stopped"; exit 0' TERM
printf '%s' "$$" > "$PUTNAMI_PROJECT_ROOT/serve.pid"
printf '%s\n' '`+readyLine+`'
while :; do
  printf '%s\n' '{"v":2,"type":"log","level":"info","message":"fixture api is serving"}'
  sleep 0.05
done
`)
	disabled := false
	manifest := extensionproto.Manifest{
		Name: "@fixture/serve", Version: "1.0.0", CLIContract: protocolcli.CommandKindContract,
		Commands: map[string]extensionproto.CommandDefinition{
			"serve": {
				ActivationFiles: []string{"serve.project"},
				Traits:          &extensionproto.CommandTraits{Workload: extensionproto.WorkloadServe},
				Flags: map[string]extensionproto.FlagDefinition{
					"port":  {Type: "number"},
					"watch": {Type: "boolean", Default: true},
				},
				Run: []extensionproto.PipelineStep{{ID: "serve", Task: "serve-run"}},
			},
		},
		Tasks: map[string]extensionproto.TaskDefinition{
			"serve-run": {
				Kind: "command", Command: "/bin/sh", Args: []string{"{extensionRoot}/serve.sh"},
				Cache: &extensionproto.TaskCachePolicy{Enabled: &disabled}, TimeoutMs: -1,
			},
		},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	clitest.WriteFile(t, filepath.Join(root, "extension", "putnami.extension.json"), string(data))
	clitest.WriteFile(t, filepath.Join(root, "stdin.json"), `{"SERVE_ORIGIN":"http://127.0.0.1:43120"}`)
	return root
}

// TestSupervisedServeRunHelper is the supervised serve run of
// TestSupervisedServeRunStopsOnceItsSupervisorIsKilled: the real CLI, started
// by a shell that stands for compose. It records its pid, then runs.
func TestSupervisedServeRunHelper(t *testing.T) {
	root := os.Getenv(supervisedRootEnv)
	if root == "" {
		t.Skip("helper process only")
	}
	if err := os.WriteFile(filepath.Join(root, "run.pid"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	app, err := cli.NewApp()
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	t.Chdir(root)
	os.Exit(app.Run(context.Background(), supervisedArgs(os.Getenv(supervisedWatchEnv) == "true")))
}

// supervisedArgs are serveArgs, without --no-watch for a watched run.
func supervisedArgs(watch bool) []string {
	if !watch {
		return serveArgs
	}
	var args []string
	for _, arg := range serveArgs {
		if arg != "--no-watch" {
			args = append(args, arg)
		}
	}
	return args
}

// A supervisor killed outright signals nothing to the supervised serve run it
// started. The run sees the loss, stops its serve step as SIGTERM does, and
// exits, watched or not; its writes to the stdout and stderr whose reader died
// with the supervisor fail without ending it before it stopped the step.
func TestSupervisedServeRunStopsOnceItsSupervisorIsKilled(t *testing.T) {
	clitest.RequireShell(t)
	spectest.Proves(t, "cli/supervised-serve-run", "a-lost-supervisor-stops-the-run", "a-killed-supervisor-stops-the-serve-step")
	for _, tc := range []struct {
		name  string
		watch bool
	}{
		{"not watched", false},
		{"watched", true},
	} {
		t.Run(tc.name, func(t *testing.T) { killSupervisorAndWaitForTheStop(t, tc.watch) })
	}
}

func killSupervisorAndWaitForTheStop(t *testing.T, watch bool) {
	root := longServeFixture(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	// sh is the supervisor: it starts the run in the background, then waits.
	supervisor := exec.Command("/bin/sh", "-c", `"$0" -test.run='^TestSupervisedServeRunHelper$' -test.timeout=120s < "$1" & wait`,
		executable, filepath.Join(root, "stdin.json"))
	supervisor.Env = append(os.Environ(),
		supervisedRootEnv+"="+root,
		supervisedWatchEnv+"="+strconv.FormatBool(watch),
		"PUTNAMI_STORE_DIR="+filepath.Join(t.TempDir(), "store"),
		"DO_NOT_TRACK=1",
	)
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderrReader, stderrWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	supervisor.Stdout, supervisor.Stderr = stdoutWriter, stderrWriter
	if err := supervisor.Start(); err != nil {
		t.Fatalf("start the supervisor: %v", err)
	}
	_ = stdoutWriter.Close()
	_ = stderrWriter.Close()
	defer func() { _ = supervisor.Process.Kill(); _ = supervisor.Wait() }()
	// The supervisor reads both streams while it lives, as compose does.
	drained := make(chan string, 1)
	go func() {
		data := make([]byte, 64<<10)
		var tail []byte
		for {
			n, err := stderrReader.Read(data)
			tail = append(tail, data[:n]...)
			if len(tail) > 8<<10 {
				tail = tail[len(tail)-8<<10:]
			}
			if err != nil {
				drained <- string(tail)
				return
			}
		}
	}()
	go func() {
		data := make([]byte, 64<<10)
		for {
			if _, err := stdoutReader.Read(data); err != nil {
				return
			}
		}
	}()

	stepPID := waitForPID(t, filepath.Join(root, "example", "serve.pid"), startBound, drained)
	runPID := waitForPID(t, filepath.Join(root, "run.pid"), time.Second, drained)
	stepGroup, err := syscall.Getpgid(stepPID)
	if err != nil {
		t.Fatalf("read the serve step's process group: %v", err)
	}
	// The run shares the supervisor's group, which is this test's: only a
	// group of its own is safe to kill on a failure.
	if runGroup, err := syscall.Getpgid(runPID); err != nil || stepGroup == runGroup || stepGroup == syscall.Getpgrp() {
		_ = syscall.Kill(runPID, syscall.SIGKILL)
		t.Fatalf("the serve step runs in process group %d, want one of its own (run's group %d, %v)", stepGroup, runGroup, err)
	}
	// Both ran at the last read, so the ids are still theirs; once the test
	// saw them end, it signals neither again.
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			_ = syscall.Kill(-stepGroup, syscall.SIGKILL)
			_ = syscall.Kill(runPID, syscall.SIGKILL)
		}
	})

	// The supervisor dies outright, and the streams it read close with it.
	if err := supervisor.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL the supervisor: %v", err)
	}
	_ = supervisor.Wait()
	_ = stdoutReader.Close()
	_ = stderrReader.Close()

	deadline := time.Now().Add(stopBound)
	for groupRunning(t, stepGroup) || processRunning(runPID) {
		if time.Now().After(deadline) {
			t.Fatalf("%s after the supervisor's loss: serve step group %d running = %v, run %d running = %v",
				stopBound, stepGroup, groupRunning(t, stepGroup), runPID, processRunning(runPID))
		}
		time.Sleep(50 * time.Millisecond)
	}
	stopped = true
	if data, err := os.ReadFile(filepath.Join(root, "example", "serve.stopped")); err != nil || string(data) != "stopped" {
		t.Errorf("the serve step ended without its SIGTERM stop (serve.stopped: %q, %v)", data, err)
	}
}

// waitForPID waits up to bound for path to hold a pid and returns it. On a
// timeout it reports the tail of the run's stderr.
func waitForPID(t *testing.T, path string, bound time.Duration, stderr <-chan string) int {
	t.Helper()
	deadline := time.Now().Add(bound)
	for {
		if data, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
				return pid
			}
		}
		select {
		case tail := <-stderr:
			t.Fatalf("the run ended before it wrote %s; stderr tail:\n%s", filepath.Base(path), tail)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("no pid in %s after %s", path, bound)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// groupRunning reports whether a process of the process group runs. A zombie,
// which kill(0) still reaches until its adopter reaps it, does not run.
func groupRunning(t *testing.T, pgid int) bool {
	t.Helper()
	if errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH) {
		return false
	}
	out, err := exec.Command("ps", "-A", "-o", "pgid=,stat=").Output()
	if err != nil {
		t.Fatalf("ps: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == strconv.Itoa(pgid) && !strings.HasPrefix(fields[1], "Z") {
			return true
		}
	}
	return false
}

// processRunning reports whether pid runs. A zombie does not run.
func processRunning(pid int) bool {
	if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		return false
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	return err == nil && !strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}
