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
	// The root-span note and the next cursor must not pollute the JSONL stdout
	// stream; traces honor --all, so the cursor hint advertises it.
	if len(cap.stderr) != 2 || !strings.Contains(cap.stderr[0], "--trace") ||
		!strings.Contains(cap.stderr[1], "c1") || !strings.Contains(cap.stderr[1], "--all") {
		t.Fatalf("stderr = %q, want the root-span note then a cursor hint containing c1 and --all", cap.stderr)
	}
}

// TestStreamTracesListingCarriesNoSpanCount pins the listing's honesty: the
// listing route answers each trace's root span alone, with spanCount 1 for a
// trace of 34 spans (Cloud Trace's ROOTSPAN view). The CLI must not print that
// count as the trace's, on either surface, and must say where the whole trace is.
func TestStreamTracesListingCarriesNoSpanCount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"traces":[{"traceId":"0197b20ade976f39996f99d576977e6d","name":"GET /","start":"2026-11-22T11:30:00Z","duration":80000000,"spanCount":1,
			"spans":[{"traceId":"0197b20ade976f39996f99d576977e6d","spanId":"s1","name":"GET /","start":"2026-11-22T11:30:00Z","duration":80000000}]}]}`))
	}))
	defer srv.Close()

	t.Run("jsonl", func(t *testing.T) {
		cap := &captureIO{}
		ctx := newCtx(t, srv, nil, cap)
		if err := streamTraces(context.Background(), ctx, map[string]any{"output": "jsonl"}, url.Values{}, ctx.IO); err != nil {
			t.Fatalf("streamTraces: %v", err)
		}
		if len(cap.stdout) != 1 {
			t.Fatalf("stdout = %q, want one trace object", cap.stdout)
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(cap.stdout[0]), &obj); err != nil {
			t.Fatalf("stdout not a JSON object: %v", err)
		}
		if _, ok := obj["spanCount"]; ok {
			t.Fatalf("listing item carries spanCount, which reads as the whole trace: %q", cap.stdout[0])
		}
		if spans, _ := obj["spans"].([]any); len(spans) != 1 {
			t.Fatalf("listing item spans = %v, want the root span", obj["spans"])
		}
		if len(cap.stderr) != 1 || cap.stderr[0] != rootSpanNote {
			t.Fatalf("stderr = %q, want the root-span note", cap.stderr)
		}
	})
	t.Run("human", func(t *testing.T) {
		cap := &captureIO{}
		ctx := newCtx(t, srv, nil, cap)
		if err := streamTraces(context.Background(), ctx, map[string]any{}, url.Values{}, ctx.IO); err != nil {
			t.Fatalf("streamTraces: %v", err)
		}
		if len(cap.stdout) != 2 || strings.Contains(cap.stdout[0], "span") || cap.stdout[1] != rootSpanNote {
			t.Fatalf("stdout = %q, want one trace line without a span count, then the root-span note", cap.stdout)
		}
	})
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
	if len(cap.stdout) != 2 {
		t.Fatalf("stdout = %q, want one human line and the root-span note", cap.stdout)
	}
	if got, want := cap.stdout[0], "2026-07-03T12:00:00Z      523ms  GET /a  t1"; got != want {
		t.Fatalf("line = %q, want %q", got, want)
	}
	if cap.stdout[1] != rootSpanNote {
		t.Fatalf("stdout[1] = %q, want the root-span note", cap.stdout[1])
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
	if len(cap.stderr) != 1 || cap.stderr[0] != rootSpanNote {
		t.Fatalf("stderr = %q, want the root-span note and no cursor hint after --all drained", cap.stderr)
	}
}

// wholeTraceBody is the trace read's answer for one trace of three spans over
// two services: the app's server span, its read span, and work-api's span.
const wholeTraceBody = `{"traceId":"0197b20ade976f39996f99d576977e6d","name":"GET /","start":"2026-11-22T11:30:00Z","duration":80000000,"spanCount":3,"spans":[
	{"traceId":"0197b20ade976f39996f99d576977e6d","spanId":"w1","parentSpanId":"r1","name":"GET /v1/work","start":"2026-11-22T11:30:00.020Z","duration":30000000,"attributes":{"service":"work-api"}},
	{"traceId":"0197b20ade976f39996f99d576977e6d","spanId":"s1","name":"GET /","start":"2026-11-22T11:30:00Z","duration":80000000,"attributes":{"service":"surfaces/workloads/app"}},
	{"traceId":"0197b20ade976f39996f99d576977e6d","spanId":"r1","parentSpanId":"s1","name":"read work","start":"2026-11-22T11:30:00.010Z","duration":50000000}
]}`

// traceReadServer answers the trace read for 0197b20ade976f39996f99d576977e6d and 404 for any other id,
// recording each request path and query.
func traceReadServer(t *testing.T, paths, queries *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*paths = append(*paths, r.URL.Path)
		*queries = append(*queries, r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/v1/workspaces/ws-acme/traces/0197b20ade976f39996f99d576977e6d" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"not_found","message":"trace not found in the resolved workspace"}`))
			return
		}
		_, _ = w.Write([]byte(wholeTraceBody))
	}))
}

