package grpc

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"go.putnami.dev/api"
	phttp "go.putnami.dev/http"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

type bridgeUserParams struct {
	ID string `json:"id" validate:"required,uuid"`
}

type bridgePlainIDParams struct {
	ID string `json:"id" validate:"required"`
}

type bridgeListQuery struct {
	Limit int `json:"limit" validate:"min=1,max=100"`
}

type bridgeCreateBody struct {
	Name  string `json:"name" validate:"required"`
	Email string `json:"email" validate:"required,email"`
}

type bridgeUser struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

// fakeServer collects the routes registered by the api plugin and the bridge.
type fakeServer struct {
	calls   map[string]phttp.Handler
	streams map[string]phttp.StreamHandler
}

func newFakeServer() *fakeServer {
	return &fakeServer{calls: map[string]phttp.Handler{}, streams: map[string]phttp.StreamHandler{}}
}

func (f *fakeServer) Handle(method, path string, handler phttp.Handler) {
	f.calls[method+" "+path] = handler
}

func (f *fakeServer) AddPendingInjectedHandler(_ *phttp.InjectedHandler) {}

func TestApiBridge_RegistersRPCPaths(t *testing.T) {
	spectest.Proves(t, "go/grpc-services", "api-bridge", "bridge-registers-connect-style-rpc-paths")
	server := newFakeServer()
	apiPlugin := api.New(server)

	apiPlugin.Register(api.Endpoint("GET", "/users").
		Query(api.Type[bridgeListQuery]()).
		HandleRaw(func(_ *phttp.Context) *phttp.Response { return phttp.JSON([]bridgeUser{}) }))
	apiPlugin.Register(api.Endpoint("POST", "/users").
		Body(api.Type[bridgeCreateBody]()).
		HandleRaw(func(_ *phttp.Context) *phttp.Response { return phttp.JSON(bridgeUser{ID: "1"}) }))
	apiPlugin.Register(api.Endpoint("GET", "/users/{id}").
		Params(api.Type[bridgeUserParams]()).
		HandleRaw(func(_ *phttp.Context) *phttp.Response { return phttp.JSON(bridgeUser{}) }))

	bridge := NewApiBridge(apiPlugin, server, WithPackage("test.v1"))

	if err := apiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("api Configure: %v", err)
	}
	if err := bridge.Configure(context.Background(), nil); err != nil {
		t.Fatalf("bridge Configure: %v", err)
	}

	expected := []string{
		"POST /test.v1.ApiService/ListUsers",
		"POST /test.v1.ApiService/CreateUsers",
		"POST /test.v1.ApiService/GetUsers",
	}
	for _, key := range expected {
		if _, ok := server.calls[key]; !ok {
			t.Errorf("missing bridge route %q; have %v", key, keys(server.calls))
		}
	}
}

func TestApiBridge_ForwardsBodyAndPathParams(t *testing.T) {
	spectest.Proves(t, "go/grpc-services", "api-bridge", "bridge-forwards-body-and-path-params")
	server := newFakeServer()
	apiPlugin := api.New(server)

	var captured *bridgeCreateBody
	var capturedID string
	apiPlugin.Register(api.Endpoint("PUT", "/users/{id}").
		Params(api.Type[bridgeUserParams]()).
		Body(api.Type[bridgeCreateBody]()).
		Handle(func(ctx *phttp.EndpointContext) *phttp.Response {
			body, _ := phttp.BodyAs[bridgeCreateBody](ctx)
			captured = &body
			capturedID = ctx.Params["id"]
			return phttp.JSON(bridgeUser{ID: ctx.Params["id"], Name: body.Name, Email: body.Email})
		}))

	bridge := NewApiBridge(apiPlugin, server, WithPackage("test.v1"))

	if err := apiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("api Configure: %v", err)
	}
	if err := bridge.Configure(context.Background(), nil); err != nil {
		t.Fatalf("bridge Configure: %v", err)
	}

	rpcKey := "POST /test.v1.ApiService/UpdateUsers"
	handler, ok := server.calls[rpcKey]
	if !ok {
		t.Fatalf("RPC route %q not registered; have %v", rpcKey, keys(server.calls))
	}

	// Nested envelope: params and body each have their own "id" key without colliding.
	payload := map[string]any{
		"params": map[string]any{"id": "550e8400-e29b-41d4-a716-446655440000"},
		"body": map[string]any{
			"name":  "Alice",
			"email": "alice@example.com",
		},
	}
	bodyBytes, _ := json.Marshal(payload)
	req := httptest.NewRequest("POST", rpcKey[len("POST "):], bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	resp := handler(phttp.NewContext(httptest.NewRecorder(), req))

	if resp == nil || resp.Status != 200 {
		t.Fatalf("bridge response status = %v", resp)
	}
	if captured == nil {
		t.Fatal("body was not delivered to handler")
	}
	if captured.Name != "Alice" || captured.Email != "alice@example.com" {
		t.Errorf("body = %+v", captured)
	}
	if capturedID != "550e8400-e29b-41d4-a716-446655440000" {
		t.Errorf("path param id = %q", capturedID)
	}
}

