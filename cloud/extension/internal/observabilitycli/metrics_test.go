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

// TestBuildMetricsQueryTranslatesFlags pins the flag→query-param contract with
// the metrics endpoint: env→environment, window passed through, the two
// --from shapes, --to, limit and cursor.
func TestBuildMetricsQueryTranslatesFlags(t *testing.T) {
	now := fixedNow()
	cases := []struct {
		name   string
		params map[string]any
		env    string
		want   url.Values
	}{
		{
			name:   "window and pagination",
			params: map[string]any{"window": "1h", "limit": "50", "cursor": "c0"},
			env:    "prod",
			want: url.Values{
				"environment": {"prod"},
				"window":      {"1h"},
				"limit":       {"50"},
				"cursor":      {"c0"},
			},
		},
		{
			name:   "from duration becomes from=now-d",
			params: map[string]any{"from": "15m"},
			env:    "staging",
			want: url.Values{
				"environment": {"staging"},
				"from":        {now.Add(-15 * time.Minute).UTC().Format(time.RFC3339)},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildMetricsQuery(tc.params, tc.env, now)
			if err != nil {
				t.Fatalf("buildMetricsQuery: %v", err)
			}
			if got.Encode() != tc.want.Encode() {
				t.Fatalf("query = %q, want %q", got.Encode(), tc.want.Encode())
			}
		})
	}
}

func TestBuildMetricsQueryRejectsBadInputs(t *testing.T) {
	now := fixedNow()
	cases := []struct {
		name   string
		params map[string]any
	}{
		{"non-positive window", map[string]any{"window": "0s"}},
		{"malformed window", map[string]any{"window": "soon"}},
		{"negative limit", map[string]any{"limit": "-3"}},
		{"malformed to", map[string]any{"to": "midnight"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildMetricsQuery(tc.params, "prod", now)
			if err == nil || clicore.ExitCode(err) != clicore.ExitUsage {
				t.Fatalf("err=%v code=%d, want ExitUsage", err, clicore.ExitCode(err))
			}
		})
	}
}

// TestStreamMetricsJSONLEmitsOneObjectPerSeries is the agent-surface invariant:
// under --output=jsonl stdout is exactly one JSON object per series and the
// request hits the metrics endpoint with the window intact.
func TestStreamMetricsJSONLEmitsOneObjectPerSeries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/v1/workspaces/ws-acme/metrics" {
			t.Errorf("path = %q, want /v1/workspaces/ws-acme/metrics", got)
		}
		if r.URL.Query().Get("environment") != "prod" || r.URL.Query().Get("window") != "1h" {
			t.Errorf("query = %q, want environment=prod&window=1h", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"series":[
			{"name":"http.requests","labels":{"env":"prod"},"points":[{"timestamp":"2026-07-03T12:00:00Z","value":1}]},
			{"name":"http.latency","points":[{"timestamp":"2026-07-03T12:00:00Z","value":2}]}
		]}`))
	}))
	defer srv.Close()

	cap := &captureIO{}
	ctx := newCtx(t, srv, nil, cap)
	ctx.AuthToken = clicore.NewBearer("token")
	base := url.Values{"environment": {"prod"}, "window": {"1h"}}
	params := map[string]any{"output": "jsonl"}

	if err := streamMetrics(context.Background(), ctx, params, base, ctx.IO); err != nil {
		t.Fatalf("streamMetrics: %v", err)
	}
	if len(cap.stdout) != 2 {
		t.Fatalf("stdout lines = %d (%q), want 2 series objects", len(cap.stdout), cap.stdout)
	}
	for i, line := range cap.stdout {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("stdout[%d] not a JSON object: %v (%q)", i, err, line)
		}
		if obj["name"] == nil {
			t.Fatalf("stdout[%d] missing name: %q", i, line)
		}
	}
	if len(cap.stderr) != 0 {
		t.Fatalf("stderr = %q, want no hint (single page drained)", cap.stderr)
	}
}

// TestStreamMetricsHumanFormat renders a plain page: name, point count, latest
// sample and sorted labels.
func TestStreamMetricsHumanFormat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"series":[{"name":"http.requests","labels":{"env":"prod","service":"api"},"points":[{"timestamp":"2026-07-03T12:00:00Z","value":1},{"timestamp":"2026-07-03T12:01:00Z","value":4}]}]}`))
	}))
	defer srv.Close()

	cap := &captureIO{}
	ctx := newCtx(t, srv, nil, cap)
	if err := streamMetrics(context.Background(), ctx, map[string]any{}, url.Values{}, ctx.IO); err != nil {
		t.Fatalf("streamMetrics: %v", err)
	}
	if len(cap.stdout) != 1 {
		t.Fatalf("stdout = %q, want one human line", cap.stdout)
	}
	line := cap.stdout[0]
	for _, want := range []string{"http.requests", "2 points", "latest=4@2026-07-03T12:01:00Z", "{env=prod service=api}"} {
		if !strings.Contains(line, want) {
			t.Fatalf("line %q missing %q", line, want)
		}
	}
}

func TestStreamMetricsHumanEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"series":[]}`))
	}))
	defer srv.Close()
	cap := &captureIO{}
	ctx := newCtx(t, srv, nil, cap)
	if err := streamMetrics(context.Background(), ctx, map[string]any{}, url.Values{}, ctx.IO); err != nil {
		t.Fatalf("streamMetrics: %v", err)
	}
	if len(cap.stdout) != 1 || !strings.Contains(cap.stdout[0], "No metrics found") {
		t.Fatalf("stdout = %q, want a No-metrics-found message", cap.stdout)
	}
}

// TestStreamMetricsDoesNotFollowAll pins the metrics-specific contract: the
// metrics surface declares no --all flag, so even when `all` is set it fetches
// exactly one page and surfaces the next cursor WITHOUT advertising --all.
func TestStreamMetricsDoesNotFollowAll(t *testing.T) {
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"series":[{"name":"http.requests","points":[]}],"nextCursor":"c1"}`))
	}))
	defer srv.Close()

	cap := &captureIO{}
	ctx := newCtx(t, srv, nil, cap)
	params := map[string]any{"output": "jsonl", "all": true}
	if err := streamMetrics(context.Background(), ctx, params, url.Values{}, ctx.IO); err != nil {
		t.Fatalf("streamMetrics: %v", err)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want exactly 1 (metrics does not follow --all)", requests)
	}
	if len(cap.stderr) != 1 || !strings.Contains(cap.stderr[0], "c1") {
		t.Fatalf("stderr = %q, want a single cursor hint containing c1", cap.stderr)
	}
	if strings.Contains(cap.stderr[0], "--all") {
		t.Fatalf("stderr = %q, must not advertise --all for metrics", cap.stderr)
	}
}
