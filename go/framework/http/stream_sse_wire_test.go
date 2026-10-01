package http

import (
	"context"
	"errors"
	stdhttp "net/http"
	"net/http/httptest"
	"testing"

	perrors "go.putnami.dev/errors"
)

// testSSEWire is a negotiated wire this package knows nothing about: the api
// plugin fills the real one from the client contract.
var testSSEWire = &SSEWire{
	Header:     "X-Test-Wire",
	Token:      "test.v1",
	Negotiates: func(lines []string) bool { return len(lines) == 1 && lines[0] == "test.v1" },
	Complete:   "event: done\ndata: {}\n\n",
}

// serveTestSSE drives one SSE request through a route that speaks wire.
func serveTestSSE(t *testing.T, wire *SSEWire, ctx context.Context, writer stdhttp.ResponseWriter,
	lines []string, handle func(*StreamContext) error, before Handler) *Response {
	t.Helper()
	sp := NewServerPlugin(ServerConfig{})
	sp.HandleStream("/logs", StreamHandler{Mode: StreamModeServer, Handle: handle, Before: before, SSEWire: wire})
	req := httptest.NewRequest("GET", "/logs", nil).WithContext(ctx)
	req.Header.Set("Accept", "text/event-stream")
	if lines != nil {
		req.Header["X-Test-Wire"] = lines
	}
	return sp.routes.lookup("GET", "/logs").Handlers[0](NewContext(writer, req))
}

func sendOne(ctx *StreamContext) error { return ctx.Send(map[string]string{"line": "one"}) }

func TestSSEWire_NegotiatedStreamIsAcknowledgedAndEndsWithComplete(t *testing.T) {
	rec := httptest.NewRecorder()
	serveTestSSE(t, testSSEWire, context.Background(), rec, []string{"test.v1"}, sendOne, nil)
	if got := rec.Header().Get("X-Test-Wire"); got != "test.v1" {
		t.Fatalf("acknowledgment = %q, want test.v1", got)
	}
	if got, want := rec.Body.String(), "data: {\"line\":\"one\"}\n\nevent: done\ndata: {}\n\n"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

func TestSSEWire_RequestsThatDoNotNegotiateKeepTheLegacyFraming(t *testing.T) {
	for name, testCase := range map[string]struct {
		wire  *SSEWire
		lines []string
	}{
		"no marker":            {wire: testSSEWire},
		"another token":        {wire: testSSEWire, lines: []string{"test.v2"}},
		"a route with no wire": {lines: []string{"test.v1"}},
		"no negotiation rule":  {wire: &SSEWire{Header: "X-Test-Wire", Token: "test.v1"}, lines: []string{"test.v1"}},
	} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			serveTestSSE(t, testCase.wire, context.Background(), rec, testCase.lines, sendOne, nil)
			if got := rec.Header().Get("X-Test-Wire"); got != "" {
				t.Fatalf("acknowledgment = %q on a request that did not negotiate", got)
			}
			if got, want := rec.Body.String(), "data: {\"line\":\"one\"}\n\n"; got != want {
				t.Fatalf("body = %q, want the legacy framing %q", got, want)
			}
		})
	}
}

func TestSSEWire_AFailedHandlerEndsWithTheTypedErrorAndNeverWithComplete(t *testing.T) {
	rec := httptest.NewRecorder()
	serveTestSSE(t, testSSEWire, context.Background(), rec, []string{"test.v1"}, func(ctx *StreamContext) error {
		_ = sendOne(ctx)
		return perrors.NotFound("run gone")
	}, nil)
	want := "data: {\"line\":\"one\"}\n\nevent: error\ndata: {\"status\":404,\"code\":\"not_found\",\"error\":\"Not Found\",\"message\":\"run gone\"}\n\n"
	if got := rec.Body.String(); got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

func TestSSEWire_ACanceledStreamEndsWithNoTerminal(t *testing.T) {
	for name, outcome := range map[string]error{"returns": nil, "fails": errors.New("gave up"), "reports cancellation": context.Canceled} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			rec := httptest.NewRecorder()
			serveTestSSE(t, testSSEWire, ctx, rec, []string{"test.v1"}, func(stream *StreamContext) error {
				_ = sendOne(stream)
				cancel()
				<-stream.Context.Context().Done()
				return outcome
			}, nil)
			if got := rec.Header().Get("X-Test-Wire"); got != "test.v1" {
				t.Fatalf("acknowledgment = %q, want test.v1", got)
			}
			if got, want := rec.Body.String(), "data: {\"line\":\"one\"}\n\n"; got != want {
				t.Fatalf("body = %q, want no terminal after a cancellation", got)
			}
		})
	}
}

