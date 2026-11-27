//go:build linux || darwin

package machineproc

import (
	"io"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// terminalRelay relays the client's terminal to the master of the run's
// pseudo-terminal, in raw mode, so the run's terminal acts as the client's
// would: Ctrl-C reaches the run through the pseudo-terminal's line
// discipline, a prompt reads what the person types and turns echo off as it
// would, and the size follows the client's (CLI ADR 0116 §4). The suspend
// key stops the client alone: the relay pauses, the run goes on, and its
// output waits in the pseudo-terminal until the client resumes.
type terminalRelay struct {
	master  *os.File
	input   *os.File
	suspend byte
	// intr and quit are the keys that stop the run through its terminal:
	// once one was typed, a worker that ends is a run the person stopped.
	intr, quit  byte
	interrupted atomic.Bool

	mu      sync.Mutex
	restore func()

	output  chan struct{}
	winch   chan os.Signal
	closing sync.Once
}

func startRelay(master *os.File) (*terminalRelay, error) {
	suspend, intr, quit, err := mirrorTerminal(master, os.Stdin)
	if err != nil {
		return nil, err
	}
	// The relay reads stdin through a non-blocking duplicate, which its close
	// can end, and gives it back blocking.
	fd, err := unix.Dup(int(os.Stdin.Fd()))
	if err != nil {
		return nil, err
	}
	unix.CloseOnExec(fd)
	if err := unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	r := &terminalRelay{
		master: master, input: os.NewFile(uintptr(fd), "stdin"), suspend: suspend, intr: intr, quit: quit,
		output: make(chan struct{}), winch: make(chan os.Signal, 1),
	}
	if err := r.raw(); err != nil {
		r.close()
		return nil, err
	}
	signal.Notify(r.winch, syscall.SIGWINCH)
	go r.resize()
	go r.copyOutput()
	go r.copyInput()
	return r, nil
}

func (r *terminalRelay) raw() error {
	restore, err := makeRaw(os.Stdin)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.restore = restore
	r.mu.Unlock()
	return nil
}

func (r *terminalRelay) cooked() {
	r.mu.Lock()
	restore := r.restore
	r.restore = nil
	r.mu.Unlock()
	if restore != nil {
		restore()
	}
}

func (r *terminalRelay) resize() {
	for range r.winch {
		_ = copySize(r.master, os.Stdout)
	}
}

func (r *terminalRelay) copyOutput() {
	defer close(r.output)
	_, _ = io.Copy(os.Stdout, r.master)
}

func (r *terminalRelay) copyInput() {
	buf := make([]byte, 1024)
	for {
		n, err := r.input.Read(buf)
		if err != nil {
			return
		}
		chunk := buf[:n]
		for _, b := range chunk {
			if b != 0 && (b == r.intr || b == r.quit) {
				r.interrupted.Store(true)
			}
		}
		for len(chunk) > 0 {
			i := -1
			if r.suspend != 0 {
				for j, b := range chunk {
					if b == r.suspend {
						i = j
						break
					}
				}
			}
			if i < 0 {
				_, _ = r.master.Write(chunk)
				break
			}
			_, _ = r.master.Write(chunk[:i])
			chunk = chunk[i+1:]
			r.suspendClient()
		}
	}
}

// suspendClient stops the client's process group as the suspend key stops a
// command in its own process, and takes raw mode and the size again when the
// shell resumes it.
func (r *terminalRelay) suspendClient() {
	r.cooked()
	_ = syscall.Kill(0, syscall.SIGTSTP)
	// Running again: the shell resumed the client.
	_ = r.raw()
	_ = copySize(r.master, os.Stdout)
}

// drain waits for the run's last output, which ends when the worker gave
// the pseudo-terminal back.
func (r *terminalRelay) drain() {
	select {
	case <-r.output:
	case <-time.After(2 * time.Second):
	}
}

func (r *terminalRelay) close() {
	r.closing.Do(func() {
		signal.Stop(r.winch)
		close(r.winch)
		r.cooked()
		_ = unix.SetNonblock(int(r.input.Fd()), false)
		_ = r.input.Close()
		_ = r.master.Close()
	})
}
