//go:build linux || darwin

package machineproc

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"go.putnami.dev/tooling/cli/internal/fileindex"
)

// connectTimeout bounds the connection to the supervisor: a supervisor that
// does not accept in that time is not used.
const connectTimeout = 500 * time.Millisecond

// lostRunExit is the exit code of a run whose worker went away after it
// started: an interrupted run's code.
const lostRunExit = 130

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
// not run the command: none is running, it refused, or it failed before the
// run began; the caller then runs the command itself. A run that began and
// lost its worker answers lostRunExit.
func Serve(ctx context.Context, args []string) (code int, served bool) {
	if d := Decide(os.Getenv, ReadSetting(os.Getenv), DefaultOn); !d.Use {
		return 0, false
	}
	exe, err := os.Executable()
	if err != nil {
		return 0, false
	}
	build := fileindex.BuildIdentity(exe)
	if build == "" {
		return 0, false
	}
	dir, err := Dir(os.Getenv, false)
	if err != nil {
		startWanted = true
		return 0, false
	}
	c, err := dial(dir)
	if err != nil {
		startWanted = true
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
		Args: args, Env: env, Dir: wd, Umask: umask,
	}
	if err := c.send(req, os.Stdin, os.Stdout, os.Stderr); err != nil {
		return 0, false
	}
	return await(ctx, c)
}

// await reads the worker's events until the run's exit, and forwards an
// interrupt when ctx ends.
func await(ctx context.Context, c *conn) (int, bool) {
	events := make(chan event)
	failed := make(chan error, 1)
	go func() {
		for {
			var ev event
			if _, err := c.receive(&ev); err != nil {
				failed <- err
				return
			}
			events <- ev
		}
	}()
	started, interrupted := false, false
	done := ctx.Done()
	for {
		select {
		case <-done:
			done = nil
			if started && !interrupted {
				interrupted = true
				_ = c.send(signal{Signal: "interrupt"})
			}
		case ev := <-events:
			switch {
			case ev.Refused != "":
				return 0, false
			case ev.Exit != nil:
				return *ev.Exit, true
			case ev.Started:
				started = true
				if ctx.Err() != nil && !interrupted {
					interrupted = true
					_ = c.send(signal{Signal: "interrupt"})
				}
			}
		case <-failed:
			if !started {
				return 0, false
			}
			_, _ = os.Stderr.WriteString("putnami: the machine process ended during the run\n")
			return lostRunExit, true
		}
	}
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
	log, err := os.OpenFile(filepath.Join(dir, logName), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
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
