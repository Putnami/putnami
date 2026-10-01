package openapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/api"
	"go.putnami.dev/app"
	perrors "go.putnami.dev/errors"
	phttp "go.putnami.dev/http"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/security"
)

// GatewayConnectQuery names the database a byte stream opens. It travels on
// the upgrade request, like the credential.
type GatewayConnectQuery struct {
	Database string `json:"database" validate:"required"`
}

// GatewayEventIn and GatewayEventOut are the provider's own frame vocabulary.
// The framework carries them as JSON values and never wraps them.
type GatewayEventIn struct {
	Type  string `json:"type" validate:"required"`
	Topic string `json:"topic,omitempty"`
}

// GatewayEventOut is one frame the provider sends.
type GatewayEventOut struct {
	Type string `json:"type" validate:"required"`
	Data string `json:"data,omitempty"`
}

// gatewayFrameBytes is small on purpose: the emitted byte stream must split a
// larger write under the declared frame budget, and the provider must accept it.
const gatewayFrameBytes = 64

// providerWireGateway declares the two provider-owned wires the way a
// db-gateway and an event-server would: a byte tunnel whose credential and
// target travel on the upgrade, and a typed JSON frame stream under
// putnami.events.v1.
func providerWireGateway() (*phttp.ServerPlugin, *api.Plugin) {
	frameBytes := int64(gatewayFrameBytes)
	httpServer := phttp.NewServerPlugin(phttp.ServerConfig{})
	httpServer.Use(security.IdentityResolver(func(ctx *phttp.Context) *phttp.Claims {
		if ctx.Header("X-Gateway-Key") != "gateway-secret" {
			return nil
		}
		return &phttp.Claims{Subject: "gateway-consumer", ClientID: ctx.Header("X-Client-Id")}
	}))
	apiPlugin := api.New(httpServer, api.WithClientService(api.ClientServiceOptions{
		Service:     clientcontract.Service{ID: "gateway", Audience: "https://gateway.internal"},
		Credentials: map[string]clientcontract.CredentialProfile{"gateway-key": {Kind: clientcontract.CredentialAPIKey, Header: "X-Gateway-Key"}},
		Defaults: &clientcontract.Defaults{Resilience: &clientcontract.ResiliencePolicy{
			Stream: &clientcontract.StreamPolicy{MaxFrameBytes: &frameBytes},
		}},
	}))
	secured := api.ClientOperationOptions{Security: clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{
		AllOf: []clientcontract.SecurityRequirement{{Profile: "gateway-key"}},
	}}}}
	apiPlugin.Register(api.Endpoint("GET", "/v1/databases/connect").
		Description("Open a byte tunnel to one database").
		Query(api.Type[GatewayConnectQuery]()).
		Body(api.ByteStream()).
		Returns(api.ByteStream()).
		Secure(security.Options{Client: []string{"gateway.consumer"}}).
		Client(secured).
		MayThrow(perrors.CodeUnauthorized).
		Handle(api.ByteTunnel(func(ctx *api.ByteStreamContext) error {
			if _, err := ctx.Write([]byte("db=" + ctx.QueryParams().Get("database"))); err != nil {
				return err
			}
			_, err := io.Copy(ctx, ctx)
			return err
		})))
	apiPlugin.Register(api.Endpoint("GET", "/events/ws").
		Description("Subscribe to events").
		Body(api.StreamOf[GatewayEventIn]()).
		Returns(api.StreamOf[GatewayEventOut]()).
		Subprotocol("putnami.events.v1").
		Secure(security.Options{Client: []string{"gateway.consumer"}}).
		Client(secured).
		Handle(api.BidiStream(func(stream *api.BidiStreamContext[GatewayEventIn, GatewayEventOut]) error {
			for frame := range stream.Messages() {
				switch frame.Type {
				case "subscribe":
					for _, data := range []string{frame.Topic + "-1", frame.Topic + "-2"} {
						if err := stream.Send(GatewayEventOut{Type: "event", Data: data}); err != nil {
							return err
						}
					}
				case "done":
					return nil
				}
			}
			return stream.Err()
		})))
	return httpServer, apiPlugin
}