func TestApiBridge_PreservesLargeNumericParamText(t *testing.T) {
	spectest.Proves(t, "go/grpc-services", "bridge-validation", "bridge-preserves-large-numeric-param-text")
	server := newFakeServer()
	apiPlugin := api.New(server)

	var capturedID string
	apiPlugin.Register(api.Endpoint("GET", "/large/{id}").
		Params(api.Type[bridgePlainIDParams]()).
		Handle(func(ctx *phttp.EndpointContext) *phttp.Response {
			capturedID = ctx.Params["id"]
			return phttp.JSON(bridgeUser{ID: capturedID})
		}))

	bridge := NewApiBridge(apiPlugin, server, WithPackage("test.v1"))
	if err := apiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("api Configure: %v", err)
	}
	if err := bridge.Configure(context.Background(), nil); err != nil {
		t.Fatalf("bridge Configure: %v", err)
	}

	rpcKey := "POST /test.v1.ApiService/GetLarge"
	handler, ok := server.calls[rpcKey]
	if !ok {
		t.Fatalf("RPC route %q not registered; have %v", rpcKey, keys(server.calls))
	}

	req := httptest.NewRequest("POST", rpcKey[len("POST "):],
		strings.NewReader(`{"params": {"id": 9007199254740993}}`))
	req.Header.Set("Content-Type", "application/json")
	resp := handler(phttp.NewContext(httptest.NewRecorder(), req))

	if resp == nil || resp.Status != 200 {
		t.Fatalf("bridge response status = %v", resp)
	}
	if capturedID != "9007199254740993" {
		t.Errorf("path param id = %q, want exact numeric text", capturedID)
	}
}

