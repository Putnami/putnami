// Package recorded serves exchanges recorded at a production boundary, so a
// test that guards that boundary answers with the bytes production sent instead
// of a stub its author imagined.
//
// A hand-written stub answers the way its author believes the other side
// answers. When that belief is wrong, the test is green and the product is not:
// a registry refusal written as `http.Error(w, "private", 401)` never carried the
// registry's JSON, and a core-only CLI written as `echo "Unknown command"` never
// printed what a core-only CLI prints. A recording removes the belief. The file
// is what crossed the wire, and where it came from is written beside it.
//
// # Two recording shapes
//
// An HTTP response is recorded with curl, which writes the status line, the
// headers and the body exactly as they arrived:
//
//	curl -si --http1.1 'https://registry.example/path' > refusal.401.http
//
// A command exchange is a directory holding the three things a caller of a
// command can observe:
//
//	mkdir not-signed-in && some-command >not-signed-in/stdout 2>not-signed-in/stderr; echo $? >not-signed-in/exit
//
// # Redaction
//
// A recording never carries a live credential or personal data. Replace a
// secret with a value of the same length and the same shape — a JWT stays three
// base64url segments of the recorded sizes — so the size a parser meets is still
// the production size, and state the redaction in the directory's provenance
// note.
package recorded

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Response is one HTTP response recorded at a production boundary.
type Response struct {
	status int
	header http.Header
	body   []byte
}

// HTTP loads the response recorded at path. It fails tb when the file is
// missing or is not one complete HTTP/1.x response, so a truncated recording
// cannot quietly answer with an empty body.
func HTTP(tb testing.TB, path string) Response {
	tb.Helper()
	response, err := parseHTTP(path)
	if err != nil {
		tb.Fatalf("recorded response %s: %v", path, err)
	}
	return response
}

func parseHTTP(path string) (Response, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // a test names its own recording
	if err != nil {
		return Response{}, err
	}
	parsed, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(raw)), nil)
	if err != nil {
		return Response{}, fmt.Errorf("not an HTTP/1.x response (record it with curl -si --http1.1): %w", err)
	}
	defer parsed.Body.Close()
	body, err := io.ReadAll(parsed.Body)
	if err != nil {
		return Response{}, fmt.Errorf("body shorter than its headers declare: %w", err)
	}
	return Response{status: parsed.StatusCode, header: parsed.Header, body: body}, nil
}

// Status returns the recorded status code.
func (r Response) Status() int { return r.status }

// Body returns a copy of the recorded body.
func (r Response) Body() []byte { return bytes.Clone(r.body) }

// ServeHTTP writes the recorded response. The framing headers a server owns —
// Transfer-Encoding and Connection — are left to net/http, which frames the
// same body again; every other header is written as recorded.
func (r Response) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	for name, values := range r.header {
		switch http.CanonicalHeaderKey(name) {
		case "Transfer-Encoding", "Connection":
			continue
		}
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	w.WriteHeader(r.status)
	_, _ = w.Write(r.body) //nolint:gosec // G705: replays a recorded response to a loopback test client
}

// Server answers the first request with the first response, the second with
// the second, and so on. Once the responses are spent, every later request goes
// to the handler given to NewServer, or receives the last response again when
// that handler is nil — so one recording answers every request, and a
// recorded fault followed by a handler answers a retry.
type Server struct {
	*httptest.Server

	mu        sync.Mutex
	responses []Response
	next      http.Handler
	requests  []*http.Request
}

// NewServer starts a loopback server that replays responses in order, then
// hands over to next. It closes when tb ends.
func NewServer(tb testing.TB, next http.Handler, responses ...Response) *Server {
	tb.Helper()
	if len(responses) == 0 && next == nil {
		tb.Fatal("recorded.NewServer needs a response or a handler")
	}
	s := &Server{responses: responses, next: next}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	tb.Cleanup(s.Close)
	return s
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	index := len(s.requests)
	s.requests = append(s.requests, r.Clone(r.Context()))
	s.mu.Unlock()

	switch {
	case index < len(s.responses):
		s.responses[index].ServeHTTP(w, r)
	case s.next != nil:
		s.next.ServeHTTP(w, r)
	default:
		s.responses[len(s.responses)-1].ServeHTTP(w, r)
	}
}

// Requests returns the requests the server received, in arrival order. Their
// bodies are not retained.
func (s *Server) Requests() []*http.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*http.Request(nil), s.requests...)
}

// Exchange is one command invocation recorded at a production boundary: what
// the command wrote on stdout and on stderr, and the status it exited with.
type Exchange struct {
	stdout   []byte
	stderr   []byte
	exitCode int
}

// Command loads the exchange recorded in dir, which holds the files stdout,
// stderr and exit. It fails tb when one is missing or exit is not a number.
func Command(tb testing.TB, dir string) Exchange {
	tb.Helper()
	exchange, err := parseCommand(dir)
	if err != nil {
		tb.Fatalf("recorded exchange %s: %v", dir, err)
	}
	return exchange
}

func parseCommand(dir string) (Exchange, error) {
	stdout, err := os.ReadFile(filepath.Join(dir, "stdout")) //nolint:gosec // a test names its own recording
	if err != nil {
		return Exchange{}, err
	}
	stderr, err := os.ReadFile(filepath.Join(dir, "stderr")) //nolint:gosec // a test names its own recording
	if err != nil {
		return Exchange{}, err
	}
	rawExit, err := os.ReadFile(filepath.Join(dir, "exit")) //nolint:gosec // a test names its own recording
	if err != nil {
		return Exchange{}, err
	}
	code, err := strconv.Atoi(strings.TrimSpace(string(rawExit)))
	if err != nil || code < 0 || code > 255 {
		return Exchange{}, fmt.Errorf("exit holds %q, want a status from 0 to 255", strings.TrimSpace(string(rawExit)))
	}
	return Exchange{stdout: stdout, stderr: stderr, exitCode: code}, nil
}

// Stdout returns a copy of the recorded stdout.
func (e Exchange) Stdout() []byte { return bytes.Clone(e.stdout) }

// Stderr returns a copy of the recorded stderr.
func (e Exchange) Stderr() []byte { return bytes.Clone(e.stderr) }

// ExitCode returns the recorded exit status.
func (e Exchange) ExitCode() int { return e.exitCode }

// Branch makes an executable replay a different exchange when its environment
// sets Env to Value. It models a condition the recorded command itself has — a
// CLI that runs a first-use install unless told not to — and nothing more.
type Branch struct {
	Env      string
	Value    string
	Exchange Exchange
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Executable writes a program that replays exchange, or the first branch whose
// condition its environment meets, and returns the program's absolute path.
// The program is a POSIX shell script on Unix, where the test is skipped when
// /bin/sh does not exist, and a batch file cmd.exe runs on Windows.
func Executable(tb testing.TB, exchange Exchange, branches ...Branch) string {
	tb.Helper()
	return executable(tb, exchange, branches)
}
