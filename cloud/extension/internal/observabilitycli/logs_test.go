package observabilitycli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// captureIO records stdout/stderr lines and injects the test server's client.
type captureIO struct {
	stdout []string
	stderr []string
}

func newCtx(t *testing.T, srv *httptest.Server, refresh func() (clicore.Bearer, error), cap *captureIO) *queryCtx {
	t.Helper()
	return &queryCtx{
		WorkspaceContext: &clicore.WorkspaceContext{
			WorkspaceID:  "ws-acme",
			ControlPlane: srv.URL,
			AuthToken:    clicore.NewBearer("stale"),
			IO: clicore.IO{
				Client: srv.Client(),
				Stdout: func(line string) { cap.stdout = append(cap.stdout, line) },
				Stderr: func(line string) { cap.stderr = append(cap.stderr, line) },
				Now:    time.Now,
			},
			RefreshAuth: refresh,
		},
		app:         "apps/api",
		environment: "prod",
	}
}

func fixedNow() time.Time {
	return time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
}

// TestBuildLogsQueryTranslatesFlags pins the flag→query-param contract with the
// logs endpoint: env→environment, level lowercased, limit, cursor, and the
// three --since shapes (RFC3339 → from, duration → from=now-d, non-time →
// passthrough `since`).
func TestBuildLogsQueryTranslatesFlags(t *testing.T) {
	now := fixedNow()
	cases := []struct {
		name   string
		params map[string]any
		env    string
		want   url.Values
	}{
		{
			name:   "correlation and pagination",
			params: map[string]any{"level": "ERROR", "limit": "50", "cursor": "c0"},
			env:    "prod",
			want: url.Values{
				"environment": {"prod"},
				"level":       {"error"},
				"limit":       {"50"},
				"cursor":      {"c0"},
			},
		},
		{
			name:   "since duration becomes from=now-d",
			params: map[string]any{"since": "1h"},
			env:    "staging",
			want: url.Values{
				"environment": {"staging"},
				"from":        {now.Add(-time.Hour).UTC().Format(time.RFC3339)},
			},
		},
		{
			name:   "since rfc3339 becomes from",
			params: map[string]any{"since": "2026-07-03T10:00:00Z"},
			env:    "prod",
			want: url.Values{
				"environment": {"prod"},
				"from":        {"2026-07-03T10:00:00Z"},
			},
		},
		{
			name:   "since deploy:last passes through",
			params: map[string]any{"since": "deploy:last"},
			env:    "prod",
			want: url.Values{
				"environment": {"prod"},
				"since":       {"deploy:last"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildLogsQuery(tc.params, tc.env, now)
			if err != nil {
				t.Fatalf("buildLogsQuery: %v", err)
			}
			if got.Encode() != tc.want.Encode() {
				t.Fatalf("query = %q, want %q", got.Encode(), tc.want.Encode())
			}
		})
	}
}

func TestBuildLogsQueryRejectsNegativeLimit(t *testing.T) {
	_, err := buildLogsQuery(map[string]any{"limit": "-3"}, "prod", fixedNow())
	if err == nil || clicore.ExitCode(err) != clicore.ExitUsage {
		t.Fatalf("negative limit err=%v code=%d, want ExitUsage", err, clicore.ExitCode(err))
	}
}

// TestStreamLogsJSONLEmitsOneObjectPerEntry is the agent-surface invariant:
// under --output=jsonl the stdout stream is exactly one JSON object per log
// entry — no summary, no hint (the pagination note goes to stderr).
func TestStreamLogsJSONLEmitsOneObjectPerEntry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/v1/workspaces/ws-acme/logs" {
			t.Errorf("path = %q, want /v1/workspaces/ws-acme/logs", got)
		}
		if r.URL.Query().Get("environment") != "prod" || r.URL.Query().Get("level") != "error" {
			t.Errorf("query = %q, want environment=prod&level=error", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"entries":[
			{"timestamp":"2026-07-03T12:00:00Z","severity":17,"severityText":"ERROR","body":"boom"},
			{"timestamp":"2026-07-03T12:00:01Z","severity":17,"severityText":"ERROR","body":"kaboom"}
		],"nextCursor":"c1"}`))
	}))
	defer srv.Close()

	cap := &captureIO{}
	ctx := newCtx(t, srv, nil, cap)
	base := url.Values{"environment": {"prod"}, "level": {"error"}}
	params := map[string]any{"output": "jsonl"}
	ctx.AuthToken = clicore.NewBearer("token") // no refresh needed here

	if err := streamLogs(context.Background(), ctx, params, base, ctx.IO); err != nil {
		t.Fatalf("streamLogs: %v", err)
	}
	if len(cap.stdout) != 2 {
		t.Fatalf("stdout lines = %d (%q), want 2 entry objects", len(cap.stdout), cap.stdout)
	}
	for i, line := range cap.stdout {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("stdout[%d] not a JSON object: %v (%q)", i, err, line)
		}
		if obj["body"] == nil {
			t.Fatalf("stdout[%d] missing body: %q", i, line)
		}
	}
	// The next cursor must not pollute the JSONL stdout stream.
	if len(cap.stderr) != 1 || !strings.Contains(cap.stderr[0], "c1") {
		t.Fatalf("stderr = %q, want a single cursor hint containing c1", cap.stderr)
	}
}

// TestStreamLogsHumanFormat renders a plain page and the empty-result message.
func TestStreamLogsHumanFormat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"entries":[{"timestamp":"2026-07-03T12:00:00Z","severityText":"ERROR","body":"boom"}]}`))
	}))
	defer srv.Close()

	cap := &captureIO{}
	ctx := newCtx(t, srv, nil, cap)
	ctx.IO.Env = map[string]string{"NO_COLOR": "1"} // deterministic: no ANSI
	if err := streamLogs(context.Background(), ctx, map[string]any{}, url.Values{}, ctx.IO); err != nil {
		t.Fatalf("streamLogs: %v", err)
	}
	if len(cap.stdout) != 1 {
		t.Fatalf("stdout = %q, want one human line", cap.stdout)
	}
	line := cap.stdout[0]
	if strings.Contains(line, "\x1b[") {
		t.Fatalf("line carries ANSI despite NO_COLOR: %q", line)
	}
	for _, want := range []string{"2026-07-03T12:00:00Z", "ERROR", "boom"} {
		if !strings.Contains(line, want) {
			t.Fatalf("line %q missing %q", line, want)
		}
	}
}

func TestStreamLogsHumanEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"entries":[]}`))
	}))
	defer srv.Close()
	cap := &captureIO{}
	ctx := newCtx(t, srv, nil, cap)
	ctx.IO.Env = map[string]string{"NO_COLOR": "1"}
	if err := streamLogs(context.Background(), ctx, map[string]any{}, url.Values{}, ctx.IO); err != nil {
		t.Fatalf("streamLogs: %v", err)
	}
	if len(cap.stdout) != 1 || !strings.Contains(cap.stdout[0], "No logs found") {
		t.Fatalf("stdout = %q, want a No-logs-found message", cap.stdout)
	}
}

// TestStreamLogsFollowsCursorWithAll pins pagination: --all follows nextCursor
// and the second request carries the first page's cursor.
func TestStreamLogsFollowsCursorWithAll(t *testing.T) {
	var cursors []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cursors = append(cursors, r.URL.Query().Get("cursor"))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("cursor") {
		case "":
			_, _ = w.Write([]byte(`{"entries":[{"body":"a"}],"nextCursor":"c1"}`))
		case "c1":
			_, _ = w.Write([]byte(`{"entries":[{"body":"b"}]}`))
		default:
			t.Errorf("unexpected cursor %q", r.URL.Query().Get("cursor"))
			_, _ = w.Write([]byte(`{"entries":[]}`))
		}
	}))
	defer srv.Close()

	cap := &captureIO{}
	ctx := newCtx(t, srv, nil, cap)
	params := map[string]any{"output": "jsonl", "all": true}
	if err := streamLogs(context.Background(), ctx, params, url.Values{}, ctx.IO); err != nil {
		t.Fatalf("streamLogs: %v", err)
	}
	if len(cap.stdout) != 2 {
		t.Fatalf("stdout = %q, want 2 entries across 2 pages", cap.stdout)
	}
	if len(cursors) != 2 || cursors[0] != "" || cursors[1] != "c1" {
		t.Fatalf("request cursors = %v, want [\"\", \"c1\"]", cursors)
	}
	if len(cap.stderr) != 0 {
		t.Fatalf("stderr = %q, want no cursor hint after --all drained", cap.stderr)
	}
}

// TestLogsRequestRefreshesOn401 pins the auth invariant: a 401 triggers exactly
// one re-mint, the replay carries the fresh bearer, and ctx keeps it.
func TestLogsRequestRefreshesOn401(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") != "Bearer fresh" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"Authentication required"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"entries":[]}`))
	}))
	defer srv.Close()

	cap := &captureIO{}
	ctx := newCtx(t, srv, func() (clicore.Bearer, error) { return clicore.NewBearer("fresh"), nil }, cap)
	_, status, err := logsRequest(context.Background(), ctx, url.Values{})
	if err != nil || status != http.StatusOK {
		t.Fatalf("logsRequest after refresh: status=%d err=%v", status, err)
	}
	if len(seen) != 2 || seen[0] != "Bearer stale" || seen[1] != "Bearer fresh" {
		t.Fatalf("authorization sequence = %v, want [Bearer stale, Bearer fresh]", seen)
	}
	if ctx.AuthToken.Authorization() != "Bearer fresh" {
		t.Fatal("ctx.AuthToken not updated — the next page would 401 again")
	}
}

