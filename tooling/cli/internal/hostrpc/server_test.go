package hostrpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	wire "go.putnami.dev/protocol/extension/hostrpc"
	"go.putnami.dev/protocol/features/spectest"
	sdkhostrpc "go.putnami.dev/sdk/extension/hostrpc"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

const (
	feature   = "cli/job-planning-execution"
	lifecycle = "extension-command-host-rpc"
	calls     = "host-rpc-core-calls"
)

// startServer opens a channel that Stop closes when the test ends.
func startServer(t *testing.T, methods Methods) *Server {
	t.Helper()
	server, err := Start(t.Context(), methods)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Stop(); err != nil {
			t.Error(err)
		}
	})
	return server
}

// token returns the token the launched process would receive.
func token(t *testing.T, server *Server) string {
	t.Helper()
	for _, entry := range server.Env() {
		if value, ok := strings.CutPrefix(entry, wire.TokenEnv+"="); ok {
			return value
		}
	}
	t.Fatal("the channel names no token")
	return ""
}

// client connects the SDK client, as a launched extension would.
func client(t *testing.T, server *Server) *sdkhostrpc.Client {
	t.Helper()
	connected, err := sdkhostrpc.Connect(server.Socket(), token(t, server))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connected.Close() })
	return connected
}

// rawConn is a connection that writes frames as given and reads answers.
type rawConn struct {
	conn   net.Conn
	reader *bufio.Reader
}

func dialRaw(t *testing.T, server *Server) *rawConn {
	t.Helper()
	conn, err := net.Dial("unix", server.Socket())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &rawConn{conn: conn, reader: bufio.NewReader(conn)}
}

func (c *rawConn) send(t *testing.T, frame string) {
	t.Helper()
	if _, err := io.WriteString(c.conn, frame+"\n"); err != nil {
		t.Fatal(err)
	}
}

func (c *rawConn) answer(t *testing.T) wire.Response {
	t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	frame, err := wire.ReadFrame(c.reader, wire.MaxFrameBytes)
	if err != nil {
		t.Fatalf("no answer: %v", err)
	}
	var response wire.Response
	if err := wire.DecodeStrict(frame, &response); err != nil {
		t.Fatalf("the answer %s is not a response: %v", frame, err)
	}
	return response
}

// closed reports whether the host closed the connection. A host that closes
// with unread bytes may reset it instead of ending it, so any read failure
// but the deadline counts.
func (c *rawConn) closed(t *testing.T) bool {
	t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	_, err := c.reader.ReadByte()
	return err != nil && !errors.Is(err, os.ErrDeadlineExceeded)
}

func request(id, method, secret, params string) string {
	frame := `{"jsonrpc":"2.0","id":` + id + `,"method":"` + method + `","token":"` + secret + `"`
	if params != "" {
		frame += `,"params":` + params
	}
	return frame + "}"
}

func wantError(t *testing.T, response wire.Response, id string, code int) {
	t.Helper()
	if response.Error == nil || response.Error.Code != code {
		t.Fatalf("answer %+v, want error %d", response, code)
	}
	if string(response.ID) != id {
		t.Errorf("answer id %s, want %s", response.ID, id)
	}
	if len(response.Result) != 0 {
		t.Errorf("an error answer carries a result %s", response.Result)
	}
}

func TestARequestWithoutTheTokenIsRefusedAndTheConnectionCloses(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, feature, calls, "a-request-without-the-token-is-refused")
	server := startServer(t, Methods{})
	right := token(t, server)
	for _, wrong := range []string{"", strings.Repeat("0", len(right)), right[:len(right)-1], right + "0"} {
		conn := dialRaw(t, server)
		conn.send(t, request("1", wire.MethodHello, wrong, ""))
		wantError(t, conn.answer(t), "1", wire.CodeUnauthorized)
		if !conn.closed(t) {
			t.Errorf("the host kept a connection open after the token %q", wrong)
		}
	}
	conn := dialRaw(t, server)
	conn.send(t, request("1", wire.MethodHello, right, ""))
	if response := conn.answer(t); response.Error != nil {
		t.Fatalf("the launch's token was refused: %+v", response.Error)
	}
}

// A notification gets no answer, so one with another token closes the
// connection without the refusal a request would get (JSON-RPC 2.0).
func TestANotificationWithoutTheTokenClosesTheConnectionUnanswered(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, feature, calls, "a-request-without-the-token-is-refused")
	server := startServer(t, Methods{})
	conn := dialRaw(t, server)
	conn.send(t, `{"jsonrpc":"2.0","method":"host.hello","token":"wrong"}`)
	if !conn.closed(t) {
		t.Fatal("the host answered a notification with another token, or kept its connection open")
	}
}