// TestTracesTraceFlagReadsWholeTrace pins `traces --trace <id>` through the
// real entry point: it reads GET /traces/{traceId} (no listing filter) and
// prints one JSON object with every span and their count.
func TestTracesTraceFlagReadsWholeTrace(t *testing.T) {
	var paths, queries []string
	srv := traceReadServer(t, &paths, &queries)
	defer srv.Close()

	root, env, ioctx := entryHarness(t, srv)
	var stdout []string
	ioctx.Stdout = func(line string) { stdout = append(stdout, line) }
	if err := Traces(map[string]any{"output": "jsonl", "trace": "0197b20ade976f39996f99d576977e6d"}, []string{entryApp}, root, env, ioctx); err != nil {
		t.Fatalf("Traces --trace: %v", err)
	}
	if len(paths) != 1 || paths[0] != "/v1/workspaces/ws-acme/traces/0197b20ade976f39996f99d576977e6d" || queries[0] != "" {
		t.Fatalf("requests = %v ? %v, want one GET /v1/workspaces/ws-acme/traces/0197b20ade976f39996f99d576977e6d with no query", paths, queries)
	}
	if len(stdout) != 1 {
		t.Fatalf("stdout = %q, want one trace object", stdout)
	}
	var got traceDetail
	if err := json.Unmarshal([]byte(stdout[0]), &got); err != nil {
		t.Fatalf("stdout not a trace object: %v", err)
	}
	if got.TraceID != "0197b20ade976f39996f99d576977e6d" || got.SpanCount != 3 || len(got.Spans) != 3 {
		t.Fatalf("trace = %s spanCount=%d spans=%d, want 0197b20ade976f39996f99d576977e6d with 3 spans", got.TraceID, got.SpanCount, len(got.Spans))
	}
}

// TestTracesTraceFlagHumanTree pins the human rendering of a whole trace: the
// headline with the span count, then the span tree in parent and start order.
func TestTracesTraceFlagHumanTree(t *testing.T) {
	var paths, queries []string
	srv := traceReadServer(t, &paths, &queries)
	defer srv.Close()

	root, env, ioctx := entryHarness(t, srv)
	var stdout []string
	ioctx.Stdout = func(line string) { stdout = append(stdout, line) }
	if err := Traces(map[string]any{"trace": "0197b20ade976f39996f99d576977e6d"}, []string{entryApp}, root, env, ioctx); err != nil {
		t.Fatalf("Traces --trace: %v", err)
	}
	want := []string{
		"2026-11-22T11:30:00Z       80ms  3 spans  GET /  0197b20ade976f39996f99d576977e6d",
		"  +0s  80ms  GET /  (surfaces/workloads/app)",
		"    +10ms  50ms  read work",
		"      +20ms  30ms  GET /v1/work  (work-api)",
	}
	if strings.Join(stdout, "\n") != strings.Join(want, "\n") {
		t.Fatalf("stdout =\n%s\nwant\n%s", strings.Join(stdout, "\n"), strings.Join(want, "\n"))
	}
}

// TestTracesTraceFlagNotFound names the trace and the workspace on a 404.
func TestTracesTraceFlagNotFound(t *testing.T) {
	var paths, queries []string
	srv := traceReadServer(t, &paths, &queries)
	defer srv.Close()

	root, env, ioctx := entryHarness(t, srv)
	err := Traces(map[string]any{"trace": "0000000000000000000000000000dead"}, []string{entryApp}, root, env, ioctx)
	if err == nil || clicore.ExitCode(err) != clicore.ExitAPI || !strings.Contains(err.Error(), "trace 0000000000000000000000000000dead not found in workspace ws-acme") {
		t.Fatalf("err = %v (code %d), want ExitAPI naming the trace and workspace", err, clicore.ExitCode(err))
	}
}

