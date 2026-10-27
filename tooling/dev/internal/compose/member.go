package compose

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/sdk/extension/proctree"
)

// A member runs as an ordinary serve run of the putnami CLI:
//
//	putnami serve --projects <member> --port 0 [--no-watch] --serve-env - --output=jsonl [--no-color]
//
// Its environment document arrives on stdin and reaches only its serve step.
// The run writes its session stream on stdout, which compose reads for the
// serve step's process group, its readiness claims and its end, and renders its
// serve log as text on stderr, which compose passes through to Options.Logs and
// keeps the last lines of. The engine plans the serve pipeline and starts the
// serve step; compose never does.
//
// The run inherits this process's environment without interactiveEnv: putnami
// sets it for the interactive command that started this executable, and a
// serve run is no interactive command. The tasks the run starts inherit its
// environment, and a task that reads interactiveEnv writes no events.

// interactiveEnv is the variable putnami sets, to 1, in the environment of an
// interactive command's process.
const interactiveEnv = "PUTNAMI_INTERACTIVE"

// serveRunEnviron is environ, a process environment, without interactiveEnv.
func serveRunEnviron(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, entry := range environ {
		if name, _, _ := strings.Cut(entry, "="); name != interactiveEnv {
			out = append(out, entry)
		}
	}
	return out
}

// Variables compose sets in a member's environment document.
const (
	envPort           = "PORT"
	envNodeEnv        = "NODE_ENV"
	envAppEnv         = "APP_ENV"
	ephemeralPort     = "0"
	productionNodeEnv = "production"
	localAppEnv       = "local"
)

// serveRunWaitDelay bounds how long a member's serve run may keep its output
// pipes open after it exited, through a process it left behind.
const serveRunWaitDelay = 5 * time.Second

// eventTypeProcess is the type of the task event the CLI writes in a session
// stream before any event of a step's process: it names the process group
// the step runs in and its leader's start time.
const eventTypeProcess = "process"

// serveRunArgs is the argument vector of a member's serve run after the
// executable. Only a watched target watches; every member binds an ephemeral
// port and announces it. The run renders its text as opts.Verbose, opts.Quiet
// and opts.NoColor ask.
func serveRunArgs(projectID string, watch bool, opts Options) []string {
	args := []string{serveCommand, "--projects", projectID, "--port", ephemeralPort}
	if !watch {
		args = append(args, "--no-watch")
	}
	args = append(args, "--serve-env", "-", "--output=jsonl")
	for _, flag := range []struct {
		set  bool
		name string
	}{
		{opts.Verbose, "--verbose"},
		{opts.Quiet, "--quiet"},
		{opts.NoColor, "--no-color"},
	} {
		if flag.set {
			args = append(args, flag.name)
		}
	}
	return args
}