// failingListener fails its first accepts, as a listener past the open-file
// limit does, and then accepts.
type failingListener struct {
	net.Listener
	failures atomic.Int32
}

func (l *failingListener) Accept() (net.Conn, error) {
	if l.failures.Add(-1) >= 0 {
		return nil, &net.OpError{Op: "accept", Net: "unix", Err: errors.New("too many open files")}
	}
	return l.Listener.Accept()
}

func TestAFailedAcceptIsTriedAgain(t *testing.T) {
	t.Parallel()
	secret, err := newToken()
	if err != nil {
		t.Fatal(err)
	}
	dir, socket, err := openSocketDir(socketRoots())
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		_ = dir.Remove()
		t.Fatal(err)
	}
	failing := &failingListener{Listener: listener}
	failing.failures.Store(3)
	server := serveOn(t.Context(), Methods{}, dir, socket, secret, failing)
	t.Cleanup(func() {
		if err := server.Stop(); err != nil {
			t.Error(err)
		}
	})
	// A host whose accept loop ended would never answer: bound the wait.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if _, err := client(t, server).Hello(ctx); err != nil {
		t.Fatalf("the host stopped serving after a failed accept: %v", err)
	}
	if failing.failures.Load() >= 0 {
		t.Error("the host answered without accepting past the failed accepts")
	}
}

func TestHelloReturnsTheProtocolVersionAndTheMethods(t *testing.T) {
	t.Parallel()
	hello, err := client(t, startServer(t, Methods{})).Hello(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if hello.Protocol != wire.ProtocolVersion || strings.Join(hello.Methods, ",") != strings.Join(wire.Methods(), ",") {
		t.Fatalf("hello = %+v", hello)
	}
}

func TestAnUnknownMethodIsMethodNotFound(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, feature, calls, "an-unknown-method-is-method-not-found")
	server := startServer(t, Methods{})
	conn := dialRaw(t, server)
	conn.send(t, request(`"a"`, "host.shutdown", token(t, server), ""))
	response := conn.answer(t)
	wantError(t, response, `"a"`, wire.CodeMethodNotFound)
	if !strings.Contains(response.Error.Message, wire.MethodHello) {
		t.Errorf("the refusal %q does not point at %s", response.Error.Message, wire.MethodHello)
	}
	// The connection still serves.
	conn.send(t, request("2", wire.MethodHello, token(t, server), ""))
	if response := conn.answer(t); response.Error != nil {
		t.Fatalf("the next request was refused: %+v", response.Error)
	}
}

func TestAFrameOverTheLimitIsRefused(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, feature, calls, "a-frame-over-the-limit-is-refused")
	server := startServer(t, Methods{})
	conn := dialRaw(t, server)
	go func() {
		// The host stops reading once the frame passes the limit, so this
		// write may fail; the answer is what the test reads.
		_, _ = conn.conn.Write([]byte(strings.Repeat("x", wire.MaxFrameBytes+1)))
	}()
	wantError(t, conn.answer(t), "null", wire.CodeInvalidRequest)
	if !conn.closed(t) {
		t.Error("the host kept the connection open after an oversize frame")
	}
}

func TestMalformedRequestsAreRefusedAndNotificationsUnanswered(t *testing.T) {
	t.Parallel()
	server := startServer(t, Methods{})
	right := token(t, server)
	conn := dialRaw(t, server)

	conn.send(t, "not json")
	wantError(t, conn.answer(t), "null", wire.CodeParseError)
	conn.send(t, `["a request is an object"]`)
	wantError(t, conn.answer(t), "null", wire.CodeInvalidRequest)
	conn.send(t, `{"jsonrpc":"1.0","id":3,"method":"host.hello","token":"`+right+`"}`)
	wantError(t, conn.answer(t), "3", wire.CodeInvalidRequest)
	conn.send(t, `{"jsonrpc":"2.0","id":{},"method":"host.hello","token":"`+right+`"}`)
	wantError(t, conn.answer(t), "null", wire.CodeInvalidRequest)
	// A notification gets no answer: the next answer is the next request's.
	conn.send(t, `{"jsonrpc":"2.0","method":"host.hello","token":"`+right+`"}`)
	conn.send(t, request("5", wire.MethodHello, right, ""))
	if response := conn.answer(t); string(response.ID) != "5" || response.Error != nil {
		t.Fatalf("answer %+v, want request 5's result", response)
	}
}

