//go:build linux || darwin

package machineproc

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"go.putnami.dev/tooling/cli/internal/abort"
)

// RunFunc runs one command in this process, as main would: the worker calls
// it with the client's arguments once it has applied the client's context.
type RunFunc func(ctx context.Context, args []string) int

// ownFingerprint and ownRuntimeEnv are the worker's, read when it starts.
var ownFingerprint, ownRuntimeEnv string

// goTimeout bounds the wait for a client's go after Ready.
const goTimeout = 30 * time.Second

// Work runs a worker on the control channel its supervisor passed as
// descriptor 3, until that channel closes. A worker serves one run at a
// time, and applies the client's environment, working directory, umask and
// standard streams to its own process for that run (CLI ADR 0116 §4).
func Work(run RunFunc) int {
	inWorker = true
	handleSignals()
	ownFingerprint, ownRuntimeEnv = processFingerprint(), runtimeEnv(os.Environ())
	f := os.NewFile(3, "machine-ctrl")
	fc, err := net.FileConn(f)
	_ = f.Close()
	if err != nil {
		fmt.Fprintln(os.Stderr, "machine: worker:", err)
		return 1
	}
	uc, err := unixConn(fc)
	if err != nil {
		fmt.Fprintln(os.Stderr, "machine: worker:", err)
		return 1
	}
	ctrl := &conn{c: uc}
	if err := ctrl.send(control{Op: "ready"}); err != nil {
		return 1
	}
	for {
		var req request
		files, err := ctrl.receive(&req)
		if err != nil {
			return 0 // the supervisor closed the channel
		}
		if len(files) != 4 {
			closeAll(files)
			_ = ctrl.send(control{Op: "done"})
			continue
		}
		// The worker is free again before its client learns the exit code, so
		// a command the client runs next finds it idle.
		var ctrlErr error
		serve(run, req, files, func() {
			var mem runtime.MemStats
			runtime.ReadMemStats(&mem)
			ctrlErr = ctrl.send(control{Op: "done", HeldBytes: mem.HeapInuse})
		})
		if ctrlErr != nil {
			return 0
		}
	}
}

