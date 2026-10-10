package datacli

import (
	"context"
	"fmt"
	"io"
	"net"
	"testing"

	papi "go.putnami.dev/api"
	dbgatewayclient "go.putnami.dev/cloud/clients/db-gateway/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	perrors "go.putnami.dev/errors"
	phttp "go.putnami.dev/http"
	clientcontract "go.putnami.dev/protocol/clientcontract"
)

func TestGatewayURLResolution(t *testing.T) {
	tests := []struct {
		name   string
		params map[string]any
		env    map[string]string
		want   string
	}{
		{"flag wins", map[string]any{"gateway-url": "wss://flag.example"}, map[string]string{"PUTNAMI_DB_GATEWAY_URL": "wss://env.example"}, "wss://flag.example"},
		{"env fallback", map[string]any{}, map[string]string{"PUTNAMI_DB_GATEWAY_URL": "wss://env.example"}, "wss://env.example"},
		{"default", map[string]any{}, map[string]string{}, DefaultGatewayURL},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := gatewayURL(tc.params, tc.env); got != tc.want {
				t.Fatalf("gatewayURL = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBridgePortResolution(t *testing.T) {
	six := float64(6000)
	tests := []struct {
		name   string
		params map[string]any
		env    map[string]string
		want   int
	}{
		{"flag wins", map[string]any{"port": six}, map[string]string{"PUTNAMI_DB_BRIDGE_PORT": "7000"}, 6000},
		{"env fallback", map[string]any{}, map[string]string{"PUTNAMI_DB_BRIDGE_PORT": "7000"}, 7000},
		{"invalid env falls back to default", map[string]any{}, map[string]string{"PUTNAMI_DB_BRIDGE_PORT": "not-a-number"}, defaultBridgePort},
		{"default", map[string]any{}, map[string]string{}, defaultBridgePort},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := bridgePort(tc.params, tc.env); got != tc.want {
				t.Fatalf("bridgePort = %d, want %d", got, tc.want)
			}
		})
	}
}

// nonTCPListener is a net.Listener whose Addr is not a *net.TCPAddr, exercising
// listenerPort's fallback-to-requested branch.
type nonTCPListener struct{}

func (nonTCPListener) Accept() (net.Conn, error) { return nil, fmt.Errorf("closed") }
func (nonTCPListener) Close() error              { return nil }
func (nonTCPListener) Addr() net.Addr            { return &net.UnixAddr{Name: "sock", Net: "unix"} }

func TestListenerPort(t *testing.T) {
	l, err := listenLocal(0)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close() //nolint:errcheck
	port := listenerPort(l, 5432)
	if port == 0 || port == 5432 {
		t.Fatalf("listenerPort resolved %d, want the ephemeral bound port", port)
	}
	if got := listenerPort(nonTCPListener{}, 5432); got != 5432 {
		t.Fatalf("listenerPort fallback = %d, want 5432", got)
	}
}

func TestGatewayHTTPOrigin(t *testing.T) {
	tests := []struct{ in, want string }{
		{"wss://db-gateway.putnami.cloud", "https://db-gateway.putnami.cloud"},
		{"ws://127.0.0.1:9000", "http://127.0.0.1:9000"},
		{"https://already-http.example", "https://already-http.example"},
		{"http://already-http.example", "http://already-http.example"},
		{"wss://db-gateway.putnami.cloud/ignored?query=dropped", "https://db-gateway.putnami.cloud"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := gatewayHTTPOrigin(tc.in)
			if err != nil {
				t.Fatalf("gatewayHTTPOrigin(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("gatewayHTTPOrigin(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestGatewayHTTPOriginInvalid(t *testing.T) {
	for _, origin := range []string{"", "not-a-url", "://missing-scheme", "ftp://host/path"} {
		if _, err := gatewayHTTPOrigin(origin); err == nil {
			t.Fatalf("gatewayHTTPOrigin(%q) = nil error, want error", origin)
		}
	}
}

// --- fake db-gateway provider (generated-client transport, not the raw wire) ---

// echoByteTunnelHandler answers every open tunnel by echoing bytes back,
// standing in for db-gateway's real ServeTunnel (which this test does not
// import — internal to a different module — the real wire-level contract is
// proven by db-gateway's own gateway package test
// TestGeneratedClientTunnelsBytesAndCancelsPromptly).
func echoByteTunnelHandler(ctx *papi.ByteStreamContext) error {
	_, err := io.Copy(ctx, ctx)
	return err
}

// fakeGatewayValidToken is the only bearer fakeGatewayServer accepts, so
// TestGatewayTunnelDialRefreshesOn401 et al. can drive the 401-then-refresh
// path deterministically.
const fakeGatewayValidToken = "good"

// fakeGatewayServer mounts a minimal provider declaring the same operation
// shape db-gateway's real ConnectPath does (GET, byte-stream body/response,
// "user" forwarded-token profile), gating access on a static bearer so the
// 401-then-refresh path can be driven deterministically.
func fakeGatewayServer(t *testing.T) string {
	t.Helper()
	server := phttp.NewServerPlugin(phttp.ServerConfig{Port: 0})
	server.Use(func(ctx *phttp.Context, next func() *phttp.Response) *phttp.Response {
		if ctx.Request.Header.Get("Authorization") != "Bearer "+fakeGatewayValidToken {
			return phttp.Unauthorized()
		}
		return next()
	})
	apiPlugin := papi.New(server, papi.WithClientService(papi.ClientServiceOptions{
		Service: clientcontract.Service{ID: "db-gateway", Audience: "db-gateway"},
		Credentials: map[string]clientcontract.CredentialProfile{
			"user": {Kind: clientcontract.CredentialForwardedUserToken},
		},
	}))
	apiPlugin.Register(papi.Endpoint("GET", "/v1/databases/connect").
		Description("test double of db-gateway's tunnel route").
		Query(papi.Type[dbgatewayclient.ListV1DatabasesConnectQuery]()).
		Body(papi.ByteStream()).
		Returns(papi.ByteStream()).
		MayThrow(perrors.CodeUnauthorized).
		Handle(papi.ByteTunnel(echoByteTunnelHandler)))
	if err := apiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("api Configure: %v", err)
	}
	ts := server.TestServer()
	t.Cleanup(ts.Close)
	return ts.URL
}

func testGatewayTunnel(t *testing.T, baseURL string, token clicore.Bearer, refresh func() (clicore.Bearer, error)) *gatewayTunnel {
	t.Helper()
	tunnel, err := newGatewayTunnel(baseURL, "acme", &clicore.WorkspaceContext{
		WorkspaceID:  "ws-1",
		ControlPlane: baseURL,
		AuthToken:    token,
		IO:           clicore.IO{},
		RefreshAuth:  refresh,
	})
	if err != nil {
		t.Fatalf("newGatewayTunnel: %v", err)
	}
	return tunnel
}

func TestGatewayTunnelDialSucceeds(t *testing.T) {
	baseURL := fakeGatewayServer(t)
	tun := testGatewayTunnel(t, baseURL, clicore.NewBearer("good"), nil)

	ws, err := tun.dial(context.Background())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if ws == nil {
		t.Fatal("dial returned nil connection")
	}
	_ = ws.Close()
}

func TestGatewayTunnelDialRefreshesOn401(t *testing.T) {
	baseURL := fakeGatewayServer(t)
	refreshed := false
	tun := testGatewayTunnel(t, baseURL, clicore.NewBearer("stale"), func() (clicore.Bearer, error) {
		refreshed = true
		return clicore.NewBearer("good"), nil
	})

	ws, err := tun.dial(context.Background())
	if err != nil {
		t.Fatalf("dial after refresh: %v", err)
	}
	if !refreshed {
		t.Fatal("refresh was not invoked on the 401")
	}
	_ = ws.Close()
}

func TestGatewayTunnelDialFailsWhenRefreshFails(t *testing.T) {
	baseURL := fakeGatewayServer(t)
	tun := testGatewayTunnel(t, baseURL, clicore.NewBearer("stale"), func() (clicore.Bearer, error) {
		return clicore.Bearer{}, fmt.Errorf("session not refreshable")
	})

	if _, err := tun.dial(context.Background()); err == nil {
		t.Fatal("dial = nil error, want the original 401")
	}
}

func TestGatewayTunnelDialFailsWithoutRefresh(t *testing.T) {
	baseURL := fakeGatewayServer(t)
	tun := testGatewayTunnel(t, baseURL, clicore.NewBearer("stale"), nil)

	if _, err := tun.dial(context.Background()); err == nil {
		t.Fatal("dial = nil error, want a 401 with no refresh configured")
	}
}
