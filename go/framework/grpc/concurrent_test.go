package grpc

import (
	"context"
	"sync"
	"testing"

	"go.putnami.dev/inject"
	"google.golang.org/grpc"
)

// --- Concurrent Configure ---

func TestConfigure_Concurrent(t *testing.T) {
	p := NewPlugin(Config{Port: 9090})

	var wg sync.WaitGroup
	// Configure is a no-op that holds no state; concurrent calls must be safe.
	for range 30 {
		wg.Go(func() {
			if err := p.Configure(context.Background(), nil); err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
	wg.Wait()
}

// --- Concurrent DI interceptor invocations ---

func TestDIInterceptor_ConcurrentRequests(t *testing.T) {
	cc := inject.NewContainerContext("test")
	if err := cc.Start(); err != nil {
		t.Fatalf("start container: %v", err)
	}
	defer cc.Close()

	interceptor := DIInterceptor(cc)
	info := &grpc.UnaryServerInfo{FullMethod: "/test.Service/Method"}

	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			handler := func(ctx context.Context, req any) (any, error) {
				return "ok", nil
			}
			resp, err := interceptor(context.Background(), "req", info, handler)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if resp != "ok" {
				t.Errorf("expected 'ok', got %v", resp)
			}
		})
	}
	wg.Wait()
}

// --- Concurrent logging interceptor ---

func TestLoggingInterceptor_ConcurrentRequests(t *testing.T) {
	interceptor := LoggingInterceptor(nil)
	info := &grpc.UnaryServerInfo{FullMethod: "/test.Service/Concurrent"}

	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			handler := func(ctx context.Context, req any) (any, error) {
				return "logged", nil
			}
			resp, err := interceptor(context.Background(), "req", info, handler)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if resp != "logged" {
				t.Errorf("expected 'logged', got %v", resp)
			}
		})
	}
	wg.Wait()
}

// --- Concurrent recovery interceptor ---

func TestRecoveryInterceptor_ConcurrentRequests(t *testing.T) {
	interceptor := RecoveryInterceptor(nil)
	info := &grpc.UnaryServerInfo{FullMethod: "/test.Service/Recover"}

	var wg sync.WaitGroup
	// Half succeed, half panic.
	for i := range 100 {
		wg.Go(func() {
			handler := func(ctx context.Context, req any) (any, error) {
				if i%2 == 0 {
					panic("test panic")
				}
				return "ok", nil
			}
			resp, err := interceptor(context.Background(), "req", info, handler)
			if i%2 == 0 {
				if err == nil {
					t.Error("expected error from panic recovery")
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				if resp != "ok" {
					t.Errorf("expected 'ok', got %v", resp)
				}
			}
		})
	}
	wg.Wait()
}
