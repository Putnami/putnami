package progress

import (
	"bytes"
	"strings"
	"sync"
	"testing"
)

// TestInertUnderStructuredMode is the core JSONL-corruption invariant: when a
// structured output mode is active the Reporter must never write anything, so
// stdout (owned by the machine renderer) stays a clean single stream.
func TestInertUnderStructuredMode(t *testing.T) {
	var buf bytes.Buffer
	r := New(&buf, true)

	r.Phase("provisioning")
	r.Step("upload", "running")
	r.Phase("deploying")
	r.Step("rollout", "done")

	if buf.Len() != 0 {
		t.Fatalf("structured-mode Reporter wrote %q; want no output", buf.String())
	}
}

// TestNilWriterInert verifies a nil writer is treated as inert: no panic and no
// output, even in human mode.
func TestNilWriterInert(t *testing.T) {
	r := New(nil, false)
	// Must not panic.
	r.Phase("provisioning")
	r.Step("upload", "running")
}

// TestNilReporter verifies methods on a nil *Reporter are safe no-ops, so a
// caller that holds a possibly-nil reporter need not guard every call.
func TestNilReporter(t *testing.T) {
	var r *Reporter
	r.Phase("provisioning")
	r.Step("upload", "running")
}

// TestHumanModeWrites checks the exact lines produced in human mode.
func TestHumanModeWrites(t *testing.T) {
	var buf bytes.Buffer
	r := New(&buf, false)

	r.Phase("provisioning")
	r.Step("upload", "running")
	r.Step("upload", "done")

	got := buf.String()
	want := "==> provisioning\n  → upload: running\n  → upload: done\n"
	if got != want {
		t.Fatalf("human output = %q; want %q", got, want)
	}
}

// TestStepEmptyStatus verifies an empty status prints just the step name.
func TestStepEmptyStatus(t *testing.T) {
	var buf bytes.Buffer
	r := New(&buf, false)

	r.Step("upload", "")

	if got := buf.String(); got != "  → upload\n" {
		t.Fatalf("empty-status step = %q; want %q", got, "  → upload\n")
	}
}

// TestDedup verifies a repeated identical snapshot prints once, and a changed
// snapshot prints again. This is what keeps a poll loop re-observing the same
// state from spamming stderr.
func TestDedup(t *testing.T) {
	var buf bytes.Buffer
	r := New(&buf, false)

	// Same phase repeated: printed once.
	r.Phase("provisioning")
	r.Phase("provisioning")
	r.Phase("provisioning")

	// Same step repeated: printed once.
	r.Step("upload", "running")
	r.Step("upload", "running")

	// Changed step: printed again.
	r.Step("upload", "done")

	// Repeating the earlier phase after other output is a *new* line relative to
	// last, so it is not suppressed (dedup is against the immediately previous
	// emission, matching a poll loop that only re-observes the current state).
	got := buf.String()
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	want := []string{
		"==> provisioning",
		"  → upload: running",
		"  → upload: done",
	}
	if len(lines) != len(want) {
		t.Fatalf("dedup produced %d lines %q; want %d %q", len(lines), lines, len(want), want)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("line %d = %q; want %q", i, lines[i], want[i])
		}
	}
}

// TestConcurrent drives Phase/Step from many goroutines to prove the Reporter
// is safe under -race. Output content is unconstrained here; the point is the
// absence of a data race and of a panic.
func TestConcurrent(t *testing.T) {
	var buf bytes.Buffer
	r := New(&buf, false)

	const goroutines = 16
	const iterations = 200
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := range goroutines {
		go func(id int) {
			defer wg.Done()
			for range iterations {
				r.Phase("phase")
				r.Step("step", "running")
			}
		}(g)
	}
	wg.Wait()
}

// TestConcurrentStructuredInert proves the inert path is also race-free: an
// inert Reporter must stay silent even under concurrent load.
func TestConcurrentStructuredInert(t *testing.T) {
	var buf bytes.Buffer
	r := New(&buf, true)

	var wg sync.WaitGroup
	wg.Add(8)
	for range 8 {
		go func() {
			defer wg.Done()
			for range 100 {
				r.Phase("phase")
				r.Step("step", "running")
			}
		}()
	}
	wg.Wait()

	if buf.Len() != 0 {
		t.Fatalf("inert Reporter wrote %q under concurrency; want no output", buf.String())
	}
}
