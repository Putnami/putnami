package api

import (
	"encoding/json"
	"go/parser"
	"go/token"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"go.putnami.dev/errors"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

func strictSchemaObject(properties map[string]clientcontract.Schema, required ...string) clientcontract.Schema {
	allowed := false
	return clientcontract.Schema{
		Type:                 "object",
		Properties:           properties,
		Required:             required,
		AdditionalProperties: &clientcontract.AdditionalProperties{Allowed: &allowed},
	}
}

func strictUnaryOperation(path string, errors []clientcontract.DeclaredError) *clientcontract.OperationV1 {
	return &clientcontract.OperationV1{
		Stream: clientcontract.StreamUnary,
		Transports: []clientcontract.Transport{{
			Protocol: clientcontract.TransportRESTJSON,
			Path:     path,
			Encoding: clientcontract.EncodingJSON,
		}},
		Security:    clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{}}}},
		Errors:      errors,
		Idempotency: clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe},
	}
}

func TestGenerateStrictClient_EmitsTypedBindingSchemasParametersAndErrors(t *testing.T) {
	nullable := true
	user := strictSchemaObject(map[string]clientcontract.Schema{
		"id":       {Type: "integer", Format: "uint64"},
		"nickname": {Type: "string", Nullable: &nullable},
		"tags":     {Type: "array", Items: &clientcontract.Schema{Type: "string", Nullable: &nullable}},
		"metadata": {Type: "object", AdditionalProperties: &clientcontract.AdditionalProperties{Schema: &clientcontract.Schema{Type: "string", Nullable: &nullable}}},
		"state":    {Ref: "#/components/schemas/State"},
	}, "id", "tags", "metadata", "state")
	// ADR 0006: a declared error's schema describes the envelope's `details`
	// member, so it carries business detail and never the envelope's own
	// code/error/message fields.
	errorBody := strictSchemaObject(map[string]clientcontract.Schema{
		"resource": {Type: "string"},
		"reason":   {Type: "string"},
	}, "resource", "reason")
	retryable := false
	operation := strictUnaryOperation("/users/{id}", []clientcontract.DeclaredError{{
		Status: 404, Code: "http.not_found", Schema: &clientcontract.Schema{Ref: "#/components/schemas/ErrorBody"}, Retryable: &retryable,
	}, {Status: 409, Code: "http.conflict"}})
	spec := SpecIR{
		IRVersion: clientIRVersion,
		Contract: &clientcontract.DocumentV1{
			ProtocolVersion: clientcontract.ProtocolVersion,
			Service:         clientcontract.Service{ID: "users", Audience: "https://users.internal"},
			Credentials:     map[string]clientcontract.CredentialProfile{},
		},
		Schemas: map[string]clientcontract.Schema{
			"User":      user,
			"ErrorBody": errorBody,
			"State":     {Type: "string", Enum: []json.RawMessage{json.RawMessage(`"active"`), json.RawMessage(`"disabled"`)}},
		},
		Services: []ServiceIR{{Methods: []MethodIR{{
			Name: "replaceUser", OperationID: "replaceUser", HTTPMethod: "PUT", Path: "/users/{id}",
			Parameters: []ParameterIR{
				{Name: "id", Location: "path", Required: true, Schema: clientcontract.Schema{Type: "integer", Format: "uint64"}},
				{Name: "tag", Location: "query", Schema: clientcontract.Schema{Type: "array", Items: &clientcontract.Schema{Type: "string"}}},
				{Name: "X-Region", Location: "header", Required: true, Schema: clientcontract.Schema{Type: "string"}},
			},
			Request:   &RequestIR{Required: true, Content: []ContentIR{{MediaType: "application/json", Schema: &clientcontract.Schema{Ref: "#/components/schemas/User"}}}},
			Successes: []SuccessIR{{Status: 200, Content: []ContentIR{{MediaType: "application/json", Schema: &clientcontract.Schema{Ref: "#/components/schemas/User"}}}}},
			Client:    operation,
		}}}},
	}

	source, err := GenerateClientFromIR(spec, ClientGenOptions{PackageName: "usersclient", ClientName: "UsersClient"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parser.ParseFile(token.NewFileSet(), "client.gen.go", source, parser.AllErrors); err != nil {
		t.Fatalf("generated source does not parse: %v\n%s", err, source)
	}
	for _, want := range []string{
		"func RegisterUsersClient(",
		"func (c *UsersClient) ReplaceUser(",
		"Tags []*string",
		"Metadata map[string]*string",
		"Tag *[]string",
		"client.EncodeJSON(in.Body)",
		"type ReplaceUserHttpNotFoundError struct",
		"Payload *ErrorBody",
		"Payload is the",
		"provider-declared details body",
		"type ReplaceUserHttpConflictError struct",
		`case "http.conflict":`,
		"query.Add(\"tag\", value)",
	} {
		if !containsNormalized(source, want) {
			t.Errorf("generated source missing %q:\n%s", want, source)
		}
	}
	if strings.Contains(source, "map[string]any") {
		t.Fatalf("strict source contains an untyped fallback:\n%s", source)
	}
	operationJSON, err := json.Marshal(operation)
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(source, strconv.Quote(string(operationJSON))); count != 1 {
		t.Fatalf("generated operation contract appears %d times, want once", count)
	}
	if !strings.Contains(source, "client.MustServiceDescriptorWithOperations(") || !strings.Contains(source, ", replaceUserOperation)\n") {
		t.Fatal("service descriptor must reuse the method's parsed operation")
	}
}

func TestGenerateStrictClient_DoesNotDeclareUnusedZeroValue(t *testing.T) {
	spec := SpecIR{
		IRVersion: clientIRVersion,
		Contract: &clientcontract.DocumentV1{
			ProtocolVersion: clientcontract.ProtocolVersion,
			Service:         clientcontract.Service{ID: "items", Audience: "https://items.internal"},
			Credentials:     map[string]clientcontract.CredentialProfile{},
		},
		Services: []ServiceIR{{Methods: []MethodIR{{
			Name: "getItem", OperationID: "getItem", HTTPMethod: "GET", Path: "/items/{id}",
			Parameters: []ParameterIR{{
				Name: "id", Location: "path", Required: true,
				Schema: clientcontract.Schema{Type: "string"},
			}},
			Successes: []SuccessIR{{Status: 200, Content: []ContentIR{{
				MediaType: "application/json",
				Schema:    &clientcontract.Schema{Type: "string"},
			}}}},
			Client: strictUnaryOperation("/items/{id}", nil),
		}}}},
	}

	source, err := GenerateClientFromIR(spec, ClientGenOptions{PackageName: "itemsclient", ClientName: "ItemsClient"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(source, "var zero") {
		t.Fatalf("generated successful-only method declares an unused zero value:\n%s", source)
	}
}

func TestGenerateStrictClient_SchemaLessErrorDoesNotImportJSON(t *testing.T) {
	spec := SpecIR{
		IRVersion: clientIRVersion,
		Contract: &clientcontract.DocumentV1{
			ProtocolVersion: clientcontract.ProtocolVersion,
			Service:         clientcontract.Service{ID: "items", Audience: "https://items.internal"},
			Credentials:     map[string]clientcontract.CredentialProfile{},
		},
		Services: []ServiceIR{{Methods: []MethodIR{{
			Name: "deleteItem", OperationID: "deleteItem", HTTPMethod: "DELETE", Path: "/items/{id}",
			Successes: []SuccessIR{{Status: 204}},
			Client: strictUnaryOperation("/items/{id}", []clientcontract.DeclaredError{{
				Status: 404, Code: "not_found",
			}}),
		}}}},
	}

	source, err := GenerateClientFromIR(spec, ClientGenOptions{PackageName: "itemsclient", ClientName: "ItemsClient"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(source, `"encoding/json"`) {
		t.Fatalf("schema-less declared error emitted an unused encoding/json import:\n%s", source)
	}
}

func TestGenerateStrictClient_OptionalNullableRefUsesOptionalAndEmbedsRequestSchema(t *testing.T) {
	nullable := true
	spec := SpecIR{
		IRVersion: clientIRVersion,
		Contract: &clientcontract.DocumentV1{
			ProtocolVersion: clientcontract.ProtocolVersion,
			Service:         clientcontract.Service{ID: "items", Audience: "https://items.internal"},
			Credentials:     map[string]clientcontract.CredentialProfile{},
		},
		Schemas: map[string]clientcontract.Schema{
			"NullableText": {Type: "string", Nullable: &nullable},
		},
		Services: []ServiceIR{{Methods: []MethodIR{{
			Name: "updateLabel", OperationID: "updateLabel", HTTPMethod: "PATCH", Path: "/label",
			Request:   &RequestIR{Required: false, Content: []ContentIR{{MediaType: "application/json", Schema: &clientcontract.Schema{Ref: "#/components/schemas/NullableText"}}}},
			Successes: []SuccessIR{{Status: 204}},
			Client:    strictUnaryOperation("/label", nil),
		}}}},
	}

	source, err := GenerateClientFromIR(spec, ClientGenOptions{PackageName: "itemsclient", ClientName: "ItemsClient"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parser.ParseFile(token.NewFileSet(), "client.gen.go", source, parser.AllErrors); err != nil {
		t.Fatalf("generated source does not parse: %v\n%s", err, source)
	}
	for _, want := range []string{
		"Body client.Optional[NullableText]",
		"if value, present := in.Body.Value(); present",
		`\"required\":false`,
		`\"#/components/schemas/NullableText\"`,
	} {
		if !containsNormalized(source, want) {
			t.Errorf("generated optional nullable request missing %q:\n%s", want, source)
		}
	}
	if strings.Contains(source, "input.Body != nil") {
		t.Fatalf("optional nullable ref was compared with nil:\n%s", source)
	}
}

func TestGenerateStrictClient_EmitsTypedSSEServerStream(t *testing.T) {
	message := strictSchemaObject(map[string]clientcontract.Schema{"value": {Type: "string"}}, "value")
	operation := &clientcontract.OperationV1{
		Stream:     clientcontract.StreamServer,
		Messages:   &clientcontract.MessageShapes{Output: &message},
		Transports: []clientcontract.Transport{{Protocol: clientcontract.TransportSSE, Path: "/items/watch", Encoding: clientcontract.EncodingJSON}},
		Security:   clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{}}}},
		Errors:     []clientcontract.DeclaredError{},
		Idempotency: clientcontract.Idempotency{
			Kind: clientcontract.IdempotencySafe,
		},
	}
	spec := SpecIR{
		IRVersion: clientIRVersion,
		Contract: &clientcontract.DocumentV1{
			ProtocolVersion: clientcontract.ProtocolVersion,
			Service:         clientcontract.Service{ID: "items", Audience: "https://items.internal"},
			Credentials:     map[string]clientcontract.CredentialProfile{},
		},
		Services: []ServiceIR{{Methods: []MethodIR{{
			Name: "watchItems", OperationID: "watchItems", HTTPMethod: "GET", Path: "/items/watch",
			Successes: []SuccessIR{{Status: 200, Content: []ContentIR{{MediaType: "text/event-stream", Schema: &message}}}},
			Client:    operation,
		}}}},
	}
	source, err := GenerateClientFromIR(spec, ClientGenOptions{PackageName: "itemsclient", ClientName: "ItemsClient"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"func (c *ItemsClient) WatchItems(ctx context.Context, in WatchItemsInput) (*client.Stream[WatchItemsMessage], error)",
		"return client.OpenOperationServerStream[WatchItemsMessage](ctx, c.transport, call, watchItemsOperation)",
	} {
		if !containsNormalized(source, want) {
			t.Errorf("generated server stream missing %q:\n%s", want, source)
		}
	}
}

func TestGenerateStrictClient_RejectsLossyAndCollidingShapes(t *testing.T) {
	base := func() SpecIR {
		return SpecIR{
			IRVersion: clientIRVersion,
			Contract:  &clientcontract.DocumentV1{ProtocolVersion: 1, Service: clientcontract.Service{ID: "x", Audience: "https://x.internal"}, Credentials: map[string]clientcontract.CredentialProfile{}},
			Services:  []ServiceIR{{Methods: []MethodIR{{Name: "getX", OperationID: "getX", HTTPMethod: "GET", Path: "/x", Successes: []SuccessIR{{Status: 204}}, Client: strictUnaryOperation("/x", nil)}}}},
		}
	}
	tests := []struct {
		name   string
		mutate func(*SpecIR)
		want   string
	}{
		{name: "normalized schema collision", mutate: func(spec *SpecIR) {
			spec.Schemas = map[string]clientcontract.Schema{"foo-bar": {Type: "string"}, "foo_bar": {Type: "string"}}
		}, want: "derive Go type"},
		{name: "property collision", mutate: func(spec *SpecIR) {
			spec.Schemas = map[string]clientcontract.Schema{"Payload": strictSchemaObject(map[string]clientcontract.Schema{"x-y": {Type: "string"}, "x_y": {Type: "string"}})}
		}, want: "derive Go field"},
		{name: "schema and operation input collision", mutate: func(spec *SpecIR) {
			spec.Schemas = map[string]clientcontract.Schema{"GetXInput": {Type: "string"}}
		}, want: "derive Go symbol"},
		{name: "normalized declared error collision", mutate: func(spec *SpecIR) {
			spec.Services[0].Methods[0].Client.Errors = []clientcontract.DeclaredError{
				{Status: 400, Code: "foo-bar"}, {Status: 409, Code: "foo_bar"},
			}
		}, want: "derive Go symbol"},
		{name: "open object", mutate: func(spec *SpecIR) {
			spec.Schemas = map[string]clientcontract.Schema{"Payload": {Type: "object"}}
		}, want: "open object"},
		{name: "client stream without a declared result", mutate: func(spec *SpecIR) {
			operation := spec.Services[0].Methods[0].Client
			operation.Stream = clientcontract.StreamClient
			operation.Messages = &clientcontract.MessageShapes{Input: &clientcontract.Schema{Type: "string"}}
			operation.Transports = []clientcontract.Transport{{
				Protocol: clientcontract.TransportWebSocket, Path: "/x", Encoding: clientcontract.EncodingJSON,
				WebSocket: &clientcontract.WebSocketTransport{Subprotocol: clientcontract.WebSocketSubprotocolV1, Resume: false},
			}}
			spec.Services[0].Methods[0].Successes = nil
		}, want: "must declare both an input and an output message schema"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := base()
			tc.mutate(&spec)
			_, err := GenerateClientFromIR(spec, ClientGenOptions{PackageName: "xclient", ClientName: "Client"})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

// strictSpecWith builds the smallest valid first-party spec around one method,
// so a negative test states exactly the one fact it is about.
func strictSpecWith(method MethodIR, schemas map[string]clientcontract.Schema) SpecIR {
	return SpecIR{
		IRVersion: clientIRVersion,
		Contract: &clientcontract.DocumentV1{
			ProtocolVersion: clientcontract.ProtocolVersion,
			Service:         clientcontract.Service{ID: "items", Audience: "https://items.internal"},
			Credentials:     map[string]clientcontract.CredentialProfile{},
		},
		Schemas:  schemas,
		Services: []ServiceIR{{Methods: []MethodIR{method}}},
	}
}

func strictGenerationError(t *testing.T, spec SpecIR) error {
	t.Helper()
	source, err := GenerateClientFromIR(spec, ClientGenOptions{PackageName: "itemsclient", ClientName: "ItemsClient"})
	if err == nil {
		t.Fatalf("generation succeeded where it must refuse:\n%s", source)
	}
	return err
}

// D0.2: an integer's width is declared, never inferred. Choosing int64 for a
// formatless integer makes the Go client disagree with the TypeScript client
// and with the proto descriptors for the same declaration.
func TestGenerateStrictClient_RefusesAnIntegerWithNoDeclaredFormat(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "generation-refuses-unhonorable-declarations", "an-integer-without-a-declared-format-is-refused-at-generation")

	for _, tc := range []struct {
		name   string
		format string
		want   string
	}{
		{name: "no format", format: "", want: "integer schema has no format"},
		{name: "unsupported format", format: "int128", want: `integer format "int128" is not one of`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := strictSpecWith(MethodIR{
				Name: "getItem", OperationID: "getItem", HTTPMethod: "GET", Path: "/items/{id}",
				Parameters: []ParameterIR{{
					Name: "id", Location: "path", Required: true,
					Schema: clientcontract.Schema{Type: "integer", Format: tc.format},
				}},
				Successes: []SuccessIR{{Status: 204}},
				Client:    strictUnaryOperation("/items/{id}", nil),
			}, nil)
			err := strictGenerationError(t, spec)
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			for _, want := range []string{"getItem", "parameter id"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %v does not name %q", err, want)
				}
			}
		})
	}
}

// D0.2 again, on the declared widths that do emit: each format maps to exactly
// one Go type, and a client generated from a uint64 field never narrows it.
func TestGenerateStrictClient_EmitsOneGoTypePerDeclaredIntegerWidth(t *testing.T) {
	widths := map[string]string{"int32": "int32", "int64": "int64", "uint32": "uint32", "uint64": "uint64"}
	for format, goType := range widths {
		t.Run(format, func(t *testing.T) {
			body := strictSchemaObject(map[string]clientcontract.Schema{
				"count": {Type: "integer", Format: format},
			}, "count")
			spec := strictSpecWith(MethodIR{
				Name: "createItem", OperationID: "createItem", HTTPMethod: "POST", Path: "/items",
				Request:   &RequestIR{Required: true, Content: []ContentIR{{MediaType: "application/json", Schema: &clientcontract.Schema{Ref: "#/components/schemas/Counter"}}}},
				Successes: []SuccessIR{{Status: 204}},
				Client: &clientcontract.OperationV1{
					Stream:      clientcontract.StreamUnary,
					Transports:  []clientcontract.Transport{{Protocol: clientcontract.TransportRESTJSON, Path: "/items", Encoding: clientcontract.EncodingJSON}},
					Security:    clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{}}}},
					Idempotency: clientcontract.Idempotency{Kind: clientcontract.IdempotencyNonIdempotent},
				},
			}, map[string]clientcontract.Schema{"Counter": body})
			source, err := GenerateClientFromIR(spec, ClientGenOptions{PackageName: "itemsclient", ClientName: "ItemsClient"})
			if err != nil {
				t.Fatal(err)
			}
			if !containsNormalized(source, "Count "+goType) {
				t.Fatalf("format %s did not emit %s:\n%s", format, goType, source)
			}
		})
	}
}

// D0.1: selectDeclaredError matches on the wire code, so a declared error with
// no stable code collapses every typed error that shares its status back to a
// generic remote error. Throws(status) without MayThrow produces exactly that.
func TestGenerateStrictClient_RefusesADeclaredErrorWithNoStableCode(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "generation-refuses-unhonorable-declarations", "a-declared-error-without-a-stable-code-is-refused-at-generation")

	spec := strictSpecWith(MethodIR{
		Name: "getItem", OperationID: "getItem", HTTPMethod: "GET", Path: "/items",
		Successes: []SuccessIR{{Status: 204}},
		Client:    strictUnaryOperation("/items", []clientcontract.DeclaredError{{Status: 422}}),
	}, nil)
	err := strictGenerationError(t, spec)
	for _, want := range []string{"getItem", "422", ".MayThrow("} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want it to contain %q", err, want)
		}
	}
}

