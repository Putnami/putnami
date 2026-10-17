package http

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func TestRefusalEnvelope_UnroutedMethodAnswersMethodNotAllowed(t *testing.T) {
	spectest.Proves(t, "go/http-services", "refusal-envelope", "an-unrouted-method-answers-405-with-allow")
	p := NewServerPlugin(ServerConfig{Port: 0})
	p.GET("/items", func(*Context) *Response { return JSON("ok") })
	p.PUT("/items", func(*Context) *Response { return JSON("ok") })
	p.POST("/orders", func(*Context) *Response { return JSON("ok") })
	ts := p.TestServer()
	defer ts.Close()

	cases := []struct {
		method, path, allow string
	}{
		{http.MethodDelete, "/items", "GET, PUT, HEAD, OPTIONS"},
		{http.MethodGet, "/orders", "POST, OPTIONS"},
		{http.MethodHead, "/orders", "POST, OPTIONS"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, ts.URL+tc.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			if got := resp.Header.Get("Allow"); got != tc.allow {
				t.Errorf("Allow = %q, want %q", got, tc.allow)
			}
			if tc.method == http.MethodHead {
				resp.Body.Close()
				if resp.StatusCode != http.StatusMethodNotAllowed {
					t.Fatalf("status = %d, want 405", resp.StatusCode)
				}
				return
			}
			envelopeOf(t, resp, http.StatusMethodNotAllowed, "http.method_not_allowed", "The requested method is not allowed for this resource")
		})
	}

	resp, err := http.Post(ts.URL+"/missing", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	envelopeOf(t, resp, http.StatusNotFound, "not_found", "The requested resource was not found")
}

func TestRefusalEnvelope_OptionsOnlyPathAnswersNotFound(t *testing.T) {
	spectest.Proves(t, "go/http-services", "refusal-envelope", "an-unrouted-method-answers-405-with-allow")
	p := NewServerPlugin(ServerConfig{Port: 0})
	p.Handle(http.MethodOptions, "/{path...}", func(*Context) *Response { return NoContent() })
	ts := p.TestServer()
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/typo")
	if err != nil {
		t.Fatal(err)
	}
	envelopeOf(t, resp, http.StatusNotFound, "not_found", "The requested resource was not found")
}

func TestRefusalEnvelope_PreflightAllowNamesHead(t *testing.T) {
	spectest.Proves(t, "go/http-services", "refusal-envelope", "an-unrouted-method-answers-405-with-allow")
	p := NewServerPlugin(ServerConfig{Port: 0})
	p.GET("/items", func(*Context) *Response { return JSON("ok") })
	ts := p.TestServer()
	defer ts.Close()

	req, err := http.NewRequest(http.MethodOptions, ts.URL+"/items", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("Allow"); got != "GET, HEAD, OPTIONS" {
		t.Errorf("Allow = %q, want %q", got, "GET, HEAD, OPTIONS")
	}
}

func TestRefusalEnvelope_RateLimitAnswersTooManyRequests(t *testing.T) {
	spectest.Proves(t, "go/http-services", "refusal-envelope", "the-rate-limiter-answers-429-too-many-requests")
	p := NewServerPlugin(ServerConfig{Port: 0})
	p.Use(RateLimit(RateLimitOptions{
		Max:     1,
		KeyFunc: func(*Context) string { return "one-client" },
		Message: "Slow down",
	}))
	p.GET("/x", func(*Context) *Response { return JSON("ok") })
	ts := p.TestServer()
	defer ts.Close()

	first, err := http.Get(ts.URL + "/x")
	if err != nil {
		t.Fatal(err)
	}
	first.Body.Close()
	resp, err := http.Get(ts.URL + "/x")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("Retry-After header missing")
	}
	envelopeOf(t, resp, http.StatusTooManyRequests, "http.too_many_requests", "Slow down")
}

