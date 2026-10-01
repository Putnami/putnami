// Package sharedtest holds test-only helpers shared by 2+ verticals' test
// files under tooling/cli/internal/commands. Nothing here may be imported by
// a production (non-_test.go) file: that is what keeps "testing" out of the
// shipped CLI binary's dependency graph. A production helper belongs in the
// sibling package, internal/commands/shared, instead.
package sharedtest

import (
	"io"
	"os"
	"testing"
)

// CapturedStream is one drained read of a redirected pipe.
type CapturedStream struct {
	Data []byte
	Err  error
}

// DrainCapturedStream reads r to completion on its own goroutine so a writer
// larger than the pipe's buffer capacity never deadlocks against a reader
// that only starts after the writer finishes.
func DrainCapturedStream(r io.Reader) <-chan CapturedStream {
	done := make(chan CapturedStream, 1)
	go func() {
		data, err := io.ReadAll(r)
		done <- CapturedStream{Data: data, Err: err}
	}()
	return done
}

// CaptureStdout redirects the real os.Stdout file descriptor to a pipe for
// the duration of fn and returns everything written to it. Unlike an
// in-process override, this swaps the actual *os.File, so it also captures
// output written directly (fmt.Println, a spawned subprocess inheriting the
// descriptor, …) rather than only writes routed through this package's own
// output helpers.
func CaptureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()

	origStdout := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}

	os.Stdout = writer
	defer func() {
		os.Stdout = origStdout
		_ = writer.Close()
		_ = reader.Close()
	}()
	readDone := DrainCapturedStream(reader)
	runErr := fn()

	os.Stdout = origStdout
	if err := writer.Close(); err != nil {
		t.Fatalf("close stdout writer: %v", err)
	}
	result := <-readDone
	if result.Err != nil {
		t.Fatalf("read stdout: %v", result.Err)
	}

	return string(result.Data), runErr
}

// CaptureStderr redirects os.Stderr for the duration of fn and returns what
// was written. It mirrors CaptureStdout but for the stderr stream used by
// best-effort diagnostic notes, and for callers whose fn reports failure only
// via the captured text rather than a returned error.
func CaptureStderr(t *testing.T, fn func()) string {
	t.Helper()

	origStderr := os.Stderr
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}

	os.Stderr = writer
	defer func() {
		os.Stderr = origStderr
		_ = writer.Close()
		_ = reader.Close()
	}()
	readDone := DrainCapturedStream(reader)
	fn()

	os.Stderr = origStderr
	if err := writer.Close(); err != nil {
		t.Fatalf("close stderr writer: %v", err)
	}
	result := <-readDone
	if result.Err != nil {
		t.Fatalf("read stderr: %v", result.Err)
	}

	return string(result.Data)
}