// A GET or HEAD payload has no interoperable meaning; every intermediary may
// drop it. Emitting the call would lose the body silently at runtime.
func TestGenerateStrictClient_RefusesARequestBodyOnBodylessMethods(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "generation-refuses-unhonorable-declarations", "a-generated-client-never-sends-a-get-or-head-request-body")

	body := strictSchemaObject(map[string]clientcontract.Schema{"q": {Type: "string"}}, "q")
	for _, httpMethod := range []string{"GET", "HEAD"} {
		t.Run(httpMethod, func(t *testing.T) {
			spec := strictSpecWith(MethodIR{
				Name: "searchItems", OperationID: "searchItems", HTTPMethod: httpMethod, Path: "/items",
				Request:   &RequestIR{Required: true, Content: []ContentIR{{MediaType: "application/json", Schema: &clientcontract.Schema{Ref: "#/components/schemas/Query"}}}},
				Successes: []SuccessIR{{Status: 204}},
				Client:    strictUnaryOperation("/items", nil),
			}, map[string]clientcontract.Schema{"Query": body})
			err := strictGenerationError(t, spec)
			for _, want := range []string{"searchItems", httpMethod, "carry no body"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error = %v, want it to contain %q", err, want)
				}
			}
		})
	}
}

// A DELETE body is representable on the wire and stays allowed, so the refusal
// above is about GET and HEAD only, not about "requests that usually have no
// body".
func TestGenerateStrictClient_AllowsADeclaredDeleteBody(t *testing.T) {
	body := strictSchemaObject(map[string]clientcontract.Schema{"reason": {Type: "string"}}, "reason")
	spec := strictSpecWith(MethodIR{
		Name: "deleteItem", OperationID: "deleteItem", HTTPMethod: "DELETE", Path: "/items/{id}",
		Parameters: []ParameterIR{{Name: "id", Location: "path", Required: true, Schema: clientcontract.Schema{Type: "string"}}},
		Request:    &RequestIR{Required: true, Content: []ContentIR{{MediaType: "application/json", Schema: &clientcontract.Schema{Ref: "#/components/schemas/Reason"}}}},
		Successes:  []SuccessIR{{Status: 204}},
		Client: &clientcontract.OperationV1{
			Stream:      clientcontract.StreamUnary,
			Transports:  []clientcontract.Transport{{Protocol: clientcontract.TransportRESTJSON, Path: "/items/{id}", Encoding: clientcontract.EncodingJSON}},
			Security:    clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{}}}},
			Idempotency: clientcontract.Idempotency{Kind: clientcontract.IdempotencyIdempotent},
		},
	}, map[string]clientcontract.Schema{"Reason": body})
	source, err := GenerateClientFromIR(spec, ClientGenOptions{PackageName: "itemsclient", ClientName: "ItemsClient"})
	if err != nil {
		t.Fatalf("DELETE body was refused: %v", err)
	}
	if !containsNormalized(source, "client.EncodeJSON(in.Body)") {
		t.Fatalf("DELETE body was not encoded:\n%s", source)
	}
}

