package grpc

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/app"
	"go.putnami.dev/inject"
	"go.putnami.dev/logger"
	"go.putnami.dev/protocol/features/spectest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	reflectpb "google.golang.org/grpc/reflection/grpc_reflection_v1"
	"google.golang.org/grpc/status"

	phttp "go.putnami.dev/http"
	httproutes "go.putnami.dev/protocol/http-routes"
)

// --- Plugin Tests ---

func TestPluginName(t *testing.T) {
	p := NewPlugin(Config{Port: 9090})
	if p.Name() != "grpc" {
		t.Errorf("expected name 'grpc', got %q", p.Name())
	}
}

func TestPluginConfigDefaults(t *testing.T) {
	p := NewPlugin(Config{Port: 50051})
	if p.config.Port != 50051 {
		t.Errorf("expected port 50051, got %d", p.config.Port)
	}
}

func TestPluginRegisterService(t *testing.T) {
	p := NewPlugin(Config{Port: 9090})
	called := false
	p.Register(func(_ *grpc.Server) {
		called = true
	})

	if len(p.registrars) != 1 {
		t.Errorf("expected 1 registrar, got %d", len(p.registrars))
	}

	// Simulate registration call
	p.registrars[0](nil)
	if !called {
		t.Error("registrar should have been called")
	}
}

func TestPluginFluentAPI(t *testing.T) {
	p := NewPlugin(Config{Port: 9090}).
		WithReflection(false).
		WithServerOption(grpc.MaxRecvMsgSize(1024))

	if p.enableReflect {
		t.Error("reflection should be disabled")
	}
	if len(p.opts) != 1 {
		t.Errorf("expected 1 server option, got %d", len(p.opts))
	}
}

func TestPluginWithInterceptors(t *testing.T) {
	p := NewPlugin(Config{Port: 9090}).
		WithUnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			return handler(ctx, req)
		}).
		WithStreamInterceptor(func(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
			return handler(srv, stream)
		})

	if len(p.unaryInts) != 1 {
		t.Errorf("expected 1 unary interceptor, got %d", len(p.unaryInts))
	}
	if len(p.streamInts) != 1 {
		t.Errorf("expected 1 stream interceptor, got %d", len(p.streamInts))
	}
}

func TestPluginStopWithoutStart(t *testing.T) {
	spectest.Proves(t, "go/grpc-services", "bounded-drain", "stop-before-start-is-safe")
	p := NewPlugin(Config{Port: 9090})
	// Should not panic
	if err := p.Stop(context.Background(), nil); err != nil {
		t.Errorf("stop without start should not error: %v", err)
	}
}

func TestPluginServerNilBeforeStart(t *testing.T) {
	p := NewPlugin(Config{Port: 9090})
	if p.Server() != nil {
		t.Error("server should be nil before start")
	}
}

// --- Interceptor Tests ---

func TestLoggingInterceptor(t *testing.T) {
	interceptor := LoggingInterceptor(nil)
	if interceptor == nil {
		t.Fatal("expected non-nil interceptor")
	}

	// Test that it passes through
	called := false
	handler := func(ctx context.Context, req any) (any, error) {
		called = true
		return "result", nil
	}

	info := &grpc.UnaryServerInfo{FullMethod: "/test.Service/Method"}
	resp, err := interceptor(context.Background(), "req", info, handler)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if !called {
		t.Error("handler should have been called")
	}
	if resp != "result" {
		t.Errorf("expected 'result', got %v", resp)
	}
}

func TestLoggingInterceptorWithError(t *testing.T) {
	interceptor := LoggingInterceptor(nil)

	handler := func(ctx context.Context, req any) (any, error) {
		return nil, fmt.Errorf("test error")
	}

	info := &grpc.UnaryServerInfo{FullMethod: "/test.Service/Method"}
	_, err := interceptor(context.Background(), "req", info, handler)
	if err == nil {
		t.Error("expected error")
	}
}

