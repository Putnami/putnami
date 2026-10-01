package grpc

import (
	"context"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.putnami.dev/inject"
	"go.putnami.dev/logger"
	"go.putnami.dev/protocol/features/spectest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/status"
)

// These tests stand up a real gRPC server (the Plugin, listening on a loopback
// port) wired with the full built-in interceptor chain, dial it with a real
// client, and drive unary RPCs end-to-end. Unlike the unit tests — which call
// each interceptor in isolation with a synthetic handler — this exercises the
// chain as grpc.ChainUnaryInterceptor actually composes it, proving:
//
//   - a request flows through the composed chain to the handler and back,
//   - interceptor ordering matches registration (outermost first),
//   - a handler panic is recovered and surfaced to the client as codes.Internal
//     with a sanitized message (no panic detail leaks across the wire), and
//   - the logging interceptor observes the call (success and panic paths).
//
// To avoid pulling in generated protobuf code, the service is registered via a
// hand-built grpc.ServiceDesc and a raw []byte codec forced on both ends with
// grpc.ForceServerCodec / grpc.ForceCodec.

// --- Raw codec & message (no generated protobuf) ---

// rawMessage is the wire payload for the integration service: an opaque byte
// slice the rawCodec round-trips verbatim.
type rawMessage struct{ data []byte }

// rawCodec is a minimal encoding.Codec that ships *rawMessage bodies unchanged.
// Forcing it on the server (ForceServerCodec) and client (ForceCodec) lets the
// real transport carry arbitrary bytes without protobuf generation.
type rawCodec struct{}

func (rawCodec) Marshal(v any) ([]byte, error) {
	m, ok := v.(*rawMessage)
	if !ok {
		return nil, status.Errorf(codes.Internal, "rawCodec: cannot marshal %T", v)
	}
	return m.data, nil
}

func (rawCodec) Unmarshal(data []byte, v any) error {
	m, ok := v.(*rawMessage)
	if !ok {
		return status.Errorf(codes.Internal, "rawCodec: cannot unmarshal into %T", v)
	}
	// Copy: grpc may reuse the underlying receive buffer after Unmarshal returns.
	m.data = append([]byte(nil), data...)
	return nil
}

// Name is intentionally distinct so this codec never collides with a globally
// registered one; ForceServerCodec/ForceCodec select it regardless of the
// content-subtype, so it is never registered via encoding.RegisterCodec.
func (rawCodec) Name() string { return "putnami-grpc-raw-integration" }

var _ encoding.Codec = rawCodec{}

const (
	integrationServiceName = "putnami.grpc.it.Service"
	integrationEchoMethod  = "/" + integrationServiceName + "/Echo"
	integrationPanicMethod = "/" + integrationServiceName + "/Panic"
	integrationPanicDetail = "boom-secret-stack-detail"
)

// callCounter is a tiny mutex-guarded counter recording handler invocations.
type callCounter struct {
	mu sync.Mutex
	n  int
}

func (c *callCounter) inc() {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
}

func (c *callCounter) get() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// orderLog is a mutex-guarded sequence of interceptor entry tags, written on the
// server goroutine and read by the test; the lock provides the happens-before
// edge the race detector needs across goroutines.
type orderLog struct {
	mu   sync.Mutex
	tags []string
}

func (o *orderLog) record(tag string) {
	o.mu.Lock()
	o.tags = append(o.tags, tag)
	o.mu.Unlock()
}

func (o *orderLog) snapshot() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.tags...)
}

// syncSink is a race-safe logger.Sink for the integration tests. The built-in
// LoggingInterceptor / RecoveryInterceptor write entries from the server
// goroutine; the test reads them after the RPC returns. Guarding both with the
// same mutex (rather than reading logger.MemorySink.Entries unlocked) keeps the
// tests clean under `go test -race`.
type syncSink struct {
	mu      sync.Mutex
	entries []logger.LogEntry
}

func (s *syncSink) Write(e logger.LogEntry) {
	s.mu.Lock()
	s.entries = append(s.entries, e)
	s.mu.Unlock()
}

