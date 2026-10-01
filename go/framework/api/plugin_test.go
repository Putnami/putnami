package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/app"
	phttp "go.putnami.dev/http"

	"go.putnami.dev/protocol/features/spectest"
)

var errFakeFinalize = errors.New("simulated finalize failure")

// fakeServer captures Handle / AddPendingInjectedHandler calls for verification.
type fakeServer struct {
	calls            []handleCall
	pendingInjecteds int
}

type handleCall struct {
	method, path string
	handler      phttp.Handler
}

func (f *fakeServer) Handle(method, path string, handler phttp.Handler) {
	f.calls = append(f.calls, handleCall{method: method, path: path, handler: handler})
}

func (f *fakeServer) AddPendingInjectedHandler(_ *phttp.InjectedHandler) {
	f.pendingInjecteds++
}

type fakeStreamServer struct {
	fakeServer
	streams []streamCall
}

type streamCall struct {
	path    string
	handler phttp.StreamHandler
}

func (f *fakeStreamServer) HandleStream(path string, handler phttp.StreamHandler) {
	f.streams = append(f.streams, streamCall{path: path, handler: handler})
}

func TestPlugin_Register_DispatchesOnConfigure(t *testing.T) {
	server := &fakeServer{}
	p := New(server)

	p.Register(Endpoint("GET", "/health").
		HandleRaw(func(_ *phttp.Context) *phttp.Response { return phttp.JSON("ok") }))
	p.Register(Endpoint("GET", "/users/{id}").
		Params(Type[IDParams]()).
		Returns(Type[CreatedItem]()).
		Handle(func(_ *phttp.EndpointContext) *phttp.Response { return phttp.JSON(nil) }))

	if len(p.DiscoveredRoutes()) != 0 {
		t.Fatal("routes should be empty before Configure")
	}

	if err := p.Configure(context.Background(), nil); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	if len(server.calls) != 2 {
		t.Fatalf("server.Handle called %d times, want 2", len(server.calls))
	}
	if server.calls[0].method != "GET" || server.calls[0].path != "/health" {
		t.Errorf("call[0] = %+v", server.calls[0])
	}

	routes := p.DiscoveredRoutes()
	if len(routes) != 2 {
		t.Fatalf("DiscoveredRoutes = %d, want 2", len(routes))
	}
	if routes[1].ParamsSchema == nil || routes[1].ParamsSchema.Name() != "IDParams" {
		t.Errorf("route[1].ParamsSchema not captured: %+v", routes[1])
	}
	if routes[1].ReturnsSchema == nil || routes[1].ReturnsSchema.Name() != "CreatedItem" {
		t.Errorf("route[1].ReturnsSchema not captured: %+v", routes[1])
	}
}

func TestPlugin_WithPrefix(t *testing.T) {
	server := &fakeServer{}
	p := New(server, WithPrefix("/v1"))

	p.Register(Endpoint("GET", "/users").
		HandleRaw(func(_ *phttp.Context) *phttp.Response { return phttp.JSON(nil) }))
	p.Register(Endpoint("POST", "items").
		HandleRaw(func(_ *phttp.Context) *phttp.Response { return phttp.JSON(nil) }))

	if err := p.Configure(context.Background(), nil); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	if got := server.calls[0].path; got != "/v1/users" {
		t.Errorf("call[0].path = %q, want /v1/users", got)
	}
	if got := server.calls[1].path; got != "/v1/items" {
		t.Errorf("call[1].path = %q, want /v1/items", got)
	}
}

// WithPrefix goes through the platform protocol's prefix rule, so whitespace and
// slashes at either end mount where the plain prefix does, and "" or "/" mount at
// the root.
func TestPlugin_WithPrefixIsNormalized(t *testing.T) {
	for _, root := range []string{"", "/", " / "} {
		server := &fakeServer{}
		p := New(server, WithPrefix(root))
		p.Register(Endpoint("GET", "/users").
			HandleRaw(func(_ *phttp.Context) *phttp.Response { return phttp.JSON(nil) }))
		if err := p.Configure(context.Background(), nil); err != nil {
			t.Fatalf("Configure(%q): %v", root, err)
		}
		if got := server.calls[0].path; got != "/users" {
			t.Errorf("WithPrefix(%q) mounted %q, want /users", root, got)
		}
	}
	for _, prefix := range []string{"v1", " v1", "//v1", "/v1/", "/ v1"} {
		server := &fakeServer{}
		p := New(server, WithPrefix(prefix))
		p.Register(Endpoint("GET", "/users").
			HandleRaw(func(_ *phttp.Context) *phttp.Response { return phttp.JSON(nil) }))
		if err := p.Configure(context.Background(), nil); err != nil {
			t.Fatalf("Configure(%q): %v", prefix, err)
		}
		if got := server.calls[0].path; got != "/v1/users" {
			t.Errorf("WithPrefix(%q) mounted %q, want /v1/users", prefix, got)
		}
	}
}