// TestLogsRequestSurfacesServerError maps a server 4xx (e.g. unknown workspace)
// to a clear ExitAPI error. Through observability-api's generated client, the
// message is the status line, not the provider's free-text body: the generated
// client withholds it, the same disclosed trade-off the CD header and
// log-follow reads accept.
func TestLogsRequestSurfacesServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"workspace not found"}`))
	}))
	defer srv.Close()

	cap := &captureIO{}
	ctx := newCtx(t, srv, nil, cap)
	ctx.AuthToken = clicore.NewBearer("token")
	_, status, err := logsRequest(context.Background(), ctx, url.Values{})
	if status != http.StatusNotFound || err == nil {
		t.Fatalf("status=%d err=%v, want a surfaced 404 error", status, err)
	}
	if clicore.ExitCode(err) != clicore.ExitAPI || !strings.Contains(err.Error(), "404 Not Found") {
		t.Fatalf("err=%v code=%d, want ExitAPI with the status line", err, clicore.ExitCode(err))
	}
}

// TestLogsRequestSendsUserAgent guards the unified CLI User-Agent stamping.
func TestLogsRequestSendsUserAgent(t *testing.T) {
	var ua string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"entries":[]}`))
	}))
	defer srv.Close()
	cap := &captureIO{}
	ctx := newCtx(t, srv, nil, cap)
	ctx.AuthToken = clicore.NewBearer("token")
	if _, _, err := logsRequest(context.Background(), ctx, url.Values{}); err != nil {
		t.Fatalf("logsRequest: %v", err)
	}
	if ua == "" || strings.HasPrefix(ua, "Go-http-client") {
		t.Fatalf("User-Agent = %q, want the unified CLI UA", ua)
	}
}

// TestSetServiceFilter pins the flag translation: a resolved <app> is sent
// as the `service` correlation filter (the SAME canonical name the deploy path
// stamps as the `service` resource attribute), and an empty app leaves the query
// workspace-wide rather than sending a match-nothing empty filter.
func TestSetServiceFilter(t *testing.T) {
	q := url.Values{"environment": {"prod"}}
	setServiceFilter(q, "apps/api")
	if got := q.Get("service"); got != "apps/api" {
		t.Fatalf("service = %q, want apps/api", got)
	}

	empty := url.Values{"environment": {"prod"}}
	setServiceFilter(empty, "  ")
	if _, ok := empty["service"]; ok {
		t.Fatalf("empty app must not send a service filter: %v", empty)
	}
}

// TestLogsSendsServiceFilterForApp pins the end-to-end outgoing contract: the
// logs query carries service=<resolved app> so the server scopes to that app.
func TestLogsSendsServiceFilterForApp(t *testing.T) {
	var gotService string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotService = r.URL.Query().Get("service")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"entries":[]}`))
	}))
	defer srv.Close()

	cap := &captureIO{}
	ctx := newCtx(t, srv, nil, cap) // app: apps/api
	ctx.AuthToken = clicore.NewBearer("token")

	base := url.Values{"environment": {"prod"}}
	setServiceFilter(base, ctx.app)
	if err := streamLogs(context.Background(), ctx, map[string]any{"output": "jsonl"}, base, ctx.IO); err != nil {
		t.Fatalf("streamLogs: %v", err)
	}
	if gotService != "apps/api" {
		t.Fatalf("outgoing service = %q, want apps/api", gotService)
	}
}

func TestStructuredOutput(t *testing.T) {
	cases := []struct {
		params map[string]any
		want   bool
	}{
		{map[string]any{"output": "jsonl"}, true},
		{map[string]any{"output": "json"}, true},
		{map[string]any{"json": true}, true},
		{map[string]any{"output": "JSONL"}, false},
		{map[string]any{"output": "jsonl", "json": true}, false},
		{map[string]any{"output": "text", "json": true}, false},
		{map[string]any{"output": "text"}, false},
		{map[string]any{}, false},
	}
	for _, tc := range cases {
		if got := structuredOutput(tc.params); got != tc.want {
			t.Errorf("structuredOutput(%v) = %v, want %v", tc.params, got, tc.want)
		}
	}
}