// D0.8: wire v1 keeps the proto WebSocket payload, and the Go client has no
// protobuf message codec yet. The gap closes at generation, so no client ever
// compiles against a codec it does not have.
func TestGenerateStrictClient_RefusesAProtoEncodedWebSocketTransport(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "generation-refuses-unhonorable-declarations", "a-transport-no-runtime-can-honor-is-refused-at-generation")

	spec := strictSpecWith(MethodIR{
		Name: "watchItems", OperationID: "watchItems", HTTPMethod: "GET", Path: "/items/watch",
		Client: &clientcontract.OperationV1{
			Stream:   clientcontract.StreamServer,
			Messages: &clientcontract.MessageShapes{Output: &clientcontract.Schema{Type: "string"}},
			Transports: []clientcontract.Transport{{
				Protocol:       clientcontract.TransportWebSocket,
				Path:           "/items/watch",
				Encoding:       clientcontract.EncodingProto,
				ProtobufMethod: "/items.v1.Items/Watch",
				WebSocket:      &clientcontract.WebSocketTransport{Subprotocol: clientcontract.WebSocketSubprotocolV1},
			}},
			Security:    clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{}}}},
			Idempotency: clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe},
		},
	}, nil)
	err := strictGenerationError(t, spec)
	if code := errors.GetCode(err); code != CodeClientGenUnsupportedSemantic {
		t.Fatalf("error code = %q, want %q (%v)", code, CodeClientGenUnsupportedSemantic, err)
	}
	for _, want := range []string{"watchItems", "websocket", "proto"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want it to contain %q", err, want)
		}
	}
}