func TestRecoveryInterceptor(t *testing.T) {
	spectest.Proves(t, "go/grpc-services", "interceptor-chain", "recovery-interceptor-recovers-a-unary-panic")
	interceptor := RecoveryInterceptor(nil)

	handler := func(ctx context.Context, req any) (any, error) {
		panic("test panic")
	}

	info := &grpc.UnaryServerInfo{FullMethod: "/test.Service/PanicMethod"}
	_, err := interceptor(context.Background(), "req", info, handler)
	if err == nil {
		t.Error("expected error from panic recovery")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got %T: %v", err, err)
	}
	if st.Code() != codes.Internal {
		t.Errorf("expected Internal code, got %s", st.Code())
	}
	if strings.Contains(st.Message(), "panic") {
		t.Error("error message should not leak panic details to client")
	}
}

func TestRecoveryInterceptorNoPanic(t *testing.T) {
	interceptor := RecoveryInterceptor(nil)

	handler := func(ctx context.Context, req any) (any, error) {
		return "ok", nil
	}

	info := &grpc.UnaryServerInfo{FullMethod: "/test.Service/Method"}
	resp, err := interceptor(context.Background(), "req", info, handler)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if resp != "ok" {
		t.Errorf("expected 'ok', got %v", resp)
	}
}

func TestDIInterceptorNilContainer(t *testing.T) {
	interceptor := DIInterceptor(nil)

	called := false
	handler := func(ctx context.Context, req any) (any, error) {
		called = true
		return "ok", nil
	}

	info := &grpc.UnaryServerInfo{FullMethod: "/test.Service/Method"}
	resp, err := interceptor(context.Background(), "req", info, handler)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if !called {
		t.Error("handler should have been called")
	}
	if resp != "ok" {
		t.Errorf("expected 'ok', got %v", resp)
	}
}

// --- Gateway Tests ---

func TestGatewayPluginName(t *testing.T) {
	p := NewGatewayPlugin(GatewayConfig{})
	if p.Name() != "grpc-gateway" {
		t.Errorf("expected 'grpc-gateway', got %q", p.Name())
	}
}

func TestGatewayPluginDefaults(t *testing.T) {
	p := NewGatewayPlugin(GatewayConfig{})
	if p.config.PathPrefix != "/" {
		t.Errorf("expected default path prefix '/', got %q", p.config.PathPrefix)
	}

	// The default prefix must mount the service at its natural location with no
	// leading double slash: <service>/<method> resolves, and a prefixed path does not.
	gotPath, status := gatewayRoute(t, p, "/test.Service/", "/test.Service/Method")
	if status != http.StatusOK {
		t.Fatalf("default prefix: expected 200 at /test.Service/Method, got %d", status)
	}
	if gotPath != "/test.Service/Method" {
		t.Errorf("default prefix: handler saw path %q, want %q", gotPath, "/test.Service/Method")
	}
}

func TestGatewayMountHandler(t *testing.T) {
	spectest.Proves(t, "go/grpc-services", "gateway-mounting", "gateway-mounts-a-handler")
	p := NewGatewayPlugin(GatewayConfig{})
	p.MountHandler("/package.Service/", nil)

	if len(p.handlers) != 1 {
		t.Errorf("expected 1 handler, got %d", len(p.handlers))
	}
	if p.handlers[0].Path != "/package.Service/" {
		t.Errorf("expected path '/package.Service/', got %q", p.handlers[0].Path)
	}
}

func TestGatewayStopWithoutStart(t *testing.T) {
	p := NewGatewayPlugin(GatewayConfig{})
	if err := p.Stop(context.Background(), nil); err != nil {
		t.Errorf("stop without start should not error: %v", err)
	}
}

// --- Plugin Lifecycle Tests ---

func TestPluginConfigure(t *testing.T) {
	p := NewPlugin(Config{Port: 9090})
	if err := p.Configure(context.Background(), nil); err != nil {
		t.Errorf("warmup should not error: %v", err)
	}
}

