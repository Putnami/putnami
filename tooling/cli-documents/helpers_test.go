package documents

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
)

// The gates in this project read the repository itself, so every one of them
// needs the same two anchors: where the repository root is, and where the CLI
// module it reasons about lives. Both are derived by walking up from the test's
// working directory — the project root — rather than from a relative literal,
// so a test that moves inside the project keeps working.

// cliDocumentsProjectDir is this project's path from the repository root. It is
// the form the cache key's file selection is relative to, so it is stated once
// and derived from, never spelled again.
const cliDocumentsProjectDir = "tooling/cli-documents"

// cliModuleDir is the CLI module this project reads. The gates read its
// committed release plan, its recorded rehearsal verdict and its manifest, all
// of which stay where the documentation links point at them.
const cliModuleDir = "tooling/cli"

// repositoryRoot walks up to the directory holding putnami.workspace.json.
func repositoryRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, wsproto.WorkspaceConfigFilename)); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no %s above the test working directory", wsproto.WorkspaceConfigFilename)
		}
		dir = parent
	}
}

// cliModuleRoot returns the root of the CLI module these gates read.
func cliModuleRoot(t *testing.T) string {
	t.Helper()
	return filepath.Join(repositoryRoot(t), filepath.FromSlash(cliModuleDir))
}

// projectRoot returns this project's own root — the directory holding the
// manifest whose declared test inputs key these gates.
func projectRoot(t *testing.T) string {
	t.Helper()
	return filepath.Join(repositoryRoot(t), filepath.FromSlash(cliDocumentsProjectDir))
}

func requireShell(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable on this platform")
	}
}

type streamCaptureResult struct {
	data []byte
	err  error
}

// drainCapturedStream reads a captured pipe concurrently with the code under
// test. Waiting until afterwards lets a large rendering fill the finite pipe
// buffer and block forever in Write.
func drainCapturedStream(r io.Reader) <-chan streamCaptureResult {
	done := make(chan streamCaptureResult, 1)
	go func() {
		data, err := io.ReadAll(r)
		done <- streamCaptureResult{data: data, err: err}
	}()
	return done
}

// captureStdout captures the process standard output produced by fn.
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

	readDone := drainCapturedStream(r)
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

// captureStdoutStderr captures both standard streams produced by fn.
func captureStdoutStderr(t *testing.T, fn func()) string {
	t.Helper()

	origOut, origErr := os.Stdout, os.Stderr
	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe stdout: %v", err)
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		_ = wOut.Close()
		_ = rOut.Close()
		t.Fatalf("os.Pipe stderr: %v", err)
	}
	os.Stdout = wOut
	os.Stderr = wErr
	defer func() {
		os.Stdout = origOut
		os.Stderr = origErr
		_ = wOut.Close()
		_ = wErr.Close()
		_ = rOut.Close()
		_ = rErr.Close()
	}()

	outDone := drainCapturedStream(rOut)
	errDone := drainCapturedStream(rErr)
	fn()

	os.Stdout = origOut
	os.Stderr = origErr
	if err := wOut.Close(); err != nil {
		t.Fatalf("close stdout writer: %v", err)
	}
	if err := wErr.Close(); err != nil {
		t.Fatalf("close stderr writer: %v", err)
	}
	out := <-outDone
	if out.err != nil {
		t.Fatalf("read stdout: %v", out.err)
	}
	errBuf := <-errDone
	if errBuf.err != nil {
		t.Fatalf("read stderr: %v", errBuf.err)
	}
	return string(out.data) + string(errBuf.data)
}
