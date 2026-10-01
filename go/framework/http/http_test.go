package http

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"go.putnami.dev/app"
	"go.putnami.dev/logger"

	"go.putnami.dev/protocol/features/spectest"
)

// --- Router Tests ---

func TestRouterExactMatch(t *testing.T) {
	r := NewRouter[string]()
	r.Add("/users", "list-users")

	match := r.lookup("/users")
	if match == nil {
		t.Fatal("expected match")
	}
	if match.Handlers[0] != "list-users" {
		t.Errorf("expected 'list-users', got %q", match.Handlers[0])
	}
}

func TestRouterSegmentBoundaries(t *testing.T) {
	// A differently-segmented but character-identical path must not reach a
	// handler registered for another segmentation, and static routes that share
	// a character prefix across a boundary must not collide at registration.
	r := NewRouter[string]()
	r.Add("/health", "health")

	if m := r.lookup("/hea/lth"); m != nil {
		t.Errorf("/hea/lth must not match the /health handler, got %q", m.Handlers[0])
	}
	if m := r.lookup("/health"); m == nil || m.Handlers[0] != "health" {
		t.Errorf("/health must still match its own handler, got %v", m)
	}

	// Registering the two-segment sibling must not panic as a duplicate.
	r.Add("/hea/lth", "split")
	if m := r.lookup("/hea/lth"); m == nil || m.Handlers[0] != "split" {
		t.Errorf("/hea/lth must match its own handler, got %v", m)
	}
	if m := r.lookup("/health"); m == nil || m.Handlers[0] != "health" {
		t.Errorf("/health must remain intact after adding /hea/lth, got %v", m)
	}
}

func TestRouterTrailingSlash(t *testing.T) {
	// A request path with a trailing slash must reach a handler registered
	// without one. Regression guard for the OCI registry outage: "GET /v2/" —
	// the Docker Distribution base endpoint every client probes — 404'd against
	// the "/v2" registration after the segment-boundary router change.
	r := NewRouter[string]()
	r.Add("/v2", "version-check")
	if m := r.lookup("/v2/"); m == nil || m.Handlers[0] != "version-check" {
		t.Errorf("/v2/ must match the /v2 handler, got %v", m)
	}
	if m := r.lookup("/v2"); m == nil || m.Handlers[0] != "version-check" {
		t.Errorf("/v2 must still match its handler, got %v", m)
	}

	// Tolerance must NOT resurrect path confusion: a trailing slash cannot let a
	// differently-segmented path reach another segmentation's handler.
	rc := NewRouter[string]()
	rc.Add("/health", "health")
	if m := rc.lookup("/health/"); m == nil || m.Handlers[0] != "health" {
		t.Errorf("/health/ must match /health, got %v", m)
	}
	if m := rc.lookup("/hea/lth/"); m != nil {
		t.Errorf("/hea/lth/ must not match the /health handler, got %q", m.Handlers[0])
	}

	// Tolerance also applies through a path-parameter segment.
	rp := NewRouter[string]()
	rp.Add("/users/{id}", "get-user")
	if m := rp.lookup("/users/123/"); m == nil || m.Params["id"] != "123" {
		t.Errorf("/users/123/ must match /users/{id} with id=123, got %v", m)
	}

	// Registering both the bare and trailing-slash forms is not a duplicate
	// (distinct nodes under the boundary router) and must not panic.
	rb := NewRouter[string]()
	rb.Add("/v2", "bare")
	rb.Add("/v2/", "slash")
	if m := rb.lookup("/v2/"); m == nil {
		t.Fatal("/v2/ must match when both forms are registered")
	}
}

func TestRouterParamMatch(t *testing.T) {
	r := NewRouter[string]()
	r.Add("/users/{id}", "get-user")

	match := r.lookup("/users/123")
	if match == nil {
		t.Fatal("expected match")
	}
	if match.Params["id"] != "123" {
		t.Errorf("expected id='123', got %q", match.Params["id"])
	}
}

func TestRouterMultipleParams(t *testing.T) {
	r := NewRouter[string]()
	r.Add("/users/{userId}/posts/{postId}", "get-post")

	match := r.lookup("/users/42/posts/99")
	if match == nil {
		t.Fatal("expected match")
	}
	if match.Params["userId"] != "42" {
		t.Errorf("expected userId='42', got %q", match.Params["userId"])
	}
	if match.Params["postId"] != "99" {
		t.Errorf("expected postId='99', got %q", match.Params["postId"])
	}
}

func TestRouterAnonymousWildcardRemoved(t *testing.T) {
	const want = "anonymous wildcard removed; use {name...}"
	defer func() {
		if got := recover(); got != want {
			t.Fatalf("Add anonymous wildcard panic = %v, want %q", got, want)
		}
	}()

	r := NewRouter[string]()
	r.Add("/files/*", "serve-file")
}

func TestRouterCatchAllTrailing(t *testing.T) {
	r := NewRouter[string]()
	r.Add("/files/{path...}", "serve-file")

	match := r.lookup("/files/docs/readme.txt")
	if match == nil {
		t.Fatal("expected match")
	}
	if match.Params["path"] != "docs/readme.txt" {
		t.Errorf("expected path='docs/readme.txt', got %q", match.Params["path"])
	}

	// Single-segment under a catch-all should still match.
	match = r.lookup("/files/readme.txt")
	if match == nil {
		t.Fatal("expected single-segment catch-all match")
	}
	if match.Params["path"] != "readme.txt" {
		t.Errorf("expected path='readme.txt', got %q", match.Params["path"])
	}
}

func TestRouterCatchAllMiddle(t *testing.T) {
	r := NewRouter[string]()
	r.Add("/{module...}/-/blobs/upload", "blob-upload")
	r.Add("/{module...}/@v/{versionfile}", "version-file")

	cases := []struct {
		path        string
		wantModule  string
		wantVersion string
		wantPattern string
	}{
		{
			path:        "/go.putnami.dev/database/-/blobs/upload",
			wantModule:  "go.putnami.dev/database",
			wantPattern: "/{module...}/-/blobs/upload",
		},
		{
			path:        "/go.putnami.dev/protocol/diagnostic/-/blobs/upload",
			wantModule:  "go.putnami.dev/protocol/diagnostic",
			wantPattern: "/{module...}/-/blobs/upload",
		},
		{
			path:        "/go.putnami.dev/protocol/diagnostic/@v/v0.0.0.info",
			wantModule:  "go.putnami.dev/protocol/diagnostic",
			wantVersion: "v0.0.0.info",
			wantPattern: "/{module...}/@v/{versionfile}",
		},
	}
	for _, tc := range cases {
		match := r.lookup(tc.path)
		if match == nil {
			t.Fatalf("path %q: expected match", tc.path)
		}
		if match.Pattern != tc.wantPattern {
			t.Errorf("path %q: pattern = %q, want %q", tc.path, match.Pattern, tc.wantPattern)
		}
		if match.Params["module"] != tc.wantModule {
			t.Errorf("path %q: module = %q, want %q", tc.path, match.Params["module"], tc.wantModule)
		}
		if tc.wantVersion != "" && match.Params["versionfile"] != tc.wantVersion {
			t.Errorf("path %q: versionfile = %q, want %q", tc.path, match.Params["versionfile"], tc.wantVersion)
		}
	}
}