func TestBridgeValueToString(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		// JSON numbers decode to float64; large integer IDs must not be rendered
		// in scientific notation (the 1000000 -> "1e+06" corruption).
		{"large integer id", float64(1000000), "1000000"},
		{"larger integer id", float64(123456789), "123456789"},
		{"max safe-ish integer", float64(9007199254740992), "9007199254740992"},
		{"json number exact large id", json.Number("9007199254740993"), "9007199254740993"},
		{"fractional", float64(1.5), "1.5"},
		{"zero", float64(0), "0"},
		{"string passthrough", "abc", "abc"},
		{"bool", true, "true"},
		{"nil", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := bridgeValueToString(tc.in); got != tc.want {
				t.Errorf("bridgeValueToString(%#v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestApiBridge_ForwardsQueryParams(t *testing.T) {
	spectest.Proves(t, "go/grpc-services", "api-bridge", "bridge-forwards-query-params")
	server := newFakeServer()
	apiPlugin := api.New(server)

	var capturedLimit int
	apiPlugin.Register(api.Endpoint("GET", "/users").
		Query(api.Type[bridgeListQuery]()).
		Handle(func(ctx *phttp.EndpointContext) *phttp.Response {
			q, _ := phttp.QueryAs[bridgeListQuery](ctx)
			capturedLimit = q.Limit
			return phttp.JSON([]bridgeUser{})
		}))

	bridge := NewApiBridge(apiPlugin, server, WithPackage("test.v1"))
	if err := apiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("api Configure: %v", err)
	}
	if err := bridge.Configure(context.Background(), nil); err != nil {
		t.Fatalf("bridge Configure: %v", err)
	}

	rpcKey := "POST /test.v1.ApiService/ListUsers"
	handler := server.calls[rpcKey]

	body := strings.NewReader(`{"query": {"limit": 25}}`)
	req := httptest.NewRequest("POST", rpcKey[len("POST "):], body)
	req.Header.Set("Content-Type", "application/json")
	resp := handler(phttp.NewContext(httptest.NewRecorder(), req))

	if resp == nil || resp.Status != 200 {
		t.Fatalf("bridge response status = %v", resp)
	}
	if capturedLimit != 25 {
		t.Errorf("limit = %d, want 25", capturedLimit)
	}
}

func TestApiBridge_RejectsInvalidJSON(t *testing.T) {
	spectest.Proves(t, "go/grpc-services", "bridge-validation", "bridge-rejects-invalid-json")
	server := newFakeServer()
	apiPlugin := api.New(server)

	apiPlugin.Register(api.Endpoint("POST", "/users").
		Body(api.Type[bridgeCreateBody]()).
		HandleRaw(func(_ *phttp.Context) *phttp.Response { return phttp.JSON(nil) }))

	bridge := NewApiBridge(apiPlugin, server)
	if err := apiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("api Configure: %v", err)
	}
	if err := bridge.Configure(context.Background(), nil); err != nil {
		t.Fatalf("bridge Configure: %v", err)
	}

	handler := server.calls["POST /api.v1.ApiService/CreateUsers"]
	req := httptest.NewRequest("POST", "/api.v1.ApiService/CreateUsers", strings.NewReader("not-json"))
	req.Header.Set("Content-Type", "application/json")
	resp := handler(phttp.NewContext(httptest.NewRecorder(), req))

	if resp == nil || resp.Status != 400 {
		t.Errorf("expected 400 for malformed JSON, got %+v", resp)
	}
}

func TestApiBridge_SkipsDocumentOnlyEndpoints(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "document-only", "a-document-only-route-is-absent-from-the-connect-bridge")
	spectest.Proves(t, "go/grpc-services", "api-bridge", "bridge-skips-document-only-endpoints")
	// Document-only endpoints have no handler attached to the api plugin —
	// the real handler is mounted on the http server directly. Bridging them
	// would expose a Connect-style RPC URL whose handler returns 500
	// ("invalid handler type"); the bridge must skip them entirely.
	server := newFakeServer()
	apiPlugin := api.New(server)

	apiPlugin.Register(api.Endpoint("GET", "/users").
		Query(api.Type[bridgeListQuery]()).
		HandleRaw(func(_ *phttp.Context) *phttp.Response { return phttp.JSON([]bridgeUser{}) }))
	apiPlugin.Register(api.Endpoint("GET", "/.well-known/manifest").
		Returns(api.Type[bridgeUser]()).
		Document())

	bridge := NewApiBridge(apiPlugin, server, WithPackage("test.v1"))
	if err := apiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("api Configure: %v", err)
	}
	if err := bridge.Configure(context.Background(), nil); err != nil {
		t.Fatalf("bridge Configure: %v", err)
	}

	if _, ok := server.calls["POST /test.v1.ApiService/ListUsers"]; !ok {
		t.Error("expected handler-bound endpoint to be bridged")
	}
	for key := range server.calls {
		if strings.Contains(key, "WellKnownManifest") {
			t.Errorf("doc-only endpoint should not be bridged; found %q", key)
		}
	}
}

