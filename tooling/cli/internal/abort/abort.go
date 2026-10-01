// Package abort records why the process was asked to stop, so end-of-run
// reporting can name the cause instead of guessing at it.
//
// Signal delivery is process-global and arrives on its own goroutine, long
// after the run's contexts are built, so the source is recorded here rather
// than threaded through them. Readers see "" until a signal actually lands.
package abort

import (
	"os"
	"sync/atomic"
	"syscall"

	model "go.putnami.dev/cli/model/jobs"
)

// Sources reported by Source. The vocabulary itself belongs to the job model —
// it is what SessionOutcome.AbortedBy carries — so it is declared there and
// named here under its original spelling.
const (
	// SourceUser is an interactive Ctrl-C (SIGINT).
	SourceUser = model.SourceUser
	// SourceSignal is a termination request from a supervisor (SIGTERM).
	SourceSignal = model.SourceSignal
)

var source atomic.Pointer[string]

// Record notes the signal that triggered shutdown. The first signal wins: it
// is the one that canceled the run, and any later one only forces the exit.
func Record(sig os.Signal) {
	name := SourceSignal
	if sig == syscall.SIGINT {
		name = SourceUser
	}
	source.CompareAndSwap(nil, &name)
}

// Source returns the recorded abort source, or "" when no signal has arrived.
func Source() string {
	if p := source.Load(); p != nil {
		return *p
	}
	return ""
}

// Reset clears the recorded source, so a test (or a watch-mode iteration that
// survives its own cancellation) starts from a clean slate.
func Reset() { source.Store(nil) }