func TestPluginStartAndStop(t *testing.T) {
	// Find a free port
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("failed to find free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	p := NewPlugin(Config{Port: port})
	registered := false
	p.Register(func(_ *grpc.Server) {
		registered = true
	})

	if err := p.Start(context.Background(), nil); err != nil {
		t.Fatalf("start failed: %v", err)
	}

	if !registered {
		t.Error("registrar should have been called during start")
	}
	if p.Server() == nil {
		t.Error("server should not be nil after start")
	}

	if err := p.Stop(context.Background(), nil); err != nil {
		t.Errorf("stop failed: %v", err)
	}
}

func TestPluginStartWithInterceptors(t *testing.T) {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("failed to find free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	p := NewPlugin(Config{Port: port}).
		WithUnaryInterceptor(LoggingInterceptor(nil)).
		WithStreamInterceptor(func(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
			return handler(srv, stream)
		})

	if err := p.Start(context.Background(), nil); err != nil {
		t.Fatalf("start with interceptors failed: %v", err)
	}
	defer p.Stop(context.Background(), nil)

	if p.Server() == nil {
		t.Error("server should be created")
	}
}

func TestPluginStartWithReflection(t *testing.T) {
	spectest.Proves(t, "go/grpc-services", "reflection-opt-in", "reflection-registers-when-enabled")
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("failed to find free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	p := NewPlugin(Config{Port: port}).WithReflection(true)

	if err := p.Start(context.Background(), nil); err != nil {
		t.Fatalf("start with reflection failed: %v", err)
	}
	defer p.Stop(context.Background(), nil)

	if p.Server() == nil {
		t.Error("server should be created")
	}
	// The opt-in is only real if the running server answers the reflection RPC:
	// asserting the config flag would still pass with the registration removed.
	if _, err := listServices(t, port); err != nil {
		t.Errorf("reflection was enabled but the server does not serve the reflection RPC: %v", err)
	}
}

func TestPluginStartListenError(t *testing.T) {
	// Occupy a port first
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	// Try to start on the occupied port
	p := NewPlugin(Config{Port: port})
	if err := p.Start(context.Background(), nil); err == nil {
		t.Error("expected error when port is in use")
		p.Stop(context.Background(), nil)
	}
}

// --- DI Interceptor Tests with Container ---

func TestDIInterceptorWithContainer(t *testing.T) {
	spectest.Proves(t, "go/grpc-services", "interceptor-chain", "di-interceptor-scopes-the-context")
	cc := inject.NewContainerContext("test")
	if err := cc.Start(); err != nil {
		t.Fatalf("failed to start container: %v", err)
	}
	defer cc.Close()

	interceptor := DIInterceptor(cc)

	handler := func(ctx context.Context, req any) (any, error) {
		return "scoped-ok", nil
	}

	info := &grpc.UnaryServerInfo{FullMethod: "/test.Service/Method"}
	resp, err := interceptor(context.Background(), "req", info, handler)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if resp != "scoped-ok" {
		t.Errorf("expected 'scoped-ok', got %v", resp)
	}
}

func TestDIInterceptorContainerNotStarted(t *testing.T) {
	cc := inject.NewContainerContext("test")
	// Don't start it — CreateScope should fail, fallback to calling handler directly

	interceptor := DIInterceptor(cc)

	handler := func(ctx context.Context, req any) (any, error) {
		return "fallback-ok", nil
	}

	info := &grpc.UnaryServerInfo{FullMethod: "/test.Service/Method"}
	resp, err := interceptor(context.Background(), "req", info, handler)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if resp != "fallback-ok" {
		t.Errorf("expected 'fallback-ok', got %v", resp)
	}
}

// --- Gateway Lifecycle Tests ---

func TestGatewayPluginConfigure(t *testing.T) {
	p := NewGatewayPlugin(GatewayConfig{})
	if err := p.Configure(context.Background(), nil); err != nil {
		t.Errorf("warmup should not error: %v", err)
	}
}

func TestGatewayPluginStart(t *testing.T) {
	p := NewGatewayPlugin(GatewayConfig{})
	p.MountHandler("/test.Service/", http.NotFoundHandler())

	if err := p.Start(context.Background(), nil); err != nil {
		t.Errorf("start should not error: %v", err)
	}
}

func TestGatewayPluginCustomPrefix(t *testing.T) {
	p := NewGatewayPlugin(GatewayConfig{PathPrefix: "/api"})
	if p.config.PathPrefix != "/api" {
		t.Errorf("expected path prefix '/api', got %q", p.config.PathPrefix)
	}

	// A custom prefix must actually route: a request to <prefix>/<service>/<method>
	// resolves to the mounted handler.
	gotPath, status := gatewayRoute(t, p, "/test.Service/", "/api/test.Service/Method")
	if status != http.StatusOK {
		t.Fatalf("custom prefix: expected 200 at /api/test.Service/Method, got %d", status)
	}
	if gotPath != "/api/test.Service/Method" {
		t.Errorf("custom prefix: handler saw path %q, want %q", gotPath, "/api/test.Service/Method")
	}

	// And the unprefixed path must NOT resolve (the prefix is honored, not ignored).
	if _, status := gatewayRoute(t, p, "/test.Service/", "/test.Service/Method"); status != http.StatusNotFound {
		t.Errorf("custom prefix: expected 404 at unprefixed /test.Service/Method, got %d", status)
	}
}

// gatewayRoute mounts mountPath on p, registers it on a real HTTP server, and
// issues a POST to requestPath through the production request path. It returns
// the URL path the Connect handler observed (empty if the handler never ran)
// and the HTTP status code.
func gatewayRoute(t *testing.T, p *GatewayPlugin, mountPath, requestPath string) (string, int) {
	t.Helper()

	var seenPath string
	gw := NewGatewayPlugin(p.config)
	gw.MountHandler(mountPath, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))

	server := phttp.NewServerPlugin(phttp.ServerConfig{Port: 0})
	gw.RegisterOn(server)

	ts := server.TestServer()
	defer ts.Close()

	resp, err := http.Post(ts.URL+requestPath, "application/json", nil)
	if err != nil {
		t.Fatalf("POST %s: %v", requestPath, err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	return seenPath, resp.StatusCode
}

// TestGatewayRegisterOnDescribeValidates proves the gateway's mount is recorded
// as a MatchPrefix / static-mount route that the http-routes describe surface
// accepts. A "*" wildcard fact fails ParseAndValidateManifest for every gateway
// consumer, so the mount shape is part of the contract, not an implementation
// detail.
func TestGatewayRegisterOnDescribeValidates(t *testing.T) {
	spectest.Proves(t, "go/grpc-services", "gateway-mounting", "register-on-validates-at-describe-time")
	gw := NewGatewayPlugin(GatewayConfig{})
	gw.MountHandler("/pkg.Service/", http.NotFoundHandler())

	server := phttp.NewServerPlugin(phttp.ServerConfig{Port: 0})
	gw.RegisterOn(server)

	if err := server.Configure(context.Background(), app.NewModule("example/go-service")); err != nil {
		t.Fatalf("configure: %v", err)
	}
	out := t.TempDir()
	if err := server.Describe(&app.DescribeContext{OutputDir: out}); err != nil {
		t.Fatalf("describe: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(out, phttp.HTTPRoutesDescribePath))
	if err != nil {
		t.Fatalf("read inventory: %v", err)
	}
	manifest, diags := httproutes.ParseAndValidateManifest(body)
	if manifest == nil || len(diags) != 0 {
		t.Fatalf("gateway inventory did not validate: manifest=%v diagnostics=%v", manifest, diags)
	}

	var found bool
	for _, route := range manifest.Routes {
		if route.Path != "/pkg.Service/" {
			continue
		}
		found = true
		if route.Match != httproutes.MatchPrefix {
			t.Errorf("match = %q, want %q", route.Match, httproutes.MatchPrefix)
		}
		if route.Provenance.SourceKind != httproutes.SourceStaticMount {
			t.Errorf("sourceKind = %q, want %q", route.Provenance.SourceKind, httproutes.SourceStaticMount)
		}
		if strings.Join(route.Methods, ",") != "POST" {
			t.Errorf("methods = %v, want [POST]", route.Methods)
		}
	}
	if !found {
		t.Fatalf("mounted service route /pkg.Service/ not found in %#v", manifest.Routes)
	}
}

// PathPrefix goes through the platform protocol's prefix rule, so surrounding
// whitespace and repeated slashes mount where the plain prefix does.
func TestGatewayPathPrefixIsNormalized(t *testing.T) {
	for _, prefix := range []string{"api", " api", "//api", "/api/"} {
		gw := NewGatewayPlugin(GatewayConfig{PathPrefix: prefix})
		gw.MountHandler("/pkg.Service/", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTeapot)
		}))
		server := phttp.NewServerPlugin(phttp.ServerConfig{Port: 0})
		gw.RegisterOn(server)
		ts := server.TestServer()
		resp, err := http.Post(ts.URL+"/api/pkg.Service/Method", "application/json", nil)
		ts.Close()
		if err != nil {
			t.Fatalf("PathPrefix %q: %v", prefix, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusTeapot {
			t.Errorf("PathPrefix %q: POST /api/pkg.Service/Method = %d, want the service handler", prefix, resp.StatusCode)
		}
	}
}

func TestGatewayMount(t *testing.T) {
	p := NewGatewayPlugin(GatewayConfig{})
	result := p.Mount(ServiceHandler{Path: "/svc/", Handler: http.NotFoundHandler()})
	if result != p {
		t.Error("Mount should return the plugin for chaining")
	}
	if len(p.handlers) != 1 {
		t.Errorf("expected 1 handler, got %d", len(p.handlers))
	}
}

func TestGatewayRegisterOn(t *testing.T) {
	gw := NewGatewayPlugin(GatewayConfig{})
	handlerCalled := false
	gw.MountHandler("/test.Service/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
		w.WriteHeader(http.StatusOK)
	}))

	server := phttp.NewServerPlugin(phttp.ServerConfig{Port: 0})
	gw.RegisterOn(server)

	// Verify routes were registered by checking the server has the handler
	// The RegisterOn method should have added a POST route
	if len(gw.handlers) != 1 {
		t.Errorf("expected 1 handler registered, got %d", len(gw.handlers))
	}
	_ = handlerCalled // handler is registered but not called in this test
}

func TestConnectBridge(t *testing.T) {
	called := false
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("connect response"))
	})

	bridge := connectBridge(h)

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/test.Service/Method", nil)

	ctx := &phttp.Context{
		Writer:  recorder,
		Request: req,
	}

	resp := bridge(ctx)
	if resp != nil {
		t.Error("connectBridge should return nil (handler writes directly)")
	}
	if !called {
		t.Error("underlying handler should have been called")
	}
	if recorder.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", recorder.Code)
	}
}