// A route an external authority owns has no protobuf method. Without a
// published descriptor the bridge computes a URL for every other route, so the
// external one must be skipped explicitly rather than handed a computed name.
func TestApiBridge_SkipsEndpointsAnExternalAuthorityOwns(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "external-contract-operations", "an-external-route-is-absent-from-the-connect-bridge")
	server := newFakeServer()
	apiPlugin := api.New(server, api.WithClientService(api.ClientServiceOptions{
		Service:     clientcontract.Service{ID: "registry", Audience: "registry"},
		Credentials: map[string]clientcontract.CredentialProfile{},
	}))
	apiPlugin.Register(api.Endpoint("GET", "/v2/_putnami/capabilities").
		Returns(api.Type[bridgeUser]()).
		HandleRaw(func(_ *phttp.Context) *phttp.Response { return phttp.JSON(bridgeUser{}) }))
	apiPlugin.Register(api.Endpoint("GET", "/v2/{name}/manifests/{reference}").
		Client(api.ClientOperationOptions{External: "OCI Distribution Specification v1.1"}).
		HandleRaw(func(_ *phttp.Context) *phttp.Response { return phttp.JSON(nil) }))

	bridge := NewApiBridge(apiPlugin, server, WithPackage("test.v1"))
	if err := apiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("api Configure: %v", err)
	}
	if err := bridge.Configure(context.Background(), nil); err != nil {
		t.Fatalf("bridge Configure: %v", err)
	}

	if _, ok := server.calls["GET /v2/{name}/manifests/{reference}"]; !ok {
		t.Fatal("the external route is no longer served over REST")
	}
	bridged := 0
	for key := range server.calls {
		if !strings.HasPrefix(key, "POST /test.v1.") {
			continue
		}
		bridged++
		if strings.Contains(key, "Manifests") {
			t.Errorf("the external route is bridged to Connect as %q", key)
		}
	}
	if bridged != 1 {
		t.Fatalf("bridged %d routes, want only the first-party one: %v", bridged, server.calls)
	}
}

// TestApiBridge_WithServiceOverridesServiceName verifies that constructing
// NewApiBridge with an option setting ServiceName to "Custom" causes the
// bridged RPC path to use "Custom" as the service name segment instead of
// the default "ApiService".
func TestApiBridge_WithServiceOverridesServiceName(t *testing.T) {
	spectest.Proves(t, "go/grpc-services", "api-bridge", "bridge-service-name-is-overridable")
	server := newFakeServer()
	apiPlugin := api.New(server)

	apiPlugin.Register(api.Endpoint("GET", "/items").
		Query(api.Type[bridgeListQuery]()).
		HandleRaw(func(_ *phttp.Context) *phttp.Response { return phttp.JSON(nil) }))

	bridge := NewApiBridge(apiPlugin, server, func(c *ApiBridgeConfig) { c.ServiceName = "Custom" })

	if err := apiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("api Configure: %v", err)
	}
	if err := bridge.Configure(context.Background(), nil); err != nil {
		t.Fatalf("bridge Configure: %v", err)
	}

	// Default package is "api.v1"; service must be overridden to "Custom".
	want := "POST /api.v1.Custom/ListItems"
	if _, ok := server.calls[want]; !ok {
		t.Errorf("expected bridge route %q; have %v", want, keys(server.calls))
	}
	// The default "ApiService" name must NOT appear.
	for key := range server.calls {
		if strings.Contains(key, "ApiService") {
			t.Errorf("default service name still present: %q (ServiceName override not applied)", key)
		}
	}
}

func keys(m map[string]phttp.Handler) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// --- The mounted Connect URL is the descriptor's method identity ---

// bridgeProtoPlugin stands in for the proto plugin: it publishes a descriptor
// and its route binding on the api plugin exactly as proto.Plugin.Configure
// does, without pulling the proto document generator into this module.
func publishBridgeProjection(apiPlugin *api.Plugin, routes map[string]string) {
	descriptor := &clientcontract.ProtobufDescriptor{
		Syntax:   "proto3",
		Package:  "api.v1",
		Services: []clientcontract.ProtobufService{{Name: "ApiService"}},
		Messages: []clientcontract.ProtobufMessage{},
		Enums:    []clientcontract.ProtobufEnum{},
	}
	apiPlugin.PublishClientProtobuf(api.ClientProtobufProjection{Descriptor: descriptor, RouteMethods: routes})
}