func TestPlugin_RootPath(t *testing.T) {
	server := &fakeServer{}
	p := New(server, WithPrefix("/v1"))

	p.Register(Endpoint("GET", "/").
		HandleRaw(func(_ *phttp.Context) *phttp.Response { return phttp.JSON(nil) }))

	if err := p.Configure(context.Background(), nil); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if got := server.calls[0].path; got != "/v1" {
		t.Errorf("root with prefix → %q, want /v1", got)
	}
}

// trackingServer verifies that hookable.AddPendingInjectedHandler is called and that
// finalization errors propagate out of api.Plugin.Configure.
type trackingServer struct {
	*fakeServer
	finalizeErr error
	calls       int
}

func (t *trackingServer) AddPendingInjectedHandler(_ *phttp.InjectedHandler) error {
	t.calls++
	return t.finalizeErr
}

func TestPlugin_PropagatesInjectFinalizationError(t *testing.T) {
	server := &trackingServer{fakeServer: &fakeServer{}, finalizeErr: errFakeFinalize}
	p := New(server)

	// An InjectedHandler with a typed (non-context) parameter forces the bridge to call
	// AddPendingInjectedHandler. The handler is never actually executed in this test.
	ih := phttp.Inject(func(_ *phttp.Context) *phttp.Response { return phttp.JSON(nil) })
	p.Register(Endpoint("GET", "/health").Handle(ih))

	err := p.Configure(context.Background(), nil)
	if err != errFakeFinalize {
		t.Fatalf("Configure returned %v, want %v", err, errFakeFinalize)
	}
	if server.calls != 1 {
		t.Errorf("AddPendingInjectedHandler called %d times, want 1", server.calls)
	}
}

func TestPlugin_DispatchedHandlerExecutes(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "single-declaration", "the-plugin-binds-a-declared-endpoint-to-the-transport")
	server := &fakeServer{}
	p := New(server)

	p.Register(Endpoint("GET", "/ping").
		HandleRaw(func(_ *phttp.Context) *phttp.Response { return phttp.JSON("pong") }))

	if err := p.Configure(context.Background(), nil); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	req := httptest.NewRequest("GET", "/ping", nil)
	resp := server.calls[0].handler(phttp.NewContext(httptest.NewRecorder(), req))
	if resp.Status != 200 {
		t.Errorf("dispatched handler status = %d, want 200", resp.Status)
	}
}

func TestPlugin_CatchAllParam_RealHTTPRoundTrip(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "catch-all-paths", "a-catch-all-parameter-matches-joined-segments-over-real-http")
	sp := phttp.NewServerPlugin(phttp.ServerConfig{})
	p := New(sp)

	type ModuleParams struct {
		Module string `json:"module" validate:"required"`
	}

	p.Register(Endpoint("POST", "/{module...}/-/blobs/upload").
		Params(Type[ModuleParams]()).
		HandleRaw(func(ctx *phttp.Context) *phttp.Response {
			return phttp.JSON(map[string]string{"module": ctx.Param("module")})
		}))

	if err := p.Configure(context.Background(), nil); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	ts := httptest.NewServer(sp.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/go.putnami.dev/protocol/diagnostic/-/blobs/upload", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got, want := body["module"], "go.putnami.dev/protocol/diagnostic"; got != want {
		t.Errorf("captured module = %q, want %q", got, want)
	}
}

func TestPlugin_DispatchesStreamEndpoint(t *testing.T) {
	server := &fakeStreamServer{}
	p := New(server, WithPrefix("/v1"))

	type Notification struct {
		Message string `json:"message"`
	}

	p.Register(Endpoint("GET", "/notifications").
		Returns(StreamOf[Notification]()).
		Handle(ServerStream(func(ctx *ServerStreamContext[Notification]) error {
			return ctx.Send(Notification{Message: "ready"})
		})))

	if err := p.Configure(context.Background(), nil); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	if len(server.streams) != 1 {
		t.Fatalf("HandleStream called %d times, want 1", len(server.streams))
	}
	if got := server.streams[0].path; got != "/v1/notifications" {
		t.Errorf("stream path = %q, want /v1/notifications", got)
	}
	if got := server.streams[0].handler.Mode; got != StreamModeServer {
		t.Errorf("stream mode = %q, want %q", got, StreamModeServer)
	}
	if got := server.streams[0].handler.ReturnsSchema.Name(); got != "Notification" {
		t.Errorf("returns schema = %q, want Notification", got)
	}

	routes := p.DiscoveredRoutes()
	if len(routes) != 1 {
		t.Fatalf("DiscoveredRoutes = %d, want 1", len(routes))
	}
	if routes[0].StreamMode != StreamModeServer {
		t.Errorf("route stream mode = %q, want %q", routes[0].StreamMode, StreamModeServer)
	}
	if routes[0].ReturnsSchema == nil || routes[0].ReturnsSchema.Name() != "Notification" {
		t.Errorf("route returns schema not captured: %+v", routes[0])
	}
}

