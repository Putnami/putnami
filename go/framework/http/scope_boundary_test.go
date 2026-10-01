package http

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"go.putnami.dev/inject"
	"go.putnami.dev/protocol/features/spectest"
)

// boundaryProbe records how the request-scope boundary finalized the scoped
// probeFinalizer materialized during a request. A shared probe lets the test
// observe the outcome even though the finalizer instance is per-request scoped.
type boundaryProbe struct {
	mu         sync.Mutex
	finalized  int
	lastErr    error
	hadOutcome bool
	// finalizeErr is what FinalizeScope returns — a stand-in for a commit that
	// fails at the request boundary after the handler already succeeded.
	finalizeErr error
}

func (p *boundaryProbe) record(outcome error) {
	p.mu.Lock()
	p.finalized++
	p.lastErr = outcome
	p.hadOutcome = true
	p.mu.Unlock()
}

func (p *boundaryProbe) snapshot() (finalized int, outcome error, hadOutcome bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.finalized, p.lastErr, p.hadOutcome
}

// probeFinalizer is a scoped value implementing inject.ScopeFinalizer; the
// request-scope boundary drives it, recording the outcome into the shared probe.
type probeFinalizer struct{ probe *boundaryProbe }

func (f *probeFinalizer) FinalizeScope(_ context.Context, outcome error) error {
	f.probe.record(outcome)
	return f.probe.finalizeErr
}

// newBoundaryServer wires a ServerPlugin with a DI container that registers the
// Scoped probeFinalizer, plus a single GET /probe route whose injected handler
// materializes it. The returned plugin is finalized via TestServer by the caller.
func newBoundaryServer(t *testing.T, probe *boundaryProbe, handler any) *ServerPlugin {
	t.Helper()
	cc := inject.NewContainerContext("http-boundary")
	if err := cc.Register(inject.Provide(
		inject.TokenOf[*probeFinalizer](),
		func(_ inject.Resolver) (any, error) { return &probeFinalizer{probe: probe}, nil },
		inject.WithScope(inject.Scoped),
	)); err != nil {
		t.Fatal(err)
	}
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })

	p := NewServerPlugin(ServerConfig{Port: 0})
	p.setContainerContext(cc)
	p.GET("/probe", Inject(handler))
	return p
}

// TestRequestScopeBoundary_CommitsOnSuccess verifies a 2xx handler finalizes the
// request scope with a nil (success) outcome.
func TestRequestScopeBoundary_CommitsOnSuccess(t *testing.T) {
	spectest.Proves(t, "go/http-services", "request-scope", "scope-commits-on-success")
	probe := &boundaryProbe{}
	p := newBoundaryServer(t, probe, func(_ *probeFinalizer, _ *Context) *Response {
		return JSON(map[string]string{"ok": "yes"})
	})
	ts := p.TestServer()
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/probe")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	finalized, outcome, had := probe.snapshot()
	if finalized != 1 {
		t.Fatalf("finalized = %d, want 1", finalized)
	}
	if !had || outcome != nil {
		t.Errorf("expected a nil (commit) outcome, got %v", outcome)
	}
}

// TestRequestScopeBoundary_RollsBackOnServerError verifies a 5xx response
// finalizes the request scope with a non-nil (rollback) outcome.
func TestRequestScopeBoundary_RollsBackOnServerError(t *testing.T) {
	spectest.Proves(t, "go/http-services", "request-scope", "scope-rolls-back-on-server-error")
	probe := &boundaryProbe{}
	p := newBoundaryServer(t, probe, func(_ *probeFinalizer, _ *Context) *Response {
		return NewResponse(http.StatusInternalServerError)
	})
	ts := p.TestServer()
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/probe")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 500 {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}

	finalized, outcome, _ := probe.snapshot()
	if finalized != 1 {
		t.Fatalf("finalized = %d, want 1", finalized)
	}
	if outcome == nil {
		t.Error("expected a non-nil (rollback) outcome for a 5xx response")
	}
}

