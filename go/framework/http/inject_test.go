package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.putnami.dev/errors"
	"go.putnami.dev/inject"
)

type testService struct {
	Name string
}

func newTestService() *testService {
	return &testService{Name: "test-svc"}
}

func TestInject_BasicHandler(t *testing.T) {
	ih := Inject(func(svc *testService, ctx *Context) *Response {
		return JSON(map[string]string{"name": svc.Name})
	})

	cc := inject.NewContainerContext("test")
	if err := cc.Register(inject.AutoProvide(newTestService)); err != nil {
		t.Fatal(err)
	}
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	if err := ih.Finalize(cc); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	ctx := NewContext(w, r)

	resp := ih.Handle(ctx)
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
	if resp.Status != 200 {
		t.Fatalf("expected 200, got %d", resp.Status)
	}
}

func TestServerPlugin_AddPendingInjectedHandler_FinalizesAfterConfigure(t *testing.T) {
	// Reproduces the Use(httpServer).Use(apiPlugin) order: the http server's Configure
	// has already run when api.Plugin.Configure forwards a fresh InjectedHandler. The
	// hook must finalize it immediately, otherwise the first request panics with
	// "handler called before DI finalization".
	cc := inject.NewContainerContext("late-finalize")
	if err := cc.Register(inject.AutoProvide(newTestService)); err != nil {
		t.Fatal(err)
	}
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cc.Close() }()

	server := NewServerPlugin(ServerConfig{})
	server.setContainerContext(cc) // simulates a Configure pass that already happened

	ih := Inject(func(svc *testService, ctx *Context) *Response {
		return JSON(map[string]string{"name": svc.Name})
	})

	if err := server.AddPendingInjectedHandler(ih); err != nil {
		t.Fatalf("AddPendingInjectedHandler returned error: %v", err)
	}

	// Handler must be ready to serve immediately, no panic.
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	resp := ih.Handle(NewContext(w, r))
	if resp == nil || resp.Status != 200 {
		t.Fatalf("late-bound handler did not finalize: resp=%v", resp)
	}
}

func TestServerPlugin_AddPendingInjectedHandler_QueuesBeforeConfigure(t *testing.T) {
	// Before Configure, no container is set; AddPendingInjectedHandler must queue the
	// handler without finalizing. Configure then drains the queue.
	server := NewServerPlugin(ServerConfig{})

	ih := Inject(func(svc *testService, ctx *Context) *Response {
		return JSON(map[string]string{"name": svc.Name})
	})

	if err := server.AddPendingInjectedHandler(ih); err != nil {
		t.Fatalf("AddPendingInjectedHandler before Configure: %v", err)
	}
	if ih.finalized {
		t.Fatal("handler should remain unfinalised before Configure")
	}
}

func TestInject_ContextFirst(t *testing.T) {
	ih := Inject(func(ctx *Context, svc *testService) *Response {
		return JSON(map[string]string{"name": svc.Name})
	})

	cc := inject.NewContainerContext("test")
	if err := cc.Register(inject.AutoProvide(newTestService)); err != nil {
		t.Fatal(err)
	}
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	if err := ih.Finalize(cc); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	ctx := NewContext(w, r)

	resp := ih.Handle(ctx)
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
}

func TestInject_MultipleDeps(t *testing.T) {
	type otherService struct{ Value int }

	ih := Inject(func(svc *testService, other *otherService, ctx *Context) *Response {
		return JSON(map[string]any{"name": svc.Name, "value": other.Value})
	})

	cc := inject.NewContainerContext("test")
	if err := cc.Register(inject.AutoProvide(newTestService)); err != nil {
		t.Fatal(err)
	}
	if err := cc.Register(inject.AutoProvide(func() *otherService {
		return &otherService{Value: 42}
	})); err != nil {
		t.Fatal(err)
	}
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	if err := ih.Finalize(cc); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	ctx := NewContext(w, r)

	resp := ih.Handle(ctx)
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
}

func TestInject_NoDeps(t *testing.T) {
	ih := Inject(func(ctx *Context) *Response {
		return JSON(map[string]string{"status": "ok"})
	})

	cc := inject.NewContainerContext("test")
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	if err := ih.Finalize(cc); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	ctx := NewContext(w, r)

	resp := ih.Handle(ctx)
	if resp == nil || resp.Status != 200 {
		t.Fatal("expected 200 response")
	}
}

