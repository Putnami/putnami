//go:build linux || darwin

package machineproc

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"go.putnami.dev/tooling/cli/internal/abort"
	"go.putnami.dev/tooling/cli/internal/fileindex"
)

// connectTimeout bounds the connection to the supervisor: a supervisor that
// does not accept in that time is not used.
const connectTimeout = 500 * time.Millisecond

// lostRunExit is the exit code of a run the user interrupted and whose worker
// then went away: an interrupted run's code.
const lostRunExit = 130

// maxLog is the size past which a starting supervisor keeps its log as
// supervisor.log.1 and begins a new one.
const maxLog = 4 << 20

// startWanted records that this command looked for a supervisor and found
// none: StartIfWanted then starts one when the command ends.
var startWanted bool

// dial connects to the supervisor of dir and checks that it runs as this
// user.
func dial(dir string) (*conn, error) {
	d := net.Dialer{Timeout: connectTimeout}
	c, err := d.Dial("unix", filepath.Join(dir, socketName))
	if err != nil {
		return nil, err
	}
	uc, err := unixConn(c)
	if err != nil {
		return nil, err
	}
	if uid, err := peerUID(uc); err != nil || uid != os.Getuid() {
		_ = uc.Close()
		return nil, errPeer
	}
	return &conn{c: uc}, nil
}

// Serve runs args through the worker of this CLI's build, with the process's
// environment as it started (SaveProcessContext), its working directory,
// umask and standard streams. served is false when the machine process did
// not run the command: none is running, it refused, it failed before the run
// began, or its worker ended during the run; the caller then runs the command
// itself. A run the user interrupted and whose worker then ended answers
// lostRunExit.
func Serve(ctx context.Context, args []string) (code int, served bool) {
	if d := Decide(os.Getenv, ReadSetting(os.Getenv), DefaultOn); !d.Use {
		return 0, false
	}
	exe, err := os.Executable()
	if err != nil {
		return 0, false
	}
	build := fileindex.BuildIdentity(selfImage(exe))
	if build == "" {
		return 0, false
	}
	dir, err := Dir(os.Getenv, false)
	if err != nil {
		startWanted = true
		return 0, false
	}
	terminal := isTerminal(os.Stdin) && isTerminal(os.Stdout)
	if !terminal && hasTerminal() {
		// A terminal that is not the command's input and output (a
		// redirected run) would reach the run's jobs through /dev/tty only.
		recordLocal(dir, "the command has a terminal but its input or output is redirected")
		return 0, false
	}
	c, err := dial(dir)
	if err != nil {
		startWanted = true
		recordLocal(dir, "no supervisor answered")
		return 0, false
	}
	defer c.c.Close() //nolint:errcheck // the run is over
	wd, err := os.Getwd()
	if err != nil {
		return 0, false
	}
	umask := unix.Umask(0)
	unix.Umask(umask)
	env := processEnv
	if env == nil {
		env = os.Environ()
	}
	req := request{
		Protocol: ProtocolVersion, Op: "run", Build: build, Executable: exe,
		Args: args, Env: env, Dir: wd, Umask: umask, Fingerprint: processFingerprint(),
		Terminal: terminal, StderrTerminal: terminal && isTerminal(os.Stderr),
		CPUShare: ReadCPUShare(os.Getenv),
	}
	if err := c.send(req, os.Stdin, os.Stdout, os.Stderr); err != nil {
		recordLocal(dir, "the request could not be sent")
		return 0, false
	}
	code, served, reason := await(ctx, c)
	if !served {
		recordLocal(dir, reason)
	}
	return code, served
}

// contextRefusalNotice is what a command prints when the worker refused it
// for a process context other than its own (CodeContext): the command runs in
// its own process.
const contextRefusalNotice = "putnami: running in this process: the machine process's worker started in another process context (identity, groups, limits, priority or control group); `putnami machine stop` stops it\n"

// readyTimeout bounds the wait for the worker to take the request: a
// supervisor that does not answer, or a worker slow to start, costs at most
// this before the command runs in its own process.
const clientReadyTimeout = 15 * time.Second