// TestTracesTraceFlagNeedsNoApp pins that --trace resolves no app: run from a
// workspace root with no app positional and no root putnami.json, it still
// reads the trace, and an upper-case id is sent lowercased.
func TestTracesTraceFlagNeedsNoApp(t *testing.T) {
	var paths, queries []string
	srv := traceReadServer(t, &paths, &queries)
	defer srv.Close()

	root, env, ioctx := entryHarness(t, srv)
	if err := Traces(map[string]any{"output": "jsonl", "trace": "0197B20ADE976F39996F99D576977E6D"}, nil, root, env, ioctx); err != nil {
		t.Fatalf("Traces --trace without an app: %v", err)
	}
	if len(paths) != 1 || paths[0] != "/v1/workspaces/ws-acme/traces/0197b20ade976f39996f99d576977e6d" {
		t.Fatalf("requests = %v, want the lowercased trace read", paths)
	}
}

// TestTracesTraceFlagRejectsMalformedID refuses an id that is not 32
// hexadecimal digits, including a bare --trace the flag parser reads as true.
func TestTracesTraceFlagRejectsMalformedID(t *testing.T) {
	for _, value := range []any{true, "", "true", "abc", "0197b20ade976f39996f99d576977e6dz"} {
		var paths, queries []string
		srv := traceReadServer(t, &paths, &queries)
		root, env, ioctx := entryHarness(t, srv)
		err := Traces(map[string]any{"trace": value}, []string{entryApp}, root, env, ioctx)
		srv.Close()
		if err == nil || clicore.ExitCode(err) != clicore.ExitUsage || len(paths) != 0 {
			t.Fatalf("--trace %v: err = %v (code %d), requests %v, want ExitUsage and no request", value, err, clicore.ExitCode(err), paths)
		}
	}
}

// TestFormatTraceDetailPrintsEverySpanOnce pins the tree on malformed traces:
// an orphan, a parent cycle and a duplicated span id each still print, so the
// tree always shows as many spans as the headline counts.
func TestFormatTraceDetailPrintsEverySpanOnce(t *testing.T) {
	at := func(ms int) time.Time { return time.Date(2026, 11, 22, 11, 30, 0, ms*int(time.Millisecond), time.UTC) }
	spans := []traceSpan{
		{SpanID: "r", Name: "root", Start: at(0)},
		{SpanID: "a", ParentSpanID: "b", Name: "cycle-a", Start: at(1)},
		{SpanID: "b", ParentSpanID: "a", Name: "cycle-b", Start: at(2)},
		{SpanID: "r", Name: "root-dup", Start: at(3)},
		{SpanID: "c", ParentSpanID: "r", Name: "child", Start: at(4)},
		{SpanID: "o", ParentSpanID: "gone", Name: "orphan", Start: at(5)},
	}
	lines := formatTraceDetail(traceDetail{TraceID: "t", Start: at(0), SpanCount: len(spans), Spans: spans})
	if len(lines) != 1+len(spans) {
		t.Fatalf("lines = %d (%q), want the headline and %d spans", len(lines), lines, len(spans))
	}
	want := []string{"root", "child", "root-dup", "orphan", "cycle-a", "cycle-b"}
	for i, name := range want {
		if line := lines[1+i]; !strings.HasSuffix(line, "  "+name) {
			t.Fatalf("line %d = %q, want span %q (tree:\n%s)", 1+i, line, name, strings.Join(lines, "\n"))
		}
	}
	if !strings.HasPrefix(lines[2], "    +4ms") || !strings.Contains(lines[2], "child") {
		t.Fatalf("lines[2] = %q, want child nested under root", lines[2])
	}
}

// TestTracesTraceFlagRejectsListingFlags refuses a listing flag next to
// --trace before any request.
func TestTracesTraceFlagRejectsListingFlags(t *testing.T) {
	for _, extra := range []map[string]any{
		{"slow": "500ms"}, {"from": "1h"}, {"to": "5m"}, {"cursor": "c1"}, {"limit": "5"}, {"all": true},
	} {
		var paths, queries []string
		srv := traceReadServer(t, &paths, &queries)
		root, env, ioctx := entryHarness(t, srv)
		params := map[string]any{"trace": "0197b20ade976f39996f99d576977e6d"}
		for k, v := range extra {
			params[k] = v
		}
		err := Traces(params, []string{entryApp}, root, env, ioctx)
		srv.Close()
		if err == nil || clicore.ExitCode(err) != clicore.ExitUsage {
			t.Fatalf("%v: err = %v (code %d), want ExitUsage", extra, err, clicore.ExitCode(err))
		}
		if len(paths) != 0 {
			t.Fatalf("%v: requests = %v, want none", extra, paths)
		}
	}
}