// The Go symbol is the provider's declared operationId when there is one, and
// the REST-idiom name shared with the proto/gRPC bridge when the operationId is
// only this framework's own synthesis.
func TestStrictMethodSymbol_PrefersTheDeclaredOperationID(t *testing.T) {
	tests := []struct {
		name   string
		method MethodIR
		want   string
	}{
		{
			name:   "authored operationId wins",
			method: MethodIR{OperationID: "replaceUser", HTTPMethod: "PUT", Path: "/users/{id}"},
			want:   "ReplaceUser",
		},
		{
			name:   "canonical synthesis falls back to the REST idiom",
			method: MethodIR{OperationID: CanonicalOperationID("POST", "/users"), HTTPMethod: "POST", Path: "/users"},
			want:   "CreateUsers",
		},
		{
			name:   "reader synthesis falls back to the REST idiom",
			method: MethodIR{OperationID: buildOperationID("GET", "/users/{id}"), HTTPMethod: "GET", Path: "/users/{id}"},
			want:   "GetUsers",
		},
		{
			name:   "absent operationId falls back to the REST idiom",
			method: MethodIR{HTTPMethod: "DELETE", Path: "/users/{id}"},
			want:   "DeleteUsers",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := strictMethodSymbol(tc.method); got != tc.want {
				t.Fatalf("strictMethodSymbol = %q, want %q", got, tc.want)
			}
		})
	}
}

