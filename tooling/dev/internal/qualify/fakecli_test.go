package qualify

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/sdk/extension/proctree"
)

// A local target starts each member as a serve run of the putnami CLI it is
// given. These tests give it this test binary, which TestMain turns into a
// fake CLI when its first argument is a command word: the fake reads the
// member's environment document on stdin, starts the fixture workload as the
// serve step in a process group of its own, in the project directory and with
// that environment, and writes the session records compose reads: the serve
// step's process event, its runtime events and its end. It writes nothing in
// the worktree, so the tree a local verdict binds to stays the committed one.

// fakeCLIInterrupted is the fake's exit code once a signal stopped it.
const fakeCLIInterrupted = 130

// fakeCLIPrepareFailure is what the fake's generate step writes on stderr
// before it fails, for a member in fixturePrepareFails mode.
const fakeCLIPrepareFailure = "fixture generate failed: boom"

// fixtureModeOf is the mode of the member whose project directory is dir, or
// "" when its fixture.json cannot be read.
func fixtureModeOf(dir string) string {
	var config localFixtureConfig
	data, err := os.ReadFile(filepath.Join(dir, "fixture.json"))
	if err != nil || json.Unmarshal(data, &config) != nil {
		return ""
	}
	return config.Mode
}

// isFakeCLIInvocation reports whether this test binary was started as a
// member's serve run: a test run's first argument is a -test flag.
func isFakeCLIInvocation(args []string) bool {
	return len(args) > 1 && args[1] != "" && !strings.HasPrefix(args[1], "-")
}

// runFakeCLI runs `serve --projects <id> [--no-watch] --serve-env - ...` and
// returns its exit code.
func runFakeCLI(args []string) int {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	project, watch := "", true
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--projects":
			if i+1 < len(args) {
				i++
				project = args[i]
			}
		case "--no-watch":
			watch = false
		}
	}
	if len(args) == 0 || args[0] != "serve" || project == "" {
		fmt.Fprintf(os.Stderr, "putnami: the fake CLI serves `serve --projects <id>`, not %q\n", args)
		return 2
	}
	document, err := io.ReadAll(os.Stdin)
	var env map[string]string
	if err == nil {
		err = json.Unmarshal(document, &env)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "putnami: --serve-env: "+fmt.Sprint(err))
		return 2
	}
	root, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "putnami: "+err.Error())
		return 2
	}
	out := &fakeStream{}
	dir := filepath.Join(root, strings.TrimPrefix(project, "/"))
	if fixtureModeOf(dir) == fixturePrepareFails {
		generate := project + ":generate~generate"
		fmt.Fprintln(os.Stderr, "["+generate+"] "+fakeCLIPrepareFailure)
		out.taskEnd(generate, protocolcli.TaskStatusFailed, 1)
		out.sessionEnd()
		return 1
	}
	key := project + ":serve~serve"
	workload, err := startFakeWorkload(dir, project, key, env, out)
	if err != nil {
		fmt.Fprintln(os.Stderr, "putnami: start the serve step: "+err.Error())
		out.taskEnd(key, protocolcli.TaskStatusFailed, 1)
		out.sessionEnd()
		return 1
	}
	select {
	case <-workload.done:
		status := protocolcli.TaskStatusSuccess
		if workload.code != 0 {
			status = protocolcli.TaskStatusFailed
		}
		out.taskEnd(key, status, workload.code)
		if watch {
			<-signals
			out.sessionEnd()
			return fakeCLIInterrupted
		}
		out.sessionEnd()
		if workload.code != 0 {
			return 1
		}
		return 0
	case <-signals:
		workload.stop()
		out.taskEnd(key, protocolcli.TaskStatusCanceled, 0)
		out.sessionEnd()
		return fakeCLIInterrupted
	}
}

// fakeWorkload is the serve step's process.
type fakeWorkload struct {
	tree *proctree.Tree
	done chan struct{}
	code int
}

// startFakeWorkload starts the fixture workload in its own process group,
// names that group in the serve step's process event, and turns the
// workload's runtime events into the serve step's records and log.
func startFakeWorkload(dir, project, key string, env map[string]string, out *fakeStream) (*fakeWorkload, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(executable)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), localFixtureEnv+"=1")
	for name, value := range env {
		cmd.Env = append(cmd.Env, name+"="+value)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr
	tree := proctree.New(cmd)
	if err := tree.Start(); err != nil {
		return nil, err
	}
	group := tree.ID()
	leaderStart, _ := proctree.StartTime(group)
	out.event(key, map[string]any{"type": "process", "group": group, "leaderStart": leaderStart})

	workload := &fakeWorkload{tree: tree, done: make(chan struct{})}
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			var event map[string]any
			if json.Unmarshal(scanner.Bytes(), &event) != nil {
				continue
			}
			delete(event, "v")
			if data, ok := event["data"].(map[string]any); ok {
				delete(event, "data")
				for name, value := range data {
					event[name] = value
				}
			}
			out.event(key, event)
			if event["type"] == "log" {
				message, _ := event["message"].(string)
				fmt.Fprintln(os.Stderr, "["+project+"] "+message)
			}
		}
		err := cmd.Wait()
		exitErr := &exec.ExitError{}
		if errors.As(err, &exitErr) {
			workload.code = exitErr.ExitCode()
		} else if err != nil {
			workload.code = 1
		}
		_ = tree.Close()
		close(workload.done)
	}()
	return workload, nil
}

// stop ends the workload: SIGTERM to its group, then SIGKILL after five
// seconds, as the job runner does.
func (w *fakeWorkload) stop() {
	_ = w.tree.Terminate()
	select {
	case <-w.done:
		return
	case <-time.After(5 * time.Second):
	}
	_ = w.tree.Kill()
	<-w.done
}

// fakeStream writes the fake run's session stream on stdout, one record per
// line.
type fakeStream struct {
	mu sync.Mutex
}

func (s *fakeStream) write(record protocolcli.SessionStreamRecord) {
	record.ProtocolVersion = 2
	record.Time = time.Now().UTC().Format(time.RFC3339Nano)
	data, _ := json.Marshal(record)
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Println(string(data))
}

func (s *fakeStream) event(key string, event map[string]any) {
	s.write(protocolcli.SessionStreamRecord{Record: protocolcli.RecordTaskEvent, Identity: &protocolcli.TaskIdentity{Key: key}, Event: event})
}

func (s *fakeStream) taskEnd(key, status string, code int) {
	s.write(protocolcli.SessionStreamRecord{
		Record:   protocolcli.RecordTaskEnd,
		Identity: &protocolcli.TaskIdentity{Key: key},
		Task:     &protocolcli.TaskRecord{Status: status, ExitCode: code},
	})
}

func (s *fakeStream) sessionEnd() {
	s.write(protocolcli.SessionStreamRecord{Record: protocolcli.RecordSessionEnd})
}