// streamRefusal sends one request to a stream route and returns its answer.
func streamRefusal(t *testing.T, stream StreamHandler, header http.Header) *http.Response {
	t.Helper()
	p := NewServerPlugin(ServerConfig{Port: 0})
	p.HandleStream("/s", stream)
	ts := httptest.NewServer(p.Handler())
	t.Cleanup(ts.Close)
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/s", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header = header
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func upgradeHeader(pairs ...string) http.Header {
	h := http.Header{}
	h.Set("Upgrade", "websocket")
	h.Set("Connection", "Upgrade")
	h.Set("Sec-WebSocket-Version", "13")
	h.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	for i := 0; i+1 < len(pairs); i += 2 {
		if pairs[i+1] == "" {
			h.Del(pairs[i])
			continue
		}
		h.Set(pairs[i], pairs[i+1])
	}
	return h
}

func TestRefusalEnvelope_StreamRouteRefusalsAnswerBadRequest(t *testing.T) {
	spectest.Proves(t, "go/http-services", "refusal-envelope", "a-refused-stream-or-upgrade-answers-400-bad-request")
	handle := func(*StreamContext) error { return nil }
	serve := func(*Context, *WebSocketConn) error { return nil }
	cases := []struct {
		name    string
		stream  StreamHandler
		header  http.Header
		message string
	}{
		{
			name:    "neither upgrade nor event stream",
			stream:  StreamHandler{Mode: StreamModeServer, Handle: handle},
			header:  http.Header{},
			message: `stream endpoint requires "Upgrade: websocket" or "Accept: text/event-stream"`,
		},
		{
			name:    "subprotocol other than the admitted one",
			stream:  StreamHandler{Mode: StreamModeBidirectional, Handle: handle, Serve: serve, Subprotocol: "tunnel.v1", AdmitOnUpgrade: true},
			header:  upgradeHeader("Sec-WebSocket-Protocol", "other.v1"),
			message: "requested websocket subprotocol is not the one this endpoint speaks",
		},
		{
			name:    "subprotocol the route does not speak",
			stream:  StreamHandler{Mode: StreamModeBidirectional, Handle: handle, Serve: serve, Subprotocol: "tunnel.v1"},
			header:  upgradeHeader("Sec-WebSocket-Protocol", "other.v1"),
			message: "requested websocket subprotocol is not supported by this endpoint",
		},
		{
			name:    "missing key",
			stream:  StreamHandler{Mode: StreamModeBidirectional, Handle: handle},
			header:  upgradeHeader("Sec-WebSocket-Key", ""),
			message: "missing Sec-WebSocket-Key",
		},
		{
			name:    "wrong version",
			stream:  StreamHandler{Mode: StreamModeBidirectional, Handle: handle},
			header:  upgradeHeader("Sec-WebSocket-Version", "8"),
			message: "unsupported WebSocket version",
		},
		{
			name:    "invalid key",
			stream:  StreamHandler{Mode: StreamModeBidirectional, Handle: handle},
			header:  upgradeHeader("Sec-WebSocket-Key", "not-a-key"),
			message: "invalid Sec-WebSocket-Key",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := streamRefusal(t, tc.stream, tc.header)
			envelopeOf(t, resp, http.StatusBadRequest, "http.bad_request", tc.message)
		})
	}
}

func TestRefusalEnvelope_WebSocketWithoutUpgradeAnswersBadRequest(t *testing.T) {
	spectest.Proves(t, "go/http-services", "refusal-envelope", "a-refused-stream-or-upgrade-answers-400-bad-request")
	p := NewServerPlugin(ServerConfig{Port: 0})
	p.GET("/ws", func(ctx *Context) *Response {
		return p.serveWebSocket(ctx, StreamHandler{Handle: func(*StreamContext) error { return nil }}, "")
	})
	ts := p.TestServer()
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/ws")
	if err != nil {
		t.Fatal(err)
	}
	envelopeOf(t, resp, http.StatusBadRequest, "http.bad_request", "websocket upgrade required")
}
