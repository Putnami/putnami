package observabilitycli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

func mustParseTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse time %q: %v", value, err)
	}
	return parsed
}

// TestRenderFrameRendersJSONLAndHuman pins the frame render invariant: each
// decoded entry renders to one compact JSON object under JSONL and to a
// severity-colored human line otherwise. Decoding the SSE wire itself (frame
// parsing, comments/heartbeats, non-data fields, a malformed payload) is now
// the generated observability-api client's job (go.putnami.dev/client
// server_stream.go), covered by the framework's own tests.
func TestRenderFrameRendersJSONLAndHuman(t *testing.T) {
	entry := logEntry{Timestamp: mustParseTime(t, "2026-07-04T12:00:00Z"), Severity: 17, SeverityText: "ERROR", Body: "boom"}

	jsonl := renderFrame(entry, true, false)
	var obj map[string]any
	if err := json.Unmarshal([]byte(jsonl), &obj); err != nil {
		t.Fatalf("jsonl not a JSON object: %v (%q)", err, jsonl)
	}
	if obj["body"] != "boom" {
		t.Fatalf("jsonl missing body: %q", jsonl)
	}

	human := renderFrame(entry, false, false)
	for _, want := range []string{"2026-07-04T12:00:00Z", "ERROR", "boom"} {
		if !strings.Contains(human, want) {
			t.Fatalf("human %q missing %q", human, want)
		}
	}
	if strings.Contains(human, "\x1b[") {
		t.Fatalf("human line carries ANSI despite color=false: %q", human)
	}
}

// sseServer starts a tail server that streams the given frames, flushes, then
// blocks until the client disconnects — mirroring the real /logs/tail loop, which
// holds the connection open until the client's context cancels. onRequest lets a
// test inspect each request (auth, path, headers).
func sseServer(t *testing.T, onRequest func(*http.Request), frames ...string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if onRequest != nil {
			onRequest(r)
		}
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("response writer is not a flusher")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl.Flush()
		for _, f := range frames {
			_, _ = io.WriteString(w, f)
		}
		fl.Flush()
		<-r.Context().Done() // hold open until the client disconnects
	}))
}

// TestFollowLogsStreamsFramesAndExitsOnInterrupt is the end-to-end tail
// invariant: --follow opens /logs/tail with an event-stream Accept + bearer,
// renders each frame live (JSONL here), and exits 0 when the ambient context is
// canceled (Ctrl-C).
func TestFollowLogsStreamsFramesAndExitsOnInterrupt(t *testing.T) {
	var gotPath, gotAccept, gotAuth string
	srv := sseServer(t, func(r *http.Request) {
		gotPath, gotAccept, gotAuth = r.URL.Path, r.Header.Get("Accept"), r.Header.Get("Authorization")
	},
		"data: {\"body\":\"one\",\"severityText\":\"INFO\"}\n\n",
		"data: {\"body\":\"two\",\"severityText\":\"INFO\"}\n\n",
	)
	defer srv.Close()

	ctx := newCtx(t, srv, nil, &captureIO{})
	ctx.AuthToken = clicore.NewBearer("token")
	lines := make(chan string, 8)
	ctx.IO.Stdout = func(s string) { lines <- s }

	reqCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- followLogs(reqCtx, ctx, map[string]any{"output": "jsonl"}, url.Values{"environment": {"prod"}}, ctx.IO)
	}()

	var got []string
	for i := 0; i < 2; i++ {
		select {
		case line := <-lines:
			got = append(got, line)
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for frame %d; got %q", i, got)
		}
	}
	cancel() // simulate Ctrl-C
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("followLogs after interrupt = %v, want clean nil exit", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("followLogs did not exit after context cancel")
	}

	if gotPath != "/v1/workspaces/ws-acme/logs/tail" {
		t.Errorf("tail path = %q, want /v1/workspaces/ws-acme/logs/tail", gotPath)
	}
	if gotAccept != "text/event-stream" {
		t.Errorf("Accept = %q, want text/event-stream", gotAccept)
	}
	if gotAuth != "Bearer token" {
		t.Errorf("Authorization = %q, want Bearer token", gotAuth)
	}
	for i, line := range got {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("stdout[%d] not JSON: %v (%q)", i, err, line)
		}
	}
}

