package http

import (
	"context"
	"net/http/httptest"
	"testing"

	"go.putnami.dev/logger"

	"go.putnami.dev/protocol/features/spectest"
)

// --- RateLimit trusted-proxy keying (exact IP and CIDR) ---

func keyForRequest(t *testing.T, keyFn func(*Context) string, remoteAddr, xff string) string {
	t.Helper()
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = remoteAddr
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	return keyFn(NewContext(httptest.NewRecorder(), req))
}

func TestRateLimitKey_ExactTrustedProxy(t *testing.T) {
	keyFn := buildRateLimitKeyFunc([]string{"10.0.0.1"})

	// From the trusted proxy: key on the XFF client IP.
	if got := keyForRequest(t, keyFn, "10.0.0.1:5555", "203.0.113.7, 10.0.0.1"); got != "203.0.113.7" {
		t.Errorf("trusted proxy: key = %q, want 203.0.113.7", got)
	}
	// From an untrusted address: ignore XFF, key on RemoteAddr.
	if got := keyForRequest(t, keyFn, "192.168.1.9:5555", "203.0.113.7"); got != "192.168.1.9" {
		t.Errorf("untrusted: key = %q, want 192.168.1.9", got)
	}
}

func TestRateLimitKey_CIDRTrustedProxy(t *testing.T) {
	keyFn := buildRateLimitKeyFunc([]string{"10.0.0.0/8"})

	// RemoteAddr inside the CIDR range is trusted → key on XFF.
	if got := keyForRequest(t, keyFn, "10.4.5.6:7777", "198.51.100.23"); got != "198.51.100.23" {
		t.Errorf("CIDR proxy: key = %q, want 198.51.100.23", got)
	}
	// RemoteAddr outside the CIDR range is not trusted → key on RemoteAddr.
	if got := keyForRequest(t, keyFn, "172.16.0.1:7777", "198.51.100.23"); got != "172.16.0.1" {
		t.Errorf("outside CIDR: key = %q, want 172.16.0.1", got)
	}
}

func TestRateLimitKey_IPv6CIDRTrustedProxy(t *testing.T) {
	keyFn := buildRateLimitKeyFunc([]string{"2001:db8::/32"})
	if got := keyForRequest(t, keyFn, "[2001:db8::1]:9999", "198.51.100.5"); got != "198.51.100.5" {
		t.Errorf("IPv6 CIDR proxy: key = %q, want 198.51.100.5", got)
	}
}

func TestRateLimitKey_NoTrustedProxiesIgnoresXFF(t *testing.T) {
	keyFn := buildRateLimitKeyFunc(nil)
	if got := keyForRequest(t, keyFn, "203.0.113.9:1234", "1.2.3.4"); got != "203.0.113.9" {
		t.Errorf("no trusted proxies: key = %q, want 203.0.113.9 (RemoteAddr)", got)
	}
}

// --- RequestID generation and propagation ---

func TestRequestID_GeneratesWhenAbsent(t *testing.T) {
	mw := RequestID()
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	ctx := NewContext(w, req)

	mw(ctx, func() *Response { return JSON("ok") })

	if ctx.RequestID == "" {
		t.Fatal("expected a generated RequestID on the context")
	}
	if w.Header().Get("X-Request-ID") != ctx.RequestID {
		t.Errorf("response X-Request-ID = %q, want %q", w.Header().Get("X-Request-ID"), ctx.RequestID)
	}
}

func TestRequestID_PropagatesInbound(t *testing.T) {
	mw := RequestID()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Request-ID", "abc-123")
	w := httptest.NewRecorder()
	ctx := NewContext(w, req)

	mw(ctx, func() *Response { return JSON("ok") })

	if ctx.RequestID != "abc-123" {
		t.Errorf("RequestID = %q, want abc-123", ctx.RequestID)
	}
	if w.Header().Get("X-Request-ID") != "abc-123" {
		t.Errorf("X-Request-ID header = %q, want abc-123", w.Header().Get("X-Request-ID"))
	}
}

func TestRequestID_FallsBackToCloudTraceHeader(t *testing.T) {
	mw := RequestID()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Cloud-Trace-Context", "trace/42")
	w := httptest.NewRecorder()
	ctx := NewContext(w, req)

	mw(ctx, func() *Response { return JSON("ok") })

	if ctx.RequestID != "trace/42" {
		t.Errorf("RequestID = %q, want trace/42", ctx.RequestID)
	}
}

func TestGenerateRequestID_Unique(t *testing.T) {
	first := generateRequestID()
	second := generateRequestID()
	if first == second {
		t.Error("expected generateRequestID to produce unique values")
	}
}