// memberRuntime is a running member: its proxy, its serve run, and what the
// run's session stream said about it.
type memberRuntime struct {
	member   *Member
	proxy    *proxy
	env      map[string]string
	sections []string
	watch    bool

	// executable, root and args start the serve run; logs receives its stderr,
	// and failures the report of a failed preparation (failureReport).
	executable string
	root       string
	args       []string
	logs       io.Writer
	failures   io.Writer

	// observeProcess is told every process group of the member — the serve
	// run's own, then each serve step's the stream names — with its leader's
	// start time. releaseProcess is told each group once its process ended.
	observeProcess func(pgid int, leaderStart string)
	releaseProcess func(pgid int)

	tree     *proctree.Tree
	stopping atomic.Bool
	// spawned closes on the serve step's first process event. The ready
	// timeout and readyMs count from it: the finite steps the serve run runs
	// before it are bounded by their own task timeouts, not by the ready
	// timeout. Only the stream reader writes spawnedAt.
	spawned   chan struct{}
	spawnOnce sync.Once
	spawnedAt time.Time
	ready     chan struct{}
	readyOnce sync.Once
	readyMs   atomic.Int64
	exited    chan struct{}
	// stoppedBeforeReady closes when a serve step ends without ever having
	// announced readiness. Under watch the serve run outlives that step (it
	// waits for a change), so exited alone would let a failed start run into
	// the ready timeout.
	stoppedBeforeReady chan struct{}
	stoppedOnce        sync.Once
	// prepareFailed closes when the serve run failed before its serve step
	// started: a finite step of the pipeline failed, or the run exited first,
	// as on a usage error.
	prepareFailed chan struct{}
	prepareOnce   sync.Once
	// startupCompleted closes on the first workload claim: the member's
	// application reports that its whole startup finished, which a server claim
	// (a listening port) does not. stoppedBeforeStartup is its counterpart of
	// stoppedBeforeReady.
	startupCompleted     chan struct{}
	startupOnce          sync.Once
	stoppedBeforeStartup chan struct{}
	stoppedStartupOnce   sync.Once

	mu sync.Mutex
	// serveKey is the identity key of the serve step, learned from its process
	// event; serveGroups are the groups of its current process.
	serveKey    string
	serveGroups []int
	// tail is the last lines of the run's text output: its stderr and the
	// lines of its stdout that are no record. streamTail is the last lines of
	// the member's output as the session stream carries it, in order: the
	// lines of stdout that are no record, the serve step's log events, and
	// the diagnostics and error of the serve step's end. stdout and stderr are
	// two pipes with no order between them, so when the serve step's end
	// arrives, its stderr may still be in flight; streamTail is then complete.
	// serveEnded reports that the current serve step's end arrived.
	tail       tailLines
	streamTail tailLines
	serveEnded bool
	// lastStatus is how the member's serve last ended: the status of the
	// serve step's end, empty while a serve step runs, and the serve run's
	// exit once the run ended after anything but a serve step's own verdict.
	// stepVerdict reports that lastStatus is such a verdict: a serve step's end
	// that the run did not cancel.
	lastStatus  string
	stepVerdict bool
	prepareErr  string
	// stepOutput is the log of each step that ran before the serve step
	// started, dropped once the serve step started. failedOutput is the
	// output of the step that failed the preparation: its log, diagnostics
	// and error.
	stepOutput   map[string][]string
	failedOutput []string
	failedStep   bool
}

func newMemberRuntime(member *Member, p *proxy, env map[string]string, sections []string, watchMode bool) *memberRuntime {
	return &memberRuntime{
		member:   member,
		proxy:    p,
		env:      env,
		sections: sections,
		watch:    watchMode,
		spawned:  make(chan struct{}),
		ready:    make(chan struct{}),
		exited:   make(chan struct{}),

		stoppedBeforeReady:   make(chan struct{}),
		prepareFailed:        make(chan struct{}),
		startupCompleted:     make(chan struct{}),
		stoppedBeforeStartup: make(chan struct{}),
	}
}

// start starts the member's serve run. Its process group is recorded before
// anything else can happen to it; the run's end closes exited.
func (rt *memberRuntime) start() error {
	document, err := json.Marshal(rt.env)
	if err != nil {
		return fmt.Errorf("encode the environment document: %w", err)
	}
	cmd := exec.Command(rt.executable, rt.args...)
	cmd.Dir = rt.root
	cmd.Env = serveRunEnviron(os.Environ())
	cmd.Stdin = bytes.NewReader(document)
	rt.stepOutput = make(map[string][]string)
	output := newOutputSink(rt.logs, rt.addOutput)
	stream := &lineWriter{line: func(line []byte) { rt.observeLine(line, output) }}
	cmd.Stdout = stream
	cmd.Stderr = output
	cmd.WaitDelay = serveRunWaitDelay
	tree := proctree.New(cmd)
	if err := tree.Start(); err != nil {
		return fmt.Errorf("start %s %s: %w", rt.executable, rt.args[0], err)
	}
	rt.tree = tree
	group := tree.ID()
	leaderStart, _ := proctree.StartTime(group)
	rt.observeProcess(group, leaderStart)
	go func() {
		err := cmd.Wait()
		stream.flush()
		output.flush()
		rt.serveRunEnded(err)
		if rt.failures != nil && rt.preparationFailed() {
			_, _ = io.WriteString(rt.failures, rt.failureReport())
		}
		_ = tree.Close()
		rt.releaseProcess(group)
		close(rt.exited)
	}()
	return nil
}

// observeLine reads one line of the serve run's stdout. A line that is not a
// session stream record joins the output.
func (rt *memberRuntime) observeLine(line []byte, output *outputSink) {
	var record protocolcli.SessionStreamRecord
	if json.Unmarshal(line, &record) != nil || record.Record == "" {
		rt.mu.Lock()
		rt.streamTail.add(stripANSI(string(line)))
		rt.mu.Unlock()
		_, _ = output.Write(append(append([]byte(nil), line...), '\n'))
		return
	}
	key := ""
	if record.Identity != nil {
		key = record.Identity.Key
	}
	switch record.Record {
	case protocolcli.RecordTaskEvent:
		rt.observeEvent(key, record.Event)
	case protocolcli.RecordTaskEnd:
		if record.Task != nil {
			rt.observeTaskEnd(key, record.Task)
		}
	}
}

