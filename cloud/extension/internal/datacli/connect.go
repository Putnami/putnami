package datacli

import (
	"context"
	stderrors "errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.putnami.dev/client"
	dbgatewayclient "go.putnami.dev/cloud/clients/db-gateway/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

const (
	// DefaultGatewayURL is the production db-gateway WebSocket origin
	// (the domain db-gateway declares in its runtime manifest). Override with
	// --gateway-url or PUTNAMI_DB_GATEWAY_URL. gatewayHTTPOrigin rewrites its
	// ws(s):// scheme to http(s):// for the generated client's ServiceBinding;
	// the tunnel path (db-gateway's gateway.ConnectPath) is the generated
	// client's own concern, not this package's.
	DefaultGatewayURL = "wss://db-gateway.putnami.cloud"
	// defaultBridgePort is the local port the bridge listens on when unset.
	defaultBridgePort = 5432
	// bridgeReadBuffer sizes the local→gateway copy buffer.
	bridgeReadBuffer = 32 * 1024
)

// reconnectBackoff is the pause before the listener re-dials the gateway after a
// live tunnel is severed server-side (the ~1h Cloud Run request cap). It is a
// package var so tests can shrink it.
var reconnectBackoff = 500 * time.Millisecond

// dbConnect opens a localhost TCP listener bridged over a WebSocket to the
// db-gateway, so psql/GUIs connect with zero GCP credentials. The listener binds
// 127.0.0.1 ONLY (never all-interfaces): a local port bridging to a live
// database must not be network-reachable. Each accepted local connection dials
// the gateway with the platform bearer (the same credential CallWithSession
// sends) and pipes bytes both ways. The listener stays up and auto-reconnects:
// an in-flight psql session may sever at the ~1h request-cap boundary while the
// grant itself stays active, and a reconnect resumes.
func dbConnect(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	database, err := requiredDatabase(params)
	if err != nil {
		return err
	}
	port := bridgePort(params, env)
	gatewayOrigin, err := gatewayHTTPOrigin(gatewayURL(params, env))
	if err != nil {
		return err
	}
	ctx, err := clicore.ResolveWorkspaceContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	tunnel, err := newGatewayTunnel(gatewayOrigin, database, ctx)
	if err != nil {
		return err
	}
	listener, err := listenLocal(port)
	if err != nil {
		return clicore.NewError(fmt.Sprintf("cannot bind local bridge on 127.0.0.1:%d: %s", port, err.Error()), clicore.ExitUsage)
	}
	defer listener.Close() //nolint:errcheck

	boundPort := listenerPort(listener, port)
	printConnectBanner(ioctx, boundPort, database)

	runCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Closing the listener unblocks the Accept below on Ctrl-C / SIGTERM so the
	// command shuts down cleanly instead of hanging on the accept syscall.
	go func() {
		<-runCtx.Done()
		_ = listener.Close()
	}()

	serveBridge(runCtx, listener, tunnel.dial, ioctx)
	return nil
}

// printConnectBanner prints the local DSN and the reconnect semantics.
func printConnectBanner(ioctx clicore.IO, port int, database string) {
	ioctx.Stdout(fmt.Sprintf("Bridging database %q on 127.0.0.1:%d (db-gateway tunnel).", database, port))
	ioctx.Stdout("Connect psql or any GUI with:")
	ioctx.Stdout(fmt.Sprintf("  postgres://<username>:<password>@127.0.0.1:%d/%s", port, database))
	ioctx.Stdout("Use the username/password from `putnami cloud db grant request`.")
	ioctx.Stdout("An in-flight session may drop at the ~1h request-cap boundary; the grant stays")
	ioctx.Stdout("active and the bridge auto-reconnects — just reconnect. Press Ctrl-C to stop.")
}

// serveBridge accepts local connections until runCtx is canceled, handing each
// to a bridge goroutine. A clean shutdown (listener closed on ctx cancel) stops
// the loop without an error.
func serveBridge(runCtx context.Context, listener net.Listener, dial dialFunc, ioctx clicore.IO) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			if runCtx.Err() != nil {
				return
			}
			// A transient accept error (not shutdown): report and keep serving.
			ioctx.Stderr("accept failed: " + err.Error())
			continue
		}
		go handleBridge(runCtx, conn, dial, ioctx)
	}
}