func TestParamsAreStrict(t *testing.T) {
	t.Parallel()
	server := startServer(t, Methods{
		Collaboration: func(context.Context, wire.CollaborationCallParams) (json.RawMessage, error) {
			return json.RawMessage(`{}`), nil
		},
		VerificationReport: func(context.Context, wire.VerificationReportParams) ([]byte, error) {
			return []byte("r"), nil
		},
	})
	right := token(t, server)
	conn := dialRaw(t, server)
	for i, tc := range []struct {
		method, params string
	}{
		{wire.MethodHello, `{"x":1}`},
		{wire.MethodCollaborationCall, ``},
		{wire.MethodCollaborationCall, `{"contract":"tasks"}`},
		{wire.MethodCollaborationCall, `{"contract":"tasks","operation":"find","inputs":{}}`},
		{wire.MethodVerificationReport, `{"project":"p","task":"t","path":"x"}`},
		{wire.MethodVerificationReport, `[]`},
	} {
		id := strconv.Quote("p" + strconv.Itoa(i))
		conn.send(t, request(id, tc.method, right, tc.params))
		wantError(t, conn.answer(t), id, wire.CodeInvalidParams)
	}
}

func TestAMethodTheLaunchDoesNotServeIsUnavailable(t *testing.T) {
	t.Parallel()
	c := client(t, startServer(t, Methods{}))
	_, err := c.CallCollaboration(t.Context(), "tasks", "find", nil)
	var refusal *wire.Error
	if !errors.As(err, &refusal) || refusal.Code != wire.CodeUnavailable {
		t.Fatalf("collaboration.call = %v, want %d", err, wire.CodeUnavailable)
	}
	_, err = c.VerificationReport(t.Context(), wire.VerificationReportParams{Project: "p", Task: "t", Path: "x", Digest: "d"})
	if !errors.As(err, &refusal) || refusal.Code != wire.CodeUnavailable {
		t.Fatalf("verification.report = %v, want %d", err, wire.CodeUnavailable)
	}
}

