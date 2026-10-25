package compose

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

// The composition tests start each member the way Up does in production, as a
// serve run of the executable Up resolves: this test binary, which TestMain
// turns into a fake putnami CLI when its first argument is a command word. The
// fake reads the member's fixture.json, starts the fixture workload as its serve
// step in a process group of its own, and writes the records compose reads:
// the serve step's process event, its readiness claims and its end. It renders
// the workload's log on stderr, as a `--serve-env` jsonl run does.

// Fake CLI behaviors, the cli member of a project's fixture.json.
const (
	// fakeCLIUsage exits 2 with a usage error on stderr and no record, as the
	// CLI does on a flag it refuses.
	fakeCLIUsage = "usage"
	// fakeCLIFailPrepare fails the serve pipeline's finite generate step and
	// exits 1 without starting the serve step.
	fakeCLIFailPrepare = "fail-prepare"
	// fakeCLIStrayLine writes a line that is not a record on stdout first.
	fakeCLIStrayLine = "stray-line"
	// fakeCLIRestart replaces the serve step once, after its first readiness
	// claim, as a watched run does on a change.
	fakeCLIRestart = "restart"
	// fakeCLISlowPrepare runs the generate step for fakeCLISlowPrepareDelay
	// before it starts the serve step.
	fakeCLISlowPrepare = "slow-prepare"
)

// fakeCLISlowPrepareDelay is how long a slow-prepare run's generate step takes.
const fakeCLISlowPrepareDelay = 3 * time.Second

// Files the fake CLI writes in the project directory, and the stop order it
// appends to at the workspace root.
const (
	fakeCLIArgsFile   = "argv.json"
	fakeCLIGroupsFile = "groups.log"
	fakeCLIStopFile   = "stop-order.log"
)

// fakeCLIInterrupted is the fake's exit code once SIGTERM stopped it.
const fakeCLIInterrupted = 130

// isFakeCLIInvocation reports whether this test binary was started as a member's
// serve run: a test run's first argument is a -test flag.
func isFakeCLIInvocation(args []string) bool {
	return len(args) > 1 && args[1] != "" && !strings.HasPrefix(args[1], "-")
}

// fakeServeRun is a parsed serve run of the fake CLI.
type fakeServeRun struct {
	root    string
	project string
	dir     string
	watch   bool
	fixture diskFixture
	out     *fakeStream
}

// runFakeCLI runs the fake CLI over args, the arguments after the executable.
func runFakeCLI(args []string) int {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	run, err := parseFakeServeRun(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "putnami: "+err.Error())
		return 2
	}
	document, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "putnami: read --serve-env: "+err.Error())
		return 2
	}
	var env map[string]string
	if err := json.Unmarshal(document, &env); err != nil {
		fmt.Fprintln(os.Stderr, "putnami: --serve-env: "+err.Error())
		return 2
	}

	switch run.fixture.CLI {
	case fakeCLIUsage:
		fmt.Fprintln(os.Stderr, "putnami: serve: unknown flag --fixture-refused")
		return 2
	case fakeCLIStrayLine:
		run.out.raw("a line that is not a record")
	}
	generate := run.project + ":serve~generate"
	if run.fixture.CLI == fakeCLIFailPrepare {
		fmt.Fprintln(os.Stderr, "["+generate+"] generate failed: fixture boom")
		run.out.taskEnd(generate, protocolcli.TaskStatusFailed, 1)
		run.out.sessionEnd()
		return 1
	}
	if run.fixture.CLI == fakeCLISlowPrepare {
		time.Sleep(fakeCLISlowPrepareDelay)
	}
	run.out.taskEnd(generate, protocolcli.TaskStatusSuccess, 0)
	return run.serve(env, signals)
}

