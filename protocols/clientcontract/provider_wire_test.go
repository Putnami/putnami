package clientcontract

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

// providerWireOperation is a valid operation carried by one provider-owned
// WebSocket transport: raw octets when encoding is binary, typed JSON frames
// under a declared subprotocol when it is json.
func providerWireOperation(encoding Encoding, subprotocol string) *OperationV1 {
	operation := &OperationV1{
		Stream: StreamBidirectional,
		Transports: []Transport{{
			Protocol: TransportWebSocket, Path: "/wire", Encoding: encoding,
			WebSocket: &WebSocketTransport{Subprotocol: subprotocol, Wire: WebSocketWireProvider},
		}},
		Security:    Security{Alternatives: []SecurityAlternative{{AllOf: []SecurityRequirement{}}}},
		Errors:      []DeclaredError{},
		Idempotency: Idempotency{Kind: IdempotencyNonIdempotent},
	}
	if encoding == EncodingJSON {
		operation.Messages = &MessageShapes{Input: &Schema{Type: "string"}, Output: &Schema{Type: "string"}}
	}
	return operation
}

func providerWireDocument() *DocumentV1 {
	return &DocumentV1{
		ProtocolVersion: ProtocolVersion,
		Service:         Service{ID: "gateway", Audience: "urn:gateway"},
		Credentials:     map[string]CredentialProfile{},
	}
}