func TestInject_PanicsOnInvalidSignature(t *testing.T) {
	t.Run("not a function", func(t *testing.T) {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic")
			}
		}()
		Inject("not a function")
	})

	t.Run("wrong return type", func(t *testing.T) {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic")
			}
		}()
		Inject(func(ctx *Context) string { return "bad" })
	})

	t.Run("no context param", func(t *testing.T) {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic")
			}
		}()
		Inject(func(svc *testService) *Response { return nil })
	})
}

func TestInject_PanicsBeforeFinalize(t *testing.T) {
	ih := Inject(func(svc *testService, ctx *Context) *Response {
		return nil
	})

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic before finalization")
		}
	}()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	ctx := NewContext(w, r)
	ih.Handle(ctx)
}

func TestInject_FinalizeErrorOnMissingDep(t *testing.T) {
	ih := Inject(func(svc *testService, ctx *Context) *Response {
		return nil
	})

	cc := inject.NewContainerContext("test")
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	err := ih.Finalize(cc)
	if err == nil {
		t.Fatal("expected error for missing dependency")
	}
	// Resolution failures must carry the typed CodeScope, not a bare error.
	if got := errors.GetCode(err); got != CodeScope {
		t.Errorf("error code = %q, want %q", got, CodeScope)
	}
}