// TestFollowLogsRefreshesOn401 pins the streaming auth invariant: a 401 on
// connect triggers exactly one re-mint and the tail reconnects with the fresh
// bearer (inherited from doAuthed), without surfacing an error.
func TestFollowLogsRefreshesOn401(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer fresh" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"Authentication required"}`))
			return
		}
		fl := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl.Flush()
		_, _ = io.WriteString(w, "data: {\"body\":\"live\",\"severityText\":\"INFO\"}\n\n")
		fl.Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx := newCtx(t, srv, func() (clicore.Bearer, error) { return clicore.NewBearer("fresh"), nil }, &captureIO{})
	lines := make(chan string, 4)
	ctx.IO.Stdout = func(s string) { lines <- s }

	reqCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- followLogs(reqCtx, ctx, map[string]any{"output": "jsonl"}, url.Values{}, ctx.IO) }()

	select {
	case <-lines:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for a frame after re-mint")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("followLogs = %v, want nil after re-mint + interrupt", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("followLogs did not exit after cancel")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] != "Bearer stale" || seen[1] != "Bearer fresh" {
		t.Fatalf("authorization sequence = %v, want [Bearer stale, Bearer fresh]", seen)
	}
	if ctx.AuthToken.Authorization() != "Bearer fresh" {
		t.Fatal("ctx.AuthToken not updated after re-mint")
	}
}

// TestFollowLogsSurfacesTerminalError pins fail-closed on a server rejection: a
// 4xx (e.g. unknown workspace) is terminal — the tail surfaces it as ExitAPI and
// does NOT reconnect. The generated client withholds the provider's free-text
// message (clicore.ServiceCallError), so the surfaced text is the
// status line, not the response body.
func TestFollowLogsSurfacesTerminalError(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"workspace not found"}`))
	}))
	defer srv.Close()

	ctx := newCtx(t, srv, nil, &captureIO{})
	ctx.AuthToken = clicore.NewBearer("token")

	err := followLogs(context.Background(), ctx, map[string]any{"output": "jsonl"}, url.Values{}, ctx.IO)
	if err == nil || clicore.ExitCode(err) != clicore.ExitAPI || !strings.Contains(err.Error(), "404") {
		t.Fatalf("err = %v (code %d), want ExitAPI carrying the 404 status line", err, clicore.ExitCode(err))
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 1 {
		t.Fatalf("server hits = %d, want 1 (a 4xx must not trigger a reconnect)", hits)
	}
}

// TestFollowLogsReconnectIsBounded pins that a flapping tail that streams nothing
// cannot spin forever: the client reconnects a bounded number of times and then
// exits cleanly.
func TestFollowLogsReconnectIsBounded(t *testing.T) {
	oldMax, oldBackoff := followMaxReconnects, followBackoff
	followMaxReconnects, followBackoff = 2, time.Millisecond
	defer func() { followMaxReconnects, followBackoff = oldMax, oldBackoff }()

	var mu sync.Mutex
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		fl := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl.Flush() // 200 then immediate close: a connection that streams no frame
	}))
	defer srv.Close()

	ctx := newCtx(t, srv, nil, &captureIO{})
	ctx.AuthToken = clicore.NewBearer("token")

	done := make(chan error, 1)
	go func() { done <- followLogs(context.Background(), ctx, map[string]any{}, url.Values{}, ctx.IO) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("followLogs = %v, want nil after exhausting the reconnect budget", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("followLogs did not stop reconnecting — the budget is unbounded")
	}

	mu.Lock()
	defer mu.Unlock()
	if hits != followMaxReconnects+1 {
		t.Fatalf("server hits = %d, want %d (initial + %d bounded reconnects)", hits, followMaxReconnects+1, followMaxReconnects)
	}
}
