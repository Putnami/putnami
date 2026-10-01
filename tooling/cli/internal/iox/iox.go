// Package iox provides small I/O helpers that intentionally discard errors.
//
// In a CLI context, write failures to stdout/stderr are non-recoverable, and
// best-effort capture of subprocess output should never surface read errors.
// This package centralizes the intentional error discarding so that callers
// do not need per-call annotations.
package iox

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"sync"
)

// stdoutCapture serializes the short-lived stdout captures the structured CLI
// dispatcher uses while it turns legacy JSONL payloads into one ResultV2
// document. Commands are process-local and run one at a time; the mutex keeps
// tests and any future in-process callers from interleaving two captures.
var (
	stdoutCaptureMu  sync.Mutex
	stdoutOverrideMu sync.RWMutex
	stdoutOverride   io.Writer
)

// Stdout returns the current stdout destination. Callers which need an
// io.Writer (rather than one of the Fprint helpers below) use it so structured
// dispatch can collect their payload before writing the command envelope.
func Stdout() io.Writer { return stdoutWriter{} }

type stdoutWriter struct{}

func (stdoutWriter) Write(p []byte) (int, error) {
	stdoutOverrideMu.RLock()
	defer stdoutOverrideMu.RUnlock()
	if stdoutOverride != nil {
		return stdoutOverride.Write(p)
	}
	return os.Stdout.Write(p)
}

// CaptureStdout runs fn with writes routed through this package's stdout
// helpers collected in memory. It is deliberately narrow: it leaves stderr
// alone and does not alter direct os.Stdout writes, which makes it safe for
// the human-output paths and lets the dispatcher own the final machine result.
func CaptureStdout(fn func() error) (output string, err error) {
	stdoutCaptureMu.Lock()
	defer stdoutCaptureMu.Unlock()

	var captured bytes.Buffer
	stdoutOverrideMu.Lock()
	stdoutOverride = &captured
	stdoutOverrideMu.Unlock()
	defer func() {
		stdoutOverrideMu.Lock()
		stdoutOverride = nil
		stdoutOverrideMu.Unlock()
		output = captured.String()
	}()

	return "", fn()
}

func destination(w io.Writer) io.Writer {
	if w == os.Stdout {
		return Stdout()
	}
	return w
}

// Fprintf formats according to a format specifier and writes to w.
func Fprintf(w io.Writer, format string, a ...any) {
	fmt.Fprintf(destination(w), format, a...)
}

// Fprintln formats using default formatting and appends a newline.
func Fprintln(w io.Writer, a ...any) {
	fmt.Fprintln(destination(w), a...)
}

// Fprint formats using default formatting and writes to w.
func Fprint(w io.Writer, a ...any) {
	fmt.Fprint(destination(w), a...)
}

// Write writes raw bytes to w, discarding any error.
// CLI output writes are non-recoverable; see package doc.
func Write(w io.Writer, b []byte) {
	destination(w).Write(b) //nolint:errcheck // fire-and-forget CLI output
}

// DefaultReadCap is the maximum number of bytes ReadCapped retains before
// truncating. 8 MiB is ample for capturing subprocess diagnostics while
// preventing a runaway child process from exhausting the parent CLI's heap.
const DefaultReadCap = 8 * 1024 * 1024

const truncationMarker = "\n[output truncated]\n"

// ReadCapped reads from r and returns at most maxBytes bytes as a string.
//
// If r yields more than maxBytes bytes, the result is truncated to maxBytes
// with a trailing marker appended, and the remainder of r is drained and
// discarded. Draining is essential when r is a subprocess pipe: leaving bytes
// unread keeps the OS pipe buffer full, which can block the child on write and
// deadlock a subsequent cmd.Wait(). Read errors are ignored (best-effort
// capture).
func ReadCapped(r io.Reader, maxBytes int) string {
	data, _ := io.ReadAll(io.LimitReader(r, int64(maxBytes)+1))
	if len(data) > maxBytes {
		_, _ = io.Copy(io.Discard, r)
		return string(data[:maxBytes]) + truncationMarker
	}
	return string(data)
}
