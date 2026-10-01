package openapi

import (
	"context"
	"strings"
	"testing"

	"go.putnami.dev/api"
	phttp "go.putnami.dev/http"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

// transportOrderEvent is the declared message of the streams these tests
// project.
type transportOrderEvent struct {
	ID string `json:"id" validate:"required"`
}

// declaredOrderContract builds one real provider and returns the published
// operation contract of its single stream endpoint, or the refusal the
// declaration earned.
func declaredOrderContract(t *testing.T, endpoint api.EndpointDefinition) (*clientcontract.OperationV1, error) {
	t.Helper()
	httpServer := phttp.NewServerPlugin(phttp.ServerConfig{})
	apiPlugin := api.New(httpServer, api.WithClientService(api.ClientServiceOptions{
		Service: clientcontract.Service{ID: "streams", Audience: "https://streams.internal"},
	}))
	apiPlugin.Register(endpoint)
	openapiPlugin := NewPlugin(PluginOptions{Title: "Streams", Version: "1.0.0"}).From(apiPlugin)
	if err := apiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("api Configure: %v", err)
	}
	// A projection refusal surfaces here: the document fails to serialize with
	// the route named rather than publishing a contract no client could satisfy.
	if err := openapiPlugin.Configure(context.Background(), nil); err != nil {
		return nil, err
	}
	raw, err := openapiPlugin.OpenAPISpecJSON()
	if err != nil {
		return nil, err
	}
	ir, err := api.ReadOpenAPISpec(raw)
	if err != nil {
		t.Fatalf("strict reader rejected the projected contract: %v\n%s", err, raw)
	}
	for _, service := range ir.Services {
		for _, method := range service.Methods {
			if method.OperationID == "getEvents" {
				return method.Client, nil
			}
		}
	}
	t.Fatal("the projected document does not carry the declared operation")
	return nil, nil
}

// transportProtocols names the published transports in published order.
func transportProtocols(operation *clientcontract.OperationV1) []clientcontract.TransportProtocol {
	protocols := make([]clientcontract.TransportProtocol, 0, len(operation.Transports))
	for _, transport := range operation.Transports {
		protocols = append(protocols, transport.Protocol)
	}
	return protocols
}

// The declaration owns the wire preference order. A provider that states
// WebSocket first publishes it first, and every generated client dispatches in
// that order without a consumer flag anywhere.
func TestOpenAPI_OperationDeclaresItsTransportOrder(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "declared-transport-order",
		"a-declared-transport-order-is-the-published-order")

	cases := []struct {
		name     string
		declared []clientcontract.TransportProtocol
		want     []clientcontract.TransportProtocol
	}{
		{
			name: "framework order when the operation declares none",
			want: []clientcontract.TransportProtocol{clientcontract.TransportSSE, clientcontract.TransportWebSocket},
		},
		{
			name:     "websocket declared first",
			declared: []clientcontract.TransportProtocol{clientcontract.TransportWebSocket, clientcontract.TransportSSE},
			want:     []clientcontract.TransportProtocol{clientcontract.TransportWebSocket, clientcontract.TransportSSE},
		},
		{
			name:     "a narrowed order publishes only what it names",
			declared: []clientcontract.TransportProtocol{clientcontract.TransportWebSocket},
			want:     []clientcontract.TransportProtocol{clientcontract.TransportWebSocket},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			operation, err := declaredOrderContract(t, api.Endpoint("GET", "/events").
				Returns(api.StreamOf[transportOrderEvent]()).
				Client(api.ClientOperationOptions{Transports: testCase.declared}).
				Handle(api.ServerStream(func(_ *api.ServerStreamContext[transportOrderEvent]) error { return nil })))
			if err != nil {
				t.Fatal(err)
			}
			got := transportProtocols(operation)
			if len(got) != len(testCase.want) {
				t.Fatalf("published transports = %v, want %v", got, testCase.want)
			}
			for index := range got {
				if got[index] != testCase.want[index] {
					t.Fatalf("published transports = %v, want %v", got, testCase.want)
				}
			}
		})
	}
}