// observeEvent reads a task event. The CLI's process event names the serve
// step and its process group, before any event the step emits; a readiness
// claim counts only from that step. Before it, a step's log events are its
// output for a failure report.
func (rt *memberRuntime) observeEvent(key string, event map[string]any) {
	switch event["type"] {
	case eventTypeProcess:
		group, _ := event["group"].(float64)
		leaderStart, _ := event["leaderStart"].(string)
		if group <= 0 || key == "" {
			return
		}
		rt.mu.Lock()
		rt.serveKey = key
		rt.serveGroups = append(rt.serveGroups, int(group))
		// A serve step runs: no end describes the member's serve yet. The
		// preparation succeeded, so no step's output reports a failure.
		rt.lastStatus, rt.stepVerdict = "", false
		rt.serveEnded = false
		rt.stepOutput = nil
		rt.mu.Unlock()
		rt.observeProcess(int(group), leaderStart)
		rt.spawnOnce.Do(func() {
			rt.spawnedAt = time.Now()
			close(rt.spawned)
		})
	case eventTypeLog:
		rt.mu.Lock()
		defer rt.mu.Unlock()
		line := logLine(event)
		switch {
		case key == "" || line == "":
		case key == rt.serveKey:
			rt.streamTail.add(line)
		case rt.stepOutput != nil:
			rt.stepOutput[key] = lastLines(append(rt.stepOutput[key], line), stepOutputLines)
		}
	case string(runtimeproto.EventReady):
		rt.mu.Lock()
		serve := key != "" && key == rt.serveKey
		rt.mu.Unlock()
		if !serve {
			return
		}
		if claim, err := runtimeproto.ExtractReadyData(event); err == nil && claim != nil {
			rt.observeClaim(claim)
		}
	}
}

// observeClaim records a typed readiness claim. A claim with an endpoint moves
// the proxy to the announced port; a claim without one (a workload claim)
// leaves the backend where an earlier claim put it, because it says nothing
// about where the member listens. A workload claim also records that the
// member's application completed its startup. A claim outside the closed
// target vocabulary is ignored: the member stays not ready.
func (rt *memberRuntime) observeClaim(claim *runtimeproto.ReadyData) {
	if claim.Target != runtimeproto.ReadyTargetServer && claim.Target != runtimeproto.ReadyTargetWorkload {
		return
	}
	if port := claimedPort(claim); port > 0 {
		rt.proxy.setBackend(port)
	}
	rt.readyOnce.Do(func() {
		rt.readyMs.Store(time.Since(rt.spawnedAt).Milliseconds())
		close(rt.ready)
	})
	if claim.Target == runtimeproto.ReadyTargetWorkload {
		rt.startupOnce.Do(func() { close(rt.startupCompleted) })
	}
}

// claimedPort is the port a claim announces: its first http(s) endpoint's,
// else its first endpoint's, else 0.
func claimedPort(claim *runtimeproto.ReadyData) int {
	for _, endpoint := range claim.Endpoints {
		if endpoint.Scheme == runtimeproto.ReadySchemeHTTP || endpoint.Scheme == runtimeproto.ReadySchemeHTTPS {
			return endpoint.Port
		}
	}
	if len(claim.Endpoints) > 0 {
		return claim.Endpoints[0].Port
	}
	return 0
}