func TestInject_NilCC_ContextOnly(t *testing.T) {
	// Context-only handler should finalize successfully without a DI container.
	ih := Inject(func(ctx *Context) *Response {
		return JSON(map[string]string{"status": "ok"})
	})

	if err := ih.Finalize(nil); err != nil {
		t.Fatalf("context-only handler should finalize with nil cc: %v", err)
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	resp := ih.Handle(NewContext(w, r))
	if resp == nil || resp.Status != 200 {
		t.Fatal("expected 200 response")
	}
}

func TestInject_NilCC_WithDeps_Errors(t *testing.T) {
	// Handler with DI deps should return a clear error when cc is nil.
	ih := Inject(func(svc *testService, ctx *Context) *Response {
		return nil
	})

	err := ih.Finalize(nil)
	if err == nil {
		t.Fatal("expected error for DI deps with nil cc")
	}
	// The no-container failure must carry the typed CodeScope too.
	if got := errors.GetCode(err); got != CodeScope {
		t.Errorf("error code = %q, want %q", got, CodeScope)
	}
}

func TestInject_ServerConfigure_NilCC_ContextOnly(t *testing.T) {
	// Server with context-only Inject handler and no DI container.
	server := NewServerPlugin(ServerConfig{Port: 0})
	server.GET("/test", Inject(func(ctx *Context) *Response {
		return JSON(map[string]string{"status": "ok"})
	}))

	// Configure without setting a container context.
	if err := server.Configure(context.Background(), nil); err != nil {
		t.Fatalf("context-only handler should configure without DI: %v", err)
	}

	// Verify handler works.
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	match := server.routes.lookup("GET", "/test")
	if match == nil {
		t.Fatal("expected route match")
	}
	resp := match.Handlers[0](NewContext(w, r))
	if resp == nil || resp.Status != http.StatusOK {
		t.Fatalf("expected 200, got %v", resp)
	}
}

func TestInject_ServerConfigure_NilCC_WithDeps_Errors(t *testing.T) {
	// Server with DI-dependent Inject handler but no container should
	// return an error at Configure time, not panic at request time.
	server := NewServerPlugin(ServerConfig{Port: 0})
	server.GET("/test", Inject(func(svc *testService, ctx *Context) *Response {
		return nil
	}))

	err := server.Configure(context.Background(), nil)
	if err == nil {
		t.Fatal("expected configure error for DI deps with no container")
	}
}

func TestInject_WithServerPlugin(t *testing.T) {
	server := NewServerPlugin(ServerConfig{Port: 0})
	server.GET("/test", Inject(func(svc *testService, ctx *Context) *Response {
		return JSON(map[string]string{"name": svc.Name})
	}))

	if len(server.pending) != 1 {
		t.Fatalf("expected 1 pending handler, got %d", len(server.pending))
	}
}

func TestInject_ServerConfigure(t *testing.T) {
	server := NewServerPlugin(ServerConfig{Port: 0})
	server.GET("/test", Inject(func(svc *testService, ctx *Context) *Response {
		return JSON(map[string]string{"name": svc.Name})
	}))

	cc := inject.NewContainerContext("test")
	if err := cc.Register(inject.AutoProvide(newTestService)); err != nil {
		t.Fatal(err)
	}
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	server.setContainerContext(cc)

	// Simulate what the app does: call Configure
	if err := server.Configure(context.Background(), nil); err != nil {
		t.Fatal(err)
	}

	// Verify handler works after finalization via the server's routing
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	match := server.routes.lookup("GET", "/test")
	if match == nil {
		t.Fatal("expected route match")
	}
	ctx := NewContext(w, r)
	resp := match.Handlers[0](ctx)
	if resp == nil || resp.Status != http.StatusOK {
		t.Fatalf("expected 200, got %v", resp)
	}
}

// --- Scoped dependency tests ---

// A failed per-request scoped resolution must return a generic 500, not
// leak the internal DI/provider error text to the client.
func TestInject_ScopedResolutionFailureIsSanitized(t *testing.T) {
	scopedSeq = 0
	ih := Inject(func(counter *scopedCounter, ctx *Context) *Response {
		return JSON(map[string]int{"id": counter.ID})
	})

	cc := inject.NewContainerContext("test")
	if err := cc.Register(inject.AutoProvideScoped(newScopedCounter)); err != nil {
		t.Fatal(err)
	}
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	if err := ih.Finalize(cc); err != nil {
		t.Fatal(err)
	}

	// Call without attaching a DI scope to the request context: scoped
	// resolution fails inside Handle.
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	resp := ih.Handle(NewContext(w, r))
	if resp == nil || resp.Status != 500 {
		t.Fatalf("expected a 500 on scoped resolution failure, got %+v", resp)
	}

	if err := resp.WriteTo(w); err != nil {
		t.Fatal(err)
	}
	body := w.Body.String()
	// The internal resolution error (token type, "no active scope") must not leak.
	if strings.Contains(body, "scope") || strings.Contains(body, "scopedCounter") || strings.Contains(body, "registered") {
		t.Errorf("response leaked internal DI error: %s", body)
	}
	if !strings.Contains(body, "Internal Server Error") {
		t.Errorf("expected a generic message, got: %s", body)
	}
}

type scopedCounter struct {
	ID int
}

var scopedSeq int

func newScopedCounter() *scopedCounter {
	scopedSeq++
	return &scopedCounter{ID: scopedSeq}
}

func TestInject_ScopedDep(t *testing.T) {
	scopedSeq = 0

	ih := Inject(func(counter *scopedCounter, ctx *Context) *Response {
		return JSON(map[string]int{"id": counter.ID})
	})

	cc := inject.NewContainerContext("test")
	if err := cc.Register(inject.AutoProvideScoped(newScopedCounter)); err != nil {
		t.Fatal(err)
	}
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	if err := ih.Finalize(cc); err != nil {
		t.Fatal(err)
	}

	// Verify the handler has scoped params
	if !ih.hasScoped {
		t.Fatal("expected hasScoped to be true")
	}

	// Create a scope and call the handler — each scope should get a new counter
	scope1, err := cc.CreateScope()
	if err != nil {
		t.Fatal(err)
	}
	defer scope1.Close()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	r = r.WithContext(scope1.Context(r.Context()))
	resp1 := ih.Handle(NewContext(w, r))
	if resp1 == nil {
		t.Fatal("expected non-nil response")
	}

	scope2, err := cc.CreateScope()
	if err != nil {
		t.Fatal(err)
	}
	defer scope2.Close()

	w2 := httptest.NewRecorder()
	r2 := httptest.NewRequest("GET", "/test", nil)
	r2 = r2.WithContext(scope2.Context(r2.Context()))
	resp2 := ih.Handle(NewContext(w2, r2))
	if resp2 == nil {
		t.Fatal("expected non-nil response")
	}

	// The two responses should have different counter IDs (scoped = new per request)
	if resp1.Status != 200 || resp2.Status != 200 {
		t.Fatalf("expected 200, got %d and %d", resp1.Status, resp2.Status)
	}
}

func TestInject_MixedSingletonAndScoped(t *testing.T) {
	scopedSeq = 0

	ih := Inject(func(svc *testService, counter *scopedCounter, ctx *Context) *Response {
		return JSON(map[string]any{"name": svc.Name, "id": counter.ID})
	})

	cc := inject.NewContainerContext("test")
	if err := cc.Register(inject.AutoProvide(newTestService)); err != nil {
		t.Fatal(err)
	}
	if err := cc.Register(inject.AutoProvideScoped(newScopedCounter)); err != nil {
		t.Fatal(err)
	}
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	if err := ih.Finalize(cc); err != nil {
		t.Fatal(err)
	}

	if !ih.hasScoped {
		t.Fatal("expected hasScoped true for mixed handler")
	}

	// Singleton param should be pre-resolved
	singletonIdx := ih.params[0].index
	if ih.params[0].isScoped {
		t.Fatal("testService should not be scoped")
	}
	if ih.resolved[singletonIdx].IsZero() {
		t.Fatal("singleton should be pre-resolved")
	}

	// Scoped param should be a zero placeholder
	scopedIdx := ih.params[1].index
	if !ih.params[1].isScoped {
		t.Fatal("scopedCounter should be scoped")
	}
	if !ih.resolved[scopedIdx].IsZero() {
		t.Fatal("scoped should be zero placeholder before request")
	}

	// Call with a scope
	scope, err := cc.CreateScope()
	if err != nil {
		t.Fatal(err)
	}
	defer scope.Close()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	r = r.WithContext(scope.Context(r.Context()))
	resp := ih.Handle(NewContext(w, r))
	if resp == nil || resp.Status != 200 {
		t.Fatal("expected 200 response")
	}
}

func TestInject_ScopedViaWithContext(t *testing.T) {
	// Mirrors the real server path: scope is set via ctx.WithContext(),
	// NOT by modifying the HTTP request context directly.
	scopedSeq = 0

	ih := Inject(func(counter *scopedCounter, ctx *Context) *Response {
		return JSON(map[string]int{"id": counter.ID})
	})

	cc := inject.NewContainerContext("test")
	if err := cc.Register(inject.AutoProvideScoped(newScopedCounter)); err != nil {
		t.Fatal(err)
	}
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	if err := ih.Finalize(cc); err != nil {
		t.Fatal(err)
	}

	scope, err := cc.CreateScope()
	if err != nil {
		t.Fatal(err)
	}
	defer scope.Close()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	// Create context with plain request (no scope on r.Context())
	ctx := NewContext(w, r)
	// Attach scope via WithContext — this is what server.go does
	ctx = ctx.WithContext(scope.Context(r.Context()))

	resp := ih.Handle(ctx)
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
	if resp.Status != 200 {
		t.Fatalf("expected 200, got %d", resp.Status)
	}
}

func TestInject_ScopedWithoutScope_ReturnsError(t *testing.T) {
	scopedSeq = 0

	ih := Inject(func(counter *scopedCounter, ctx *Context) *Response {
		return JSON(map[string]int{"id": counter.ID})
	})

	cc := inject.NewContainerContext("test")
	if err := cc.Register(inject.AutoProvideScoped(newScopedCounter)); err != nil {
		t.Fatal(err)
	}
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	if err := ih.Finalize(cc); err != nil {
		t.Fatal(err)
	}

	// Call WITHOUT a scope in context — should return 500 error response, not panic
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	resp := ih.Handle(NewContext(w, r))
	if resp == nil {
		t.Fatal("expected error response, got nil")
	}
	if resp.Status != 500 {
		t.Fatalf("expected 500, got %d", resp.Status)
	}
}

func TestInject_FinalizeIdempotent_SameContainer(t *testing.T) {
	ih := Inject(func(svc *testService, ctx *Context) *Response {
		return JSON(map[string]string{"name": svc.Name})
	})

	cc := inject.NewContainerContext("test")
	if err := cc.Register(inject.AutoProvide(newTestService)); err != nil {
		t.Fatal(err)
	}
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	// Finalize twice with the same container — should be a no-op the second time
	if err := ih.Finalize(cc); err != nil {
		t.Fatal(err)
	}
	if err := ih.Finalize(cc); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	resp := ih.Handle(NewContext(w, r))
	if resp == nil || resp.Status != 200 {
		t.Fatal("expected 200")
	}
}

func TestInject_FinalizeReFinalizesWithNewContainer(t *testing.T) {
	ih := Inject(func(svc *testService, ctx *Context) *Response {
		return JSON(map[string]string{"name": svc.Name})
	})

	// First container (simulates Validate)
	cc1 := inject.NewContainerContext("test1")
	if err := cc1.Register(inject.AutoProvide(newTestService)); err != nil {
		t.Fatal(err)
	}
	if err := cc1.Start(); err != nil {
		t.Fatal(err)
	}
	if err := ih.Finalize(cc1); err != nil {
		t.Fatal(err)
	}
	cc1.Close()

	// Second container (simulates Start after Validate)
	cc2 := inject.NewContainerContext("test2")
	if err := cc2.Register(inject.AutoProvide(newTestService)); err != nil {
		t.Fatal(err)
	}
	if err := cc2.Start(); err != nil {
		t.Fatal(err)
	}
	defer cc2.Close()

	// Re-finalize with new container — should re-resolve deps
	if err := ih.Finalize(cc2); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	resp := ih.Handle(NewContext(w, r))
	if resp == nil || resp.Status != 200 {
		t.Fatal("expected 200 with re-finalized container")
	}
}