func (s *syncSink) Flush() error { return nil }
func (s *syncSink) Close() error { return nil }

func (s *syncSink) snapshot() []logger.LogEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]logger.LogEntry(nil), s.entries...)
}

// integrationServiceDesc builds a ServiceDesc with an Echo method (returns the
// request bytes) and a Panic method (panics with integrationPanicDetail). The
// handlers mirror the shape protoc-gen-go-grpc generates so the interceptor
// chain wraps them exactly as it would a real service.
func integrationServiceDesc(echoes *callCounter) *grpc.ServiceDesc {
	return &grpc.ServiceDesc{
		ServiceName: integrationServiceName,
		HandlerType: (*any)(nil),
		Methods: []grpc.MethodDesc{
			{
				MethodName: "Echo",
				Handler: func(_ any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
					in := &rawMessage{}
					if err := dec(in); err != nil {
						return nil, err
					}
					info := &grpc.UnaryServerInfo{FullMethod: integrationEchoMethod}
					handler := func(_ context.Context, req any) (any, error) {
						echoes.inc()
						return &rawMessage{data: req.(*rawMessage).data}, nil
					}
					if interceptor == nil {
						return handler(ctx, in)
					}
					return interceptor(ctx, in, info, handler)
				},
			},
			{
				MethodName: "Panic",
				Handler: func(_ any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
					in := &rawMessage{}
					if err := dec(in); err != nil {
						return nil, err
					}
					info := &grpc.UnaryServerInfo{FullMethod: integrationPanicMethod}
					handler := func(_ context.Context, _ any) (any, error) {
						panic(integrationPanicDetail)
					}
					if interceptor == nil {
						return handler(ctx, in)
					}
					return interceptor(ctx, in, info, handler)
				},
			},
		},
		Metadata: "integration",
	}
}

// orderRecorder is a passthrough unary interceptor that records its tag on entry.
// Interleaving these between the real interceptors makes the composed chain
// order directly observable.
func orderRecorder(tag string, order *orderLog) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		order.record(tag)
		return handler(ctx, req)
	}
}

// integrationServer bundles the live client connection and the observation
// hooks (chain order, server logs, handler-call count) for a running test
// server. Teardown is registered via t.Cleanup.
type integrationServer struct {
	conn   *grpc.ClientConn
	order  *orderLog
	sink   *syncSink
	echoes *callCounter
}

// startIntegrationServer starts a Plugin on a free loopback port wired with the
// full interceptor chain and returns a dialed client plus observation hooks.
func startIntegrationServer(t *testing.T, cc *inject.ContainerContext) *integrationServer {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	sink := &syncSink{}
	log := logger.New("grpc-it", logger.LevelDebug, sink)
	order := &orderLog{}
	echoes := &callCounter{}

	p := NewPlugin(Config{Port: port}).
		WithServerOption(grpc.ForceServerCodec(rawCodec{})).
		// Registration order == chain order (outermost first). The recorder
		// tags between the real interceptors make that order observable, while
		// the real Logging/Recovery/DI interceptors do the actual work.
		WithUnaryInterceptor(orderRecorder("logging", order)).
		WithUnaryInterceptor(LoggingInterceptor(log)).
		WithUnaryInterceptor(orderRecorder("recovery", order)).
		WithUnaryInterceptor(RecoveryInterceptor(log)).
		WithUnaryInterceptor(orderRecorder("di", order)).
		WithUnaryInterceptor(DIInterceptor(cc)).
		WithUnaryInterceptor(orderRecorder("handler-edge", order)).
		Register(func(s *grpc.Server) {
			s.RegisterService(integrationServiceDesc(echoes), struct{}{})
		})

	if err := p.Start(context.Background(), nil); err != nil {
		t.Fatalf("start: %v", err)
	}

	conn, err := grpc.NewClient(
		"passthrough:///"+net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		_ = p.Stop(context.Background(), nil)
		t.Fatalf("dial: %v", err)
	}

	t.Cleanup(func() {
		conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = p.Stop(ctx, nil)
	})

	return &integrationServer{conn: conn, order: order, sink: sink, echoes: echoes}
}