// failingWriter refuses every body write, the way a stalled consumer's write
// deadline does.
type failingWriter struct{ *httptest.ResponseRecorder }

func (w failingWriter) Write([]byte) (int, error) { return 0, errors.New("write deadline exceeded") }
func (w failingWriter) Flush()                    { w.ResponseRecorder.Flush() }

func TestSSEWire_AStreamWhoseSendFailedNeverEndsWithComplete(t *testing.T) {
	rec := httptest.NewRecorder()
	var sendErr error
	serveTestSSE(t, testSSEWire, context.Background(), failingWriter{rec}, []string{"test.v1"}, func(ctx *StreamContext) error {
		sendErr = sendOne(ctx)
		return nil // a handler that ignores the failed send
	}, nil)
	if sendErr == nil {
		t.Fatal("the send did not observe the failed write")
	}
	if got := rec.Body.String(); got != "" {
		t.Fatalf("body = %q, want nothing after a failed send", got)
	}
}

func TestSSEWire_ARefusedAdmissionIsNotAcknowledged(t *testing.T) {
	rec := httptest.NewRecorder()
	resp := serveTestSSE(t, testSSEWire, context.Background(), rec, []string{"test.v1"}, sendOne, func(*Context) *Response {
		return JSONStatus(stdhttp.StatusUnauthorized, map[string]string{"error": "Unauthorized"})
	})
	if resp == nil || resp.Status != stdhttp.StatusUnauthorized {
		t.Fatalf("response = %+v, want the 401 refusal", resp)
	}
	if got := rec.Header().Get("X-Test-Wire"); got != "" {
		t.Fatalf("acknowledgment = %q on a refused admission", got)
	}
}

// A handler that returns without error after the server began draining never
// reports success on a negotiated stream; a legacy stream keeps its framing.
func TestSSEWire_ADrainingServerNeverEndsANegotiatedStreamWithComplete(t *testing.T) {
	for name, lines := range map[string][]string{"negotiated": {"test.v1"}, "legacy": nil} {
		t.Run(name, func(t *testing.T) {
			sp := NewServerPlugin(ServerConfig{})
			sp.HandleStream("/logs", StreamHandler{Mode: StreamModeServer, SSEWire: testSSEWire, Handle: func(stream *StreamContext) error {
				_ = sendOne(stream)
				sp.beginDrain()
				return nil // returns before it could observe the drain
			}})
			req := httptest.NewRequest("GET", "/logs", nil)
			req.Header.Set("Accept", "text/event-stream")
			if lines != nil {
				req.Header["X-Test-Wire"] = lines
			}
			rec := httptest.NewRecorder()
			sp.routes.lookup("GET", "/logs").Handlers[0](NewContext(rec, req))
			if got, want := rec.Body.String(), "data: {\"line\":\"one\"}\n\n"; got != want {
				t.Fatalf("body = %q, want %q", got, want)
			}
		})
	}
}

// Stop cancels the handler of a negotiated stream, which then ends with no
// terminal instead of running until the shutdown deadline cuts it.
func TestSSEWire_StopEndsTheHandlerOfANegotiatedStream(t *testing.T) {
	sp := NewServerPlugin(ServerConfig{})
	started := make(chan struct{})
	go func() {
		<-started
		_ = sp.Stop(context.Background(), nil)
	}()
	rec := httptest.NewRecorder()
	serveTestSSEOn(t, sp, rec, func(stream *StreamContext) error {
		_ = sendOne(stream)
		close(started)
		<-stream.Context.Context().Done()
		return nil
	})
	if got, want := rec.Body.String(), "data: {\"line\":\"one\"}\n\n"; got != want {
		t.Fatalf("body = %q, want no terminal after a drain", got)
	}
}

func serveTestSSEOn(t *testing.T, sp *ServerPlugin, writer stdhttp.ResponseWriter, handle func(*StreamContext) error) {
	t.Helper()
	sp.HandleStream("/logs", StreamHandler{Mode: StreamModeServer, Handle: handle, SSEWire: testSSEWire})
	req := httptest.NewRequest("GET", "/logs", nil)
	req.Header.Set("Accept", "text/event-stream")
	req.Header["X-Test-Wire"] = []string{"test.v1"}
	sp.routes.lookup("GET", "/logs").Handlers[0](NewContext(writer, req))
}