// TestRequestID_InjectsIntoContext asserts the correlation ID is retrievable
// from the downstream context.Context — the bridge the context-aware logger
// reads via logger.TraceIDFromContext — not just from ctx.RequestID and the
// response header.
func TestRequestID_InjectsIntoContext(t *testing.T) {
	mw := RequestID()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Request-ID", "ctx-corr-1")
	ctx := NewContext(httptest.NewRecorder(), req)

	var fromCtx string
	mw(ctx, func() *Response {
		// next() runs with the mutated context, exactly as a handler would.
		fromCtx = RequestIDFromContext(ctx.Context())
		return JSON("ok")
	})

	if fromCtx != "ctx-corr-1" {
		t.Errorf("RequestIDFromContext(downstream) = %q, want ctx-corr-1", fromCtx)
	}
	// The same value must remain readable on the context after the chain too.
	if got := RequestIDFromContext(ctx.Context()); got != "ctx-corr-1" {
		t.Errorf("RequestIDFromContext(after) = %q, want ctx-corr-1", got)
	}
}

// TestRequestID_GeneratedIDInContext ensures the generated (header-absent) ID is
// also propagated through the context and matches ctx.RequestID byte-for-byte.
func TestRequestID_GeneratedIDInContext(t *testing.T) {
	mw := RequestID()
	ctx := NewContext(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))

	var fromCtx string
	mw(ctx, func() *Response {
		fromCtx = RequestIDFromContext(ctx.Context())
		return JSON("ok")
	})

	if ctx.RequestID == "" {
		t.Fatal("expected a generated RequestID")
	}
	if fromCtx != ctx.RequestID {
		t.Errorf("context request ID = %q, want %q (ctx.RequestID)", fromCtx, ctx.RequestID)
	}
}

// TestRequestIDFromContext_Empty documents that a context without a request ID
// yields "" (so the logger bridge is a safe no-op when RequestID is not used).
func TestRequestIDFromContext_Empty(t *testing.T) {
	if got := RequestIDFromContext(context.Background()); got != "" {
		t.Errorf("RequestIDFromContext(empty) = %q, want \"\"", got)
	}
}

// TestRequestID_BridgesToContextAwareLogger asserts the end-to-end correlation
// goal of the fix: a context-aware log emitted downstream of RequestID() picks
// up the request ID as its TraceID via logger.TraceIDFromContext. It exercises
// whatever extractor is installed (the package-default bridge unless a richer
// one was registered), so it stays valid under both wirings.
func TestRequestID_BridgesToContextAwareLogger(t *testing.T) {
	if logger.TraceIDFromContext == nil {
		t.Skip("no logger.TraceIDFromContext extractor installed")
	}

	mw := RequestID()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Request-ID", "trace-bridge-9")
	ctx := NewContext(httptest.NewRecorder(), req)

	var traceID string
	mw(ctx, func() *Response {
		traceID = logger.TraceIDFromContext(ctx.Context())
		return JSON("ok")
	})

	if traceID != "trace-bridge-9" {
		t.Errorf("logger.TraceIDFromContext = %q, want trace-bridge-9", traceID)
	}
}

// --- Chain composition ---

func TestChain_Order(t *testing.T) {
	spectest.Proves(t, "go/http-services", "middleware-order", "middleware-runs-in-registration-order")
	var order []string
	mw := func(name string) Middleware {
		return func(ctx *Context, next func() *Response) *Response {
			order = append(order, "pre:"+name)
			resp := next()
			order = append(order, "post:"+name)
			return resp
		}
	}

	handler := Chain(mw("a"), mw("b"))(func(_ *Context) *Response {
		order = append(order, "handler")
		return JSON("ok")
	})

	req := httptest.NewRequest("GET", "/", nil)
	handler(NewContext(httptest.NewRecorder(), req))

	want := []string{"pre:a", "pre:b", "handler", "post:b", "post:a"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

func TestChain_ShortCircuit(t *testing.T) {
	spectest.Proves(t, "go/http-services", "middleware-order", "middleware-that-skips-next-short-circuits")
	handlerCalled := false
	block := func(_ *Context, _ func() *Response) *Response {
		return Forbidden() // never calls next()
	}
	handler := Chain(block)(func(_ *Context) *Response {
		handlerCalled = true
		return JSON("ok")
	})

	resp := handler(NewContext(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil)))
	if handlerCalled {
		t.Error("handler should not run when middleware short-circuits")
	}
	if resp.Status != 403 {
		t.Errorf("status = %d, want 403", resp.Status)
	}
}

// BenchmarkChain locks in that the per-route composition runs once: the
// composed handler is built outside the loop, and only the request invocation
// is measured.
func BenchmarkChain(b *testing.B) {
	noop := func(ctx *Context, next func() *Response) *Response { return next() }
	composed := Chain(noop, noop, noop)(func(_ *Context) *Response { return JSON("ok") })
	req := httptest.NewRequest("GET", "/", nil)
	ctx := NewContext(httptest.NewRecorder(), req)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = composed(ctx)
	}
}