// await reads the worker's events until the run's exit, and forwards a
// signal when ctx ends. The run starts only once the client sent "go" after
// the worker's Ready, so a client that stopped waiting runs the command
// itself and the worker never does. A run the machine process refused, or
// whose worker ended before its exit, is not served: the caller runs it
// itself, unless the user interrupted it.
func await(ctx context.Context, c *conn) (code int, served bool, reason string) {
	type received struct {
		ev    event
		files []*os.File
	}
	events := make(chan received)
	failed := make(chan error, 1)
	go func() {
		for {
			var ev event
			files, err := c.receive(&ev)
			if err != nil {
				failed <- err
				return
			}
			events <- received{ev, files}
		}
	}()
	var relay *terminalRelay
	defer func() {
		if relay != nil {
			relay.close()
		}
	}()
	ready := time.NewTimer(clientReadyTimeout)
	defer ready.Stop()
	readyC := ready.C
	gone, started, interrupted := false, false, false
	done := ctx.Done()
	forward := func() {
		if interrupted || !gone {
			return
		}
		interrupted = true
		kind := "terminate"
		if abort.Source() == abort.SourceUser {
			kind = "interrupt"
		}
		_ = c.send(clientSignal{Signal: kind})
	}
	lost := func(why string) (int, bool, string) {
		if interrupted || (relay != nil && relay.interrupted.Load()) {
			return lostRunExit, true, why
		}
		if started {
			_, _ = os.Stderr.WriteString("putnami: the machine process ended during the run; running it again in this process\n")
		}
		return 0, false, why
	}
	for {
		select {
		case <-readyC:
			return 0, false, "the machine process did not take the command in time"
		case <-done:
			done = nil
			if !gone {
				return 0, false, "interrupted before the worker took the command"
			}
			forward()
		case got := <-events:
			ev := got.ev
			if ev.Ready && ev.Terminal && !gone {
				if len(got.files) != 1 {
					closeAll(got.files)
					return 0, false, "the worker sent no terminal"
				}
				var err error
				if relay, err = startRelay(got.files[0]); err != nil {
					_ = got.files[0].Close()
					relay = nil
					return 0, false, "the terminal could not be relayed: " + err.Error()
				}
			} else {
				closeAll(got.files)
			}
			switch {
			case ev.Refused != "":
				if ev.Refused == CodeContext {
					// A worker of another process context is the one refusal
					// the command reports, in one line.
					_, _ = os.Stderr.WriteString(contextRefusalNotice)
				}
				return 0, false, "refused: " + ev.Refused
			case ev.Ready && !gone:
				readyC = nil
				if err := c.send(clientSignal{Signal: "go"}); err != nil {
					return 0, false, "the go could not be sent"
				}
				gone = true
				if ctx.Err() != nil {
					forward()
				}
			case ev.Lost:
				return lost("the worker ended during the run")
			case ev.Exit != nil:
				if relay != nil {
					relay.drain()
				}
				return *ev.Exit, true, ""
			case ev.Started:
				started = true
			}
		case <-failed:
			if !gone {
				return 0, false, "the connection closed before the worker took the command"
			}
			return lost("the connection closed before the run's exit")
		}
	}
}

// lastLocalName records why the last command meant for the machine process
// ran in its own process; `putnami machine status` shows it.
const lastLocalName = "last-local.json"

// LastLocal is why the last command meant for the machine process ran in its
// own process.
type LastLocal struct {
	Time   string `json:"time"`
	Reason string `json:"reason"`
	Dir    string `json:"dir"`
}

func recordLocal(dir, reason string) {
	wd, _ := os.Getwd()
	data, err := json.Marshal(LastLocal{Time: time.Now().UTC().Format(time.RFC3339), Reason: reason, Dir: wd})
	if err != nil {
		return
	}
	path := filepath.Join(dir, lastLocalName)
	tmp := path + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		_ = os.Rename(tmp, path)
	}
}

// ReadLastLocal is the last command that ran in its own process although the
// machine process was on, or nil.
func ReadLastLocal() *LastLocal {
	dir, err := Dir(os.Getenv, false)
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(dir, lastLocalName))
	if err != nil {
		return nil
	}
	var last LastLocal
	if json.Unmarshal(data, &last) != nil {
		return nil
	}
	return &last
}

// Query sends op ("status" or "stop") to the running supervisor.
func Query(op string) (*Reply, error) {
	dir, err := Dir(os.Getenv, false)
	if err != nil {
		return nil, ErrNotRunning
	}
	c, err := dial(dir)
	if err != nil {
		return nil, ErrNotRunning
	}
	defer c.c.Close() //nolint:errcheck // one request
	if err := c.send(request{Protocol: ProtocolVersion, Op: op}); err != nil {
		return nil, err
	}
	var r Reply
	if _, err := c.receive(&r); err != nil {
		return nil, err
	}
	return &r, nil
}

// ErrNotRunning says no supervisor answers.
var ErrNotRunning = errors.New("no machine process is running")

// StartIfWanted starts a supervisor, detached, when this command looked for
// one and found none. It never waits for it.
func StartIfWanted() {
	if !startWanted {
		return
	}
	startWanted = false
	_ = Start()
}

// Start starts a supervisor from this executable, detached in its own
// session, its output appended to its log. A supervisor that already runs
// makes the new one exit at once (its lock).
func Start() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	dir, err := Dir(os.Getenv, true)
	if err != nil {
		return err
	}
	logPath := filepath.Join(dir, logName)
	if info, err := os.Stat(logPath); err == nil && info.Size() > maxLog {
		_ = os.Rename(logPath, logPath+".1")
	}
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer log.Close()                                  //nolint:errcheck // the child holds its own copy
	cmd := exec.Command(exe, "__machine", "supervise") //nolint:gosec // this executable
	cmd.Dir = dir
	cmd.Env = supervisorEnv(os.Environ())
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