// invoke performs a unary RPC through the real client with the raw codec forced.
// WaitForReady makes the first RPC block until the lazy connection is
// established (bounded by ctx) rather than fail-fast while still CONNECTING, so
// the test never races the server's accept loop.
func (s *integrationServer) invoke(ctx context.Context, method string, payload []byte) ([]byte, error) {
	req := &rawMessage{data: payload}
	resp := &rawMessage{}
	if err := s.conn.Invoke(ctx, method, req, resp, grpc.ForceCodec(rawCodec{}), grpc.WaitForReady(true)); err != nil {
		return nil, err
	}
	return resp.data, nil
}

// --- end-to-end interceptor chain: ordering and panic recovery ---

// TestIntegration_SuccessfulRoundTripThroughChain proves a unary RPC flows all
// the way through the composed interceptor chain to the handler and back, that
// the chain runs interceptors in registration order (outermost first), and that
// the logging interceptor observed the (successful) call.
func TestIntegration_SuccessfulRoundTripThroughChain(t *testing.T) {
	spectest.Proves(t, "go/grpc-services", "interceptor-chain", "unary-interceptors-run-in-registration-order")
	cc := inject.NewContainerContext("it-success")
	if err := cc.Start(); err != nil {
		t.Fatalf("start container: %v", err)
	}
	defer cc.Close()

	srv := startIntegrationServer(t, cc)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	got, err := srv.invoke(ctx, integrationEchoMethod, []byte("ping"))
	if err != nil {
		t.Fatalf("Echo RPC failed: %v", err)
	}
	if string(got) != "ping" {
		t.Errorf("Echo payload = %q, want %q", got, "ping")
	}

	// Handler actually ran (the round-trip reached the server, not a stale cache).
	if n := srv.echoes.get(); n != 1 {
		t.Errorf("handler invocation count = %d, want 1", n)
	}

	// The chain ran each interceptor exactly once, in registration order
	// (grpc.ChainUnaryInterceptor invokes the first-registered outermost).
	want := []string{"logging", "recovery", "di", "handler-edge"}
	if got := srv.order.snapshot(); !sameOrder(got, want) {
		t.Errorf("interceptor entry order = %v, want %v", got, want)
	}

	// The logging interceptor observed the successful call at DEBUG with the
	// method as a structured field.
	if !sinkHasMethod(srv.sink, logger.LevelDebug, "rpc handled", integrationEchoMethod) {
		t.Errorf("expected a DEBUG 'rpc handled' log for %s; entries=%v",
			integrationEchoMethod, summarize(srv.sink))
	}
}