// TestRouterCatchAllSuffixPrecedence pins that a bare catch-all terminal never
// shadows its sibling fixed-suffix routes: a gomod-style server registers
// /{module...} alongside /{module...}/@v/list etc., and each request must reach
// the handler whose suffix it carries, with the terminal only matching paths no
// suffix route claims.
func TestRouterCatchAllSuffixPrecedence(t *testing.T) {
	spectest.Proves(t, "go/http-services", "routing", "catch-all-may-precede-a-literal-suffix")
	r := NewRouter[string]()
	r.Add("/{module...}", "vanity-meta")
	r.Add("/{module...}/@v/list", "version-list")
	r.Add("/{module...}/@v/{versionfile}", "version-file")
	r.Add("/{module...}/@latest", "latest")
	r.Add("/{module...}/-/blobs/upload", "blob-upload")

	cases := []struct {
		path        string
		wantHandler string
		wantModule  string
	}{
		{"/github.com/user/repo/@v/list", "version-list", "github.com/user/repo"},
		{"/mod/@v/list", "version-list", "mod"},
		{"/go.putnami.dev/x/y/@v/v1.2.3.info", "version-file", "go.putnami.dev/x/y"},
		{"/mod/@latest", "latest", "mod"},
		{"/mod/-/blobs/upload", "blob-upload", "mod"},
		// No suffix route claims these, so the terminal absorbs the full path.
		{"/github.com/user/repo", "vanity-meta", "github.com/user/repo"},
		{"/mod", "vanity-meta", "mod"},
		// "@v/list" is a valid module tail when nothing precedes it for the
		// catch-all to capture (it must consume at least one segment), so the
		// terminal takes over.
		{"/@v/list", "vanity-meta", "@v/list"},
	}
	for _, tc := range cases {
		match := r.lookup(tc.path)
		if match == nil {
			t.Fatalf("path %q: expected match", tc.path)
		}
		if len(match.Handlers) != 1 || match.Handlers[0] != tc.wantHandler {
			t.Errorf("path %q: handlers = %v, want [%q]", tc.path, match.Handlers, tc.wantHandler)
		}
		if match.Params["module"] != tc.wantModule {
			t.Errorf("path %q: module = %q, want %q", tc.path, match.Params["module"], tc.wantModule)
		}
	}
}

func TestRouterCatchAllRequiresOneSegment(t *testing.T) {
	spectest.Proves(t, "go/http-services", "routing", "catch-all-requires-at-least-one-segment")
	r := NewRouter[string]()
	r.Add("/{module...}/-/blobs/upload", "blob-upload")

	// No segments before the literal suffix → no match.
	if r.lookup("/-/blobs/upload") != nil {
		t.Error("expected no match for empty catch-all capture")
	}
}

// TestRouterCatchAllLongPath exercises a long path against a {name...} catch-all
// with a fixed suffix. It pins the captured value byte-for-byte (the O(n) tail-
// trimming back-off must stay identical to strings.Join(all[:take], "/")) and
// matching of the longest-prefix capture across many segments, covering the
// algorithmic-amplification fix without changing routing semantics.
func TestRouterCatchAllLongPath(t *testing.T) {
	spectest.Proves(t, "go/http-services", "routing", "catch-all-captures-joined-segments")
	r := NewRouter[string]()
	r.Add("/{module...}/-/blobs/upload", "blob-upload")

	const nSegs = 256
	segs := make([]string, nSegs)
	for i := range segs {
		segs[i] = fmt.Sprintf("seg%d", i)
	}
	wantModule := strings.Join(segs, "/")
	path := "/" + wantModule + "/-/blobs/upload"

	match := r.lookup(path)
	if match == nil {
		t.Fatalf("expected match for %d-segment catch-all path", nSegs)
	}
	if match.Pattern != "/{module...}/-/blobs/upload" {
		t.Errorf("pattern = %q, want /{module...}/-/blobs/upload", match.Pattern)
	}
	if match.Params["module"] != wantModule {
		t.Errorf("module capture mismatch (len got %d want %d)", len(match.Params["module"]), len(wantModule))
	}

	// Back-off must also find a shorter, non-maximal capture: a path whose
	// suffix matches only after consuming fewer-than-all leading segments.
	// "/a/b/-/blobs/upload" forces take=2 (module="a/b") after the maximal
	// take=5 capture fails to match the suffix subtree.
	if m := r.lookup("/a/b/-/blobs/upload"); m == nil || m.Params["module"] != "a/b" {
		t.Errorf("back-off capture = %+v, want module=a/b", m)
	}

	// A long path that never satisfies the suffix must cleanly miss (and not
	// leave a stale capture); this is the worst-case back-off path for perf.
	if m := r.lookup("/" + wantModule + "/-/blobs/nope"); m != nil {
		t.Errorf("expected no match for unmatched suffix, got %+v", m)
	}
}

// BenchmarkRouterCatchAllLongPathMiss measures the worst case for the back-off
// loop: a long path under a catch-all whose fixed suffix never matches, so the
// matcher backs off across every segment. With the O(n) tail-trim capture this
// stays linear; the prior strings.Join-per-iteration form was O(n²).
func BenchmarkRouterCatchAllLongPathMiss(b *testing.B) {
	r := NewRouter[string]()
	r.Add("/{module...}/-/blobs/upload", "blob-upload")

	segs := make([]string, 256)
	for i := range segs {
		segs[i] = fmt.Sprintf("seg%d", i)
	}
	path := "/" + strings.Join(segs, "/") + "/-/blobs/nope"

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if r.lookup(path) != nil {
			b.Fatal("unexpected match")
		}
	}
}

func TestRouterSingleParamRejectsSlashes(t *testing.T) {
	spectest.Proves(t, "go/http-services", "routing", "single-segment-param-never-matches-a-slash")
	r := NewRouter[string]()
	r.Add("/users/{id}", "get-user")

	// /users/a/b shouldn't be captured by {id} since {id} captures exactly one segment.
	if match := r.lookup("/users/a/b"); match != nil {
		t.Errorf("single-segment param should not match multi-segment path, got params=%v", match.Params)
	}
}

func TestRouterCatchAllPanicsOnConflictingNames(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for conflicting catch-all param names")
		}
	}()
	r := NewRouter[string]()
	r.Add("/{a...}/x", "x")
	r.Add("/{b...}/y", "y")
}

func TestRouterNoMatch(t *testing.T) {
	r := NewRouter[string]()
	r.Add("/users", "list")

	if r.lookup("/posts") != nil {
		t.Error("expected no match")
	}
}

func TestRouterRootPath(t *testing.T) {
	r := NewRouter[string]()
	r.Add("/", "root")

	match := r.lookup("/")
	if match == nil {
		t.Fatal("expected match for root")
	}
}

func TestRouterRoutes(t *testing.T) {
	r := NewRouter[string]()
	r.Add("/users", "a")
	r.Add("/users/{id}", "b")
	r.Add("/posts", "c")

	routes := r.Routes()
	if len(routes) != 3 {
		t.Errorf("expected 3 routes, got %d", len(routes))
	}
}

func TestRouterDuplicateRoutePanics(t *testing.T) {
	spectest.Proves(t, "go/http-services", "routing", "duplicate-method-and-pattern-fails-registration")
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for duplicate route registration")
		}
	}()
	router := NewRouter[string]()
	router.Add("/users", "a")
	router.Add("/users", "b")
}