// symbolCollisionMethod is a unary GET operation whose operationId is the
// canonical synthesis of its route, as a Go provider publishes it, with one
// string parameter per {name} in the path.
func symbolCollisionMethod(path string) MethodIR {
	operationID := CanonicalOperationID("GET", path)
	var parameters []ParameterIR
	for _, segment := range strings.Split(strings.Trim(path, "/"), "/") {
		if strings.HasPrefix(segment, "{") {
			parameters = append(parameters, ParameterIR{
				Name: strings.Trim(segment, "{}"), Location: "path", Required: true, Schema: clientcontract.Schema{Type: "string"},
			})
		}
	}
	return MethodIR{
		Name: toCamelCase(operationID), OperationID: operationID, HTTPMethod: "GET", Path: path,
		Parameters: parameters,
		Successes:  []SuccessIR{{Status: 204}},
		Client:     strictUnaryOperation(path, nil),
	}
}

// Two routes that differ only by a path parameter reduce to one REST-idiom
// symbol, because that derivation drops path parameters. The route with fewer
// path parameters keeps the symbol a consumer already calls, and the other
// takes the exported form of its operationId, which keeps them. An operation
// with no sibling is untouched, and the order the operations arrive in never
// changes a symbol.
func TestStrictMethodSymbols_RoutesThatDifferOnlyByAPathParameterGetDistinctSymbols(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "stable-operation-identity", "routes-sharing-a-go-client-symbol-are-disambiguated-by-their-path-parameters")
	tests := []struct {
		name        string
		operationID func(httpMethod, path string) string
	}{
		{name: "provider-published operationIds", operationID: CanonicalOperationID},
		{name: "reader-synthesized operationIds", operationID: buildOperationID},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			method := func(path string) MethodIR {
				return MethodIR{OperationID: tc.operationID("GET", path), HTTPMethod: "GET", Path: path}
			}
			list, get, runs := method("/x/{a}/deploy"), method("/x/{a}/deploy/{b}"), method("/x/{a}/runs")
			want := map[string]string{
				list.OperationID: "GetXDeploy",
				get.OperationID:  "GetXADeployB",
				runs.OperationID: "GetXRuns",
			}
			for _, order := range [][]MethodIR{{list, get, runs}, {runs, get, list}, {get, list, runs}} {
				got, err := strictMethodSymbols([]ServiceIR{{Methods: order}})
				if err != nil {
					t.Fatalf("strictMethodSymbols: %v", err)
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("symbols for order %s, %s, %s = %v, want %v", order[0].Path, order[1].Path, order[2].Path, got, want)
				}
			}
		})
	}
}

