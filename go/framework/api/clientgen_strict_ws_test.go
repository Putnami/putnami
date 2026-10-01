package api

import (
	"strconv"
	"strings"
	"testing"

	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

// wsStreamPath is the one route every declaration in this file is about.
const wsStreamPath = "/items/exchange"

// wsTransport is the transport a first-party provider publishes for a stream it
// serves over the published WebSocket wire.
func wsTransport() clientcontract.Transport {
	return clientcontract.Transport{
		Protocol: clientcontract.TransportWebSocket, Path: wsStreamPath, Encoding: clientcontract.EncodingJSON,
		WebSocket: &clientcontract.WebSocketTransport{Subprotocol: clientcontract.WebSocketSubprotocolV1},
	}
}

// wsStreamMethod builds one declared stream operation, so each test states the
// single fact it is about and nothing else.
func wsStreamMethod(mode clientcontract.StreamMode, transports []clientcontract.Transport,
	messages *clientcontract.MessageShapes) MethodIR {
	return MethodIR{
		Name: "exchangeItems", OperationID: "exchangeItems", HTTPMethod: "GET", Path: wsStreamPath,
		Successes: []SuccessIR{{Status: 200}},
		Client: &clientcontract.OperationV1{
			Stream:      mode,
			Messages:    messages,
			Transports:  transports,
			Security:    clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{}}}},
			Errors:      []clientcontract.DeclaredError{},
			Idempotency: clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe},
		},
	}
}

func wsMessageShapes(input, output *clientcontract.Schema) *clientcontract.MessageShapes {
	return &clientcontract.MessageShapes{Input: input, Output: output}
}

// A server stream has one generated entrypoint whatever its declared transport
// order is: client.OpenOperationServerStream walks the declared list at call
// time — Connect, SSE or the first-party WebSocket wire — opens the first
// transport it can carry, and falls back only when the provider says that wire
// is not served. A fallback is a second opening of the same method, so it cannot
// be a second generated method, which is exactly why the emitter stops choosing
// here. The generated request still follows the first declared transport,
// because that is the path the stream is opened on.
func TestGenerateStrictClient_ServerStreamFollowsDeclaredTransportOrder(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "websocket-stream-emission", "a-server-stream-follows-the-declared-transport-order")

	message := strictSchemaObject(map[string]clientcontract.Schema{"value": {Type: "string"}}, "value")
	sse := clientcontract.Transport{Protocol: clientcontract.TransportSSE, Path: wsStreamPath, Encoding: clientcontract.EncodingJSON}
	socket := wsTransport()
	socket.Path = wsStreamPath + "/socket"
	resuming := wsTransport()
	resuming.WebSocket = &clientcontract.WebSocketTransport{
		Subprotocol: clientcontract.WebSocketSubprotocolV1, Resume: true,
	}
	cases := []struct {
		name       string
		transports []clientcontract.Transport
		wantPath   string
	}{
		{
			name:       "sse declared first",
			transports: []clientcontract.Transport{sse, socket},
			wantPath:   wsStreamPath,
		},
		{
			name:       "websocket declared first",
			transports: []clientcontract.Transport{socket, sse},
			wantPath:   wsStreamPath + "/socket",
		},
		{
			name:       "websocket only",
			transports: []clientcontract.Transport{socket},
			wantPath:   wsStreamPath + "/socket",
		},
		{
			// Resume is a provider capability the runtime honors; it changes no
			// emitted line, so a declaration that carries it emits the same
			// method as one that does not.
			name:       "websocket declaring resume",
			transports: []clientcontract.Transport{resuming},
			wantPath:   wsStreamPath,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			method := wsStreamMethod(clientcontract.StreamServer, testCase.transports, wsMessageShapes(nil, &message))
			source, err := GenerateClientFromIR(strictSpecWith(method, nil),
				ClientGenOptions{PackageName: "itemsclient", ClientName: "ItemsClient"})
			if err != nil {
				t.Fatal(err)
			}
			signature := "func (c *ItemsClient) ExchangeItems(ctx context.Context, in ExchangeItemsInput) (*client.Stream[ExchangeItemsMessage], error)"
			if !containsNormalized(source, signature) {
				t.Errorf("generated server stream missing %q:\n%s", signature, source)
			}
			opener := "return client.OpenOperationServerStream[ExchangeItemsMessage](ctx, c.transport, call, exchangeItemsOperation)"
			if !containsNormalized(source, opener) {
				t.Errorf("generated server stream missing %q:\n%s", opener, source)
			}
			for _, pinned := range []string{"OpenServerStreamWS", "client.OpenServerStream[", "OpenServerStreamConnect"} {
				if strings.Contains(source, pinned) {
					t.Errorf("generated server stream pinned %q at generation time:\n%s", pinned, source)
				}
			}
			path := "path := " + strconv.Quote(testCase.wantPath)
			if !containsNormalized(source, path) {
				t.Errorf("generated server stream missing %q:\n%s", path, source)
			}
		})
	}
}