func TestTheContractPublishesAProviderOwnedWireAsTheOperationsOnlyTransport(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "provider-owned-websocket-wires",
		"the-contract-publishes-the-provider-owned-wire-as-the-only-transport")
	_, apiPlugin := providerWireGateway()
	openapiPlugin := NewPlugin(PluginOptions{Title: "Gateway", Version: "1.0.0"}).From(apiPlugin)
	if err := apiPlugin.Configure(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if err := openapiPlugin.Configure(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	raw, err := openapiPlugin.OpenAPISpecJSON()
	if err != nil {
		t.Fatalf("publish the provider document: %v", err)
	}
	var document struct {
		Paths map[string]map[string]struct {
			Description string                     `json:"description"`
			Responses   map[string]json.RawMessage `json:"responses"`
			Client      struct {
				Stream     string            `json:"stream"`
				Messages   json.RawMessage   `json:"messages"`
				Transports []json.RawMessage `json:"transports"`
			} `json:"x-putnami-client"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	connect := document.Paths["/v1/databases/connect"]["get"]
	if len(connect.Client.Transports) != 1 || compactJSON(t, connect.Client.Transports[0]) !=
		`{"encoding":"binary","path":"/v1/databases/connect","protocol":"websocket","websocket":{"resume":false,"wire":"provider"}}` {
		t.Fatalf("byte stream transports = %s", connect.Client.Transports)
	}
	if connect.Client.Stream != "bidirectional" || connect.Client.Messages != nil {
		t.Fatalf("byte stream = %s with messages %s, want bidirectional without messages", connect.Client.Stream, connect.Client.Messages)
	}
	if _, ok := connect.Responses["101"]; !ok || !strings.Contains(connect.Description, "Raw octets in binary messages") {
		t.Fatalf("byte stream operation = %q %v", connect.Description, connect.Responses)
	}
	events := document.Paths["/events/ws"]["get"]
	if len(events.Client.Transports) != 1 || compactJSON(t, events.Client.Transports[0]) !=
		`{"encoding":"json","path":"/events/ws","protocol":"websocket","websocket":{"resume":false,"subprotocol":"putnami.events.v1","wire":"provider"}}` {
		t.Fatalf("typed wire transports = %s", events.Client.Transports)
	}
	if !strings.Contains(string(events.Client.Messages), `"input"`) || !strings.Contains(string(events.Client.Messages), `"output"`) {
		t.Fatalf("typed wire messages = %s", events.Client.Messages)
	}
	if _, err := api.ReadOpenAPISpec(raw); err != nil {
		t.Fatalf("the strict reader refused the published provider wires: %v", err)
	}

	// A declared transport order cannot invent a transport beside the wire.
	httpServer := phttp.NewServerPlugin(phttp.ServerConfig{})
	narrowed := api.New(httpServer, api.WithClientService(api.ClientServiceOptions{
		Service: clientcontract.Service{ID: "gateway", Audience: "https://gateway.internal"},
	}))
	narrowed.Register(api.Endpoint("GET", "/events/ws").
		Body(api.StreamOf[GatewayEventIn]()).Returns(api.StreamOf[GatewayEventOut]()).
		Subprotocol("putnami.events.v1").
		Client(api.ClientOperationOptions{Transports: []clientcontract.TransportProtocol{clientcontract.TransportSSE}}).
		Handle(api.BidiStream(func(*api.BidiStreamContext[GatewayEventIn, GatewayEventOut]) error { return nil })))
	refused := NewPlugin(PluginOptions{Title: "Gateway", Version: "1.0.0"}).From(narrowed)
	if err := narrowed.Configure(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	err = refused.Configure(t.Context(), nil)
	if err == nil {
		_, err = refused.OpenAPISpecJSON()
	}
	if err == nil || !strings.Contains(err.Error(), `"sse"`) {
		t.Fatalf("an SSE order on a provider-owned wire was published: %v", err)
	}
}

// compactJSON strips the published document's indentation, so a member is
// compared by its exact canonical bytes: the document sorts object keys.
func compactJSON(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		t.Fatal(err)
	}
	return compact.String()
}

// providerWireConsumer runs inside a throwaway module, in the generated
// package, against the provider above over a real socket.
const providerWireConsumer = `package gatewayclient

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"testing"

	"go.putnami.dev/client"
)

func bind(t *testing.T, key string) *GatewayClient {
	t.Helper()
	transport, err := client.NewServiceClientBinding(client.ServiceBinding{
		URL: os.Getenv("GATEWAY_PROVIDER_URL"), ClientID: "gateway.consumer", AllowInsecure: true,
		Credentials: map[string]client.CredentialBinding{"gateway-key": {Source: client.CredentialSourceStatic, Value: key}},
	}, serviceDescriptor)
	if err != nil {
		t.Fatal(err)
	}
	return NewGatewayClient(transport)
}

func TestTheByteStreamIsAReadWriteCloserOverTheProviderTunnel(t *testing.T) {
	ctx := context.Background()
	var in ListV1DatabasesConnectInput
	in.Query.Database = "main"
	tunnel, err := bind(t, "gateway-secret").ListV1DatabasesConnect(ctx, in)
	if err != nil {
		t.Fatalf("open the tunnel: %v", err)
	}
	var stream io.ReadWriteCloser = tunnel
	greeting := make([]byte, len("db=main"))
	if _, err := io.ReadFull(stream, greeting); err != nil || string(greeting) != "db=main" {
		t.Fatalf("greeting = %q, %v", greeting, err)
	}
	// 300 octets that are neither UTF-8 nor JSON, past the 64-octet frame
	// budget: the runtime splits them, the provider echoes every one.
	payload := bytes.Repeat([]byte{0x00, 0xff, 0x80}, 100)
	if n, err := stream.Write(payload); err != nil || n != len(payload) {
		t.Fatalf("write = %d, %v", n, err)
	}
	echoed := make([]byte, len(payload))
	if _, err := io.ReadFull(stream, echoed); err != nil || !bytes.Equal(echoed, payload) {
		t.Fatalf("echo = %v, %v", echoed, err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tunnel.Err(); err != nil {
		t.Fatalf("a normal close ended with %v", err)
	}
	if _, err := stream.Write([]byte{1}); err == nil {
		t.Fatal("a closed tunnel accepted a write")
	}
}

func TestTheUpgradeCarriesTheDeclaredCredential(t *testing.T) {
	var in ListV1DatabasesConnectInput
	in.Query.Database = "main"
	_, err := bind(t, "wrong-secret").ListV1DatabasesConnect(context.Background(), in)
	var remote *client.RemoteError
	if !errors.As(err, &remote) || remote.StatusCode != 401 {
		t.Fatalf("a refused credential answered %v, want the typed 401", err)
	}
}

func TestTheFrameStreamCarriesTheProviderFramesWithoutAnEnvelope(t *testing.T) {
	ctx := context.Background()
	events, err := bind(t, "gateway-secret").ListEventsWs(ctx, ListEventsWsInput{})
	if err != nil {
		t.Fatalf("open the event stream: %v", err)
	}
	topic := "orders"
	if err := events.Send(ctx, GatewayEventIn{Type: "subscribe", Topic: &topic}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"orders-1", "orders-2"} {
		frame, err := events.Recv(ctx)
		if err != nil || frame.Type != "event" || frame.Data == nil || *frame.Data != want {
			t.Fatalf("frame = %+v, %v; want the %s event", frame, err, want)
		}
	}
	if err := events.Send(ctx, GatewayEventIn{Type: "done"}); err != nil {
		t.Fatal(err)
	}
	if _, err := events.Recv(ctx); err != io.EOF {
		t.Fatalf("after the provider ended the stream: %v, want io.EOF", err)
	}
	if err := events.Err(); err != nil {
		t.Fatalf("a normal provider close ended with %v", err)
	}
}
`

// The Go half of provider declaration → generation → compiled client → real
// bound call, for both provider-owned wires: a real api provider declares
// them, the in-app describer generates the Go client from the contract it
// publishes, and the emitted package — compiled against this repository's
// runtime — opens both over a real socket.
func TestTheEmittedGoClientCarriesBothProviderOwnedWiresThroughTheRealRuntime(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "provider-owned-websocket-wires",
		"the-emitted-go-client-carries-both-provider-owned-wires-through-the-real-runtime")
	httpServer, apiPlugin := providerWireGateway()
	openapiPlugin := NewPlugin(PluginOptions{Title: "Gateway", Version: "1.0.0"}).From(apiPlugin)
	clientsPlugin := api.Clients(api.ClientsOptions{
		Go: api.GoClientOptions{PackageName: "gatewayclient", ClientName: "GatewayClient"},
	}).From(apiPlugin)
	application := app.New("gateway")
	application.Use(apiPlugin).Use(openapiPlugin).Use(clientsPlugin)
	out := t.TempDir()
	if err := application.Describe(out, []string{"openapi", "clients"}); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	source, err := os.ReadFile(filepath.Join(out, api.ClientStageDir, "go", "client.gen.go"))
	if err != nil {
		t.Fatalf("read generated client: %v", err)
	}
	for _, want := range []string{
		"(*client.ByteStream, error) {",
		"(*client.FrameStream[GatewayEventIn, GatewayEventOut], error) {",
	} {
		if !strings.Contains(string(source), want) {
			t.Fatalf("generated client is missing %q\n%s", want, source)
		}
	}

	provider := httptest.NewServer(httpServer.Handler())
	defer provider.Close()
	moduleDir := t.TempDir()
	writeGeneratedClientModule(t, moduleDir, string(source))
	if err := os.WriteFile(filepath.Join(moduleDir, "consumer_test.go"), []byte(providerWireConsumer), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("go", "test", "-count=1", "./...")
	command.Dir = moduleDir
	command.Env = append(os.Environ(), "GOPROXY=off", "GOFLAGS=-mod=readonly", "GOWORK=off",
		"GATEWAY_PROVIDER_URL="+provider.URL)
	if output, testErr := command.CombinedOutput(); testErr != nil {
		t.Fatalf("the emitted client failed against the real provider: %v\n%s\n--- source:\n%s", testErr, output, source)
	}
}