// An authored operationId is author intent and is never renamed, even when it
// shares a symbol with a synthesized route that has fewer path parameters: the
// synthesized one yields.
func TestStrictMethodSymbols_AnAuthoredOperationIDKeepsItsSymbol(t *testing.T) {
	authored := MethodIR{OperationID: "getXDeploy", HTTPMethod: "GET", Path: "/x/{a}/deploy/{b}"}
	synthesized := MethodIR{OperationID: CanonicalOperationID("GET", "/x/{a}/deploy"), HTTPMethod: "GET", Path: "/x/{a}/deploy"}
	for _, order := range [][]MethodIR{{authored, synthesized}, {synthesized, authored}} {
		got, err := strictMethodSymbols([]ServiceIR{{Methods: order}})
		if err != nil {
			t.Fatalf("strictMethodSymbols: %v", err)
		}
		want := map[string]string{"getXDeploy": "GetXDeploy", synthesized.OperationID: "GetXADeploy"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("symbols = %v, want %v", got, want)
		}
	}
}

// The rule renames only synthesized operations, so two operations that still
// share a Go symbol after it are refused, never emitted as one method.
func TestGenerateStrictClient_RefusesOperationsThatStillShareAGoMethod(t *testing.T) {
	authored := func(operationID, path string) MethodIR {
		method := symbolCollisionMethod(path)
		method.OperationID, method.Name = operationID, operationID
		return method
	}
	tests := []struct {
		name    string
		methods []MethodIR
		symbol  string
	}{
		{
			name:    "two authored operationIds normalize to one symbol",
			methods: []MethodIR{authored("get-x", "/x"), authored("getX", "/y")},
			symbol:  "GetX",
		},
		{
			name: "a disambiguated symbol meets an authored one",
			methods: []MethodIR{
				symbolCollisionMethod("/x/{a}/deploy"),
				symbolCollisionMethod("/x/{a}/deploy/{b}"),
				authored("getXADeployB", "/z"),
			},
			symbol: "GetXADeployB",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := strictSpecWith(tc.methods[0], nil)
			spec.Services[0].Methods = tc.methods
			err := strictGenerationError(t, spec)
			if code := errors.GetCode(err); code != CodeClientGenCollision {
				t.Fatalf("error code = %q, want %q (%v)", code, CodeClientGenCollision, err)
			}
			if want := "derive Go method \"" + tc.symbol + "\""; !strings.Contains(err.Error(), want) {
				t.Fatalf("error = %v, want it to contain %s", err, want)
			}
		})
	}
}