func TestCollaborationCallReturnsTheMethodsEnvelope(t *testing.T) {
	t.Parallel()
	envelope := `{"contract":"tasks","operation":"find","outcome":"ok","result":{"items":[]}}`
	var received wire.CollaborationCallParams
	c := client(t, startServer(t, Methods{
		Collaboration: func(_ context.Context, call wire.CollaborationCallParams) (json.RawMessage, error) {
			received = call
			return json.RawMessage(envelope), nil
		},
	}))
	got, err := c.CallCollaboration(t.Context(), "tasks", "find", json.RawMessage(`{"projects":["app"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != envelope {
		t.Errorf("envelope %s, want %s", got, envelope)
	}
	if received.Contract != "tasks" || received.Operation != "find" || string(received.Input) != `{"projects":["app"]}` {
		t.Errorf("the method received %+v", received)
	}
}

func TestVerificationReportReturnsTheMethodsBytesAndRefusal(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, feature, calls, "verification-report-returns-what-the-cache-corroborates")
	report := []byte("<testsuites>\x00\xff</testsuites>")
	ref := wire.VerificationReportParams{Project: "app", Task: "app:test", Path: "out/report.xml", Digest: "sha256:00"}
	c := client(t, startServer(t, Methods{
		VerificationReport: func(_ context.Context, got wire.VerificationReportParams) ([]byte, error) {
			switch {
			case got == ref:
				return report, nil
			case got.Project == "plain":
				return nil, errors.New("disk on fire")
			default:
				return nil, &wire.Error{Code: wire.CodeNotVerified, Message: "no producer"}
			}
		},
	}))
	got, err := c.VerificationReport(t.Context(), ref)
	if err != nil || string(got) != string(report) {
		t.Fatalf("VerificationReport = %q, %v, want the method's bytes", got, err)
	}
	var refusal *wire.Error
	other := ref
	other.Digest = "sha256:11"
	if _, err := c.VerificationReport(t.Context(), other); !errors.As(err, &refusal) ||
		refusal.Code != wire.CodeNotVerified || refusal.Message != "no producer" {
		t.Fatalf("an uncorroborated report = %v, want the method's refusal", err)
	}
	other.Project = "plain"
	if _, err := c.VerificationReport(t.Context(), other); !errors.As(err, &refusal) ||
		refusal.Code != wire.CodeInternalError || refusal.Message != "disk on fire" {
		t.Fatalf("a failing method = %v, want an internal error", err)
	}
}

func TestCallsAreServedOneAtATime(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, feature, calls, "calls-are-served-one-at-a-time")
	var running, most atomic.Int32
	server := startServer(t, Methods{
		Collaboration: func(context.Context, wire.CollaborationCallParams) (json.RawMessage, error) {
			now := running.Add(1)
			defer running.Add(-1)
			for {
				seen := most.Load()
				if now <= seen || most.CompareAndSwap(seen, now) {
					break
				}
			}
			time.Sleep(10 * time.Millisecond)
			return json.RawMessage(`{}`), nil
		},
	})
	var wg sync.WaitGroup
	for range 4 {
		c := client(t, server)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 3 {
				if _, err := c.CallCollaboration(t.Context(), "tasks", "find", nil); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if got := most.Load(); got != 1 {
		t.Fatalf("%d calls ran at once, want 1", got)
	}
}

func TestStopCancelsTheCallInFlight(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, feature, calls, "stopping-cancels-the-call-in-flight")
	started := make(chan struct{})
	canceled := make(chan struct{})
	server, err := Start(t.Context(), Methods{
		Collaboration: func(ctx context.Context, _ wire.CollaborationCallParams) (json.RawMessage, error) {
			close(started)
			<-ctx.Done()
			close(canceled)
			return nil, ctx.Err()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	c, err := sdkhostrpc.Connect(server.Socket(), token(t, server))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	answered := make(chan error, 1)
	go func() {
		_, err := c.CallCollaboration(t.Context(), "tasks", "find", nil)
		answered <- err
	}()
	<-started
	if err := server.Stop(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-canceled:
	case <-time.After(30 * time.Second):
		t.Fatal("Stop did not cancel the call in flight")
	}
	if err := <-answered; err == nil {
		t.Fatal("a call the host stopped returned a result")
	}
}

func TestStopRemovesTheSocketAndItsPrivateDirectory(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, feature, lifecycle, "the-channel-closes-when-the-process-exits-whatever-its-exit")
	server, err := Start(t.Context(), Methods{})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(server.Socket())
	if !strings.HasPrefix(filepath.Base(dir), dirPrefix) {
		t.Errorf("the socket directory %s is not named %s*", dir, dirPrefix)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode != 0o700 {
			t.Errorf("the socket directory has mode %04o, want 0700", mode)
		}
	}
	if len(server.Socket()) > maxSocketPath {
		t.Errorf("the socket path %s is longer than %d bytes", server.Socket(), maxSocketPath)
	}
	env := server.Env()
	if len(env) != 2 || env[0] != wire.SocketEnv+"="+server.Socket() || len(token(t, server)) != 2*wire.TokenBytes {
		t.Errorf("Env() = %q, want the socket and a %d-byte hex token", env, wire.TokenBytes)
	}
	c := client(t, server)
	if _, err := c.Hello(t.Context()); err != nil {
		t.Fatal(err)
	}

	if err := server.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := server.Stop(); err != nil {
		t.Fatalf("a second Stop failed: %v", err)
	}
	for _, path := range []string{server.Socket(), dir} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s survived Stop: %v", path, err)
		}
	}
	if _, err := c.Hello(t.Context()); err == nil {
		t.Error("a connection survived Stop")
	}
}

func TestEveryChannelHasItsOwnToken(t *testing.T) {
	t.Parallel()
	first, second := startServer(t, Methods{}), startServer(t, Methods{})
	if token(t, first) == token(t, second) || first.Socket() == second.Socket() {
		t.Fatal("two channels share a token or a socket")
	}
	c, err := sdkhostrpc.Connect(first.Socket(), token(t, second))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	var refusal *wire.Error
	if _, err := c.Hello(t.Context()); !errors.As(err, &refusal) || refusal.Code != wire.CodeUnauthorized {
		t.Fatalf("another channel's token = %v, want %d", err, wire.CodeUnauthorized)
	}
}

func TestTheSocketDirectoryFallsBackWhenARootIsTooLong(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the /tmp fallback is a Unix one")
	}
	t.Parallel()
	long := filepath.Join(t.TempDir(), strings.Repeat("d", maxSocketPath))
	if err := os.MkdirAll(long, 0o700); err != nil {
		t.Fatal(err)
	}
	server, err := startIn(t.Context(), Methods{}, []string{long, "/tmp"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Stop() })
	if !strings.HasPrefix(server.Socket(), "/tmp/") {
		t.Fatalf("socket %s, want one under /tmp", server.Socket())
	}
	entries, err := os.ReadDir(long)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the refused root keeps %d entries", len(entries))
	}

	if _, err := startIn(t.Context(), Methods{}, []string{long}); err == nil ||
		!strings.Contains(err.Error(), "longer than") {
		t.Fatalf("a channel with no usable root = %v, want an error naming the length", err)
	}
}

func TestAHostedRunOpensNoChannel(t *testing.T) {
	spectest.Proves(t, feature, lifecycle, "a-hosted-run-opens-no-channel")
	t.Cleanup(runcredential.SetForTest("hosted-bearer"))
	server, err := Start(t.Context(), Methods{})
	if !errors.Is(err, ErrHosted) || server != nil {
		t.Fatalf("Start = %v, %v, want ErrHosted", server, err)
	}
}
