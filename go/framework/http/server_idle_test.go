package http

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

// The idle tests send two requests separated by idlePause. idleProbeTimeout
// is well below the pause, so a connection whose idle bound is
// idleProbeTimeout is closed before the second request leaves the client.
const (
	idleProbeTimeout = 100 * time.Millisecond
	idlePause        = 300 * time.Millisecond
)

// serveIdleProbe serves a 204 route through the http.Server that Start builds
// from config, on a loopback port, and returns the route's URL.
func serveIdleProbe(t *testing.T, config ServerConfig) string {
	t.Helper()
	plugin := NewServerPlugin(config)
	plugin.GET("/ping", func(*Context) *Response { return NoContent() })
	_, server := plugin.buildServer(plugin.Handler())
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return "http://" + listener.Addr().String() + "/ping"
}

// dialsAcrossAnIdlePause sends a GET, leaves the connection idle for
// idlePause, sends a second GET, and returns how many TCP connections the
// client dialed for both. With h2c the client speaks HTTP/2 with prior
// knowledge over cleartext; otherwise it speaks HTTP/1.1.
func dialsAcrossAnIdlePause(t *testing.T, url string, h2c bool) int32 {
	t.Helper()
	var dials atomic.Int32
	var dialer net.Dialer
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			dials.Add(1)
			return dialer.DialContext(ctx, network, address)
		},
	}
	wantProto := "HTTP/1.1"
	if h2c {
		var protocols http.Protocols
		protocols.SetUnencryptedHTTP2(true)
		transport.Protocols = &protocols
		wantProto = "HTTP/2.0"
	}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second}

	for request := 1; request <= 2; request++ {
		if request == 2 {
			time.Sleep(idlePause)
		}
		resp, err := client.Get(url)
		if err != nil {
			t.Fatalf("request %d: %v", request, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent || resp.Proto != wantProto {
			t.Fatalf("request %d answered %d over %s, want 204 over %s", request, resp.StatusCode, resp.Proto, wantProto)
		}
	}
	return dials.Load()
}

// TestServerPlugin_IdleHTTP1ConnectionOutlivesReadTimeout pins that the idle
// bound of an HTTP/1.1 keep-alive connection is IdleTimeout, never
// ReadTimeout: with ReadTimeout at 100 ms and IdleTimeout unset, the second
// request after a 300 ms pause reuses the first connection.
func TestServerPlugin_IdleHTTP1ConnectionOutlivesReadTimeout(t *testing.T) {
	spectest.Proves(t, "go/http-services", "idle-connections", "an-idle-http1-connection-outlives-the-read-timeout")
	url := serveIdleProbe(t, ServerConfig{ReadTimeout: idleProbeTimeout})
	if dials := dialsAcrossAnIdlePause(t, url, false); dials != 1 {
		t.Fatalf("HTTP/1.1 client dialed %d connections across a %v idle pause, want 1", dials, idlePause)
	}
}

// TestServerPlugin_IdleH2CConnectionOutlivesReadTimeout pins the same bound
// for HTTP/2 over cleartext: with ReadTimeout at 100 ms and IdleTimeout unset,
// the second request after a 300 ms pause reuses the first connection.
func TestServerPlugin_IdleH2CConnectionOutlivesReadTimeout(t *testing.T) {
	spectest.Proves(t, "go/http-services", "idle-connections", "an-idle-h2c-connection-outlives-the-read-timeout")
	url := serveIdleProbe(t, ServerConfig{ReadTimeout: idleProbeTimeout})
	if dials := dialsAcrossAnIdlePause(t, url, true); dials != 1 {
		t.Fatalf("h2c client dialed %d connections across a %v idle pause, want 1", dials, idlePause)
	}
}

// TestServerPlugin_ExplicitIdleTimeoutClosesIdleConnection pins that an
// explicit IdleTimeout still bounds an idle connection over both protocols:
// ReadTimeout keeps its 30s default, IdleTimeout is 100 ms, and the second
// request after a 300 ms pause dials a new connection.
func TestServerPlugin_ExplicitIdleTimeoutClosesIdleConnection(t *testing.T) {
	spectest.Proves(t, "go/http-services", "idle-connections", "an-explicit-idle-timeout-closes-an-idle-connection")
	for _, protocol := range []struct {
		name string
		h2c  bool
	}{{name: "http1"}, {name: "h2c", h2c: true}} {
		t.Run(protocol.name, func(t *testing.T) {
			url := serveIdleProbe(t, ServerConfig{IdleTimeout: idleProbeTimeout})
			if dials := dialsAcrossAnIdlePause(t, url, protocol.h2c); dials != 2 {
				t.Fatalf("%s client dialed %d connections across a %v idle pause, want 2", protocol.name, dials, idlePause)
			}
		})
	}
}