// Two routes reducing to the same base name (POST /users and POST /users/{id}
// both yield CreateUsers) used to mount one URL twice while the descriptor
// declared two methods, so a generated client called a method nothing served.
func TestApiBridge_MountsTheDescriptorMethodIdentities(t *testing.T) {
	spectest.Proves(t, "go/grpc-services", "api-bridge", "bridge-mounts-the-declared-protobuf-method-identity")
	server := newFakeServer()
	apiPlugin := api.New(server, api.WithClientService(api.ClientServiceOptions{
		Service: clientcontract.Service{ID: "users", Audience: "urn:users"},
	}))
	apiPlugin.Register(api.Endpoint("POST", "/users").
		Body(api.Type[bridgeCreateBody]()).
		HandleRaw(func(_ *phttp.Context) *phttp.Response { return phttp.JSON(bridgeUser{}) }))
	apiPlugin.Register(api.Endpoint("POST", "/users/{id}").
		Params(api.Type[bridgePlainIDParams]()).
		HandleRaw(func(_ *phttp.Context) *phttp.Response { return phttp.JSON(bridgeUser{}) }))
	if err := apiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("api Configure: %v", err)
	}
	publishBridgeProjection(apiPlugin, map[string]string{
		"POST /users":      "/api.v1.ApiService/CreateUsers",
		"POST /users/{id}": "/api.v1.ApiService/CreateUsersById",
	})

	bridge := NewApiBridge(apiPlugin, server)
	if err := bridge.Configure(context.Background(), nil); err != nil {
		t.Fatalf("bridge Configure: %v", err)
	}
	if err := bridge.Start(context.Background(), nil); err != nil {
		t.Fatalf("bridge Start: %v", err)
	}
	for _, want := range []string{"POST /api.v1.ApiService/CreateUsers", "POST /api.v1.ApiService/CreateUsersById"} {
		if _, ok := server.calls[want]; !ok {
			t.Errorf("route %q was not mounted; have %v", want, sortedCallKeys(server))
		}
	}
	encodings := apiPlugin.ClientConnectEncodings()
	if len(encodings) != 2 || encodings[0] != clientcontract.EncodingJSON || encodings[1] != clientcontract.EncodingProto {
		t.Errorf("published Connect encodings = %v, want [json proto] — a published descriptor is the proto codec", encodings)
	}
}

// Registration order decides whether the descriptor binding is available during
// Configure. An unlucky order must fail loudly instead of publishing a contract
// whose method identities no URL serves.
func TestApiBridge_StartRefusesUrlsThatAreNotTheDeclaredMethod(t *testing.T) {
	spectest.Proves(t, "go/grpc-services", "api-bridge", "bridge-refuses-to-start-when-its-urls-are-not-the-declared-methods")
	server := newFakeServer()
	apiPlugin := api.New(server, api.WithClientService(api.ClientServiceOptions{
		Service: clientcontract.Service{ID: "users", Audience: "urn:users"},
	}))
	apiPlugin.Register(api.Endpoint("POST", "/users").
		Body(api.Type[bridgeCreateBody]()).
		HandleRaw(func(_ *phttp.Context) *phttp.Response { return phttp.JSON(bridgeUser{}) }))
	if err := apiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("api Configure: %v", err)
	}

	// The bridge configures first: it mounts its own locally computed name.
	bridge := NewApiBridge(apiPlugin, server)
	if err := bridge.Configure(context.Background(), nil); err != nil {
		t.Fatalf("bridge Configure: %v", err)
	}
	// The proto plugin then publishes a different identity for the same route.
	publishBridgeProjection(apiPlugin, map[string]string{"POST /users": "/api.v1.ApiService/CreateUsersById"})

	err := bridge.Start(context.Background(), nil)
	if err == nil {
		t.Fatal("Start accepted a URL that is not the declared protobuf method")
	}
	if !strings.Contains(err.Error(), "the protobuf descriptor declares") {
		t.Errorf("error = %v, want it to name the descriptor disagreement", err)
	}
}

