package logger

// Tests for exported lifecycle and sink surfaces:
//   - Logger.Flush and Logger.Close (logger.go:182/192)
//   - ConsoleSink.Flush, ConsoleSink.Close, ConsoleSink.Err (console_sink.go)
//   - JSONSink.Flush, JSONSink.Close, JSONSink.Err (json_sink.go)

import (
	"errors"
	"io"
	"sync"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// errSink is a Sink whose Flush and Close return configurable errors.
// Write is a no-op. Used to exercise Logger.Flush and Logger.Close error paths.
type errSink struct {
	flushErr   error
	closeErr   error
	flushCalls int
	closeCalls int
}

func (s *errSink) Write(_ LogEntry) {}
func (s *errSink) Flush() error {
	s.flushCalls++
	return s.flushErr
}
func (s *errSink) Close() error {
	s.closeCalls++
	return s.closeErr
}

// errWriter is an io.Writer that always returns a configurable error.
// It is safe for concurrent use (ConsoleSink/JSONSink both hold their own mu,
// but the writer itself is accessed from within that lock, so the extra mu
// here is just defensive).
type errWriter struct {
	mu  sync.Mutex
	err error
}

func newErrWriter(err error) *errWriter { return &errWriter{err: err} }

func (w *errWriter) Write(_ []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return 0, w.err
}

// --- Logger.Flush and Logger.Close ---

// TestLoggerFlushError verifies that Logger.Flush returns the error from a
// failing sink (exercises the error-return branch in logger.go:Flush).
func TestLoggerFlushError(t *testing.T) {
	flushErr := errors.New("flush failed")
	log := New("flush-err-test", LevelDebug, &errSink{flushErr: flushErr})

	if err := log.Flush(); err == nil {
		t.Fatal("expected Flush to return an error, got nil")
	} else if err != flushErr {
		t.Fatalf("Flush error = %v, want first sink error %v", err, flushErr)
	}
}

func TestLoggerFlushVisitsEverySinkAndReturnsFirstError(t *testing.T) {
	spectest.Proves(t, "go/structured-logging", "lifecycle", "flush-visits-every-sink")
	firstErr := errors.New("first flush failed")
	secondErr := errors.New("second flush failed")
	first := &errSink{flushErr: firstErr}
	second := &errSink{flushErr: secondErr}
	log := New("flush-all-test", LevelDebug, first, second)

	err := log.Flush()
	if err != firstErr {
		t.Fatalf("Flush error = %v, want first sink error %v", err, firstErr)
	}
	if first.flushCalls != 1 || second.flushCalls != 1 {
		t.Fatalf("flush calls = (%d, %d), want (1, 1)", first.flushCalls, second.flushCalls)
	}
}

// TestLoggerCloseError verifies that Logger.Close returns the error from a
// failing sink (exercises the error-return branch in logger.go:Close).
func TestLoggerCloseError(t *testing.T) {
	closeErr := errors.New("close failed")
	log := New("close-err-test", LevelDebug, &errSink{closeErr: closeErr})

	if err := log.Close(); err == nil {
		t.Fatal("expected Close to return an error, got nil")
	} else if err != closeErr {
		t.Fatalf("Close error = %v, want first sink error %v", err, closeErr)
	}
}

func TestLoggerCloseVisitsEverySinkAndReturnsFirstError(t *testing.T) {
	spectest.Proves(t, "go/structured-logging", "lifecycle", "close-visits-every-sink")
	firstErr := errors.New("first close failed")
	secondErr := errors.New("second close failed")
	first := &errSink{closeErr: firstErr}
	second := &errSink{closeErr: secondErr}
	log := New("close-all-test", LevelDebug, first, second)

	err := log.Close()
	if err != firstErr {
		t.Fatalf("Close error = %v, want first sink error %v", err, firstErr)
	}
	if first.closeCalls != 1 || second.closeCalls != 1 {
		t.Fatalf("close calls = (%d, %d), want (1, 1)", first.closeCalls, second.closeCalls)
	}
}

// --- ConsoleSink.Err ---

// TestConsoleSinkErr verifies that a write failure is captured by Err().
// We inject a failing writer for both stdout and stderr, then write an entry
// at Info level (routed to stdout) and confirm Err() returns a non-nil error.
func TestConsoleSinkErr(t *testing.T) {
	writeErr := errors.New("stdout broken")
	sink := newConsoleSinkWriters(newErrWriter(writeErr), newErrWriter(writeErr))

	// Err() must be nil before any writes.
	if err := sink.Err(); err != nil {
		t.Fatalf("expected nil Err before write, got: %v", err)
	}

	// Write an Info entry — routed to stdout (our errWriter).
	sink.Write(LogEntry{
		Level:   LevelInfo,
		Message: "hello",
	})

	if err := sink.Err(); err == nil {
		t.Fatal("expected non-nil Err after failing write, got nil")
	}
}

// TestConsoleSinkErrStderr verifies that a write failure on stderr (Warn/Error
// entries) is also captured by Err().
func TestConsoleSinkErrStderr(t *testing.T) {
	writeErr := errors.New("stderr broken")
	sink := newConsoleSinkWriters(io.Discard, newErrWriter(writeErr))

	sink.Write(LogEntry{
		Level:   LevelError,
		Message: "crash",
	})

	if err := sink.Err(); err == nil {
		t.Fatal("expected non-nil Err after failing stderr write, got nil")
	}
}

// --- JSONSink.Err ---

// TestJSONSinkErr verifies that a json.Encoder write failure is captured by Err().
func TestJSONSinkErr(t *testing.T) {
	writeErr := errors.New("json write broken")
	sink := NewJSONSinkWriter(newErrWriter(writeErr))

	// Err() must be nil before any writes.
	if err := sink.Err(); err != nil {
		t.Fatalf("expected nil Err before write, got: %v", err)
	}

	sink.Write(LogEntry{
		Level:   LevelInfo,
		Message: "should fail",
	})

	if err := sink.Err(); err == nil {
		t.Fatal("expected non-nil Err after failing JSON encode, got nil")
	}
}

// --- ConsoleSink.Flush and ConsoleSink.Close (no-op stubs) ---

// TestConsoleSinkFlushClose exercises the Flush and Close no-ops on ConsoleSink
// to bring them from 0% to covered.
func TestConsoleSinkFlushClose(t *testing.T) {
	sink := newConsoleSinkWriters(io.Discard, io.Discard)

	if err := sink.Flush(); err != nil {
		t.Errorf("ConsoleSink.Flush returned unexpected error: %v", err)
	}
	if err := sink.Close(); err != nil {
		t.Errorf("ConsoleSink.Close returned unexpected error: %v", err)
	}
}

// --- JSONSink.Flush and JSONSink.Close (no-op stubs) ---

// TestJSONSinkFlushClose exercises the Flush and Close no-ops on JSONSink
// to bring them from 0% to covered.
func TestJSONSinkFlushClose(t *testing.T) {
	sink := NewJSONSinkWriter(io.Discard)

	if err := sink.Flush(); err != nil {
		t.Errorf("JSONSink.Flush returned unexpected error: %v", err)
	}
	if err := sink.Close(); err != nil {
		t.Errorf("JSONSink.Close returned unexpected error: %v", err)
	}
}