// observeTaskEnd reads a task's end. The serve step's end empties the proxy's
// backend and releases its process group; a finite step that fails before any
// serve step started fails the preparation, with its own output as the
// failure report.
func (rt *memberRuntime) observeTaskEnd(key string, task *protocolcli.TaskRecord) {
	status := task.Status
	if task.ExitCode != 0 {
		status += ", exit code " + strconv.Itoa(task.ExitCode)
	}
	rt.mu.Lock()
	serve := key != "" && key == rt.serveKey
	spawned := rt.serveKey != ""
	var groups []int
	if serve {
		groups, rt.serveGroups = rt.serveGroups, nil
		rt.lastStatus, rt.stepVerdict = status, task.Status != protocolcli.TaskStatusCanceled
		for _, line := range taskOutput(task) {
			rt.streamTail.add(line)
		}
		rt.serveEnded = true
	}
	failed := task.Status == protocolcli.TaskStatusFailed && !spawned && !rt.isReady()
	if failed && rt.stepOutput != nil && !rt.failedStep {
		rt.failedStep = true
		rt.failedOutput = append(append([]string(nil), rt.stepOutput[key]...), taskOutput(task)...)
	}
	if rt.stepOutput != nil {
		delete(rt.stepOutput, key)
	}
	rt.mu.Unlock()
	if serve {
		rt.proxy.setBackend(0)
		for _, group := range groups {
			rt.releaseProcess(group)
		}
		rt.stepStopped()
		return
	}
	if failed {
		rt.failPreparation("the serve run's step " + key + " failed before the serve step started (" + status + ")")
	}
}

// serveRunEnded records how the serve run ended. A run that never started a
// serve step failed its preparation. The run's own exit becomes the member's
// last status unless the serve step's end, as the step itself ended, is the
// last thing that happened: while a serve step runs no end describes it, and a
// canceled step is one the run stopped, not the step's verdict.
func (rt *memberRuntime) serveRunEnded(err error) {
	rt.proxy.setBackend(0)
	code := 0
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		code = exitErr.ExitCode()
	case err != nil:
		code = -1
	}
	rt.mu.Lock()
	spawned := rt.serveKey != ""
	if !rt.stepVerdict {
		rt.lastStatus = "serve run ended, exit code " + strconv.Itoa(code)
	}
	rt.mu.Unlock()
	if !spawned && !rt.stopping.Load() {
		rt.failPreparation("the serve run exited before the serve step started (exit code " + strconv.Itoa(code) + ")")
	}
	rt.stepStopped()
}

// stepStopped signals the waiters of a claim that did not arrive, after the
// exit was recorded, so a waiter reads the status.
func (rt *memberRuntime) stepStopped() {
	if !rt.isReady() {
		rt.stoppedOnce.Do(func() { close(rt.stoppedBeforeReady) })
	}
	select {
	case <-rt.startupCompleted:
	default:
		rt.stoppedStartupOnce.Do(func() { close(rt.stoppedBeforeStartup) })
	}
}

func (rt *memberRuntime) isReady() bool {
	select {
	case <-rt.ready:
		return true
	default:
		return false
	}
}

func (rt *memberRuntime) failPreparation(message string) {
	rt.mu.Lock()
	if rt.prepareErr == "" {
		rt.prepareErr = message
	}
	rt.mu.Unlock()
	rt.prepareOnce.Do(func() { close(rt.prepareFailed) })
}

// preparationFailed reports whether the serve run failed before its serve
// step started.
func (rt *memberRuntime) preparationFailed() bool {
	select {
	case <-rt.prepareFailed:
		return true
	default:
		return false
	}
}

// eventTypeLog is the type of a runtime log event.
const eventTypeLog = "log"

// stepOutputLines bounds the log lines kept of each step before the serve step
// starts. The CLI bounds a failed task's error to the last 50 lines of its
// stderr the same way.
const stepOutputLines = 50

// failureReport is the text a failures writer receives once the preparation
// failed: the member and why, then the output of the step that failed it — its
// log events, its diagnostics and its error, from the session stream — or,
// when no step failed, as when the run refused its arguments, the last lines
// the run wrote. Steps that succeeded, and the serve log, are not in it.
func (rt *memberRuntime) failureReport() string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	lines := rt.preparationOutput()
	var report strings.Builder
	report.WriteString(rt.member.Project.ID + ": " + rt.prepareErr)
	if len(lines) == 0 {
		report.WriteString(", without output\n")
		return report.String()
	}
	report.WriteString(":\n")
	for _, line := range lines {
		report.WriteString("  | " + line + "\n")
	}
	return report.String()
}

// preparationOutput is the output of a failed preparation: the failed step's
// own, read from the session stream, else the tail of the run's output. The
// step's own output is complete once its end arrived, while its stderr may
// still be in flight. The caller holds rt.mu.
func (rt *memberRuntime) preparationOutput() []string {
	if rt.failedStep {
		return append([]string(nil), rt.failedOutput...)
	}
	return rt.tail.snapshot()
}