// TestRequestScopeBoundary_CommitsOnClientError verifies a 4xx response is a
// handled outcome that commits — a handler that wants a 4xx to roll back uses the
// rollback-only escape hatch, not the status code.
func TestRequestScopeBoundary_CommitsOnClientError(t *testing.T) {
	spectest.Proves(t, "go/http-services", "request-scope", "scope-commits-on-client-error")
	probe := &boundaryProbe{}
	p := newBoundaryServer(t, probe, func(_ *probeFinalizer, _ *Context) *Response {
		return NewResponse(http.StatusConflict) // 409
	})
	ts := p.TestServer()
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/probe")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 409 {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}

	finalized, outcome, had := probe.snapshot()
	if finalized != 1 {
		t.Fatalf("finalized = %d, want 1", finalized)
	}
	if !had || outcome != nil {
		t.Errorf("expected a nil (commit) outcome for a 4xx, got %v", outcome)
	}
}

// TestRequestScopeBoundary_RollsBackOnPanic verifies a handler panic — with no
// Recovery middleware to convert it — rolls the request scope back before the
// panic propagates, so no transaction/connection leaks.
func TestRequestScopeBoundary_RollsBackOnPanic(t *testing.T) {
	spectest.Proves(t, "go/http-services", "request-scope", "scope-rolls-back-on-panic")
	probe := &boundaryProbe{}
	p := newBoundaryServer(t, probe, func(_ *probeFinalizer, _ *Context) *Response {
		panic("boom")
	})
	ts := p.TestServer()
	defer ts.Close()

	// No Recovery middleware is registered, so after the boundary rolls back it
	// re-panics to preserve the existing behavior: net/http aborts the connection
	// per request. The client sees a transport error (EOF), which is expected —
	// what matters is that the scope was still finalized (rolled back) first.
	resp, err := http.Get(ts.URL + "/probe")
	if err == nil {
		resp.Body.Close()
	}

	finalized, outcome, _ := probe.snapshot()
	if finalized != 1 {
		t.Fatalf("finalized = %d, want 1 (panic must still finalize)", finalized)
	}
	if outcome == nil {
		t.Error("expected a non-nil (rollback) outcome on panic")
	}
}

// TestRequestScopeBoundary_RollsBackWithRecoveryMiddleware verifies the same
// rollback happens when a Recovery middleware converts the panic to a 500 before
// it reaches the boundary — the boundary then sees a 5xx and rolls back.
func TestRequestScopeBoundary_RollsBackWithRecoveryMiddleware(t *testing.T) {
	spectest.Proves(t, "go/http-services", "request-scope", "scope-rolls-back-under-recovery-middleware")
	probe := &boundaryProbe{}
	p := newBoundaryServer(t, probe, func(_ *probeFinalizer, _ *Context) *Response {
		panic("boom")
	})
	p.Use(Recovery())
	ts := p.TestServer()
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/probe")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 500 {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}

	finalized, outcome, _ := probe.snapshot()
	if finalized != 1 {
		t.Fatalf("finalized = %d, want 1", finalized)
	}
	if outcome == nil {
		t.Error("expected a non-nil (rollback) outcome when Recovery converts the panic to 500")
	}
}

// TestRequestScopeBoundary_FailedCommitDowngradesToSanitized500 covers the
// clause that keeps the boundary honest: the handler returned 200, but
// committing its unit of work failed, so the client must not be told the write
// succeeded. The response is replaced with a 500, and the underlying error text
// — which can name schemas, constraints or connection strings — must not reach
// the body.
func TestRequestScopeBoundary_FailedCommitDowngradesToSanitized500(t *testing.T) {
	spectest.Proves(t, "go/http-services", "request-scope", "failed-commit-downgrades-to-a-sanitized-500")

	const secret = "pq: duplicate key value violates unique constraint \"users_email_key\""
	probe := &boundaryProbe{finalizeErr: errors.New(secret)}
	p := newBoundaryServer(t, probe, func(_ *probeFinalizer, _ *Context) *Response {
		return JSON(map[string]string{"ok": "yes"})
	})
	ts := p.TestServer()
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/probe")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (a failed commit must not report success)", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), secret) {
		t.Errorf("commit error text leaked to the client: %s", body)
	}
	if strings.Contains(string(body), "users_email_key") {
		t.Errorf("constraint name leaked to the client: %s", body)
	}

	// The boundary still finalized exactly once, with the handler's success as
	// the outcome it tried to commit.
	finalized, outcome, had := probe.snapshot()
	if finalized != 1 {
		t.Fatalf("finalized = %d, want 1", finalized)
	}
	if !had || outcome != nil {
		t.Errorf("outcome = %v, want nil (the handler did succeed; the commit is what failed)", outcome)
	}
}