// A client or bidirectional stream has no Connect URL by design, and the
// descriptor still declares its method. Start must not read that as a
// registration-order mistake: a provider that mixes WebSocket duplex streams
// with the Connect bridge starts, and still mounts nothing for the duplex one.
func TestApiBridge_StartAcceptsADeclaredDuplexStreamItDoesNotMount(t *testing.T) {
	server := newFakeServer()
	apiPlugin := api.New(server, api.WithClientService(api.ClientServiceOptions{
		Service: clientcontract.Service{ID: "users", Audience: "urn:users"},
	}))
	apiPlugin.Register(api.Endpoint("POST", "/users").
		Body(api.Type[bridgeCreateBody]()).
		HandleRaw(func(_ *phttp.Context) *phttp.Response { return phttp.JSON(bridgeUser{}) }))
	apiPlugin.Register(api.Endpoint("GET", "/users/{id}/edits").
		Params(api.Type[bridgePlainIDParams]()).
		Body(api.StreamOf[bridgeCreateBody]()).
		Returns(api.Type[bridgeUser]()).
		Handle(api.ClientStream(func(*api.ClientStreamContext[bridgeCreateBody, bridgeUser]) error { return nil })))
	if err := apiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("api Configure: %v", err)
	}
	publishBridgeProjection(apiPlugin, map[string]string{
		"POST /users":           "/api.v1.ApiService/CreateUsers",
		"GET /users/{id}/edits": "/api.v1.ApiService/GetUsersEdits",
	})

	bridge := NewApiBridge(apiPlugin, server)
	if err := bridge.Configure(context.Background(), nil); err != nil {
		t.Fatalf("bridge Configure: %v", err)
	}
	if err := bridge.Start(context.Background(), nil); err != nil {
		t.Fatalf("bridge Start refused a provider with a declared client stream: %v", err)
	}
	if _, mounted := server.calls["POST /api.v1.ApiService/GetUsersEdits"]; mounted {
		t.Error("the bridge mounted a Connect URL for a client stream")
	}
}

func sortedCallKeys(server *fakeServer) []string {
	keys := make([]string, 0, len(server.calls))
	for key := range server.calls {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// A published descriptor that leaves a route out — the proto projection does
// that for a route carrying opaque JSON, which has no lossless proto3 form —
// gets no Connect URL for it. A locally computed name would serve a method the
// descriptor does not declare. The route keeps its REST transport.
func TestApiBridge_MountsNoURLForARouteTheDescriptorLeavesOut(t *testing.T) {
	server := newFakeServer()
	apiPlugin := api.New(server, api.WithClientService(api.ClientServiceOptions{
		Service: clientcontract.Service{ID: "users", Audience: "urn:users"},
	}))
	apiPlugin.Register(api.Endpoint("POST", "/users").
		Body(api.Type[bridgeCreateBody]()).
		HandleRaw(func(_ *phttp.Context) *phttp.Response { return phttp.JSON(bridgeUser{}) }))
	apiPlugin.Register(api.Endpoint("GET", "/audit").
		HandleRaw(func(_ *phttp.Context) *phttp.Response { return phttp.JSON(map[string]any{}) }))
	if err := apiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("api Configure: %v", err)
	}
	publishBridgeProjection(apiPlugin, map[string]string{"POST /users": "/api.v1.ApiService/CreateUsers"})

	bridge := NewApiBridge(apiPlugin, server)
	if err := bridge.Configure(context.Background(), nil); err != nil {
		t.Fatalf("bridge Configure: %v", err)
	}
	if err := bridge.Start(context.Background(), nil); err != nil {
		t.Fatalf("bridge Start: %v", err)
	}
	mounted := sortedCallKeys(server)
	if _, ok := server.calls["POST /api.v1.ApiService/CreateUsers"]; !ok {
		t.Fatalf("the declared method was not mounted; have %v", mounted)
	}
	for _, key := range mounted {
		if strings.HasPrefix(key, "POST /api.v1.ApiService/") && key != "POST /api.v1.ApiService/CreateUsers" {
			t.Errorf("the bridge mounted %s for a route the descriptor leaves out", key)
		}
	}
}