func parseFakeServeRun(args []string) (*fakeServeRun, error) {
	root, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	run := &fakeServeRun{root: root, watch: true, out: &fakeStream{}}
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--projects":
			if i+1 < len(args) {
				i++
				run.project = args[i]
			}
		case "--no-watch":
			run.watch = false
		}
	}
	if run.project == "" {
		return nil, fmt.Errorf("serve: no --projects")
	}
	run.dir = filepath.Join(root, strings.TrimPrefix(run.project, "/"))
	argv, _ := json.Marshal(args)
	if err := os.WriteFile(filepath.Join(run.dir, fakeCLIArgsFile), argv, 0o600); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(run.dir, "fixture.json"))
	if err == nil {
		err = json.Unmarshal(data, &run.fixture)
	}
	if err != nil {
		return nil, fmt.Errorf("fixture.json: %w", err)
	}
	return run, nil
}

// serve runs the serve step until it ends, then, under watch, until a signal.
func (r *fakeServeRun) serve(env map[string]string, signals <-chan os.Signal) int {
	key := r.project + ":serve~serve"
	for attempt := 0; ; attempt++ {
		workload, err := r.startWorkload(key, env)
		if err != nil {
			fmt.Fprintln(os.Stderr, "putnami: start the serve step: "+err.Error())
			r.out.taskEnd(key, protocolcli.TaskStatusFailed, 1)
			r.out.sessionEnd()
			return 1
		}
		var restart <-chan struct{}
		if r.fixture.CLI == fakeCLIRestart && attempt == 0 {
			restart = workload.ready
		}
		select {
		case <-workload.done:
			status := protocolcli.TaskStatusSuccess
			if workload.code != 0 {
				status = protocolcli.TaskStatusFailed
			}
			r.out.taskEnd(key, status, workload.code)
			if !r.watch {
				r.out.sessionEnd()
				if workload.code != 0 {
					return 1
				}
				return 0
			}
			<-signals
			r.recordStop()
			r.out.sessionEnd()
			return fakeCLIInterrupted
		case <-restart:
			workload.stop()
			r.out.taskEnd(key, protocolcli.TaskStatusCanceled, 0)
		case <-signals:
			r.recordStop()
			workload.stop()
			r.out.taskEnd(key, protocolcli.TaskStatusCanceled, 0)
			r.out.sessionEnd()
			return fakeCLIInterrupted
		}
	}
}

// recordStop appends the member to the workspace's stop order.
func (r *fakeServeRun) recordStop() {
	file, err := os.OpenFile(filepath.Join(r.root, fakeCLIStopFile), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = file.WriteString(r.project + "\n")
	_ = file.Close()
}

// fakeWorkload is the serve step's process.
type fakeWorkload struct {
	tree  *proctree.Tree
	ready chan struct{}
	done  chan struct{}
	code  int
}

// startWorkload starts the fixture workload in its own process group, names
// that group in the serve step's process event, and turns the workload's
// runtime events into the serve step's records and log.
func (r *fakeServeRun) startWorkload(key string, env map[string]string) (*fakeWorkload, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(executable)
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(),
		fixtureModeEnv+"="+r.fixture.Mode,
		fixtureMemberEnv+"="+r.fixture.Member,
		fixtureRecordEnv+"="+r.fixture.Record,
	)
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
	r.recordGroup(group)
	r.out.event(key, map[string]any{"type": "process", "group": group, "leaderStart": leaderStart})

	workload := &fakeWorkload{tree: tree, ready: make(chan struct{}), done: make(chan struct{})}
	go func() {
		var readyOnce sync.Once
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
			r.out.event(key, event)
			switch event["type"] {
			case "log":
				message, _ := event["message"].(string)
				fmt.Fprintln(os.Stderr, "["+r.project+"] "+message)
			case "ready":
				readyOnce.Do(func() { close(workload.ready) })
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

// recordGroup appends a serve step's process group to the project's record.
func (r *fakeServeRun) recordGroup(group int) {
	file, err := os.OpenFile(filepath.Join(r.dir, fakeCLIGroupsFile), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(file, "%d\n", group)
	_ = file.Close()
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
	s.raw(string(data))
}

func (s *fakeStream) raw(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Println(line)
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