func TestPlugin_StreamEndpointRequiresStreamServer(t *testing.T) {
	server := &fakeServer{}
	p := New(server)

	type Notification struct {
		Message string `json:"message"`
	}

	p.Register(Endpoint("GET", "/notifications").
		Returns(StreamOf[Notification]()).
		Handle(ServerStream(func(_ *ServerStreamContext[Notification]) error { return nil })))

	if err := p.Configure(context.Background(), nil); err == nil {
		t.Fatal("expected Configure error for server without stream support")
	}
}

func TestPlugin_DocumentOnly_RecordsRouteWithoutTransportBinding(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "document-only", "a-document-only-endpoint-is-not-bound-to-the-transport")
	server := &fakeServer{}
	p := New(server, WithPrefix("/v1"))

	p.Register(Endpoint("GET", "/.well-known/manifest").
		Description("Canonical manifest").
		Returns(Type[CreatedItem]()).
		Document())

	if err := p.Configure(context.Background(), nil); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	if len(server.calls) != 0 {
		t.Errorf("server.Handle called %d times, want 0 for document-only endpoint", len(server.calls))
	}

	routes := p.DiscoveredRoutes()
	if len(routes) != 1 {
		t.Fatalf("DiscoveredRoutes = %d, want 1", len(routes))
	}
	if got, want := routes[0].Path, "/v1/.well-known/manifest"; got != want {
		t.Errorf("route path = %q, want %q", got, want)
	}
	if routes[0].Description != "Canonical manifest" {
		t.Errorf("route description = %q, want %q", routes[0].Description, "Canonical manifest")
	}
	if routes[0].ReturnsSchema == nil || routes[0].ReturnsSchema.Name() != "CreatedItem" {
		t.Errorf("route returns schema not captured: %+v", routes[0])
	}
	if !routes[0].DocumentOnly {
		t.Error("doc-only endpoint should set DocumentOnly=true on the discovered route")
	}
}

func TestPlugin_DocumentOnly_CoexistsWithDirectlyMountedHandler(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "document-only", "a-document-only-endpoint-does-not-shadow-a-directly-mounted-handler")
	// Models the canonical pattern that motivated Document(): the handler is
	// mounted on the transport server *directly* (outside the api builder)
	// and the same path is registered through the api plugin solely to
	// publish its schema for OpenAPI / typed-client codegen.
	//
	// Concretely, the events server does:
	//
	//	server.Handle("GET", "/.well-known/putnami/events", manifestHandler)
	//	api.Register(Endpoint("GET", "/.well-known/putnami/events").Document())
	//
	// The previous shape — both registrations going through api.Plugin —
	// produced two DiscoveredRoute entries with identical (Method, Path)
	// which would in turn make proto.Generate emit duplicate RPC names. The
	// correct pattern produces exactly one discovered route.
	server := &fakeServer{}
	p := New(server)

	server.Handle("GET", "/canonical", func(_ *phttp.Context) *phttp.Response { return phttp.JSON(nil) })

	p.Register(Endpoint("GET", "/canonical").
		Returns(Type[CreatedItem]()).
		Document())

	if err := p.Configure(context.Background(), nil); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	// Only the direct mount called Handle — Configure must not re-register
	// the doc-only definition (which would panic on duplicate route in the
	// real http server).
	if len(server.calls) != 1 {
		t.Errorf("server.Handle called %d times, want 1 (only the direct mount)", len(server.calls))
	}

	routes := p.DiscoveredRoutes()
	if len(routes) != 1 {
		t.Fatalf("DiscoveredRoutes = %d, want 1 (no duplicate (method, path) entries)", len(routes))
	}
	if !routes[0].DocumentOnly {
		t.Error("discovered route should have DocumentOnly=true so consumers can skip it")
	}
}