// --- RouteController Tests ---

func TestRouteController(t *testing.T) {
	rc := newRouteController()
	rc.Add("GET", "/users", func(ctx *Context) *Response { return JSON("get") })
	rc.Add("POST", "/users", func(ctx *Context) *Response { return JSON("post") })

	if rc.lookup("GET", "/users") == nil {
		t.Error("expected GET match")
	}
	if rc.lookup("POST", "/users") == nil {
		t.Error("expected POST match")
	}
	if rc.lookup("DELETE", "/users") != nil {
		t.Error("expected no DELETE match")
	}
}

// --- Response Tests ---

func TestJSONResponse(t *testing.T) {
	resp := JSON(map[string]string{"hello": "world"})
	if resp.Status != 200 {
		t.Errorf("expected 200, got %d", resp.Status)
	}
	if resp.Headers.Get("Content-Type") != "application/json" {
		t.Error("expected application/json content type")
	}
}

func TestTextResponse(t *testing.T) {
	resp := Text("hello")
	w := httptest.NewRecorder()
	resp.WriteTo(w)

	if w.Code != 200 {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != "hello" {
		t.Errorf("expected 'hello', got %q", w.Body.String())
	}
}

func TestRedirectResponse(t *testing.T) {
	resp := Redirect("/login", 302)
	if resp.Status != 302 {
		t.Errorf("expected 302, got %d", resp.Status)
	}
	if resp.Headers.Get("Location") != "/login" {
		t.Error("expected Location header")
	}
}

func TestResponseWriteTo(t *testing.T) {
	resp := JSONStatus(201, map[string]string{"id": "abc"})
	w := httptest.NewRecorder()
	resp.WriteTo(w)

	if w.Code != 201 {
		t.Errorf("expected 201, got %d", w.Code)
	}

	var body map[string]string
	json.NewDecoder(w.Body).Decode(&body)
	if body["id"] != "abc" {
		t.Errorf("expected id='abc', got %q", body["id"])
	}
}

func TestResponseWithHeader(t *testing.T) {
	resp := JSON(nil).WithHeader("X-Custom", "value")
	if resp.Headers.Get("X-Custom") != "value" {
		t.Error("expected custom header")
	}
}

// --- Context Tests ---

func TestContextQueryParams(t *testing.T) {
	req := httptest.NewRequest("GET", "/search?q=test&page=2", nil)
	ctx := NewContext(httptest.NewRecorder(), req)

	if ctx.Query("q") != "test" {
		t.Errorf("expected 'test', got %q", ctx.Query("q"))
	}
	if ctx.Query("page") != "2" {
		t.Errorf("expected '2', got %q", ctx.Query("page"))
	}
}

func TestContextParam(t *testing.T) {
	ctx := &Context{Params: map[string]string{"id": "42"}}
	if ctx.Param("id") != "42" {
		t.Error("expected '42'")
	}
	if ctx.Param("missing") != "" {
		t.Error("expected empty for missing param")
	}
}

func TestContextBody(t *testing.T) {
	body := `{"name":"Alice","age":30}`
	req := httptest.NewRequest("POST", "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx := NewContext(httptest.NewRecorder(), req)

	var data map[string]any
	if err := ctx.Body(&data); err != nil {
		t.Fatal(err)
	}
	if data["name"] != "Alice" {
		t.Errorf("expected 'Alice', got %v", data["name"])
	}
}

func TestContextIsSecured(t *testing.T) {
	ctx := &Context{}
	if ctx.IsSecured() {
		t.Error("should not be secured without user")
	}
	ctx.User = &Claims{Subject: "user-1"}
	if !ctx.IsSecured() {
		t.Error("should be secured with user")
	}
}

// --- Middleware Tests ---

func TestMiddlewareChain(t *testing.T) {
	var order []string

	mw1 := func(ctx *Context, next func() *Response) *Response {
		order = append(order, "before-1")
		resp := next()
		order = append(order, "after-1")
		return resp
	}
	mw2 := func(ctx *Context, next func() *Response) *Response {
		order = append(order, "before-2")
		resp := next()
		order = append(order, "after-2")
		return resp
	}

	handler := Chain(mw1, mw2)(func(ctx *Context) *Response {
		order = append(order, "handler")
		return JSON("ok")
	})

	req := httptest.NewRequest("GET", "/", nil)
	ctx := NewContext(httptest.NewRecorder(), req)
	handler(ctx)

	expected := []string{"before-1", "before-2", "handler", "after-2", "after-1"}
	if !reflect.DeepEqual(order, expected) {
		t.Errorf("expected %v, got %v", expected, order)
	}
}

func TestMiddlewareShortCircuit(t *testing.T) {
	authMw := func(ctx *Context, next func() *Response) *Response {
		if ctx.Header("Authorization") == "" {
			return Unauthorized()
		}
		return next()
	}

	handler := Chain(authMw)(func(ctx *Context) *Response {
		return JSON("ok")
	})

	// Without auth
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	resp := handler(NewContext(w, req))
	if resp.Status != 401 {
		t.Errorf("expected 401, got %d", resp.Status)
	}

	// With auth
	req = httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer token")
	resp = handler(NewContext(httptest.NewRecorder(), req))
	if resp.Status != 200 {
		t.Errorf("expected 200, got %d", resp.Status)
	}
}

// --- Recovery Middleware Tests ---

func TestRecoveryMiddleware(t *testing.T) {
	handler := Chain(Recovery())(func(ctx *Context) *Response {
		panic("something went wrong")
	})

	req := httptest.NewRequest("GET", "/", nil)
	resp := handler(NewContext(httptest.NewRecorder(), req))
	if resp == nil {
		t.Fatal("expected response from recovery")
	}
	if resp.Status != 500 {
		t.Errorf("expected 500, got %d", resp.Status)
	}
}

func TestRecoveryMiddlewareNoError(t *testing.T) {
	handler := Chain(Recovery())(func(ctx *Context) *Response {
		return JSON("ok")
	})

	req := httptest.NewRequest("GET", "/", nil)
	resp := handler(NewContext(httptest.NewRecorder(), req))
	if resp.Status != 200 {
		t.Errorf("expected 200, got %d", resp.Status)
	}
}

// --- Logging Middleware Tests ---

// httpTerminalGroup extracts the closed "http" group attr from a terminal
// record, failing the test when it is absent or not a map.
func httpTerminalGroup(t *testing.T, e logger.LogEntry) map[string]any {
	t.Helper()
	for _, a := range e.Attrs {
		if a.Key == "http" {
			m, ok := a.Value.Any().(map[string]any)
			if !ok {
				t.Fatalf("http attr is not a map: %T", a.Value.Any())
			}
			return m
		}
	}
	t.Fatalf("no http group attr on terminal record")
	return nil
}

func TestLoggingMiddleware(t *testing.T) {
	sink := logger.NewMemorySink()
	log := logger.New("test", logger.LevelDebug, sink)

	handler := Chain(Logging(LoggerOptions{Logger: log}))(func(ctx *Context) *Response {
		return JSON("ok")
	})

	req := httptest.NewRequest("GET", "/users", nil)
	handler(NewContext(httptest.NewRecorder(), req))

	entries := sink.Entries
	if len(entries) != 1 {
		t.Fatalf("expected 1 log entry, got %d", len(entries))
	}
	if entries[0].Message != "[GET] /users" {
		t.Errorf("expected message %q, got %q", "[GET] /users", entries[0].Message)
	}
	if entries[0].Level != logger.LevelInfo {
		t.Errorf("expected INFO level for 2xx, got %v", entries[0].Level)
	}

	group := httpTerminalGroup(t, entries[0])
	// Closed schema: exactly method, routePath, status, outcome, durationMs.
	if len(group) != 5 {
		t.Errorf("expected http group to have exactly 5 keys, got %d: %v", len(group), group)
	}
	if group["method"] != "GET" {
		t.Errorf("method = %v, want GET", group["method"])
	}
	if group["routePath"] != "/users" {
		t.Errorf("routePath = %v, want /users", group["routePath"])
	}
	if group["status"] != 200 {
		t.Errorf("status = %v, want 200", group["status"])
	}
	if group["outcome"] != "success" {
		t.Errorf("outcome = %v, want success", group["outcome"])
	}
	if _, ok := group["durationMs"].(int64); !ok {
		t.Errorf("durationMs missing or not int64: %T %v", group["durationMs"], group["durationMs"])
	}
	// requestId must NOT appear anywhere on the record (closed schema; the
	// correlation id travels only as the top-level traceId).
	if _, ok := group["requestId"]; ok {
		t.Errorf("http group must not carry requestId, got %v", group["requestId"])
	}
	if _, ok := group["duration"]; ok {
		t.Errorf("http group must use durationMs, not duration")
	}
}

func TestLoggingMiddlewareTraceID(t *testing.T) {
	sink := logger.NewMemorySink()
	log := logger.New("test", logger.LevelDebug, sink)

	// RequestID must run before Logging so the correlation id bridges into the
	// terminal record's top-level traceId (via logger.TraceIDFromContext).
	handler := Chain(RequestID(), Logging(LoggerOptions{Logger: log}))(func(ctx *Context) *Response {
		return JSON("ok")
	})

	req := httptest.NewRequest("GET", "/users", nil)
	req.Header.Set("X-Request-ID", "trace-abc")
	handler(NewContext(httptest.NewRecorder(), req))

	entries := sink.Entries
	if len(entries) != 1 {
		t.Fatalf("expected 1 log entry, got %d", len(entries))
	}
	if entries[0].TraceID != "trace-abc" {
		t.Errorf("traceId = %q, want %q", entries[0].TraceID, "trace-abc")
	}
	group := httpTerminalGroup(t, entries[0])
	if _, ok := group["requestId"]; ok {
		t.Errorf("http group must not carry requestId")
	}
}

func TestLoggingMiddlewareExclude(t *testing.T) {
	sink := logger.NewMemorySink()
	log := logger.New("test", logger.LevelDebug, sink)

	handler := Chain(Logging(LoggerOptions{
		Logger:  log,
		Exclude: []string{"/_/health"},
	}))(func(ctx *Context) *Response {
		return JSON("ok")
	})

	// Excluded path — no log
	req := httptest.NewRequest("GET", "/_/health", nil)
	handler(NewContext(httptest.NewRecorder(), req))

	if len(sink.Entries) != 0 {
		t.Errorf("expected no log entries for excluded path, got %d", len(sink.Entries))
	}

	// Normal path — should log
	req = httptest.NewRequest("GET", "/users", nil)
	handler(NewContext(httptest.NewRecorder(), req))

	if len(sink.Entries) != 1 {
		t.Errorf("expected 1 log entry, got %d", len(sink.Entries))
	}
}

func TestLoggingMiddlewareErrorLevel(t *testing.T) {
	sink := logger.NewMemorySink()
	log := logger.New("test", logger.LevelDebug, sink)

	handler := Chain(Logging(LoggerOptions{Logger: log}))(func(ctx *Context) *Response {
		return InternalError("fail")
	})

	req := httptest.NewRequest("GET", "/", nil)
	handler(NewContext(httptest.NewRecorder(), req))

	entries := sink.Entries
	if len(entries) != 1 {
		t.Fatalf("expected 1 log entry, got %d", len(entries))
	}
	if entries[0].Level != logger.LevelError {
		t.Errorf("expected error level for 500, got %v", entries[0].Level)
	}
	// The 5xx message is identical to success — no StatusText / error text.
	if entries[0].Message != "[GET] /" {
		t.Errorf("expected message %q (no status/error text), got %q", "[GET] /", entries[0].Message)
	}
	group := httpTerminalGroup(t, entries[0])
	if group["status"] != 500 {
		t.Errorf("status = %v, want 500", group["status"])
	}
	if group["outcome"] != "failure" {
		t.Errorf("outcome = %v, want failure", group["outcome"])
	}
}

// A handler that captures the real error via SetRequestError (as the framework
// does at its error→response mapping sites) surfaces it as the structured error
// on the 5xx terminal record, while the message stays free of error text.
func TestLoggingMiddlewareRequestError(t *testing.T) {
	sink := logger.NewMemorySink()
	log := logger.New("test", logger.LevelDebug, sink)

	handler := Chain(Logging(LoggerOptions{Logger: log}))(func(ctx *Context) *Response {
		logger.SetRequestError(ctx.Context(), fmt.Errorf("boom"))
		return InternalError("Internal Server Error")
	})

	req := httptest.NewRequest("GET", "/conformance/orders", nil)
	handler(NewContext(httptest.NewRecorder(), req))

	entries := sink.Entries
	if len(entries) != 1 {
		t.Fatalf("expected 1 log entry, got %d", len(entries))
	}
	if entries[0].Message != "[GET] /conformance/orders" {
		t.Errorf("message = %q, want %q", entries[0].Message, "[GET] /conformance/orders")
	}
	if strings.Contains(entries[0].Message, "boom") {
		t.Errorf("message must not carry error text, got %q", entries[0].Message)
	}
	if entries[0].Error == nil {
		t.Fatal("expected structured error on 5xx terminal record")
	}
	if entries[0].Error.Message != "boom" {
		t.Errorf("error.message = %q, want boom", entries[0].Error.Message)
	}
	group := httpTerminalGroup(t, entries[0])
	if group["outcome"] != "failure" {
		t.Errorf("outcome = %v, want failure", group["outcome"])
	}
}

// Recovery converts a panic into a 500 and records the panic error via
// SetRequestError, which the outer Logging middleware then surfaces on the
// terminal record.
func TestLoggingMiddlewareRecoveryError(t *testing.T) {
	sink := logger.NewMemorySink()
	log := logger.New("test", logger.LevelDebug, sink)

	// Logging outer, Recovery inner: Logging installs the bag before Recovery
	// runs, so Recovery's SetRequestError reaches this record.
	handler := Chain(Logging(LoggerOptions{Logger: log}), Recovery())(func(ctx *Context) *Response {
		panic("kaboom")
	})

	req := httptest.NewRequest("GET", "/conformance/orders", nil)
	resp := handler(NewContext(httptest.NewRecorder(), req))
	if resp == nil || resp.Status != 500 {
		t.Fatalf("expected 500 response, got %v", resp)
	}

	// The Recovery middleware emits its own "panic recovered" record; the
	// terminal record is the one carrying the http group.
	var terminal *logger.LogEntry
	for i := range sink.Entries {
		if sink.Entries[i].Message == "[GET] /conformance/orders" {
			terminal = &sink.Entries[i]
		}
	}
	if terminal == nil {
		t.Fatalf("no terminal record found in %d entries", len(sink.Entries))
	}
	if terminal.Level != logger.LevelError {
		t.Errorf("terminal level = %v, want ERROR", terminal.Level)
	}
	if terminal.Error == nil {
		t.Fatal("expected structured error carried from the recovered panic")
	}
	if !strings.Contains(terminal.Error.Message, "kaboom") {
		t.Errorf("error.message = %q, want it to contain kaboom", terminal.Error.Message)
	}
	group := httpTerminalGroup(t, *terminal)
	if group["status"] != 500 || group["outcome"] != "failure" {
		t.Errorf("group status/outcome = %v/%v, want 500/failure", group["status"], group["outcome"])
	}
}

// TestLoggingMiddlewareTerminalJSON pins the fully-rendered terminal records to
// the frozen conformance shape: a success INFO record and a 5xx ERROR record,
// both with the closed http group, top-level traceId, and error text absent
// from the message; the 5xx record additionally carries a structured error.
func TestLoggingMiddlewareTerminalJSON(t *testing.T) {
	render := func(h Handler, method, path, reqID string) map[string]any {
		var buf bytes.Buffer
		log := logger.New("http", logger.LevelDebug, logger.NewJSONSinkWriter(&buf))
		handler := Chain(RequestID(), Logging(LoggerOptions{Logger: log}))(h)
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("X-Request-ID", reqID)
		handler(NewContext(httptest.NewRecorder(), req))
		// The terminal record is the last non-empty JSON line.
		lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
		var rec map[string]any
		if err := json.Unmarshal([]byte(lines[len(lines)-1]), &rec); err != nil {
			t.Fatalf("unmarshal terminal record: %v (line=%q)", err, lines[len(lines)-1])
		}
		return rec
	}

	success := render(func(ctx *Context) *Response { return JSON("ok") }, "GET", "/conformance/orders", "trace-1")
	t.Logf("success terminal record: %s", mustJSON(t, success))
	if success["severity"] != "INFO" {
		t.Errorf("severity = %v, want INFO", success["severity"])
	}
	if success["message"] != "[GET] /conformance/orders" {
		t.Errorf("message = %v", success["message"])
	}
	if success["logger"] != "http" {
		t.Errorf("logger = %v, want http", success["logger"])
	}
	if success["traceId"] != "trace-1" {
		t.Errorf("traceId = %v, want trace-1", success["traceId"])
	}
	if _, ok := success["error"]; ok {
		t.Errorf("success record must not carry an error field")
	}
	sg, ok := success["http"].(map[string]any)
	if !ok {
		t.Fatalf("http group not a JSON object: %T", success["http"])
	}
	wantKeys := map[string]bool{"method": true, "routePath": true, "status": true, "outcome": true, "durationMs": true}
	for k := range sg {
		if !wantKeys[k] {
			t.Errorf("unexpected key %q in http group (closed schema)", k)
		}
	}
	for k := range wantKeys {
		if _, ok := sg[k]; !ok {
			t.Errorf("missing key %q in http group", k)
		}
	}
	if sg["outcome"] != "success" || sg["status"] != float64(200) {
		t.Errorf("group outcome/status = %v/%v, want success/200", sg["outcome"], sg["status"])
	}

	failure := render(func(ctx *Context) *Response {
		logger.SetRequestError(ctx.Context(), fmt.Errorf("boom"))
		return InternalError("Internal Server Error")
	}, "GET", "/conformance/orders", "trace-2")
	t.Logf("5xx terminal record: %s", mustJSON(t, failure))
	if failure["severity"] != "ERROR" {
		t.Errorf("severity = %v, want ERROR", failure["severity"])
	}
	if failure["message"] != "[GET] /conformance/orders" {
		t.Errorf("5xx message = %v, must equal success message with no error text", failure["message"])
	}
	fg, ok := failure["http"].(map[string]any)
	if !ok {
		t.Fatalf("5xx http group not a JSON object: %T", failure["http"])
	}
	if fg["outcome"] != "failure" || fg["status"] != float64(500) {
		t.Errorf("5xx group outcome/status = %v/%v, want failure/500", fg["outcome"], fg["status"])
	}
	errObj, ok := failure["error"].(map[string]any)
	if !ok {
		t.Fatalf("5xx record must carry a structured error object, got %T", failure["error"])
	}
	if errObj["message"] != "boom" {
		t.Errorf("error.message = %v, want boom", errObj["message"])
	}
	if _, ok := errObj["name"]; !ok {
		t.Errorf("error object must carry a name")
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// --- Rate Limit Middleware Tests ---

func TestRateLimitMiddleware(t *testing.T) {
	mw := RateLimit(RateLimitOptions{
		WindowMs: 60_000,
		Max:      3,
		KeyFunc:  func(_ *Context) string { return "test-key" },
	})

	handler := Chain(mw)(func(ctx *Context) *Response {
		return JSON("ok")
	})

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest("GET", "/", nil)
		w := httptest.NewRecorder()
		resp := handler(NewContext(w, req))
		if resp.Status != 200 {
			t.Errorf("request %d: expected 200, got %d", i+1, resp.Status)
		}
	}

	// 4th request should be rate limited
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	resp := handler(NewContext(w, req))
	if resp.Status != 429 {
		t.Errorf("expected 429 after limit exceeded, got %d", resp.Status)
	}
	if resp.Headers.Get("Retry-After") == "" {
		t.Error("expected Retry-After header")
	}
}

func TestRateLimitHeaders(t *testing.T) {
	mw := RateLimit(RateLimitOptions{
		Max:     5,
		KeyFunc: func(_ *Context) string { return "headers-test" },
	})

	handler := Chain(mw)(func(ctx *Context) *Response {
		return JSON("ok")
	})

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	handler(NewContext(w, req))

	if w.Header().Get("RateLimit-Limit") != "5" {
		t.Errorf("expected RateLimit-Limit=5, got %q", w.Header().Get("RateLimit-Limit"))
	}
	if w.Header().Get("RateLimit-Remaining") != "4" {
		t.Errorf("expected RateLimit-Remaining=4, got %q", w.Header().Get("RateLimit-Remaining"))
	}
}

func TestRateLimitDefaultKey(t *testing.T) {
	keyFunc := buildRateLimitKeyFunc(nil)

	// Without trusted proxies, X-Forwarded-For is ignored
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Forwarded-For", "192.168.1.1, 10.0.0.1")
	ctx := NewContext(httptest.NewRecorder(), req)
	key := keyFunc(ctx)
	if key == "192.168.1.1" {
		t.Error("should not trust X-Forwarded-For without trusted proxies")
	}

	// With trusted proxy matching RemoteAddr, X-Forwarded-For is used
	// httptest sets RemoteAddr to "192.0.2.1:1234"
	trustedKeyFunc := buildRateLimitKeyFunc([]string{"192.0.2.1"})
	req = httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Forwarded-For", "10.0.0.1, 192.0.2.1")
	ctx = NewContext(httptest.NewRecorder(), req)
	key = trustedKeyFunc(ctx)
	if key != "10.0.0.1" {
		t.Errorf("expected first IP from X-Forwarded-For with trusted proxy, got %q", key)
	}

	// Fallback to RemoteAddr (no XFF header)
	req = httptest.NewRequest("GET", "/", nil)
	ctx = NewContext(httptest.NewRecorder(), req)
	key = trustedKeyFunc(ctx)
	if key != "192.0.2.1" {
		t.Errorf("expected RemoteAddr (without port), got %q", key)
	}
}

// --- Compression Middleware Tests ---

func TestCompressionMiddleware(t *testing.T) {
	// Create a large enough response to exceed threshold
	data := strings.Repeat("hello world ", 200)

	mw := Compression(CompressionOptions{Threshold: 100})
	handler := Chain(mw)(func(ctx *Context) *Response {
		return Text(data)
	})

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	resp := handler(NewContext(w, req))
	resp.WriteTo(w)

	if w.Header().Get("Content-Encoding") != "gzip" {
		t.Error("expected Content-Encoding: gzip")
	}
	if w.Header().Get("Vary") != "Accept-Encoding" {
		t.Error("expected Vary: Accept-Encoding")
	}

	// Decompress and verify content
	gz, err := gzip.NewReader(w.Body)
	if err != nil {
		t.Fatal(err)
	}
	decompressed, err := io.ReadAll(gz)
	if err != nil {
		t.Fatal(err)
	}
	if string(decompressed) != data {
		t.Error("decompressed content does not match original")
	}
}

func TestCompressionSkipsSmallResponses(t *testing.T) {
	mw := Compression(CompressionOptions{Threshold: 1024})
	handler := Chain(mw)(func(ctx *Context) *Response {
		return Text("small")
	})

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	resp := handler(NewContext(w, req))
	resp.WriteTo(w)

	if w.Header().Get("Content-Encoding") == "gzip" {
		t.Error("should not compress small responses")
	}
}

func TestCompressionSkipsWithoutAcceptEncoding(t *testing.T) {
	data := strings.Repeat("hello ", 500)
	mw := Compression(CompressionOptions{Threshold: 100})
	handler := Chain(mw)(func(ctx *Context) *Response {
		return Text(data)
	})

	req := httptest.NewRequest("GET", "/", nil)
	// No Accept-Encoding header
	w := httptest.NewRecorder()
	resp := handler(NewContext(w, req))
	resp.WriteTo(w)

	if w.Header().Get("Content-Encoding") == "gzip" {
		t.Error("should not compress without Accept-Encoding: gzip")
	}
}

func TestCompressionSkipsNonCompressible(t *testing.T) {
	data := strings.Repeat("\x00\x01\x02", 500)
	mw := Compression(CompressionOptions{Threshold: 100})
	handler := Chain(mw)(func(ctx *Context) *Response {
		r := NewResponse(200)
		r.raw = []byte(data)
		r.Headers.Set("Content-Type", "application/octet-stream")
		return r
	})

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	resp := handler(NewContext(w, req))
	resp.WriteTo(w)

	if w.Header().Get("Content-Encoding") == "gzip" {
		t.Error("should not compress non-compressible content types")
	}
}

// --- Content Negotiation Tests ---

func TestConcreteMediaType(t *testing.T) {
	cases := []struct {
		value string
		want  bool
	}{
		{"application/octet-stream", true},
		{"text/plain; charset=utf-8", true},
		{"image/png", true},
		{"", false},
		{"*/*", false},
		{"image/*", false},
		{"text", false},
		{"/plain", false},
		{"text/", false},
		{"bad media/type", false},
		{"text/plain; charset", false},
		{"text/plain\r\n", false},
		{"text/plain\n", false},
	}
	for _, c := range cases {
		if got := ConcreteMediaType(c.value); got != c.want {
			t.Errorf("ConcreteMediaType(%q) = %v, want %v", c.value, got, c.want)
		}
	}
}

func TestParseAcceptEmpty(t *testing.T) {
	types := ParseAccept("")
	if len(types) != 0 {
		t.Errorf("expected empty, got %d types", len(types))
	}
}

func TestParseAcceptSingle(t *testing.T) {
	types := ParseAccept("application/json")
	if len(types) != 1 {
		t.Fatalf("expected 1, got %d", len(types))
	}
	if types[0].Full != "application/json" {
		t.Errorf("expected application/json, got %q", types[0].Full)
	}
	if types[0].Quality != 1.0 {
		t.Errorf("expected quality 1.0, got %f", types[0].Quality)
	}
}

func TestParseAcceptMultipleWithQuality(t *testing.T) {
	types := ParseAccept("text/html, application/json;q=0.9, */*;q=0.1")
	if len(types) != 3 {
		t.Fatalf("expected 3, got %d", len(types))
	}
	// Should be sorted by quality: text/html (1.0), application/json (0.9), */* (0.1)
	if types[0].Full != "text/html" {
		t.Errorf("expected text/html first, got %q", types[0].Full)
	}
	if types[1].Full != "application/json" {
		t.Errorf("expected application/json second, got %q", types[1].Full)
	}
	if types[2].Full != "*/*" {
		t.Errorf("expected */* third, got %q", types[2].Full)
	}
}

func TestNegotiateContentType(t *testing.T) {
	offered := []string{"application/json", "text/html", "text/plain"}

	// Client prefers HTML
	best := NegotiateContentType("text/html, application/json;q=0.9", offered)
	if best != "text/html" {
		t.Errorf("expected text/html, got %q", best)
	}

	// Client prefers JSON
	best = NegotiateContentType("application/json", offered)
	if best != "application/json" {
		t.Errorf("expected application/json, got %q", best)
	}

	// Wildcard accepts first offered
	best = NegotiateContentType("*/*", offered)
	if best != "application/json" {
		t.Errorf("expected application/json (first offered), got %q", best)
	}

	// No match
	best = NegotiateContentType("application/xml", offered)
	if best != "" {
		t.Errorf("expected empty for no match, got %q", best)
	}

	// Empty accept → first offered
	best = NegotiateContentType("", offered)
	if best != "application/json" {
		t.Errorf("expected first offered for empty accept, got %q", best)
	}
}

func TestNegotiateContentTypeSubtypeWildcard(t *testing.T) {
	offered := []string{"application/json", "text/html"}

	best := NegotiateContentType("text/*", offered)
	if best != "text/html" {
		t.Errorf("expected text/html for text/*, got %q", best)
	}
}

// --- Response BodyBytes Tests ---

func TestResponseBodyBytesRaw(t *testing.T) {
	resp := Text("hello")
	bytes, err := resp.BodyBytes()
	if err != nil {
		t.Fatal(err)
	}
	if string(bytes) != "hello" {
		t.Errorf("expected 'hello', got %q", string(bytes))
	}
}

func TestResponseBodyBytesJSON(t *testing.T) {
	resp := JSON(map[string]string{"key": "value"})
	bytes, err := resp.BodyBytes()
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]string
	json.Unmarshal(bytes, &data)
	if data["key"] != "value" {
		t.Errorf("expected key=value, got %q", data["key"])
	}
}

func TestResponseBodyBytesNil(t *testing.T) {
	resp := NoContent()
	bytes, err := resp.BodyBytes()
	if err != nil {
		t.Fatal(err)
	}
	if bytes != nil {
		t.Error("expected nil bytes for no content")
	}
}

// EndpointBuilder tests live in go.putnami.dev/api (see api/endpoint_test.go and
// api/plugin_test.go). The builder used to live in this package; it now belongs to the
// transport-agnostic api/ package, and only the runtime EndpointContext stays here.

// --- Health Plugin Tests ---

func TestHealthPlugin(t *testing.T) {
	hp := NewHealthPlugin()
	handler := hp.Handler()

	// Before start — unavailable
	req := httptest.NewRequest("GET", "/_/health", nil)
	resp := handler(NewContext(httptest.NewRecorder(), req))
	if resp.Status != 503 {
		t.Errorf("expected 503 before start, got %d", resp.Status)
	}

	// After start — ok
	hp.Start(context.Background(), nil)
	resp = handler(NewContext(httptest.NewRecorder(), req))
	if resp.Status != 200 {
		t.Errorf("expected 200 after start, got %d", resp.Status)
	}

	// After stop — unavailable
	hp.Stop(context.Background(), nil)
	resp = handler(NewContext(httptest.NewRecorder(), req))
	if resp.Status != 503 {
		t.Errorf("expected 503 after stop, got %d", resp.Status)
	}
}

// --- Integration Test: ServerPlugin with httptest ---

func TestServerPluginIntegration(t *testing.T) {
	server := NewServerPlugin(ServerConfig{Port: 0})
	server.GET("/hello", func(ctx *Context) *Response {
		return JSON(map[string]string{"message": "hello"})
	})
	server.GET("/users/{id}", func(ctx *Context) *Response {
		return JSON(map[string]string{"id": ctx.Param("id")})
	})

	// Build the handler directly for testing
	chain := Chain(server.middlewares...)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		ctx := NewContext(w, r)
		match := server.routes.lookup(r.Method, r.URL.Path)
		if match == nil {
			NotFound().WriteTo(w)
			return
		}
		ctx.Route = match.Pattern
		ctx.Params = match.Params
		handler := chain(match.Handlers[0])
		resp := handler(ctx)
		if resp != nil {
			resp.WriteTo(w)
		}
	})

	ts := httptest.NewServer(mux)
	defer ts.Close()

	// Test /hello
	resp, _ := http.Get(ts.URL + "/hello")
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if body["message"] != "hello" {
		t.Errorf("expected 'hello', got %q", body["message"])
	}

	// Test /users/42
	resp, _ = http.Get(ts.URL + "/users/42")
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if body["id"] != "42" {
		t.Errorf("expected '42', got %q", body["id"])
	}

	// Test 404
	resp, _ = http.Get(ts.URL + "/notfound")
	if resp.StatusCode != 404 {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- Health Plugin coverage ---

func TestHealthPlugin_Name(t *testing.T) {
	hp := NewHealthPlugin()
	if hp.Name() != "health" {
		t.Errorf("Name() = %q, want health", hp.Name())
	}
}

func TestHealthPlugin_Configure(t *testing.T) {
	hp := NewHealthPlugin()
	if err := hp.Configure(context.Background(), nil); err != nil {
		t.Errorf("Configure: %v", err)
	}
}

func TestHealthPlugin_CheckersHealthy(t *testing.T) {
	hp := NewHealthPlugin()
	hp.AddChecker("db", func(_ context.Context) error { return nil })
	hp.AddChecker("cache", func(_ context.Context) error { return nil })
	hp.Start(context.Background(), nil)

	req := httptest.NewRequest("GET", "/_/health", nil)
	resp := hp.Handler()(NewContext(httptest.NewRecorder(), req))

	if resp.Status != 200 {
		t.Errorf("expected 200, got %d", resp.Status)
	}
	body, _ := resp.BodyBytes()
	if !strings.Contains(string(body), `"status":"ok"`) {
		t.Errorf("expected status ok, got %s", body)
	}
	if !strings.Contains(string(body), `"db":"ok"`) {
		t.Errorf("expected db check, got %s", body)
	}
}

func TestHealthPlugin_CheckersDegraded(t *testing.T) {
	hp := NewHealthPlugin()
	hp.AddChecker("db", func(_ context.Context) error { return fmt.Errorf("connection refused") })
	hp.AddChecker("cache", func(_ context.Context) error { return nil })
	hp.Start(context.Background(), nil)

	req := httptest.NewRequest("GET", "/_/health", nil)
	resp := hp.Handler()(NewContext(httptest.NewRecorder(), req))

	if resp.Status != 503 {
		t.Errorf("expected 503 for degraded, got %d", resp.Status)
	}
	body, _ := resp.BodyBytes()
	if !strings.Contains(string(body), `"status":"degraded"`) {
		t.Errorf("expected degraded status, got %s", body)
	}
	// Raw dependency error text must not be exposed to clients; a sanitized
	// "unavailable" status is reported instead.
	if strings.Contains(string(body), "connection refused") {
		t.Errorf("health response leaked raw error text: %s", body)
	}
	if !strings.Contains(string(body), `"db":"unavailable"`) {
		t.Errorf("expected sanitized \"unavailable\" status for failing check, got %s", body)
	}
}

func TestHealthPlugin_RegisterOn(t *testing.T) {
	hp := NewHealthPlugin()
	server := NewServerPlugin(ServerConfig{Port: 0})
	hp.RegisterOn(server)

	routes := server.routes.lookup("GET", "/_/health")
	if routes == nil {
		t.Fatal("expected /_/health route to be registered")
	}
}

// healthyPlugin is a test stub implementing app.HealthChecker.
type healthyPlugin struct {
	name string
	err  error
}

func (h *healthyPlugin) Name() string                        { return h.name }
func (h *healthyPlugin) CheckHealth(_ context.Context) error { return h.err }

func TestHealthPlugin_AutoDiscovers_HealthChecker(t *testing.T) {
	hp := NewHealthPlugin()
	root := app.NewModule("root")
	root.Use(NewServerPlugin(ServerConfig{}))
	root.Use(hp)
	root.Use(&healthyPlugin{name: "db", err: nil})

	child := app.NewModule("sub")
	child.Use(&healthyPlugin{name: "cache", err: nil})
	root.Use(child)

	if err := hp.Configure(context.Background(), root); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	hp.Start(context.Background(), nil)

	req := httptest.NewRequest("GET", "/_/health", nil)
	resp := hp.Handler()(NewContext(httptest.NewRecorder(), req))
	if resp.Status != 200 {
		t.Errorf("expected 200, got %d", resp.Status)
	}
	body, _ := resp.BodyBytes()
	for _, want := range []string{`"db":"ok"`, `"cache":"ok"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("body missing %s: %s", want, body)
		}
	}
}

func TestHealthPlugin_AutoDiscovery_PreservesAddChecker(t *testing.T) {
	hp := NewHealthPlugin()
	// The explicit checker fails; the auto-discovered probe of the same name is
	// healthy. If the explicit one wins, "db" reports "unavailable" (degraded);
	// if it were overwritten, it would report "ok".
	hp.AddChecker("db", func(_ context.Context) error { return fmt.Errorf("explicit wins") })

	root := app.NewModule("root")
	root.Use(NewServerPlugin(ServerConfig{}))
	root.Use(hp)
	// Auto-discovered probe of the same name should NOT overwrite the explicit one.
	root.Use(&healthyPlugin{name: "db", err: nil})

	if err := hp.Configure(context.Background(), root); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	hp.Start(context.Background(), nil)

	req := httptest.NewRequest("GET", "/_/health", nil)
	resp := hp.Handler()(NewContext(httptest.NewRecorder(), req))
	body, _ := resp.BodyBytes()
	if !strings.Contains(string(body), `"db":"unavailable"`) {
		t.Errorf("AddChecker entry (failing) should win over auto-discovered (healthy): %s", body)
	}
}

func TestHealthPlugin_AutoDiscovery_DegradedFromChildModule(t *testing.T) {
	hp := NewHealthPlugin()
	root := app.NewModule("root")
	root.Use(NewServerPlugin(ServerConfig{}))
	root.Use(hp)

	child := app.NewModule("sub")
	child.Use(&healthyPlugin{name: "cache", err: fmt.Errorf("connection refused")})
	root.Use(child)

	if err := hp.Configure(context.Background(), root); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	hp.Start(context.Background(), nil)

	req := httptest.NewRequest("GET", "/_/health", nil)
	resp := hp.Handler()(NewContext(httptest.NewRecorder(), req))
	if resp.Status != 503 {
		t.Errorf("expected 503 for degraded, got %d", resp.Status)
	}
	body, _ := resp.BodyBytes()
	if !strings.Contains(string(body), `"status":"degraded"`) {
		t.Errorf("expected degraded status, got %s", body)
	}
}

func TestHealthPlugin_Configure_FromNestedModule_WalksToRoot(t *testing.T) {
	// Confirms Root() resolution: HealthPlugin registered in a child module
	// still sees siblings under the root via owner.Root().CollectPlugins().
	hp := NewHealthPlugin()
	root := app.NewModule("root")
	root.Use(NewServerPlugin(ServerConfig{}))
	root.Use(&healthyPlugin{name: "db", err: nil})

	child := app.NewModule("api")
	child.Use(hp)
	root.Use(child)

	if err := hp.Configure(context.Background(), child); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	hp.Start(context.Background(), nil)

	req := httptest.NewRequest("GET", "/_/health", nil)
	resp := hp.Handler()(NewContext(httptest.NewRecorder(), req))
	body, _ := resp.BodyBytes()
	if !strings.Contains(string(body), `"db":"ok"`) {
		t.Errorf("expected root-level probe to be discovered from child module: %s", body)
	}
}

// contributingPlugin registers a HealthChecker via app.Contribute during its
// own Configure — the case interface-implementation can't express (one plugin,
// many probes) and the case that exposed the ordering bug.
type contributingPlugin struct {
	probe string
}

func (c *contributingPlugin) Name() string { return "contributor" }
func (c *contributingPlugin) Configure(_ context.Context, owner *app.Module) error {
	app.Contribute[app.HealthChecker](owner, &healthyPlugin{name: c.probe})
	return nil
}

func TestHealthPlugin_DiscoversContributionFromLaterConfiguredPlugin(t *testing.T) {
	// Regression: the aggregator is registered (and configured) BEFORE the
	// plugin that contributes a probe. Discovery must still see it. Pre-fix,
	// discovery ran inline in HealthPlugin.Configure and missed contributions
	// made by plugins configured afterwards.
	hp := NewHealthPlugin()
	// The test calls the handler itself, so the application needs no server.
	handler := hp.Handler()
	a := app.New("t")
	a.Use(hp)                                       // configured first
	a.Use(&contributingPlugin{probe: "late-probe"}) // contributes during its Configure, after hp
	a.Run(func(context.Context) error { return nil })
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	req := httptest.NewRequest("GET", "/_/health", nil)
	resp := handler(NewContext(httptest.NewRecorder(), req))
	body, _ := resp.BodyBytes()
	if !strings.Contains(string(body), `"late-probe":"ok"`) {
		t.Errorf("probe contributed after the aggregator's Configure should still be discovered: %s", body)
	}
}

// --- Context coverage ---

func TestContext_Host(t *testing.T) {
	req := httptest.NewRequest("GET", "http://example.com/path", nil)
	ctx := NewContext(httptest.NewRecorder(), req)
	if ctx.Host() != "example.com" {
		t.Errorf("Host() = %q, want example.com", ctx.Host())
	}
}

func TestContext_ContentType(t *testing.T) {
	req := httptest.NewRequest("POST", "/", nil)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	ctx := NewContext(httptest.NewRecorder(), req)
	if ct := ctx.ContentType(); ct != "application/json" {
		t.Errorf("ContentType() = %q, want application/json", ct)
	}
}

func TestContext_ContentType_Empty(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	ctx := NewContext(httptest.NewRecorder(), req)
	if ct := ctx.ContentType(); ct != "" {
		t.Errorf("ContentType() = %q, want empty", ct)
	}
}

func TestContext_Accept(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept", "text/html")
	ctx := NewContext(httptest.NewRecorder(), req)
	if a := ctx.Accept(); a != "text/html" {
		t.Errorf("Accept() = %q, want text/html", a)
	}
}

func TestContext_WithContext(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	ctx := NewContext(httptest.NewRecorder(), req)

	type ctxKey struct{}
	newCtx := context.WithValue(ctx.Context(), ctxKey{}, "test-value")
	ctx2 := ctx.WithContext(newCtx)

	if ctx2.Context().Value(ctxKey{}) != "test-value" {
		t.Error("WithContext did not preserve value")
	}
	// Original should not be modified
	if ctx.Context().Value(ctxKey{}) != nil {
		t.Error("original context was modified")
	}
}

func TestNewContextCarriesInboundBearerOnlyInRequestScope(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Authorization", "Bearer inbound-user-token")
	ctx := NewContext(httptest.NewRecorder(), request)
	if token, ok := ForwardedBearerTokenFromContext(ctx.Context()); !ok || token != "inbound-user-token" {
		t.Fatalf("forwarded bearer = %q, %v", token, ok)
	}
	without := NewContext(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if token, ok := ForwardedBearerTokenFromContext(without.Context()); ok || token != "" {
		t.Fatalf("bearer leaked between requests: %q, %v", token, ok)
	}
}

func TestContext_QueryParams(t *testing.T) {
	req := httptest.NewRequest("GET", "/search?q=test&page=2&q=other", nil)
	ctx := NewContext(httptest.NewRecorder(), req)

	params := ctx.QueryParams()
	if len(params["q"]) != 2 {
		t.Errorf("expected 2 values for q, got %d", len(params["q"]))
	}

	// Second call should return cached result
	params2 := ctx.QueryParams()
	if params2.Get("page") != "2" {
		t.Errorf("cached QueryParams page = %q, want 2", params2.Get("page"))
	}
}

func TestContext_Param_NilParams(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	ctx := NewContext(httptest.NewRecorder(), req)
	if v := ctx.Param("id"); v != "" {
		t.Errorf("Param on nil params = %q, want empty", v)
	}
}

func TestContext_RawBody(t *testing.T) {
	body := strings.NewReader(`raw body content`)
	req := httptest.NewRequest("POST", "/", body)
	ctx := NewContext(httptest.NewRecorder(), req)

	data, err := ctx.RawBody()
	if err != nil {
		t.Fatalf("RawBody: %v", err)
	}
	if string(data) != "raw body content" {
		t.Errorf("RawBody = %q, want 'raw body content'", data)
	}
}

func TestContext_SetHeader(t *testing.T) {
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	ctx := NewContext(w, req)

	ctx.SetHeader("X-Custom", "value")
	if w.Header().Get("X-Custom") != "value" {
		t.Errorf("SetHeader did not set header")
	}
}
