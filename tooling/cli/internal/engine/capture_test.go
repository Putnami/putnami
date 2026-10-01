package engine

import (
	"bytes"
	"io"
	"os"
	"testing"
)

type capturedEngineStream struct {
	data []byte
	err  error
}

func drainCapturedEngineStream(r io.Reader) <-chan capturedEngineStream {
	done := make(chan capturedEngineStream, 1)
	go func() {
		data, err := io.ReadAll(r)
		done <- capturedEngineStream{data: data, err: err}
	}()
	return done
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns
// everything written to stdout during the call. The engine's stages report on
// the process streams, so the stage tests read them back the same way the CLI
// shell's own tests do.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	defer func() {
		os.Stdout = orig
		_ = w.Close()
		_ = r.Close()
	}()
	readDone := drainCapturedEngineStream(r)

	fn()

	os.Stdout = orig
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	result := <-readDone
	if result.err != nil {
		t.Fatalf("read stdout: %v", result.err)
	}
	return string(result.data)
}

func TestCaptureStdout_DrainsBeyondPipeCapacity(t *testing.T) {
	want := bytes.Repeat([]byte("x"), 256*1024)
	got := captureStdout(t, func() {
		if _, err := os.Stdout.Write(want); err != nil {
			t.Fatalf("write stdout capacity probe: %v", err)
		}
	})
	if !bytes.Equal([]byte(got), want) {
		t.Fatalf("captured %d bytes, want %d", len(got), len(want))
	}
}

// captureStderr runs fn with os.Stderr redirected to a pipe and returns
// everything written to stderr during the call.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	defer func() {
		os.Stderr = orig
		_ = w.Close()
		_ = r.Close()
	}()
	readDone := drainCapturedEngineStream(r)

	fn()

	os.Stderr = orig
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	result := <-readDone
	if result.err != nil {
		t.Fatalf("read stderr: %v", result.err)
	}
	return string(result.data)
}