func TestPlugin_DocumentOnly_StreamRecordsModeWithoutBinding(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "document-only", "a-document-only-stream-records-its-mode-without-binding")
	server := &fakeStreamServer{}
	p := New(server)

	type Notification struct {
		Message string `json:"message"`
	}

	p.Register(Endpoint("GET", "/notifications").
		Returns(StreamOf[Notification]()).
		Document())

	if err := p.Configure(context.Background(), nil); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	if len(server.streams) != 0 {
		t.Errorf("HandleStream called %d times, want 0 for document-only stream", len(server.streams))
	}
	routes := p.DiscoveredRoutes()
	if len(routes) != 1 {
		t.Fatalf("DiscoveredRoutes = %d, want 1", len(routes))
	}
	if routes[0].StreamMode != StreamModeServer {
		t.Errorf("stream mode = %q, want %q", routes[0].StreamMode, StreamModeServer)
	}
	if routes[0].ReturnsSchema == nil || routes[0].ReturnsSchema.Name() != "Notification" {
		t.Errorf("returns schema not captured: %+v", routes[0])
	}
	if !routes[0].DocumentOnly {
		t.Error("doc-only stream should set DocumentOnly=true")
	}
}

// streamInventoryNotification is the message type of the server stream the
// route-inventory test declares.
type streamInventoryNotification struct {
	Message string `json:"message"`
}

// TestPlugin_StreamEndpointsReachTheRouteInventory proves an endpoint that
// Configure dispatches as a stream lands in schema/http-routes.json, the
// public-edge artifact a default-deny gateway policy admits on. A bound stream
// route missing from that file is a route the provider serves and the edge
// refuses. A document-only stream binds no transport, so it must stay out.
func TestPlugin_StreamEndpointsReachTheRouteInventory(t *testing.T) {
	server := phttp.NewServerPlugin(phttp.ServerConfig{})
	p := New(server, WithPrefix("/v1"))

	// A provider-owned byte tunnel: the WebSocket wire the provider owns.
	p.Register(Endpoint("GET", "/tunnel").
		Body(ByteStream()).
		Returns(ByteStream()).
		Handle(ByteTunnel(func(*ByteStreamContext) error { return nil })))
	// A typed server stream on a templated path.
	p.Register(Endpoint("GET", "/rooms/{roomID}/events").
		Returns(StreamOf[streamInventoryNotification]()).
		Handle(ServerStream(func(*ServerStreamContext[streamInventoryNotification]) error { return nil })))
	// A document-only stream: schema metadata, no transport binding.
	p.Register(Endpoint("GET", "/documented").
		Returns(StreamOf[streamInventoryNotification]()).
		Document())
	// A unary endpoint, for the baseline the stream routes must match.
	p.Register(Endpoint("POST", "/messages").
		Handle(func(_ *phttp.EndpointContext) *phttp.Response { return phttp.JSON(nil) }))

	if err := p.Configure(t.Context(), nil); err != nil {
		t.Fatalf("api configure: %v", err)
	}
	if err := server.Configure(t.Context(), app.NewModule("example/go-service")); err != nil {
		t.Fatalf("server configure: %v", err)
	}
	out := t.TempDir()
	if err := server.Describe(&app.DescribeContext{OutputDir: out}); err != nil {
		t.Fatalf("describe: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(out, phttp.HTTPRoutesDescribePath))
	if err != nil {
		t.Fatalf("read inventory: %v", err)
	}
	var manifest struct {
		Routes []struct {
			Match      string   `json:"match"`
			Path       string   `json:"path"`
			Methods    []string `json:"methods"`
			PublicEdge bool     `json:"publicEdge"`
			Provenance struct {
				SourceKind string `json:"sourceKind"`
			} `json:"provenance"`
		} `json:"routes"`
	}
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatalf("decode inventory: %v", err)
	}

	want := map[string]struct {
		match   string
		methods string
	}{
		"/v1/tunnel":                {"exact", "GET,HEAD"},
		"/v1/rooms/{roomID}/events": {"template", "GET,HEAD"},
		"/v1/messages":              {"exact", "POST"},
	}
	for _, route := range manifest.Routes {
		if route.Path == "/v1/documented" {
			t.Fatalf("document-only stream reached the inventory: %s", body)
		}
		expected, known := want[route.Path]
		if !known {
			t.Fatalf("unexpected route %q in %s", route.Path, body)
		}
		if route.Match != expected.match {
			t.Errorf("%s match = %q, want %q", route.Path, route.Match, expected.match)
		}
		if got := strings.Join(route.Methods, ","); got != expected.methods {
			t.Errorf("%s methods = %q, want %q", route.Path, got, expected.methods)
		}
		if route.Provenance.SourceKind != "typed-api" {
			t.Errorf("%s sourceKind = %q, want typed-api", route.Path, route.Provenance.SourceKind)
		}
		if !route.PublicEdge {
			t.Errorf("%s publicEdge = false, want true", route.Path)
		}
		delete(want, route.Path)
	}
	if len(want) != 0 {
		t.Fatalf("missing routes in inventory: %v", want)
	}
}
