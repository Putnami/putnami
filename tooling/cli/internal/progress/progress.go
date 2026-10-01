// Package progress provides a small, reusable primitive for streaming
// human-readable progress (phases and steps) while a long-running command
// handler works.
//
// The primitive exists to solve one correctness problem that command handlers
// otherwise hand-roll: a command that emits machine output (JSONL / cloud
// logging) must keep stdout a clean single-stream so an agent parsing it is
// never corrupted by a stray progress line. A Reporter therefore:
//
//   - writes only to the supplied writer (callers pass os.Stderr), never
//     stdout, so the final result stream is reserved for the command;
//   - is automatically inert when a structured output mode is active — every
//     method is a no-op and nothing is ever written — so extensions and
//     commands get "no interleaving" for free by construction;
//   - deduplicates against the last emitted line so a poll loop that
//     re-observes the same snapshot does not re-print it.
//
// This lifts a pattern from a TypeScript deploy-progress emitter
// (snapshot-deduped, stderr-only, gated on structured mode) into the Go
// framework CLI.
package progress

import (
	"io"
	"sync"

	"go.putnami.dev/tooling/cli/internal/iox"
)

// phasePrefix marks a named phase line. stepPrefix (indented, with a small
// arrow) marks a step underneath the current phase. These are cosmetic
// stderr-only markers, not the JSONL protocol vocabulary.
const (
	phasePrefix = "==> "
	stepPrefix  = "  → "
)

// Reporter streams deduplicated, human-readable progress to a writer.
//
// A Reporter is safe for concurrent use: a poll loop may drive Phase/Step from
// a background goroutine while the main handler also reports. When constructed
// in structured mode (or with a nil writer) the Reporter is inert and every
// method returns without writing.
type Reporter struct {
	mu sync.Mutex
	w  io.Writer
	// inert is true when no output must ever be produced: either a structured
	// output mode is active (machine stdout must stay a clean stream) or there
	// is no writer to write to.
	inert bool
	// last is the most recently emitted line (without its trailing newline).
	// A subsequent identical line is suppressed so a poll loop re-observing an
	// unchanged snapshot stays silent after the first print.
	last string
}

// New returns a Reporter that writes progress to w.
//
// When structured is true (a machine output mode such as "jsonl" or
// "cloud-logging" is active) or w is nil, the returned Reporter is inert:
// every method is a no-op and nothing is ever written. This is the inertness
// guarantee that keeps a machine stdout stream uncorrupted.
func New(w io.Writer, structured bool) *Reporter {
	return &Reporter{w: w, inert: structured || w == nil}
}

// Phase begins a named phase, printing a deduplicated line to the writer.
// It is a no-op on an inert Reporter, and prints nothing if the identical
// phase line was the most recent emission.
func (r *Reporter) Phase(name string) {
	r.emit(phasePrefix + name)
}

// Step reports a step transition with a human-cosmetic status. The line is
// deduplicated against the last emission, so re-reporting an unchanged
// (name, status) pair prints nothing. status is free-form stderr text (not the
// JSONL protocol status vocabulary); an empty status prints just the step name.
// Step is a no-op on an inert Reporter.
func (r *Reporter) Step(name, status string) {
	line := stepPrefix + name
	if status != "" {
		line += ": " + status
	}
	r.emit(line)
}

// emit writes a single deduplicated line (adding the trailing newline) under
// the mutex. It returns immediately when the Reporter is inert.
func (r *Reporter) emit(line string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inert {
		return
	}
	if line == r.last {
		return
	}
	r.last = line
	iox.Fprintln(r.w, line)
}