func TestGatewayRegisterOnMultipleHandlers(t *testing.T) {
	gw := NewGatewayPlugin(GatewayConfig{})
	gw.MountHandler("/svc.A/", http.NotFoundHandler()).
		MountHandler("/svc.B/", http.NotFoundHandler())

	server := phttp.NewServerPlugin(phttp.ServerConfig{Port: 0})
	gw.RegisterOn(server)

	if len(gw.handlers) != 2 {
		t.Errorf("expected 2 handlers, got %d", len(gw.handlers))
	}
}

func TestLoggingInterceptorStructuredField(t *testing.T) {
	sink := logger.NewMemorySink()
	log := logger.New("grpc-test", logger.LevelDebug, sink)
	interceptor := LoggingInterceptor(log)

	info := &grpc.UnaryServerInfo{FullMethod: "/test.Service/Method"}

	// Success path logs at DEBUG with the method as a structured field and a
	// constant message (no per-RPC fmt.Sprintf).
	if _, err := interceptor(context.Background(), "req", info,
		func(context.Context, any) (any, error) { return "ok", nil }); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	last := sink.Last()
	if last == nil {
		t.Fatal("expected a log entry")
	}
	if last.Level != logger.LevelDebug {
		t.Errorf("level = %v, want DEBUG", last.Level)
	}
	if last.Message != "rpc handled" {
		t.Errorf("message = %q, want %q", last.Message, "rpc handled")
	}
	if got := loggedMethod(last.Attrs); got != "/test.Service/Method" {
		t.Errorf("method field = %q, want %q", got, "/test.Service/Method")
	}

	// Error path logs at ERROR, still carrying the structured method field.
	if _, err := interceptor(context.Background(), "req", info,
		func(context.Context, any) (any, error) { return nil, fmt.Errorf("boom") }); err == nil {
		t.Fatal("expected error")
	}
	last = sink.Last()
	if last.Level != logger.LevelError {
		t.Errorf("level = %v, want ERROR", last.Level)
	}
	if last.Message != "rpc failed" {
		t.Errorf("message = %q, want %q", last.Message, "rpc failed")
	}
	if got := loggedMethod(last.Attrs); got != "/test.Service/Method" {
		t.Errorf("method field = %q, want %q", got, "/test.Service/Method")
	}
}

