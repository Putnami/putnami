package grpc

import (
	"context"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestConfigShutdownTimeoutDefault(t *testing.T) {
	spectest.Proves(t, "go/grpc-services", "bounded-drain", "shutdown-timeout-defaults-when-unset")
	if got := (Config{}).shutdownTimeout(); got != defaultShutdownTimeout {
		t.Errorf("shutdownTimeout() = %v, want default %v", got, defaultShutdownTimeout)
	}
	if got := (Config{ShutdownTimeout: 2 * time.Second}).shutdownTimeout(); got != 2*time.Second {
		t.Errorf("shutdownTimeout() = %v, want 2s", got)
	}
}

// With an in-flight RPC, an unbounded GracefulStop blocks forever. Stop must
// bound the drain by ShutdownTimeout and force-close, so it returns promptly.
func TestPluginStopForcesAfterTimeout(t *testing.T) {
	spectest.Proves(t, "go/grpc-services", "bounded-drain", "stop-force-closes-after-the-deadline")
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	var startOnce sync.Once
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)

	p := NewPlugin(Config{Port: port, ShutdownTimeout: 100 * time.Millisecond})
	p.Register(func(s *grpc.Server) {
		s.RegisterService(&grpc.ServiceDesc{
			ServiceName: "test.Blocker",
			HandlerType: (*any)(nil),
			Streams: []grpc.StreamDesc{{
				StreamName:    "Block",
				ServerStreams: true,
				ClientStreams: true,
				Handler: func(_ any, stream grpc.ServerStream) error {
					startOnce.Do(func() { close(started) })
					// Hold the RPC open until the server is force-closed (the
					// stream context is canceled) or the test releases us.
					select {
					case <-stream.Context().Done():
					case <-release:
					}
					return nil
				},
			}},
			Metadata: "test",
		}, struct{}{})
	})

	if err := p.Start(context.Background(), nil); err != nil {
		t.Fatalf("start: %v", err)
	}

	conn, err := grpc.NewClient(
		"passthrough:///"+net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	streamCtx, cancelStream := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelStream()
	if _, err := conn.NewStream(streamCtx,
		&grpc.StreamDesc{ServerStreams: true, ClientStreams: true},
		"/test.Blocker/Block",
		grpc.WaitForReady(true),
	); err != nil {
		t.Fatalf("new stream: %v", err)
	}

	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("server stream handler never started")
	}

	// Stop must return well within the force window, not hang on the RPC.
	done := make(chan error, 1)
	go func() { done <- p.Stop(context.Background(), nil) }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Stop returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Stop hung past ShutdownTimeout — force path did not trigger")
	}
}