// logLine is a log event as one line of output, "<level>: <message>", or ""
// for a debug or trace event and for one without a message.
func logLine(event map[string]any) string {
	level, _ := event["level"].(string)
	message, _ := event["message"].(string)
	message = strings.TrimSpace(stripANSI(message))
	switch {
	case message == "", level == "debug", level == "trace":
		return ""
	case level == "":
		return message
	default:
		return level + ": " + message
	}
}

// taskOutput is what a task's end record says of its output: its diagnostics,
// then its error message, which holds the last lines of its stderr.
func taskOutput(task *protocolcli.TaskRecord) []string {
	var lines []string
	for _, diagnostic := range task.Diagnostics {
		line := diagnostic.Severity + ": " + diagnostic.Message
		switch {
		case diagnostic.File != "" && diagnostic.Line > 0:
			line = diagnostic.File + ":" + strconv.Itoa(diagnostic.Line) + ": " + line
		case diagnostic.File != "":
			line = diagnostic.File + ": " + line
		}
		lines = append(lines, stripANSI(line))
	}
	if task.Error != nil {
		for _, line := range strings.Split(stripANSI(task.Error.Message), "\n") {
			if line = strings.TrimRight(line, "\r"); strings.TrimSpace(line) != "" {
				lines = append(lines, line)
			}
		}
	}
	return lines
}

// lastLines is the last n of lines.
func lastLines(lines []string, n int) []string {
	if len(lines) > n {
		return append([]string(nil), lines[len(lines)-n:]...)
	}
	return lines
}

// addOutput keeps a line of the serve run's output for a failure's detail.
func (rt *memberRuntime) addOutput(line []byte) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.tail.add(stripANSI(string(line)))
}

// exitDetail is how the member's serve last ended, and the last lines of its
// output. Once the serve step's end arrived, the output is the session
// stream's, which holds all of the step's output by then; the run's text
// output may still miss lines the step wrote before its end. Before that end,
// or when the stream carried no output, it is the run's text output.
func (rt *memberRuntime) exitDetail() (string, []string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if lines := rt.streamTail.snapshot(); rt.serveEnded && len(lines) > 0 {
		return rt.lastStatus, lines
	}
	return rt.lastStatus, rt.tail.snapshot()
}

// stop ends the member's serve run: SIGTERM to its process group, which the
// CLI answers as an interrupted `putnami serve` does — it stops its serve step
// and exits — then SIGKILL once memberStopTimeout passed or ctx is done. It
// waits for the killed run no longer than ctx allows, and names a run that is
// still running after that.
func (rt *memberRuntime) stop(ctx context.Context) string {
	if rt.tree == nil {
		return ""
	}
	rt.stopping.Store(true)
	select {
	case <-rt.exited:
		return ""
	default:
	}
	_ = rt.tree.Terminate()
	timer := time.NewTimer(memberStopTimeout)
	defer timer.Stop()
	select {
	case <-rt.exited:
		return ""
	case <-timer.C:
	case <-ctx.Done():
	}
	_ = rt.tree.Kill()
	killed := time.NewTimer(serveRunWaitDelay + groupReleaseWait)
	defer killed.Stop()
	select {
	case <-rt.exited:
		return ""
	case <-killed.C:
	case <-ctx.Done():
	}
	if ctx.Err() != nil {
		return "serve run of " + rt.member.Project.ID + " did not stop before the teardown deadline"
	}
	return "serve run of " + rt.member.Project.ID + " did not stop within " + memberStopTimeout.String()
}

// waitReady blocks until the member announced readiness, its serve run failed
// or ended, or timeout elapsed after its serve step started.
func (rt *memberRuntime) waitReady(ctx context.Context, timeout time.Duration) error {
	id := rt.member.Project.ID
	select {
	case <-rt.spawned:
	case <-rt.prepareFailed:
		return rt.notReady(id)
	case <-rt.exited:
		return rt.notReady(id)
	case <-ctx.Done():
		return canceledBeforeReady(id, ctx.Err())
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-rt.ready:
		return nil
	case <-rt.prepareFailed:
		return rt.notReady(id)
	case <-rt.stoppedBeforeReady:
		return rt.notReady(id)
	case <-rt.exited:
		return rt.notReady(id)
	case <-timer.C:
		_, tail := rt.exitDetail()
		err := newError(CodeReadyTimeout, id, PhaseReadiness,
			"no typed ready event within "+timeout.String())
		err.Detail = tail
		return err
	case <-ctx.Done():
		return canceledBeforeReady(id, ctx.Err())
	}
}

