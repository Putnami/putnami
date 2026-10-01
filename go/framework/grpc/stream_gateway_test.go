package grpc

import (
	"context"
	"net/http"
	"testing"

	phttp "go.putnami.dev/http"
	"go.putnami.dev/inject"
	"go.putnami.dev/protocol/features/spectest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// fakeServerStream is a minimal grpc.ServerStream for exercising the stream
// interceptors without a running server.
type fakeServerStream struct {
	ctx context.Context
}

func (f *fakeServerStream) SetHeader(metadata.MD) error  { return nil }
func (f *fakeServerStream) SendHeader(metadata.MD) error { return nil }
func (f *fakeServerStream) SetTrailer(metadata.MD)       {}
func (f *fakeServerStream) Context() context.Context {
	if f.ctx != nil {
		return f.ctx
	}
	return context.Background()
}
func (f *fakeServerStream) SendMsg(any) error { return nil }
func (f *fakeServerStream) RecvMsg(any) error { return nil }

// --- stream interceptors: recovery, logging, and DI scoping ---

func TestRecoveryStreamInterceptor(t *testing.T) {
	spectest.Proves(t, "go/grpc-services", "interceptor-chain", "recovery-interceptor-recovers-a-stream-panic")
	interceptor := RecoveryStreamInterceptor(nil)
	info := &grpc.StreamServerInfo{FullMethod: "/test.Service/Stream"}
	handler := func(_ any, _ grpc.ServerStream) error { panic("boom") }

	err := interceptor(nil, &fakeServerStream{}, info, handler)
	if err == nil {
		t.Fatal("expected recovery to convert the panic into an error")
	}
	if status.Code(err) != codes.Internal {
		t.Errorf("expected codes.Internal, got %v", status.Code(err))
	}
}

func TestLoggingStreamInterceptor(t *testing.T) {
	called := false
	interceptor := LoggingStreamInterceptor(nil)
	info := &grpc.StreamServerInfo{FullMethod: "/test.Service/Stream"}
	handler := func(_ any, _ grpc.ServerStream) error { called = true; return nil }

	if err := interceptor(nil, &fakeServerStream{}, info, handler); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Error("expected the handler to be invoked")
	}
}

func TestDIStreamInterceptor_ScopesContext(t *testing.T) {
	spectest.Proves(t, "go/grpc-services", "interceptor-chain", "di-stream-interceptor-scopes-the-context")
	cc := inject.NewContainerContext("test")
	if err := cc.Start(); err != nil {
		t.Fatalf("start container: %v", err)
	}
	defer cc.Close()

	interceptor := DIStreamInterceptor(cc)
	info := &grpc.StreamServerInfo{FullMethod: "/test.Service/Stream"}
	var gotCtx context.Context
	handler := func(_ any, ss grpc.ServerStream) error { gotCtx = ss.Context(); return nil }

	if err := interceptor(nil, &fakeServerStream{ctx: context.Background()}, info, handler); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotCtx == nil {
		t.Error("expected the handler to observe a (scoped) context")
	}
}

func TestDIStreamInterceptor_NilContainerPassthrough(t *testing.T) {
	interceptor := DIStreamInterceptor(nil)
	info := &grpc.StreamServerInfo{FullMethod: "/x"}
	called := false
	handler := func(_ any, _ grpc.ServerStream) error { called = true; return nil }

	if err := interceptor(nil, &fakeServerStream{}, info, handler); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Error("expected passthrough to call the handler when cc is nil")
	}
}

// --- gateway mounting: RegisterOn is what makes handlers reachable ---

func TestGatewayRegisterOnMarksMounted(t *testing.T) {
	spectest.Proves(t, "go/grpc-services", "gateway-mounting", "register-on-marks-the-gateway-mounted")
	gw := NewGatewayPlugin(GatewayConfig{})
	gw.MountHandler("/svc/", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	if gw.mounted {
		t.Fatal("gateway should not be mounted before RegisterOn")
	}

	gw.RegisterOn(phttp.NewServerPlugin(phttp.ServerConfig{Port: 0}))
	if !gw.mounted {
		t.Error("RegisterOn should mark the gateway mounted")
	}
	// Start on a mounted gateway must succeed.
	if err := gw.Start(context.Background(), nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
}

func TestGatewayStartWithoutRegisterOn(t *testing.T) {
	spectest.Proves(t, "go/grpc-services", "gateway-mounting", "gateway-stays-unmounted-without-register-on")
	gw := NewGatewayPlugin(GatewayConfig{})
	gw.MountHandler("/svc/", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	// RegisterOn was never called: Start must not error, and the gateway must
	// remain unmounted (it logs a warning that the services are unreachable).
	if err := gw.Start(context.Background(), nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if gw.mounted {
		t.Error("gateway must remain unmounted when RegisterOn is not called")
	}
}