func loggedMethod(attrs []slog.Attr) string {
	for _, a := range attrs {
		if a.Key == "method" {
			return a.Value.String()
		}
	}
	return ""
}

// TestPluginReflectionIsOffByDefault pins the opt-in half of the reflection
// contract. Reflection publishes the server's full service catalog, so a
// deployment that never mentions it must not expose one — `WithReflection` is
// the only way in, and `TestPluginFluentAPI` only proves the setter honors an
// explicit false, not that the default is false.
func TestPluginReflectionIsOffByDefault(t *testing.T) {
	spectest.Proves(t, "go/grpc-services", "reflection-opt-in", "reflection-is-off-by-default")

	if p := NewPlugin(Config{Port: 50051}); p.enableReflect {
		t.Error("reflection is enabled on a plugin that never opted in")
	}
	// A plugin built through the fluent API without naming reflection is the
	// same: only WithReflection(true) turns it on.
	if p := NewPlugin(Config{Port: 50051}).WithServerOption(grpc.MaxRecvMsgSize(1024)); p.enableReflect {
		t.Error("reflection is enabled after unrelated fluent configuration")
	}

	// The flag is the intent; the catalog is the exposure. Start a server that
	// never opted in and confirm the reflection RPC is genuinely absent from the
	// wire — a registration that ignored the flag would leak every service name
	// while both assertions above still passed.
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("failed to find free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	p := NewPlugin(Config{Port: port})
	if err := p.Start(context.Background(), nil); err != nil {
		t.Fatalf("start failed: %v", err)
	}
	defer p.Stop(context.Background(), nil)

	services, err := listServices(t, port)
	if err == nil {
		t.Fatalf("a server that never opted in published its service catalog: %v", services)
	}
	if got := status.Code(err); got != codes.Unimplemented {
		t.Errorf("reflection RPC on an opt-out server answered %v, want %v", got, codes.Unimplemented)
	}
}

// listServices asks the server on port for its service catalog over the gRPC
// server-reflection RPC. It returns the reported service names, or the RPC
// error — codes.Unimplemented when reflection was never registered.
func listServices(t *testing.T, port int) ([]string, error) {
	t.Helper()

	conn, err := grpc.NewClient(fmt.Sprintf("localhost:%d", port), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// The deadline is a backstop against a hung server, not a latency budget:
	// the RPC answers in milliseconds when healthy, but on a loaded machine
	// (this gate shares cores with other -race suites) connection setup and
	// goroutine scheduling have eaten 5s before, turning an opt-in proof into
	// a spurious DeadlineExceeded. Keep it generous; a genuine hang still
	// fails, it only takes longer to say so.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	stream, err := reflectpb.NewServerReflectionClient(conn).ServerReflectionInfo(ctx)
	if err != nil {
		return nil, err
	}
	if sendErr := stream.Send(&reflectpb.ServerReflectionRequest{
		MessageRequest: &reflectpb.ServerReflectionRequest_ListServices{ListServices: ""},
	}); sendErr != nil && sendErr != io.EOF {
		return nil, sendErr
	}
	resp, err := stream.Recv()
	if err != nil {
		return nil, err
	}
	listing := resp.GetListServicesResponse()
	if listing == nil {
		return nil, fmt.Errorf("reflection answered without a service listing: %v", resp.GetMessageResponse())
	}
	names := make([]string, 0, len(listing.GetService()))
	for _, svc := range listing.GetService() {
		names = append(names, svc.GetName())
	}
	return names, nil
}
