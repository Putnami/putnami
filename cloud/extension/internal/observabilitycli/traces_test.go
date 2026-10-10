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

// TestBuildTracesQueryTranslatesFlags pins the flag→query-param contract with
// the traces endpoint: env→environment, slow passed through, the two
// --from shapes (RFC3339 → from, duration → from=now-d), --to, limit and cursor.
func TestBuildTracesQueryTranslatesFlags(t *testing.T) {
	now := fixedNow()
	cases := []struct {
		name   string
		params map[string]any
		env    string
		want   url.Values
	}{
		{
			name:   "slow filter and pagination",
			params: map[string]any{"slow": "500ms", "limit": "50", "cursor": "c0"},
			env:    "prod",
			want: url.Values{
				"environment": {"prod"},
				"slow":        {"500ms"},
				"limit":       {"50"},
				"cursor":      {"c0"},
			},
		},
		{
			name:   "from duration becomes from=now-d",
			params: map[string]any{"from": "1h"},
			env:    "staging",
			want: url.Values{
				"environment": {"staging"},
				"from":        {now.Add(-time.Hour).UTC().Format(time.RFC3339)},
			},
		},
		{
			name:   "from/to rfc3339 pass through",
			params: map[string]any{"from": "2026-07-03T10:00:00Z", "to": "2026-07-03T11:00:00Z"},
			env:    "prod",
			want: url.Values{
				"environment": {"prod"},
				"from":        {"2026-07-03T10:00:00Z"},
				"to":          {"2026-07-03T11:00:00Z"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildTracesQuery(tc.params, tc.env, now)
			if err != nil {
				t.Fatalf("buildTracesQuery: %v", err)
			}
			if got.Encode() != tc.want.Encode() {
				t.Fatalf("query = %q, want %q", got.Encode(), tc.want.Encode())
			}
		})
	}
}

func TestBuildTracesQueryRejectsBadInputs(t *testing.T) {
	now := fixedNow()
	cases := []struct {
		name   string
		params map[string]any
	}{
		{"non-positive slow", map[string]any{"slow": "0s"}},
		{"malformed slow", map[string]any{"slow": "soon"}},
		{"negative limit", map[string]any{"limit": "-3"}},
		{"malformed from", map[string]any{"from": "yesterday"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildTracesQuery(tc.params, "prod", now)
			if err == nil || clicore.ExitCode(err) != clicore.ExitUsage {
				t.Fatalf("err=%v code=%d, want ExitUsage", err, clicore.ExitCode(err))
			}
		})
	}
}

// TestStreamTracesJSONLEmitsOneObjectPerTrace is the agent-surface invariant:
// under --output=jsonl stdout is exactly one JSON object per trace — the
// pagination hint goes to stderr — and the request hits the traces endpoint with
// the slow filter intact.
func TestStreamTracesJSONLEmitsOneObjectPerTrace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/v1/workspaces/ws-acme/traces" {
			t.Errorf("path = %q, want /v1/workspaces/ws-acme/traces", got)
		}
		if r.URL.Query().Get("environment") != "prod" || r.URL.Query().Get("slow") != "500ms" {
			t.Errorf("query = %q, want environment=prod&slow=500ms", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"traces":[
			{"traceId":"t1","name":"GET /a","start":"2026-07-03T12:00:00Z","duration":523000000,"spanCount":3},
			{"traceId":"t2","name":"GET /b","start":"2026-07-03T12:00:01Z","duration":1200000000,"spanCount":7}
		],"nextCursor":"c1"}`))
	}))
	defer srv.Close()

	cap := &captureIO{}
	ctx := newCtx(t, srv, nil, cap)
	ctx.AuthToken = clicore.NewBearer("token")
	base := url.Values{"environment": {"prod"}, "slow": {"500ms"}}
	params := map[string]any{"output": "jsonl"}

	if err := streamTraces(context.Background(), ctx, params, base, ctx.IO); err != nil {
		t.Fatalf("streamTraces: %v", err)
	}
	if len(cap.stdout) != 2 {
		t.Fatalf("stdout lines = %d (%q), want 2 trace objects", len(cap.stdout), cap.stdout)
	}
	for i, line := range cap.stdout {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("stdout[%d] not a JSON object: %v (%q)", i, err, line)
		}
		if obj["traceId"] == nil {
			t.Fatalf("stdout[%d] missing traceId: %q", i, line)
		}
	}
	// The next cursor must not pollute the JSONL stdout stream; traces honor
	// --all, so its hint advertises it.
	if len(cap.stderr) != 1 || !strings.Contains(cap.stderr[0], "c1") || !strings.Contains(cap.stderr[0], "--all") {
		t.Fatalf("stderr = %q, want a single cursor hint containing c1 and --all", cap.stderr)
	}
}

// TestStreamTracesHumanFormat renders a plain page and the empty-result message.
func TestStreamTracesHumanFormat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"traces":[{"traceId":"t1","name":"GET /a","start":"2026-07-03T12:00:00Z","duration":523000000,"spanCount":3}]}`))
	}))
	defer srv.Close()

	cap := &captureIO{}
	ctx := newCtx(t, srv, nil, cap)
	if err := streamTraces(context.Background(), ctx, map[string]any{}, url.Values{}, ctx.IO); err != nil {
		t.Fatalf("streamTraces: %v", err)
	}
	if len(cap.stdout) != 1 {
		t.Fatalf("stdout = %q, want one human line", cap.stdout)
	}
	for _, want := range []string{"2026-07-03T12:00:00Z", "523ms", "3 spans", "GET /a", "t1"} {
		if !strings.Contains(cap.stdout[0], want) {
			t.Fatalf("line %q missing %q", cap.stdout[0], want)
		}
	}
}

func TestStreamTracesHumanEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"traces":[]}`))
	}))
	defer srv.Close()
	cap := &captureIO{}
	ctx := newCtx(t, srv, nil, cap)
	if err := streamTraces(context.Background(), ctx, map[string]any{}, url.Values{}, ctx.IO); err != nil {
		t.Fatalf("streamTraces: %v", err)
	}
	if len(cap.stdout) != 1 || !strings.Contains(cap.stdout[0], "No traces found") {
		t.Fatalf("stdout = %q, want a No-traces-found message", cap.stdout)
	}
}

// TestStreamTracesFollowsCursorWithAll pins pagination: --all follows nextCursor
// and the second request carries the first page's cursor.
func TestStreamTracesFollowsCursorWithAll(t *testing.T) {
	var cursors []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cursors = append(cursors, r.URL.Query().Get("cursor"))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("cursor") {
		case "":
			_, _ = w.Write([]byte(`{"traces":[{"traceId":"t1"}],"nextCursor":"c1"}`))
		case "c1":
			_, _ = w.Write([]byte(`{"traces":[{"traceId":"t2"}]}`))
		default:
			t.Errorf("unexpected cursor %q", r.URL.Query().Get("cursor"))
			_, _ = w.Write([]byte(`{"traces":[]}`))
		}
	}))
	defer srv.Close()

	cap := &captureIO{}
	ctx := newCtx(t, srv, nil, cap)
	params := map[string]any{"output": "jsonl", "all": true}
	if err := streamTraces(context.Background(), ctx, params, url.Values{}, ctx.IO); err != nil {
		t.Fatalf("streamTraces: %v", err)
	}
	if len(cap.stdout) != 2 {
		t.Fatalf("stdout = %q, want 2 traces across 2 pages", cap.stdout)
	}
	if len(cursors) != 2 || cursors[0] != "" || cursors[1] != "c1" {
		t.Fatalf("request cursors = %v, want [\"\", \"c1\"]", cursors)
	}
	if len(cap.stderr) != 0 {
		t.Fatalf("stderr = %q, want no cursor hint after --all drained", cap.stderr)
	}
}