// TestIntegration_PanicRecoveredAsInternalNoLeak proves a handler panic is
// caught by the recovery interceptor and surfaced to the client as
// codes.Internal with a generic message that does not leak the panic detail —
// and that the chain order places logging OUTSIDE recovery (so logging records
// the recovered RPC as a failed call rather than itself panicking).
func TestIntegration_PanicRecoveredAsInternalNoLeak(t *testing.T) {
	spectest.Proves(t, "go/grpc-services", "interceptor-chain", "recovery-converts-a-handler-panic-to-internal")
	cc := inject.NewContainerContext("it-panic")
	if err := cc.Start(); err != nil {
		t.Fatalf("start container: %v", err)
	}
	defer cc.Close()

	srv := startIntegrationServer(t, cc)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := srv.invoke(ctx, integrationPanicMethod, []byte("trigger"))
	if err == nil {
		t.Fatal("expected an error from the panicking RPC, got nil")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected a gRPC status error, got %T: %v", err, err)
	}
	if st.Code() != codes.Internal {
		t.Errorf("status code = %s, want %s", st.Code(), codes.Internal)
	}
	// The sanitized message must not carry the panic value across the wire.
	if got := st.Message(); got != "internal server error" {
		t.Errorf("status message = %q, want sanitized %q", got, "internal server error")
	}
	if strings.Contains(st.Message(), integrationPanicDetail) || strings.Contains(st.Message(), "panic") {
		t.Errorf("status message leaked panic detail: %q", st.Message())
	}

	// Ordering: recovery sits inside logging, so on a panic the recovery
	// interceptor converts it to an error BEFORE the (outer) logging interceptor
	// returns — logging therefore records the call as failed and the panic never
	// escapes past logging as a live Go panic. The "handler-edge" recorder, being
	// innermost, still ran (the panic happened in the handler it wrapped).
	want := []string{"logging", "recovery", "di", "handler-edge"}
	if got := srv.order.snapshot(); !sameOrder(got, want) {
		t.Errorf("interceptor entry order = %v, want %v", got, want)
	}

	// Logging (outer of recovery) saw the recovered RPC as an ERROR-level failure
	// for the panicking method. If recovery were outer of logging, logging's
	// handler() call would have panicked and this entry would be absent.
	if !sinkHasMethod(srv.sink, logger.LevelError, "rpc failed", integrationPanicMethod) {
		t.Errorf("expected an ERROR 'rpc failed' log for %s (proves logging wraps recovery); entries=%v",
			integrationPanicMethod, summarize(srv.sink))
	}

	// The recovery interceptor logged the panic server-side at ERROR with the
	// grpc.panic code (the detail stays in the server log, never on the wire).
	if !sinkHasPanicLog(srv.sink) {
		t.Errorf("expected a server-side panic log (code %s); entries=%v",
			CodeGrpcPanic, summarize(srv.sink))
	}
}

// TestIntegration_ChainEndToEndMixedTraffic drives both a success and a panic
// over the same connection and asserts the chain stays healthy: the post-panic
// RPC still succeeds (recovery did not poison the server or connection).
func TestIntegration_ChainEndToEndMixedTraffic(t *testing.T) {
	cc := inject.NewContainerContext("it-mixed")
	if err := cc.Start(); err != nil {
		t.Fatalf("start container: %v", err)
	}
	defer cc.Close()

	srv := startIntegrationServer(t, cc)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 1) success
	if got, err := srv.invoke(ctx, integrationEchoMethod, []byte("a")); err != nil || string(got) != "a" {
		t.Fatalf("first Echo: got %q err %v", got, err)
	}
	// 2) panic -> Internal, recovered (server survives)
	if _, err := srv.invoke(ctx, integrationPanicMethod, []byte("x")); status.Code(err) != codes.Internal {
		t.Fatalf("Panic RPC: code = %s, want Internal (err=%v)", status.Code(err), err)
	}
	// 3) success again on the SAME connection — proves the panic did not crash
	//    the server process or wedge the transport.
	if got, err := srv.invoke(ctx, integrationEchoMethod, []byte("b")); err != nil || string(got) != "b" {
		t.Fatalf("post-panic Echo: got %q err %v", got, err)
	}

	if n := srv.echoes.get(); n != 2 {
		t.Errorf("successful handler invocations = %d, want 2", n)
	}
}

// --- helpers ---

func sameOrder(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// sinkHasMethod reports whether the sink recorded an entry at the given level
// with the given message and a "method" attribute equal to method.
func sinkHasMethod(sink *syncSink, level logger.Level, message, method string) bool {
	for _, e := range sink.snapshot() {
		if e.Level == level && e.Message == message && loggedMethod(e.Attrs) == method {
			return true
		}
	}
	return false
}

// sinkHasPanicLog reports whether the sink recorded the recovery interceptor's
// server-side panic entry (ERROR level carrying the grpc.panic error code).
func sinkHasPanicLog(sink *syncSink) bool {
	for _, e := range sink.snapshot() {
		if e.Level == logger.LevelError && e.Error != nil && e.Error.Code == string(CodeGrpcPanic) {
			return true
		}
	}
	return false
}

// summarize renders sink entries as "LEVEL:message" for failure diagnostics.
func summarize(sink *syncSink) []string {
	entries := sink.snapshot()
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Level.String()+":"+e.Message)
	}
	return out
}