// The emitted client and its ownership manifest name every operation with the
// same symbol, including an operation the collision rule renamed, and an
// omission that dissolves a collision leaves the survivor its ordinary symbol
// in both.
func TestGeneratedGoManifest_NamesEachOperationAsTheEmittedClientDoes(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "stable-operation-identity", "the-emitted-go-client-and-its-manifest-name-each-operation-alike")
	list := symbolCollisionMethod("/x/{a}/deploy")
	get := symbolCollisionMethod("/x/{a}/deploy/{b}")
	runs := symbolCollisionMethod("/x/{a}/runs")
	tests := []struct {
		name string
		omit []string
		want map[string]string
	}{
		{
			name: "every operation",
			want: map[string]string{list.OperationID: "GetXDeploy", get.OperationID: "GetXADeployB", runs.OperationID: "GetXRuns"},
		},
		{
			name: "the sibling omitted",
			omit: []string{get.OperationID},
			want: map[string]string{list.OperationID: "GetXDeploy", runs.OperationID: "GetXRuns"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := strictSpecWith(get, nil)
			spec.Services[0].Methods = []MethodIR{runs, get, list}
			source, err := GenerateClientFromIR(spec, ClientGenOptions{PackageName: "xclient", ClientName: "XClient", OmitOperations: tc.omit})
			if err != nil {
				t.Fatalf("GenerateClientFromIR: %v", err)
			}
			if _, err := parser.ParseFile(token.NewFileSet(), "client.gen.go", source, parser.AllErrors); err != nil {
				t.Fatalf("generated client does not parse: %v\n%s", err, source)
			}
			cfg := clientGenConfig{Go: clientGenConfigGo{ClientName: "XClient", OmitOperations: tc.omit}}
			manifest, err := generatedGoManifestForImportPath("example.com/x/clients/go", cfg, []byte("{}"), []byte(source), spec)
			if err != nil {
				t.Fatalf("generatedGoManifestForImportPath: %v", err)
			}
			got := map[string]string{}
			for _, operation := range manifest.Operations {
				got[operation.OperationID] = operation.MethodSymbol
				signature := "func (c *XClient) " + operation.MethodSymbol + "(ctx context.Context, in " + operation.MethodSymbol + "Input) error {"
				if !strings.Contains(source, signature) {
					t.Errorf("manifest names %s %s, the client declares no %q:\n%s", operation.OperationID, operation.MethodSymbol, signature, source)
				}
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("manifest symbols = %v, want %v", got, tc.want)
			}
			if methods := strings.Count(source, "func (c *XClient) "); methods != len(tc.want) {
				t.Fatalf("client declares %d methods, manifest names %d:\n%s", methods, len(tc.want), source)
			}
		})
	}
}