// A declared order cannot add a wire. The derived list is what the bound server
// actually serves, so naming anything else is a refusal with the route named,
// never a published transport nobody answers.
func TestOpenAPI_RefusesADeclaredTransportThisProviderDoesNotServe(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "declared-transport-order",
		"a-declared-order-cannot-publish-a-transport-the-provider-does-not-serve")

	cases := []struct {
		name     string
		declared []clientcontract.TransportProtocol
		want     string
	}{
		{
			name:     "a unary transport for a stream",
			declared: []clientcontract.TransportProtocol{clientcontract.TransportRESTJSON},
			want:     `declares transport "rest-json", which this provider does not serve for a server stream shape`,
		},
		{
			name: "the same transport twice",
			declared: []clientcontract.TransportProtocol{
				clientcontract.TransportSSE, clientcontract.TransportSSE,
			},
			want: `declares transport "sse" twice in its client transport order`,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := declaredOrderContract(t, api.Endpoint("GET", "/events").
				Returns(api.StreamOf[transportOrderEvent]()).
				Client(api.ClientOperationOptions{Transports: testCase.declared}).
				Handle(api.ServerStream(func(_ *api.ServerStreamContext[transportOrderEvent]) error { return nil })))
			if err == nil {
				t.Fatal("an unserved transport was published")
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("refusal = %v, want %q", err, testCase.want)
			}
		})
	}
}

// Resume is declared once, on the operation, and published on the WebSocket
// transport that carries it.
func TestOpenAPI_PublishesDeclaredStreamResume(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "declared-transport-order",
		"declared-stream-resume-is-published-on-the-websocket-transport")

	operation, err := declaredOrderContract(t, api.Endpoint("GET", "/events").
		Returns(api.StreamOf[transportOrderEvent]()).
		Client(api.ClientOperationOptions{
			Transports: []clientcontract.TransportProtocol{clientcontract.TransportWebSocket},
			Resume:     true,
		}).
		Handle(api.ServerStream(func(_ *api.ServerStreamContext[transportOrderEvent]) error { return nil })))
	if err != nil {
		t.Fatal(err)
	}
	if len(operation.Transports) != 1 || operation.Transports[0].WebSocket == nil ||
		!operation.Transports[0].WebSocket.Resume {
		t.Fatalf("published transports = %#v, want a resume-capable websocket transport", operation.Transports)
	}
}

// Resume continues a position the caller already consumed up to. Every shape
// where that is not sound — a duplex conversation, a stream with effects, a
// declaration with no WebSocket to carry it — is refused at projection.
func TestOpenAPI_RefusesResumeOnAShapeItCannotContinue(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "declared-transport-order",
		"resume-is-refused-on-every-shape-that-cannot-be-continued")

	cases := []struct {
		name     string
		endpoint api.EndpointDefinition
		want     string
	}{
		{
			name: "a bidirectional stream",
			endpoint: api.Endpoint("GET", "/events").
				Body(api.StreamOf[transportOrderEvent]()).
				Returns(api.StreamOf[transportOrderEvent]()).
				Client(api.ClientOperationOptions{Resume: true}).
				Handle(api.BidiStream(func(_ *api.BidiStreamContext[transportOrderEvent, transportOrderEvent]) error {
					return nil
				})),
			want: "only a server stream can be resumed",
		},
		{
			name: "a stream the provider did not declare safe",
			endpoint: api.Endpoint("GET", "/events").
				Returns(api.StreamOf[transportOrderEvent]()).
				Client(api.ClientOperationOptions{
					Resume:      true,
					Idempotency: &clientcontract.Idempotency{Kind: clientcontract.IdempotencyNonIdempotent},
				}).
				Handle(api.ServerStream(func(_ *api.ServerStreamContext[transportOrderEvent]) error { return nil })),
			want: "only a safe stream can be resumed",
		},
		{
			name: "an order that drops the websocket transport",
			endpoint: api.Endpoint("GET", "/events").
				Returns(api.StreamOf[transportOrderEvent]()).
				Client(api.ClientOperationOptions{
					Transports: []clientcontract.TransportProtocol{clientcontract.TransportSSE},
					Resume:     true,
				}).
				Handle(api.ServerStream(func(_ *api.ServerStreamContext[transportOrderEvent]) error { return nil })),
			want: "publishes no websocket transport to carry it",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := declaredOrderContract(t, testCase.endpoint)
			if err == nil {
				t.Fatal("an unsupportable resume was published")
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("refusal = %v, want %q", err, testCase.want)
			}
		})
	}
}
