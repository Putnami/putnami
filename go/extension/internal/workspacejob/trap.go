package workspacejob

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// Trap is the job's answer to SIGINT and SIGTERM, the script's
// `trap '...; exit 130' INT` and `trap '...; exit 143' TERM`.
//
// While armed, a signal cancels the job context, which kills the command in
// flight. The job then runs the armed action (a restore or a rollback) on its
// own goroutine, once the command has returned, and exits with 128 plus the
// signal number. Running the action on the job goroutine rather than on the
// signal's is what keeps it from racing the file writes it undoes.
//
// While disarmed, a signal keeps its default effect.
type Trap struct {
	mu       sync.Mutex
	cancel   context.CancelFunc
	exit     func(code int)
	signals  chan os.Signal
	stop     chan struct{}
	done     chan struct{}
	action   func()
	cleanups []*func()
	received os.Signal
}

// NewTrap returns a disarmed trap that cancels the job through cancel and ends
// the process through exit.
func NewTrap(cancel context.CancelFunc, exit func(code int)) *Trap {
	return &Trap{cancel: cancel, exit: exit}
}

// Arm makes a SIGINT or a SIGTERM run action and end the job. Arming an armed
// trap replaces its action.
func (t *Trap) Arm(action func()) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.action = action
	t.notify()
}

// OnSignal makes a SIGINT or a SIGTERM run cleanup before the job exits, after
// the armed action, until the returned release is called. It arms the trap
// when no action armed it yet, so a signal waits for the cleanup instead of
// killing the process. Unlike an action, a cleanup is never replaced: it is
// how a file that must not outlive the job, such as a credential, is removed
// even when a signal ends the job and its deferred calls never run.
func (t *Trap) OnSignal(cleanup func()) (release func()) {
	if t == nil || cleanup == nil {
		return func() {}
	}
	entry := &cleanup
	t.mu.Lock()
	t.cleanups = append(t.cleanups, entry)
	t.notify()
	t.mu.Unlock()
	return func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		for i, registered := range t.cleanups {
			if registered == entry {
				t.cleanups = append(t.cleanups[:i:i], t.cleanups[i+1:]...)
				return
			}
		}
	}
}

// notify starts receiving the signals, unless the trap already does. The
// caller holds t.mu.
func (t *Trap) notify() {
	if t.signals != nil {
		return
	}
	t.signals = make(chan os.Signal, 1)
	t.stop = make(chan struct{})
	t.done = make(chan struct{})
	signal.Notify(t.signals, os.Interrupt, syscall.SIGTERM)
	go t.wait(t.signals, t.stop, t.done)
}

// wait receives the first signal until stop is closed, and closes done when it
// returns.
func (t *Trap) wait(signals <-chan os.Signal, stop, done chan struct{}) {
	defer close(done)
	select {
	case sig := <-signals:
		t.receive(sig)
	case <-stop:
	}
}

// receive records the first signal and cancels the job so the command in
// flight returns.
func (t *Trap) receive(sig os.Signal) {
	t.mu.Lock()
	if t.received == nil {
		t.received = sig
	}
	t.mu.Unlock()
	t.cancel()
}

// Disarm restores the default effect of the signals. A signal received before
// Disarm returns still ends the job, after the armed action runs: once the
// signals are stopped, no more can arrive, and Disarm waits for the signal
// goroutine to return, then takes a signal it left on the channel, before it
// checks.
func (t *Trap) Disarm() {
	if t == nil {
		return
	}
	t.mu.Lock()
	signals, stop, done := t.signals, t.stop, t.done
	t.signals, t.stop, t.done = nil, nil, nil
	t.mu.Unlock()
	if signals != nil {
		signal.Stop(signals)
		close(stop)
		<-done
		select {
		case sig := <-signals:
			t.receive(sig)
		default:
		}
	}
	t.check()
	t.mu.Lock()
	t.action = nil
	t.mu.Unlock()
}

// check ends the job when a signal was received: it runs the armed action,
// then the registered cleanups, the last registered first, then exits with 128
// plus the signal number.
func (t *Trap) check() {
	if t == nil {
		return
	}
	t.mu.Lock()
	sig := t.received
	if sig == nil {
		// No signal: the armed action stays armed for the next check.
		t.mu.Unlock()
		return
	}
	action := t.action
	t.action = nil
	cleanups := t.cleanups
	t.cleanups = nil
	t.mu.Unlock()
	if action != nil {
		action()
	}
	for i := len(cleanups) - 1; i >= 0; i-- {
		(*cleanups[i])()
	}
	t.exit(exitCodeFor(sig))
}

// exitCodeFor is the status a shell exits with after trapping sig.
func exitCodeFor(sig os.Signal) int {
	if sig == syscall.SIGTERM {
		return 128 + int(syscall.SIGTERM)
	}
	return 128 + int(syscall.SIGINT)
}
