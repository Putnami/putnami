package datacli

import (
	"context"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// TestListenLocalBindsLoopback pins the security-critical binding: the bridge
// listens on 127.0.0.1 only, never 0.0.0.0/all interfaces.
func TestListenLocalBindsLoopback(t *testing.T) {
	l, err := listenLocal(0)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close() //nolint:errcheck
	addr := l.Addr().String()
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Fatalf("listener bound %q, want 127.0.0.1:<port>", addr)
	}
}

// TestConnectBannerDocumentsReconnect pins the AC that the connect surface tells
// the user a session may drop at the ~1h boundary while the grant stays active
// and the bridge auto-reconnects.
func TestConnectBannerDocumentsReconnect(t *testing.T) {
	var lines []string
	printConnectBanner(clicore.IO{Stdout: func(s string) { lines = append(lines, s) }}, 5432, "mydb")
	joined := strings.ToLower(strings.Join(lines, "\n"))
	for _, want := range []string{"127.0.0.1:5432", "auto-reconnect", "~1h", "grant stays"} {
		if !strings.Contains(joined, strings.ToLower(want)) {
			t.Fatalf("connect banner missing %q:\n%s", want, strings.Join(lines, "\n"))
		}
	}
}

// fakeTunnelDial returns a dialFunc backed by net.Pipe: each dial hands
// pipeBridge one end of an in-memory duplex pipe and echoes every byte back on
// a goroutine holding the other end. severAfter, when > 0, closes the server
// end after that many dials' first echo, forcing pipeBridge to report
// severedByServer and driving handleBridge's auto-reconnect. This proves the
// bridge/redial LOGIC in isolation from the wire: whether the tunnel's bytes
// travel correctly over the real WebSocket upgrade is
// db-gateway's own gateway package test
// TestGeneratedClientTunnelsBytesAndCancelsPromptly, which drives the same
// client.ByteStream production wires through here.
func fakeTunnelDial(severAfter int32) (dialFunc, *atomic.Int32) {
	var dials atomic.Int32
	dial := func(context.Context) (byteTunnel, error) {
		n := dials.Add(1)
		server, client := net.Pipe()
		go func() {
			buf := make([]byte, bridgeReadBuffer)
			for {
				nRead, err := server.Read(buf)
				if nRead > 0 {
					if _, werr := server.Write(buf[:nRead]); werr != nil {
						return
					}
				}
				if err != nil {
					return
				}
				if severAfter > 0 && n == severAfter {
					_ = server.Close()
					return
				}
			}
		}()
		return client, nil
	}
	return dial, &dials
}

// TestServeBridgeEchoAndRedial exercises the full bridge against a fake
// tunnel: bytes flow both ways (echo), and the listener re-dials after the
// tunnel is severed server-side.
func TestServeBridgeEchoAndRedial(t *testing.T) {
	dial, dials := fakeTunnelDial(1)

	prevBackoff := reconnectBackoff
	reconnectBackoff = time.Millisecond
	defer func() { reconnectBackoff = prevBackoff }()

	listener, err := listenLocal(0)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close() //nolint:errcheck

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go serveBridge(runCtx, listener, dial, clicore.IO{Stdout: func(string) {}, Stderr: func(string) {}})

	local, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial local: %v", err)
	}
	defer local.Close() //nolint:errcheck

	// First round trip over the first tunnel.
	if _, err := local.Write([]byte("hello")); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	if got := readN(t, local, 5); got != "hello" {
		t.Fatalf("echo = %q, want hello", got)
	}

	// The gateway severed the first tunnel; the listener must re-dial.
	waitFor(t, 2*time.Second, func() bool { return dials.Load() >= 2 })

	// The second (re-dialed) tunnel stays open and still bridges bytes.
	if _, err := local.Write([]byte("world")); err != nil {
		t.Fatalf("write world: %v", err)
	}
	if got := readN(t, local, 5); got != "world" {
		t.Fatalf("echo after re-dial = %q, want world", got)
	}

	if got := dials.Load(); got < 2 {
		t.Fatalf("dials = %d, want >= 2 (re-dial after server close)", got)
	}
}

// TestServeBridgeLocalHangupDoesNotRedial pins that a local-initiated close
// tears the tunnel down without triggering the auto-reconnect path (only a
// server-side sever does).
func TestServeBridgeLocalHangupDoesNotRedial(t *testing.T) {
	dial, dials := fakeTunnelDial(0)

	listener, err := listenLocal(0)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close() //nolint:errcheck

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go serveBridge(runCtx, listener, dial, clicore.IO{Stdout: func(string) {}, Stderr: func(string) {}})

	local, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial local: %v", err)
	}
	if _, err := local.Write([]byte("hi")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := readN(t, local, 2); got != "hi" {
		t.Fatalf("echo = %q, want hi", got)
	}
	_ = local.Close()

	waitFor(t, time.Second, func() bool { return dials.Load() == 1 })
	time.Sleep(20 * time.Millisecond) // give a wrongful re-dial a chance to happen
	if got := dials.Load(); got != 1 {
		t.Fatalf("dials = %d, want exactly 1 (no redial on local hangup)", got)
	}
}

func readN(t *testing.T, r net.Conn, n int) string {
	t.Helper()
	_ = r.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatalf("read %d bytes: %v", n, err)
	}
	_ = r.SetReadDeadline(time.Time{})
	return string(buf)
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}
