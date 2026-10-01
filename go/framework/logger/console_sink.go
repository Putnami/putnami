package logger

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
)

// ConsoleSink writes human-readable log entries to stdout/stderr.
// Format matches the TypeScript runtime: [traceId] [LEVEL] [logger] message ...attrs context
// Debug and info are written to stdout; warn and error to stderr.
// Error stack traces are printed separately on stderr.
type ConsoleSink struct {
	stdout  io.Writer
	stderr  io.Writer
	mu      sync.Mutex
	lastErr error
}

// NewConsoleSink creates a new console sink.
// It writes debug/info to os.Stdout and warn/error to os.Stderr.
func NewConsoleSink() *ConsoleSink {
	return &ConsoleSink{stdout: os.Stdout, stderr: os.Stderr}
}

// newConsoleSinkWriters creates a console sink with custom writers (for testing).
func newConsoleSinkWriters(stdout, stderr io.Writer) *ConsoleSink {
	return &ConsoleSink{stdout: stdout, stderr: stderr}
}

func (s *ConsoleSink) Write(entry LogEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var b strings.Builder

	// TraceID first
	if entry.TraceID != "" {
		b.WriteByte('[')
		b.WriteString(entry.TraceID)
		b.WriteString("] ")
	}

	// Level
	b.WriteByte('[')
	b.WriteString(levelTag(entry.Level))
	b.WriteString("] ")

	// Logger name
	if entry.Logger != "" {
		b.WriteByte('[')
		b.WriteString(entry.Logger)
		b.WriteString("] ")
	}

	// Message. Sanitize control bytes so an attacker-controlled value (e.g.
	// containing "\n") cannot forge a second log line on stdout/stderr.
	b.WriteString(sanitizeConsole(entry.Message))
	if entry.Error != nil && entry.Error.Message != "" && entry.Error.Message != entry.Message {
		b.WriteString(": ")
		b.WriteString(sanitizeConsole(entry.Error.Message))
	}

	// Attrs as key=value pairs. slogValueToAny resolves LogValuer values and
	// renders groups as maps, matching the JSON sink's group handling.
	for _, attr := range entry.Attrs {
		b.WriteByte(' ')
		b.WriteString(sanitizeConsole(attr.Key))
		b.WriteByte('=')
		b.WriteString(sanitizeConsole(fmt.Sprintf("%v", slogValueToAny(attr.Value))))
	}

	// Context fields as JSON if present
	if len(entry.Context) > 0 {
		b.WriteByte(' ')
		b.WriteString(formatContextInline(entry.Context))
	}

	b.WriteByte('\n')
	line := b.String()

	// Route to stdout or stderr based on level
	var w io.Writer
	if entry.Level >= slog.LevelWarn {
		w = s.stderr
	} else {
		w = s.stdout
	}

	if _, err := io.WriteString(w, line); err != nil {
		s.lastErr = err
	}

	// Print stack trace for errors on stderr
	if entry.Error != nil && entry.Error.Stack != "" {
		if _, err := io.WriteString(s.stderr, entry.Error.Stack+"\n"); err != nil {
			s.lastErr = err
		}
	}
}

// Flush is a no-op for ConsoleSink since writes are unbuffered.
func (s *ConsoleSink) Flush() error { return nil }

// Close is a no-op for ConsoleSink; the underlying writers are not owned.
func (s *ConsoleSink) Close() error { return nil }

// Err returns the last write error encountered by the sink, if any.
func (s *ConsoleSink) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
}

func levelTag(level slog.Level) string {
	switch {
	case level >= slog.LevelError:
		return "ERROR"
	case level >= slog.LevelWarn:
		return "WARN"
	case level >= slog.LevelInfo:
		return "INFO"
	default:
		return "DEBUG"
	}
}

func formatContextInline(ctx map[string]any) string {
	var b strings.Builder
	b.WriteByte('{')
	first := true
	for k, v := range ctx {
		if !first {
			b.WriteByte(',')
		}
		first = false
		fmt.Fprintf(&b, "%q:", k)
		switch val := v.(type) {
		case string:
			// %q already escapes control bytes (including \n), so a string
			// context value cannot inject a line break.
			fmt.Fprintf(&b, "%q", val)
		default:
			// Non-string values render via %v, which is not escaped; sanitize
			// control bytes so e.g. a []string element with "\n" can't forge
			// a second log line.
			b.WriteString(sanitizeConsole(fmt.Sprintf("%v", val)))
		}
	}
	b.WriteByte('}')
	return b.String()
}

// sanitizeConsole escapes control bytes that could forge or corrupt a console
// log line: the C0 range (0x00–0x1F, which includes \n and \r) and DEL (0x7F).
// Newline/CR/tab become readable named escapes (\n, \r, \t); other control
// bytes become \xNN. Printable input (the common case) is returned unchanged so
// normal messages stay readable. This is the console-path analog of the JSON
// sink's json.Encoder escaping, which already neutralizes the same vector.
func sanitizeConsole(s string) string {
	// Fast path: no control bytes, return as-is without allocating.
	if !strings.ContainsFunc(s, isConsoleControl) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !isConsoleControl(rune(c)) {
			b.WriteByte(c)
			continue
		}
		switch c {
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteString(`\x`)
			b.WriteByte(hexDigit(c >> 4))
			b.WriteByte(hexDigit(c & 0x0f))
		}
	}
	return b.String()
}

// isConsoleControl reports whether r is a control byte that must be escaped on
// the console path (C0 controls 0x00–0x1F or DEL 0x7F).
func isConsoleControl(r rune) bool {
	return r < 0x20 || r == 0x7f
}

func hexDigit(n byte) byte {
	const digits = "0123456789ABCDEF"
	return digits[n&0x0f]
}
