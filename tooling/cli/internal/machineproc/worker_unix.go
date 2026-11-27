//go:build linux || darwin

package machineproc

import (
	"context"
	"fmt"
	"net"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"go.putnami.dev/tooling/cli/internal/abort"
)

// RunFunc runs one command in this process, as main would: the worker calls
// it with the client's arguments once it has applied the client's context.
type RunFunc func(ctx context.Context, args []string) int

// forceTimeout bounds a run after its client asked it to stop and then went
// away: past it the worker exits, which ends the run's jobs with it.
const forceTimeout = 30 * time.Second

// Work runs a worker on the control channel its supervisor passed as
// descriptor 3, until that channel closes. A worker serves one run at a
// time, and applies the client's environment, working directory, umask and
// standard streams to its own process for that run (CLI ADR 0116 §4).
func Work(run RunFunc) int {
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
	restore, err := apply(req, files[:3])
	if err != nil {
		release()
		_ = client.send(event{Refused: "machine.context"})
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	abort.Reset()
	if err := client.send(event{Started: true}); err != nil {
		restore()
		return
	}
	finished := make(chan struct{})
	go listen(client, cancel, finished)
	code := run(ctx, req.Args)
	close(finished)
	restore()
	release()
	_ = client.send(event{Exit: &code})
	// A run leaves garbage; give the memory back so an idle worker keeps
	// about its held state (CLI ADR 0116 §10).
	debug.FreeOSMemory()
}

// listen cancels the run when its client asks to stop or goes away. When the
// client went away and the run outlives forceTimeout, the worker exits, which
// ends the run's jobs with it.
func listen(client *conn, cancel context.CancelFunc, finished <-chan struct{}) {
	for {
		var sig signal
		if _, err := client.receive(&sig); err != nil {
			break
		}
		if sig.Signal == "interrupt" {
			abort.Record(os.Interrupt)
			cancel()
		}
	}
	cancel()
	select {
	case <-finished:
	case <-time.After(forceTimeout):
		os.Exit(130)
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
	setEnv(append(append([]string(nil), req.Env...), RunEnv+"=1"))
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