// serve runs one request: files are the client's stdin, stdout, stderr and
// its connection. free tells the supervisor the worker is idle; serve calls
// it once, before the client learns the exit code.
func serve(run RunFunc, req request, files []*os.File, free func()) {
	defer closeAll(files)
	freed := false
	release := func() {
		if !freed {
			freed = true
			free()
		}
	}
	defer release()
	fc, err := net.FileConn(files[3])
	if err != nil {
		return
	}
	cc, err := unixConn(fc)
	if err != nil {
		return
	}
	client := &conn{c: cc}
	defer client.c.Close() //nolint:errcheck // the run is over
	// A worker whose process differs from the client's in what jobs inherit
	// beyond the run's context would start them otherwise: the client runs
	// the command itself.
	if req.Fingerprint != ownFingerprint || runtimeEnv(req.Env) != ownRuntimeEnv {
		release()
		_ = client.send(event{Refused: CodeContext})
		return
	}
	// A client at a terminal gets the master of a pseudo-terminal that
	// becomes the run's controlling terminal: the client relays its own
	// terminal to it, and the run sees a terminal as it would locally.
	var master, slave *os.File
	if req.Terminal {
		master, slave, err = openPTY()
		if err != nil {
			release()
			_ = client.send(event{Refused: CodeTerminal})
			return
		}
		defer slave.Close() //nolint:errcheck // the run is over
	}
	// The run starts on the client's go only: a client that stopped waiting
	// runs the command itself, and this worker then never does.
	var ready []*os.File
	if master != nil {
		ready = []*os.File{master}
	}
	err = client.send(event{Ready: true, Terminal: master != nil}, ready...)
	if master != nil {
		_ = master.Close() // the client holds it now
	}
	if err != nil {
		return
	}
	_ = client.c.SetReadDeadline(time.Now().Add(goTimeout))
	var start clientSignal
	if _, err := client.receive(&start); err != nil || start.Signal != "go" {
		return
	}
	_ = client.c.SetReadDeadline(time.Time{})
	std := files[:3]
	var dropTerminal func()
	if slave != nil {
		if dropTerminal, err = takeTerminal(slave); err != nil {
			release()
			_ = client.send(event{Lost: true})
			return
		}
		std = []*os.File{slave, slave, files[2]}
		if req.StderrTerminal {
			std[2] = slave
		}
	}
	restore, err := apply(req, std)
	if err != nil {
		if dropTerminal != nil {
			dropTerminal()
		}
		release()
		_ = client.send(event{Lost: true})
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	abort.Reset()
	stop := newStopper(cancel)
	setStopper(stop)
	defer setStopper(nil)
	if err := client.send(event{Started: true}); err != nil {
		restore()
		if dropTerminal != nil {
			dropTerminal()
		}
		return
	}
	finished := make(chan struct{})
	go listen(client, stop, finished)
	code := run(ctx, req.Args)
	close(finished)
	stop.done()
	restore()
	if dropTerminal != nil {
		dropTerminal()
	}
	release()
	_ = client.send(event{Exit: &code})
	// A run leaves garbage; give the memory back so an idle worker keeps
	// about its held state (CLI ADR 0116 §10).
	debug.FreeOSMemory()
}

// shutdownTimeout and interactiveShutdownTimeout bound a run after its first
// stop, as the CLI's main does for a run in its own process.
const (
	shutdownTimeout            = 10 * time.Second
	interactiveShutdownTimeout = abort.InteractiveStopBudget + shutdownTimeout
)

// stopper applies the CLI's two-signal policy to the run a worker serves: the
// first stop records its source and cancels the run, a second one, or the
// bound after the first, kills the interactive child and ends the worker,
// whose supervisor then ends what the run had started.
type stopper struct {
	mu       sync.Mutex
	cancel   context.CancelFunc
	stopped  bool
	finished chan struct{}
}

func newStopper(cancel context.CancelFunc) *stopper {
	return &stopper{cancel: cancel, finished: make(chan struct{})}
}

func (s *stopper) stop(sig syscall.Signal) {
	s.mu.Lock()
	first := !s.stopped
	s.stopped = true
	s.mu.Unlock()
	if !first {
		abort.KillInteractive()
		os.Exit(130)
	}
	abort.Record(sig)
	s.cancel()
	bound := shutdownTimeout
	if abort.InteractiveHeld() {
		bound = interactiveShutdownTimeout
	}
	go func() {
		select {
		case <-s.finished:
		case <-time.After(bound):
			abort.KillInteractive()
			os.Exit(130)
		}
	}()
}

func (s *stopper) done() { close(s.finished) }

var (
	currentMu   sync.Mutex
	currentStop *stopper
)

func setStopper(s *stopper) {
	currentMu.Lock()
	currentStop = s
	currentMu.Unlock()
}

// handleSignals routes the signals a worker receives: from its run's
// terminal, Ctrl-C (SIGINT) and Ctrl-\ (SIGQUIT) stop the run as a terminal
// stops a run in its own process; the hangup of a terminal it gives back is
// no stop. The handlers are reset in every process the run starts.
func handleSignals() {
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGHUP, syscall.SIGTERM)
	go func() {
		for sig := range signals {
			currentMu.Lock()
			s := currentStop
			currentMu.Unlock()
			switch {
			case sig == syscall.SIGHUP:
			case s != nil && sig == syscall.SIGTERM:
				s.stop(syscall.SIGTERM)
			case sig == syscall.SIGTERM:
				os.Exit(0)
			case s != nil:
				s.stop(syscall.SIGINT)
			}
		}
	}()
}

// listen stops the run when its client asks to or goes away; a client that
// went away stops it as a termination does.
func listen(client *conn, stop *stopper, finished <-chan struct{}) {
	for {
		var sig clientSignal
		if _, err := client.receive(&sig); err != nil {
			break
		}
		switch sig.Signal {
		case "interrupt":
			stop.stop(syscall.SIGINT)
		case "terminate":
			stop.stop(syscall.SIGTERM)
		}
	}
	select {
	case <-finished:
	default:
		stop.stop(syscall.SIGTERM)
	}
}

// apply sets the client's context on this process and returns what restores
// the worker's own.
func apply(req request, std []*os.File) (func(), error) {
	own := os.Environ()
	ownDir, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	saved := make([]int, 3)
	for i := range saved {
		// Close-on-exec: the run's jobs inherit the client's streams on 0 to
		// 2, never the worker's own.
		fd, err := unix.FcntlInt(uintptr(i), unix.F_DUPFD_CLOEXEC, 3)
		if err != nil {
			for _, s := range saved[:i] {
				_ = unix.Close(s)
			}
			return nil, err
		}
		saved[i] = fd
	}
	if err := os.Chdir(req.Dir); err != nil {
		for _, s := range saved {
			_ = unix.Close(s)
		}
		return nil, err
	}
	setEnv(req.Env)
	for i, f := range std {
		_ = unix.Dup2(int(f.Fd()), i)
	}
	ownUmask := unix.Umask(req.Umask)
	return func() {
		unix.Umask(ownUmask)
		for i, s := range saved {
			_ = unix.Dup2(s, i)
			_ = unix.Close(s)
		}
		_ = os.Chdir(ownDir)
		setEnv(own)
	}, nil
}

func setEnv(env []string) {
	os.Clearenv()
	for _, kv := range env {
		if name, value, ok := strings.Cut(kv, "="); ok && name != "" {
			_ = os.Setenv(name, value)
		}
	}
}