// canceledBeforeReady is the failure of a start canceled before the member's
// ready event.
func canceledBeforeReady(id string, cause error) error {
	return newError(CodeStartFailed, id, PhaseReadiness, "canceled while waiting for the ready event: "+cause.Error())
}

// notReady is the failure of a member whose serve run stopped short of its
// ready event: a failed preparation when the serve step never started, the
// serve step's exit otherwise.
func (rt *memberRuntime) notReady(id string) error {
	select {
	case <-rt.prepareFailed:
		rt.mu.Lock()
		message := rt.prepareErr
		output := lastLines(rt.preparationOutput(), maxDetailLines)
		rt.mu.Unlock()
		err := newError(CodePrepareFailed, id, PhasePrepare, message)
		err.Detail = output
		return err
	default:
	}
	return rt.exitedBefore(id, "its ready event")
}

// exitedBefore is the failure of a serve step that ended before what, with its
// exit status and the tail of its output.
func (rt *memberRuntime) exitedBefore(id, what string) error {
	status, tail := rt.exitDetail()
	err := newError(CodeMemberExited, id, PhaseReadiness, "the serve step exited before "+what)
	if status != "" {
		err.Message += " (" + status + ")"
	}
	err.Detail = tail
	return err
}

// waitStartup blocks until the member's application reported completed
// startup (a workload claim), its serve step stopped first, or ctx is done. A
// claim observed before the stop counts: the member did complete its startup.
func (rt *memberRuntime) waitStartup(ctx context.Context) error {
	select {
	case <-rt.startupCompleted:
		return nil
	default:
	}
	select {
	case <-rt.startupCompleted:
		return nil
	case <-rt.stoppedBeforeStartup:
	case <-rt.exited:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-rt.startupCompleted:
		return nil
	default:
	}
	return rt.exitedBefore(rt.member.Project.ID, "its application reported completed startup")
}

// memberEnv is a member's environment document, the variables its serve step
// alone receives: an ephemeral port, production mode for a dependency and for
// an unwatched target, and the CONFIG_DATA document when there is one.
//
// Production mode selects the build, not the configuration: a composition runs
// on this machine, so a production member still reads its local config files
// (conf/.env.local.yaml, conf/.secrets.local.yaml), as a watched one does.
// APP_ENV names that config environment; a TypeScript runtime would otherwise
// derive it from NODE_ENV. An APP_ENV the caller exported is kept: the serve
// run inherits it.
func memberEnv(production bool, document []byte) map[string]string {
	env := map[string]string{envPort: ephemeralPort}
	if production {
		env[envNodeEnv] = productionNodeEnv
		if os.Getenv(envAppEnv) == "" {
			env[envAppEnv] = localAppEnv
		}
	}
	if len(document) > 0 {
		env[ConfigDataEnv] = string(document)
	}
	return env
}

// describeProcessGroup is how a leftover process group is named.
func describeProcessGroup(member string, pgid int) string {
	return fmt.Sprintf("process group %s of %s", strconv.Itoa(pgid), member)
}

// lineWriter hands every complete line written to it, without its newline, to
// line; flush hands it an unterminated last one.
type lineWriter struct {
	mu   sync.Mutex
	buf  []byte
	line func([]byte)
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.line(bytes.TrimSuffix(w.buf[:i], []byte("\r")))
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

func (w *lineWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) > 0 {
		w.line(w.buf)
		w.buf = nil
	}
}

// outputSink is a serve run's text output: passed through to logs as it
// arrives, and split into lines for the failure detail. A write never fails,
// so a closed log stream cannot stall the run on a full pipe.
type outputSink struct {
	mu    sync.Mutex
	logs  io.Writer
	lines *lineWriter
}

func newOutputSink(logs io.Writer, line func([]byte)) *outputSink {
	return &outputSink{logs: logs, lines: &lineWriter{line: line}}
}

func (s *outputSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.logs != nil {
		_, _ = s.logs.Write(p)
	}
	return s.lines.Write(p)
}

func (s *outputSink) flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lines.flush()
}

// ansiEscape matches the terminal control sequences a text renderer colors its
// output with.
var ansiEscape = regexp.MustCompile("\x1b\\[[0-9;?]*[A-Za-z]")

func stripANSI(line string) string { return ansiEscape.ReplaceAllString(line, "") }
