package http

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

// serveLikeStart serves p through the http.Server that Start builds, so each
// response carries the write deadline the server arms from WriteTimeout.
func serveLikeStart(t *testing.T, p *ServerPlugin) string {
	t.Helper()
	ts := httptest.NewUnstartedServer(nil)
	_, ts.Config = p.buildServer(p.Handler())
	ts.Start()
	t.Cleanup(ts.Close)
	return ts.URL
}

// A route that runs one bounded pass longer than the server's write timeout
// declares its own bound. Its success is written past the server's write
// deadline, and the request scope does not turn it into a 500. The same handler
// on a route without its own bound is cut by the server's.
func TestRouteWithItsOwnWriteTimeoutAnswersPastTheServerBound(t *testing.T) {
	spectest.Proves(t, "go/http-services", "route-write-timeout", "a-route-with-its-own-write-timeout-answers-past-the-server-bound")
	p := NewServerPlugin(ServerConfig{WriteTimeout: 100 * time.Millisecond})
	p.setContainerContext(startedContainer(t))
	pass := func(*Context) *Response {
		time.Sleep(300 * time.Millisecond) // one pass, past the server's bound
		return JSON(map[string]string{"pass": "done"})
	}
	p.Route(http.MethodPost, "/jobs/sweep", pass, WithWriteTimeout(5*time.Second))
	p.Route(http.MethodPost, "/jobs/unbounded", pass)
	url := serveLikeStart(t, p)

	resp, err := http.Post(url+"/jobs/sweep", "application/json", nil)
	if err != nil {
		t.Fatalf("the route's own bound did not hold the answer: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != `{"pass":"done"}` {
		t.Fatalf("status = %d body = %s, want 200 with the pass result", resp.StatusCode, body)
	}

	resp, err = http.Post(url+"/jobs/unbounded", "application/json", nil)
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Fatal("a route without its own bound answered 200 past the server's write timeout")
		}
	}
}

// The route's bound replaces the server's for that route alone: its request
// context ends at nine tenths of it, and its response write deadline moves to
// it. Sibling routes keep the server's deadline, a mounted subtree takes its
// mount's bound, and a long-lived request still runs without a deadline.
func TestRouteWriteTimeoutMovesItsRequestDeadline(t *testing.T) {
	spectest.Proves(t, "go/http-services", "route-write-timeout", "a-route-write-timeout-moves-its-request-deadline")
	const serverTimeout = time.Second
	p := NewServerPlugin(ServerConfig{WriteTimeout: serverTimeout})
	var remaining time.Duration
	var hasDeadline bool
	probe := func(ctx *Context) *Response {
		var deadline time.Time
		deadline, hasDeadline = ctx.Context().Deadline()
		remaining = time.Until(deadline)
		return NoContent()
	}
	p.Route(http.MethodPost, "/jobs/sweep", probe, WithWriteTimeout(10*time.Second))
	p.Route(http.MethodPost, "/orders", probe)
	p.RouteMount(http.MethodPost, "/batch/", probe, WithWriteTimeout(20*time.Second))
	handler := p.Handler()

	for _, tc := range []struct {
		path, accept    string
		bound           time.Duration
		wantDeadline    bool
		wantWriteMoveTo time.Duration
	}{
		{path: "/jobs/sweep", bound: 9 * time.Second, wantDeadline: true, wantWriteMoveTo: 10 * time.Second},
		{path: "/orders", bound: serverTimeout - serverTimeout/10, wantDeadline: true},
		{path: "/batch/imports/42", bound: 18 * time.Second, wantDeadline: true, wantWriteMoveTo: 20 * time.Second},
		{path: "/jobs/sweep", accept: "text/event-stream"},
	} {
		t.Run(tc.path+" "+tc.accept, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tc.path, nil)
			if tc.accept != "" {
				req.Header.Set("Accept", tc.accept)
			}
			rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
			start := time.Now()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want 204", rec.Code)
			}
			if hasDeadline != tc.wantDeadline {
				t.Fatalf("request deadline set = %v, want %v", hasDeadline, tc.wantDeadline)
			}
			if tc.wantDeadline && (remaining <= tc.bound-time.Second || remaining > tc.bound) {
				t.Errorf("request deadline is %v away, want just under %v", remaining, tc.bound)
			}
			if tc.wantWriteMoveTo == 0 {
				if len(rec.deadlines) != 0 {
					t.Errorf("write deadline moved to %v, want the server's left alone", rec.deadlines)
				}
				return
			}
			if len(rec.deadlines) != 1 {
				t.Fatalf("write deadline moved %d times, want once", len(rec.deadlines))
			}
			if moved := rec.deadlines[0].Sub(start); moved <= tc.wantWriteMoveTo-time.Second || moved > tc.wantWriteMoveTo+time.Second {
				t.Errorf("write deadline moved %v ahead, want %v", moved, tc.wantWriteMoveTo)
			}
		})
	}
}

func TestWithWriteTimeoutRefusesANonPositiveBound(t *testing.T) {
	spectest.Proves(t, "go/http-services", "route-write-timeout", "a-non-positive-route-write-timeout-panics")
	for _, timeout := range []time.Duration{0, -time.Second} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("WithWriteTimeout(%v) did not panic", timeout)
				}
			}()
			WithWriteTimeout(timeout)
		}()
	}
}