// A client stream sends typed messages and reads one declared result. The
// generated handle carries both schemas, so a consumer that sends the wrong
// message shape or reads the wrong result fails to compile.
func TestGenerateStrictClient_EmitsTypedClientStream(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "websocket-stream-emission", "a-client-stream-is-emitted-as-a-typed-request-stream")

	input := strictSchemaObject(map[string]clientcontract.Schema{"delta": {Type: "integer", Format: "int64"}}, "delta")
	output := strictSchemaObject(map[string]clientcontract.Schema{"total": {Type: "integer", Format: "int64"}}, "total")
	method := wsStreamMethod(clientcontract.StreamClient,
		[]clientcontract.Transport{wsTransport()}, wsMessageShapes(&input, &output))
	source, err := GenerateClientFromIR(strictSpecWith(method, nil),
		ClientGenOptions{PackageName: "itemsclient", ClientName: "ItemsClient"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"func (c *ItemsClient) ExchangeItems(ctx context.Context, in ExchangeItemsInput) (*client.RequestStream[ExchangeItemsSend, ExchangeItemsMessage], error)",
		"return client.OpenClientStream[ExchangeItemsSend, ExchangeItemsMessage](ctx, c.transport, request, exchangeItemsOperation)",
	} {
		if !containsNormalized(source, want) {
			t.Errorf("generated client stream missing %q:\n%s", want, source)
		}
	}
}

// A bidirectional stream reads and writes at once; its declared result is the
// last value the provider sends before the conversation ends.
func TestGenerateStrictClient_EmitsTypedBidiStream(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "websocket-stream-emission", "a-bidirectional-stream-is-emitted-as-a-typed-bidi-stream")

	input := strictSchemaObject(map[string]clientcontract.Schema{"delta": {Type: "integer", Format: "int64"}}, "delta")
	output := strictSchemaObject(map[string]clientcontract.Schema{"total": {Type: "integer", Format: "int64"}}, "total")
	method := wsStreamMethod(clientcontract.StreamBidirectional,
		[]clientcontract.Transport{wsTransport()}, wsMessageShapes(&input, &output))
	source, err := GenerateClientFromIR(strictSpecWith(method, nil),
		ClientGenOptions{PackageName: "itemsclient", ClientName: "ItemsClient"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"func (c *ItemsClient) ExchangeItems(ctx context.Context, in ExchangeItemsInput) (*client.BidiStream[ExchangeItemsSend, ExchangeItemsMessage], error)",
		"return client.OpenBidiStream[ExchangeItemsSend, ExchangeItemsMessage](ctx, c.transport, request, exchangeItemsOperation)",
	} {
		if !containsNormalized(source, want) {
			t.Errorf("generated bidi stream missing %q:\n%s", want, source)
		}
	}
	// No phase rule reaches the emitted code: the runtime owns the published
	// conversation, and a generated method that numbered frames or tracked a
	// half-close would hold a second, divergent state machine.
	for _, forbidden := range []string{"halfClose", "sequence", "await-ready"} {
		if strings.Contains(source, forbidden) {
			t.Errorf("generated bidi stream re-implements wire state through %q:\n%s", forbidden, source)
		}
	}
}

// Every declaration this generator cannot honor exactly is refused by name at
// generation, never by a runtime that has already opened a socket.
func TestGenerateStrictClient_RefusesUnhonorableStreamDeclarations(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "generation-refuses-unhonorable-declarations", "a-transport-no-runtime-can-honor-is-refused-at-generation")

	message := strictSchemaObject(map[string]clientcontract.Schema{"value": {Type: "string"}}, "value")
	protoWS := wsTransport()
	protoWS.Encoding = clientcontract.EncodingProto
	noSubprotocol := wsTransport()
	noSubprotocol.WebSocket = nil
	resuming := wsTransport()
	resuming.WebSocket = &clientcontract.WebSocketTransport{Subprotocol: clientcontract.WebSocketSubprotocolV1, Resume: true}

	cases := []struct {
		name   string
		method MethodIR
		want   string
	}{
		{
			name:   "websocket proto payload",
			method: wsStreamMethod(clientcontract.StreamBidirectional, []clientcontract.Transport{protoWS}, wsMessageShapes(&message, &message)),
			want:   "protobuf message codec",
		},
		{
			name:   "websocket without the first-party subprotocol",
			method: wsStreamMethod(clientcontract.StreamClient, []clientcontract.Transport{noSubprotocol}, wsMessageShapes(&message, &message)),
			want:   "without the putnami.service.v1 subprotocol",
		},
		{
			name:   "websocket resume",
			method: wsStreamMethod(clientcontract.StreamBidirectional, []clientcontract.Transport{resuming}, wsMessageShapes(&message, &message)),
			want:   "declares websocket resume",
		},
		{
			name: "duplex stream over sse",
			method: wsStreamMethod(clientcontract.StreamClient,
				[]clientcontract.Transport{{Protocol: clientcontract.TransportSSE, Path: wsStreamPath, Encoding: clientcontract.EncodingJSON}},
				wsMessageShapes(&message, &message)),
			want: "declares no transport this client can carry",
		},
		{
			name: "bidi stream without an input schema",
			method: wsStreamMethod(clientcontract.StreamBidirectional,
				[]clientcontract.Transport{wsTransport()}, wsMessageShapes(nil, &message)),
			want: "must declare both an input and an output message schema",
		},
		{
			// A server stream resolves its transport through the shared
			// dispatcher's own selection, which carries Connect as well, so the
			// refusal is that selection's and names the same fact.
			name: "server stream with no carriable transport",
			method: wsStreamMethod(clientcontract.StreamServer,
				[]clientcontract.Transport{{Protocol: clientcontract.TransportRESTJSON, Path: wsStreamPath, Encoding: clientcontract.EncodingJSON}},
				wsMessageShapes(nil, &message)),
			want: "declares no transport the Go client can drive",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := strictGenerationError(t, strictSpecWith(testCase.method, nil))
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("error = %v, want %q", err, testCase.want)
			}
			if !strings.Contains(err.Error(), "exchangeItems") {
				t.Fatalf("error = %v, want the refused operation named", err)
			}
		})
	}
}