// handleBridge pipes one local connection to the gateway, re-dialing when the
// tunnel is severed server-side while the local side is still open (the
// auto-reconnect listener). It returns — closing the local conn — when the local
// side hangs up, the context is canceled, or a fresh dial hard-fails (grant
// expired/revoked).
func handleBridge(runCtx context.Context, local net.Conn, dial dialFunc, ioctx clicore.IO) {
	defer local.Close() //nolint:errcheck
	for {
		ws, err := dial(runCtx)
		if err != nil {
			ioctx.Stderr("db-gateway tunnel dial failed: " + err.Error())
			return
		}
		severedByServer := pipeBridge(runCtx, local, ws)
		_ = ws.Close()
		if !severedByServer || runCtx.Err() != nil {
			return
		}
		select {
		case <-runCtx.Done():
			return
		case <-time.After(reconnectBackoff):
		}
	}
}

// byteTunnel is the minimal shape pipeBridge needs from a dialed tunnel: the
// generated client.ByteStream in production, a lightweight in-memory fake in
// tests.
type byteTunnel interface {
	io.Reader
	io.Writer
	Close() error
}

// pipeBridge copies bytes opaquely in both directions until one side closes or
// the context is canceled. It reports whether the SERVER side (the tunnel)
// closed first — the signal handleBridge uses to decide whether to re-dial.
//
// Crucially, when the server severs the tunnel the LOCAL connection is left OPEN
// so the auto-reconnect path can re-dial and keep serving the same psql/GUI
// socket. The local reader is unblocked with a read deadline (then restored)
// rather than by closing the socket, which would otherwise RST the connection —
// discarding in-flight bytes and breaking the re-dial.
func pipeBridge(runCtx context.Context, local net.Conn, ws byteTunnel) (severedByServer bool) {
	serverClosed := make(chan struct{})
	localClosed := make(chan struct{})

	// local → gateway.
	go func() {
		buf := make([]byte, bridgeReadBuffer)
		for {
			n, err := local.Read(buf)
			if n > 0 {
				if _, werr := ws.Write(buf[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		close(localClosed)
	}()

	// gateway → local. client.ByteStream.Read returns io.EOF once the provider
	// closes the stream normally, matching the hand-rolled codec's contract
	// this replaced.
	go func() {
		buf := make([]byte, bridgeReadBuffer)
		for {
			n, err := ws.Read(buf)
			if n > 0 {
				if _, werr := local.Write(buf[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		close(serverClosed)
	}()

	select {
	case <-serverClosed:
		// Server severed. Keep local OPEN for the re-dial; nudge the local
		// reader off its blocking Read with a deadline, then clear it so the
		// next tunnel can read again.
		_ = local.SetReadDeadline(time.Now())
		<-localClosed
		_ = local.SetReadDeadline(time.Time{})
		_ = ws.Close()
		return true
	case <-localClosed:
		// Local side hung up: tear the tunnel down and report a clean end.
		_ = ws.Close()
		<-serverClosed
		return false
	case <-runCtx.Done():
		_ = ws.Close()
		_ = local.SetReadDeadline(time.Now())
		<-serverClosed
		<-localClosed
		_ = local.SetReadDeadline(time.Time{})
		return false
	}
}

// dialFunc opens a fresh gateway tunnel. Production wires gatewayTunnel.dial;
// tests inject a closure that reaches an in-memory fake.
type dialFunc func(ctx context.Context) (byteTunnel, error)

// gatewayTunnel dials db-gateway's generated client with the platform bearer
// (the same credential CallWithSession sends), re-minting the workspace
// session once on a 401 and retrying — the same one-shot refresh
// clicore.CallWithSession performs for a unary generated call, hand-rolled here
// because a byte stream has no typed result for that helper to wrap.
type gatewayTunnel struct {
	client   *dbgatewayclient.DbGatewayClient
	database string

	mu      sync.Mutex
	wctx    *clicore.WorkspaceContext
	refresh func() (clicore.Bearer, error)
}

// newGatewayTunnel resolves the generated db-gateway client bound to
// gatewayOrigin, carrying ctx's workspace bearer forwarded per call
// (clicore.WorkspaceContext.CallContext) exactly like every other generated
// Cloud client this CLI calls — db-gateway is a different service than the
// control plane ctx already targets, so this binds its own WorkspaceContext
// copy (same bearer/refresh/IO, a different ControlPlane).
func newGatewayTunnel(gatewayOrigin, database string, ctx *clicore.WorkspaceContext) (*gatewayTunnel, error) {
	wctx := &clicore.WorkspaceContext{
		WorkspaceID:  ctx.WorkspaceID,
		ControlPlane: gatewayOrigin,
		AuthToken:    ctx.AuthToken,
		IO:           ctx.IO,
		RefreshAuth:  ctx.RefreshAuth,
	}
	dbClient, err := clicore.NewServiceClient[dbgatewayclient.DbGatewayClient](dbgatewayclient.RegisterDbGatewayClient, wctx.ServiceBinding())
	if err != nil {
		return nil, err
	}
	return &gatewayTunnel{client: dbClient, database: database, wctx: wctx, refresh: ctx.RefreshAuth}, nil
}

func (t *gatewayTunnel) dial(ctx context.Context) (byteTunnel, error) {
	stream, err := t.open(ctx)
	if err == nil {
		return stream, nil
	}
	var remote *client.RemoteError
	if !stderrors.As(err, &remote) || remote.StatusCode != http.StatusUnauthorized || t.refresh == nil {
		return nil, err
	}
	fresh, ferr := t.refresh()
	if ferr != nil {
		return nil, err
	}
	t.mu.Lock()
	t.wctx.AuthToken = fresh
	t.mu.Unlock()
	return t.open(ctx)
}

func (t *gatewayTunnel) open(ctx context.Context) (*client.ByteStream, error) {
	t.mu.Lock()
	callCtx := t.wctx.CallContext(ctx)
	t.mu.Unlock()
	database := t.database
	return t.client.ListV1DatabasesConnect(callCtx, dbgatewayclient.ListV1DatabasesConnectInput{
		Query: dbgatewayclient.ListV1DatabasesConnectQuery{Database: &database},
	})
}

// gatewayHTTPOrigin validates origin and rewrites its ws(s):// scheme to
// http(s)://, the scheme client.ServiceBinding.URL expects: the generated
// client derives the WebSocket upgrade from the operation's declared
// transport, not from the binding URL's own scheme.
func gatewayHTTPOrigin(origin string) (string, error) {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return "", clicore.NewError("invalid db-gateway URL: "+origin, clicore.ExitUsage)
	}
	switch strings.ToLower(u.Scheme) {
	case "wss", "https":
		u.Scheme = "https"
	case "ws", "http":
		u.Scheme = "http"
	default:
		return "", clicore.NewError("invalid db-gateway URL: "+origin, clicore.ExitUsage)
	}
	u.Path, u.RawQuery, u.Fragment = "", "", ""
	return u.String(), nil
}

// listenLocal binds a TCP listener on the loopback interface ONLY. Binding
// 127.0.0.1 (never 0.0.0.0/all interfaces) keeps the live-database bridge off
// the network — no other host can reach the port.
func listenLocal(port int) (net.Listener, error) {
	return net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
}

// listenerPort returns the actually-bound port (resolving an ephemeral :0),
// falling back to the requested port when the address cannot be parsed.
func listenerPort(listener net.Listener, requested int) int {
	if addr, ok := listener.Addr().(*net.TCPAddr); ok {
		return addr.Port
	}
	return requested
}

// bridgePort resolves the local listen port: --port, PUTNAMI_DB_BRIDGE_PORT,
// then the default.
func bridgePort(params map[string]any, env map[string]string) int {
	if v := clicore.NumberParam(params, "port"); v != nil {
		return int(*v)
	}
	if s := clicore.EnvGet(env, "PUTNAMI_DB_BRIDGE_PORT"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			return n
		}
	}
	return defaultBridgePort
}

// gatewayURL resolves the db-gateway origin: --gateway-url, then
// PUTNAMI_DB_GATEWAY_URL, then the production default.
func gatewayURL(params map[string]any, env map[string]string) string {
	return clicore.FirstString(
		clicore.StringParam(params, "gateway-url", "gatewayUrl"),
		clicore.EnvGet(env, "PUTNAMI_DB_GATEWAY_URL"),
		DefaultGatewayURL,
	)
}