func TestAProviderOwnedWireIsDeclaredWithItsSubprotocolAndEncoding(t *testing.T) {
	spectest.Proves(t, "client-contract/first-party-generated-clients", "provider-owned-websocket-wires",
		"a-provider-owned-wire-is-declared-with-its-encoding-and-subprotocol")
	cases := []struct {
		name     string
		encoding Encoding
		token    string
		wire     string
	}{
		{"byte stream without a subprotocol", EncodingBinary, "",
			`{"protocol":"websocket","path":"/wire","encoding":"binary","websocket":{"resume":false,"wire":"provider"}}`},
		{"byte stream under a declared subprotocol", EncodingBinary, "postgresql.tunnel.v1",
			`{"protocol":"websocket","path":"/wire","encoding":"binary","websocket":{"subprotocol":"postgresql.tunnel.v1","resume":false,"wire":"provider"}}`},
		{"typed frames under a declared subprotocol", EncodingJSON, "putnami.events.v1",
			`{"protocol":"websocket","path":"/wire","encoding":"json","websocket":{"subprotocol":"putnami.events.v1","resume":false,"wire":"provider"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			operation := providerWireOperation(tc.encoding, tc.token)
			if diags := ValidateOperation(operation, providerWireDocument()); diag.HasErrors(diags) {
				t.Fatalf("valid provider wire refused: %v", diags)
			}
			encoded, err := json.Marshal(operation.Transports[0])
			if err != nil {
				t.Fatal(err)
			}
			if string(encoded) != tc.wire {
				t.Fatalf("published transport = %s\nwant %s", encoded, tc.wire)
			}
			parsed, diags := ParseAndValidateOperation(mustMarshal(t, operation), providerWireDocument())
			if diag.HasErrors(diags) {
				t.Fatalf("the published operation does not read back: %v", diags)
			}
			if !reflect.DeepEqual(parsed, operation) {
				t.Fatalf("round trip lost a fact:\n got %#v\nwant %#v", parsed, operation)
			}
			transport := parsed.Transports[0]
			if !transport.ProviderWire() || transport.ByteStream() != (tc.encoding == EncodingBinary) {
				t.Fatalf("ProviderWire=%v ByteStream=%v for %s", transport.ProviderWire(), transport.ByteStream(), tc.encoding)
			}
			if OperationProviderWire(parsed) == nil {
				t.Fatal("OperationProviderWire did not find the declared wire")
			}
		})
	}
	// The generated inventory repeats the same transport and applies the same
	// rules, so a manifest can carry a provider wire.
	manifestDiags := validateTransports(StreamBidirectional, providerWireOperation(EncodingBinary, "").Transports)
	if diag.HasErrors(manifestDiags) {
		t.Fatalf("the manifest transport rules refuse a valid byte stream: %v", manifestDiags)
	}
}

func TestTheFirstPartyWebSocketTransportKeepsItsExactBytes(t *testing.T) {
	spectest.Proves(t, "client-contract/first-party-generated-clients", "provider-owned-websocket-wires",
		"the-first-party-websocket-transport-keeps-its-exact-bytes")
	transport := Transport{
		Protocol: TransportWebSocket, Path: "/widgets/chat", Encoding: EncodingJSON,
		WebSocket: &WebSocketTransport{Subprotocol: WebSocketSubprotocolV1},
	}
	encoded, err := json.Marshal(transport)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"protocol":"websocket","path":"/widgets/chat","encoding":"json","websocket":{"subprotocol":"putnami.service.v1","resume":false}}`
	if string(encoded) != want {
		t.Fatalf("first-party transport bytes changed:\n got %s\nwant %s", encoded, want)
	}
	if transport.ProviderWire() || transport.ByteStream() {
		t.Fatal("a first-party transport reads as a provider-owned wire")
	}
	// The whole shared corpus still reads back to the bytes it was written
	// with: no first-party transport gained a member.
	raw, err := os.ReadFile("fixtures/openapi/valid/full.openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture openAPIFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, item := range fixture.Paths {
		for _, op := range item {
			parsed, diags := ParseOperation(op.Client)
			if diag.HasErrors(diags) {
				t.Fatalf("%s does not parse: %v", op.OperationID, diags)
			}
			for _, candidate := range parsed.Transports {
				if candidate.ProviderWire() || candidate.Encoding == EncodingBinary {
					t.Fatalf("%s reads a first-party transport as provider-owned", op.OperationID)
				}
				if candidate.WebSocket == nil {
					continue
				}
				encoded := mustMarshal(t, candidate.WebSocket)
				var members map[string]json.RawMessage
				if err := json.Unmarshal(encoded, &members); err != nil {
					t.Fatal(err)
				}
				keys := make([]string, 0, len(members))
				for key := range members {
					keys = append(keys, key)
				}
				sort.Strings(keys)
				if !reflect.DeepEqual(keys, []string{"resume", "subprotocol"}) {
					t.Fatalf("%s first-party websocket members = %v", op.OperationID, keys)
				}
			}
		}
	}
}

func TestAMalformedReservedOrUndeclaredProviderWireIsRefused(t *testing.T) {
	spectest.Proves(t, "client-contract/first-party-generated-clients", "provider-owned-websocket-wires",
		"a-malformed-reserved-or-undeclared-provider-wire-is-refused")
	cases := []struct {
		name   string
		mutate func(*OperationV1)
		code   string
		field  string
	}{
		{"typed frames without a subprotocol", func(o *OperationV1) { o.Transports[0].WebSocket.Subprotocol = "" },
			ErrorCodeRequired, "transports[0].websocket.subprotocol"},
		{"the first-party token", func(o *OperationV1) { o.Transports[0].WebSocket.Subprotocol = WebSocketSubprotocolV1 },
			ErrorCodeInvalidTransport, "transports[0].websocket.subprotocol"},
		{"a token in the first-party namespace", func(o *OperationV1) { o.Transports[0].WebSocket.Subprotocol = "putnami.service.v2" },
			ErrorCodeInvalidTransport, "transports[0].websocket.subprotocol"},
		{"a token list smuggled into one token", func(o *OperationV1) { o.Transports[0].WebSocket.Subprotocol = "acme.v1, putnami.service.v1" },
			ErrorCodeInvalidTransport, "transports[0].websocket.subprotocol"},
		{"a header smuggled into a token", func(o *OperationV1) { o.Transports[0].WebSocket.Subprotocol = "acme.v1\r\nAuthorization:x" },
			ErrorCodeInvalidTransport, "transports[0].websocket.subprotocol"},
		{"an unknown wire", func(o *OperationV1) { o.Transports[0].WebSocket.Wire = "custom" },
			ErrorCodeInvalidEnum, "transports[0].websocket.wire"},
		{"the conversation's resume", func(o *OperationV1) { o.Transports[0].WebSocket.Resume = true },
			ErrorCodeInvalidResilience, "transports[0].websocket.resume"},
		{"a proto payload", func(o *OperationV1) {
			o.Transports[0].Encoding = EncodingProto
			o.Transports[0].ProtobufMethod = "/pkg.Service/Method"
		}, ErrorCodeInvalidTransport, "transports[0].encoding"},
		{"a server stream", func(o *OperationV1) {
			o.Stream = StreamServer
			o.Messages = &MessageShapes{Output: &Schema{Type: "string"}}
		}, ErrorCodeInvalidTransport, "transports[0].websocket.wire"},
		{"a second transport", func(o *OperationV1) {
			o.Transports = append(o.Transports, Transport{
				Protocol: TransportWebSocket, Path: "/wire/conversation", Encoding: EncodingJSON,
				WebSocket: &WebSocketTransport{Subprotocol: WebSocketSubprotocolV1},
			})
		}, ErrorCodeInvalidTransport, "transports"},
		{"binary octets on the conversation", func(o *OperationV1) {
			o.Transports[0].WebSocket = &WebSocketTransport{Subprotocol: WebSocketSubprotocolV1}
			o.Transports[0].Encoding = EncodingBinary
		}, ErrorCodeInvalidTransport, "transports[0].encoding"},
		{"binary octets on REST", func(o *OperationV1) {
			o.Stream = StreamUnary
			o.Messages = nil
			o.Transports = []Transport{{Protocol: TransportRESTJSON, Path: "/wire", Encoding: EncodingBinary}}
		}, ErrorCodeInvalidTransport, "transports[0]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			operation := providerWireOperation(EncodingJSON, "acme.chat.v1")
			tc.mutate(operation)
			assertProviderWireRefusal(t, ValidateOperation(operation, providerWireDocument()), tc.code, tc.field)
		})
	}
	t.Run("a message schema beside raw octets", func(t *testing.T) {
		operation := providerWireOperation(EncodingBinary, "")
		operation.Messages = &MessageShapes{Input: &Schema{Type: "string"}, Output: &Schema{Type: "string"}}
		assertProviderWireRefusal(t, ValidateOperation(operation, providerWireDocument()), ErrorCodeInvalidSchema, "messages")
	})
	for _, token := range []string{"", " ", "a b", "a,b", "a;b", "\"quoted\"", "é"} {
		if ValidWebSocketSubprotocol(token) {
			t.Errorf("ValidWebSocketSubprotocol(%q) = true", token)
		}
	}
	for _, token := range []string{"putnami.events.v1", "acme-chat_v1", "a!#$%&'*+-.^_`|~9"} {
		if !ValidWebSocketSubprotocol(token) {
			t.Errorf("ValidWebSocketSubprotocol(%q) = false", token)
		}
	}
}

func TestAFirstPartyRuntimeRefusesAProviderOwnedWire(t *testing.T) {
	spectest.Proves(t, "client-contract/first-party-generated-clients", "provider-owned-websocket-wires",
		"a-first-party-conversation-never-admits-on-a-provider-owned-wire")
	operation := providerWireOperation(EncodingJSON, "acme.chat.v1")
	init := &WebSocketInitFrameV1{
		V: ProtocolVersion, Type: WebSocketFrameInit, OperationID: "chat", ClientID: "consumer",
		DeadlineUnixMs: "0", BudgetMs: "0",
		Credentials: []WebSocketCredentialV1{}, Headers: []WebSocketHeaderV1{},
	}
	diags := ValidateWebSocketInitForOperation(init, "chat", operation, providerWireDocument(), operation.Transports[0])
	assertProviderWireRefusal(t, diags, ErrorCodeInvalidTransport, "transport")
}

// TestThePublishedSchemaEnumeratesTheProviderWire pins the schema's wire and
// encoding vocabulary to the constants the parser accepts.
func TestThePublishedSchemaEnumeratesTheProviderWire(t *testing.T) {
	spectest.Proves(t, "client-contract/first-party-generated-clients", "provider-owned-websocket-wires",
		"a-provider-owned-wire-is-declared-with-its-encoding-and-subprotocol")
	defs := loadSchema(t)["$defs"].(map[string]any)
	websocket := defs["websocketTransport"].(map[string]any)
	wire := websocket["properties"].(map[string]any)["wire"].(map[string]any)["enum"].([]any)
	if !reflect.DeepEqual(wire, []any{string(WebSocketWireProvider)}) {
		t.Fatalf("schema wire enum = %v", wire)
	}
	if required := websocket["required"].([]any); !reflect.DeepEqual(required, []any{"resume"}) {
		t.Fatalf("websocketTransport.required = %v, want [resume]", required)
	}
	encodings := defs["transport"].(map[string]any)["properties"].(map[string]any)["encoding"].(map[string]any)["enum"].([]any)
	if !reflect.DeepEqual(encodings, []any{string(EncodingJSON), string(EncodingProto), string(EncodingBinary)}) {
		t.Fatalf("schema encoding enum = %v", encodings)
	}
}

func assertProviderWireRefusal(t *testing.T, diags []diag.Diagnostic, code, field string) {
	t.Helper()
	for _, diagnostic := range diags {
		if diagnostic.Code == code && diagnostic.Field == field {
			return
		}
	}
	t.Fatalf("diagnostics %v do not contain %s at %s", diags, code, field)
}

func mustMarshal(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
