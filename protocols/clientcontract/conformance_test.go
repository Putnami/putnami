package clientcontract

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

type fixtureExpectations struct {
	Valid   []string          `json:"valid"`
	Invalid map[string]string `json:"invalid"`
}

type openAPISchemaCarrier struct {
	Schema json.RawMessage `json:"schema"`
}

type openAPIOperation struct {
	OperationID string          `json:"operationId"`
	Client      json.RawMessage `json:"x-putnami-client"`
	External    json.RawMessage `json:"x-putnami-external-contract"`
	Parameters  []struct {
		Name   string          `json:"name"`
		In     string          `json:"in"`
		Schema json.RawMessage `json:"schema"`
	} `json:"parameters"`
	RequestBody *struct {
		Content map[string]openAPISchemaCarrier `json:"content"`
	} `json:"requestBody"`
	Responses map[string]struct {
		Content map[string]openAPISchemaCarrier `json:"content"`
	} `json:"responses"`
}

type openAPIFixture struct {
	Client     json.RawMessage                        `json:"x-putnami-client"`
	Paths      map[string]map[string]openAPIOperation `json:"paths"`
	Components struct {
		Schemas map[string]json.RawMessage `json:"schemas"`
	} `json:"components"`
}

func loadExpectations(t *testing.T) fixtureExpectations {
	t.Helper()
	data, err := os.ReadFile("fixtures/openapi/expectations.json")
	if err != nil {
		t.Fatal(err)
	}
	var expected fixtureExpectations
	if err := json.Unmarshal(data, &expected); err != nil {
		t.Fatal(err)
	}
	return expected
}

func TestOpenAPIConformanceFixtures(t *testing.T) {
	spectest.Proves(t, "client-contract/first-party-generated-clients", "strict-provider-authority", "the-full-cross-protocol-provider-fixture-is-valid-for-the-go-reader")
	spectest.Proves(t, "client-contract/first-party-generated-clients", "strict-provider-authority", "unsupported-or-ambiguous-semantics-are-rejected-by-the-go-reader")
	expected := loadExpectations(t)
	assertFixtureCoverage(t, "fixtures/openapi/valid", expected.Valid)
	invalidNames := make([]string, 0, len(expected.Invalid))
	for name := range expected.Invalid {
		invalidNames = append(invalidNames, name)
	}
	sort.Strings(invalidNames)
	assertFixtureCoverage(t, "fixtures/openapi/invalid", invalidNames)

	for _, name := range expected.Valid {
		t.Run("valid/"+name, func(t *testing.T) {
			diags := validateOpenAPIFixture(t, filepath.Join("fixtures/openapi/valid", name))
			if diag.HasErrors(diags) {
				t.Fatalf("valid fixture produced diagnostics: %v", diags)
			}
		})
	}
	for _, name := range invalidNames {
		t.Run("invalid/"+name, func(t *testing.T) {
			diags := validateOpenAPIFixture(t, filepath.Join("fixtures/openapi/invalid", name))
			if !diag.HasErrors(diags) {
				t.Fatal("invalid fixture produced no error diagnostics")
			}
			want := expected.Invalid[name]
			for _, diagnostic := range diags {
				if diagnostic.Code == want {
					return
				}
			}
			t.Fatalf("diagnostics %v do not contain expected code %q", diags, want)
		})
	}
}

func validateOpenAPIFixture(t *testing.T, path string) []diag.Diagnostic {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture openAPIFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "%v", err)}
	}
	if len(fixture.Client) == 0 {
		return []diag.Diagnostic{required(ExtensionKey)}
	}
	document, diags := ParseAndValidateDocument(fixture.Client)
	if document == nil {
		return diags
	}
	schemaNames := make([]string, 0, len(fixture.Components.Schemas))
	for name := range fixture.Components.Schemas {
		schemaNames = append(schemaNames, name)
	}
	sort.Strings(schemaNames)
	for _, name := range schemaNames {
		_, schemaDiags := ParseAndValidateSchema(fixture.Components.Schemas[name])
		diags = append(diags, schemaDiags...)
	}

	httpMethods := map[string]bool{
		"delete": true, "get": true, "head": true, "options": true,
		"patch": true, "post": true, "put": true,
	}
	paths := make([]string, 0, len(fixture.Paths))
	for path := range fixture.Paths {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		methods := make([]string, 0, len(fixture.Paths[path]))
		for method := range fixture.Paths[path] {
			if httpMethods[method] {
				methods = append(methods, method)
			}
		}
		sort.Strings(methods)
		for _, method := range methods {
			operation := fixture.Paths[path][method]
			// An operation an external authority owns is not part of the
			// contract: the reader skips it whole, its own schemas included,
			// once the marker itself is well formed.
			authority, externalDiags := ExternalContractAuthority(operation.Client, operation.External)
			if len(externalDiags) > 0 || authority != "" {
				diags = append(diags, externalDiags...)
				continue
			}
			if len(operation.Client) == 0 {
				diags = append(diags, required(path+"."+method+"."+ExtensionKey))
				continue
			}
			metadata, operationDiags := ParseOperation(operation.Client)
			diags = append(diags, operationDiags...)
			if metadata != nil {
				diags = append(diags, ValidateOperationForID(operation.OperationID, metadata, document)...)
				if metadata.Resilience != nil && metadata.Resilience.Cache != nil {
					diags = append(diags, ValidateCacheKeyFields("resilience.cache", metadata.Resilience.Cache,
						fixtureCacheKeyInputs(t, operation, fixture.Components.Schemas))...)
					diags = append(diags, ValidateCacheInvalidationFields("resilience.cache", metadata.Resilience.Cache,
						fixtureCacheResponseProperties(operation, fixture.Components.Schemas))...)
				}
				diags = append(diags, ValidateSSEContinuationReferences(metadata,
					fixtureQueryParameters(operation), fixtureComponents(fixture.Components.Schemas))...)
			}
			for _, parameter := range operation.Parameters {
				if len(parameter.Schema) > 0 {
					_, schemaDiags := ParseAndValidateSchema(parameter.Schema)
					diags = append(diags, schemaDiags...)
				}
			}
			if operation.RequestBody != nil {
				diags = append(diags, validateMediaSchemas(operation.RequestBody.Content)...)
			}
			statuses := make([]string, 0, len(operation.Responses))
			for status := range operation.Responses {
				statuses = append(statuses, status)
			}
			sort.Strings(statuses)
			for _, status := range statuses {
				diags = append(diags, validateMediaSchemas(operation.Responses[status].Content)...)
			}
		}
	}
	return diags
}

// fixtureCacheKeyInputs names the inputs one fixture operation declares, the
// way a reader does before it trusts a cache declaration's key fields.
func fixtureCacheKeyInputs(t *testing.T, operation openAPIOperation, components map[string]json.RawMessage) CacheKeyInputs {
	t.Helper()
	var inputs CacheKeyInputs
	for _, parameter := range operation.Parameters {
		switch parameter.In {
		case "path":
			inputs.Path = append(inputs.Path, parameter.Name)
		case "query":
			inputs.Query = append(inputs.Query, parameter.Name)
		case "header":
			inputs.Header = append(inputs.Header, parameter.Name)
		}
	}
	if operation.RequestBody == nil {
		return inputs
	}
	inputs.Body = true
	carrier, ok := operation.RequestBody.Content["application/json"]
	if !ok || len(carrier.Schema) == 0 {
		return inputs
	}
	schema, _ := ParseAndValidateSchema(carrier.Schema)
	resolved := make(map[string]Schema, len(components))
	for name, raw := range components {
		if component, _ := ParseAndValidateSchema(raw); component != nil {
			resolved[name] = *component
		}
	}
	inputs.BodyProperties = CacheKeyBodyProperties(schema, resolved)
	return inputs
}

// fixtureQueryParameters maps the query parameters one fixture operation
// declares to their schemas, the way a reader resolves them before it trusts
// a cursor continuation.
func fixtureQueryParameters(operation openAPIOperation) map[string]Schema {
	query := map[string]Schema{}
	for _, parameter := range operation.Parameters {
		if parameter.In != "query" || len(parameter.Schema) == 0 {
			continue
		}
		if schema, _ := ParseAndValidateSchema(parameter.Schema); schema != nil {
			query[parameter.Name] = *schema
		}
	}
	return query
}

// fixtureComponents parses a fixture's component schemas.
func fixtureComponents(components map[string]json.RawMessage) map[string]Schema {
	resolved := make(map[string]Schema, len(components))
	for name, raw := range components {
		if component, _ := ParseAndValidateSchema(raw); component != nil {
			resolved[name] = *component
		}
	}
	return resolved
}

// fixtureCacheResponseProperties names the top-level properties of one fixture
// operation's JSON success bodies, the way a reader does before it trusts a
// cache declaration's invalidation fields.
func fixtureCacheResponseProperties(operation openAPIOperation, components map[string]json.RawMessage) map[string]bool {
	resolved := make(map[string]Schema, len(components))
	for name, raw := range components {
		if component, _ := ParseAndValidateSchema(raw); component != nil {
			resolved[name] = *component
		}
	}
	// The responses a reader reads: every three-digit 2xx status, and in each
	// every media type equal to application/json ignoring case.
	var bodies []*Schema
	for status, response := range operation.Responses {
		if len(status) != 3 || status[0] != '2' || status[1] < '0' || status[1] > '9' || status[2] < '0' || status[2] > '9' {
			continue
		}
		for mediaType, carrier := range response.Content {
			if strings.EqualFold(mediaType, "application/json") && len(carrier.Schema) > 0 {
				schema, _ := ParseAndValidateSchema(carrier.Schema)
				bodies = append(bodies, schema)
			}
		}
	}
	return CacheResponseProperties(bodies, resolved)
}

func validateMediaSchemas(content map[string]openAPISchemaCarrier) []diag.Diagnostic {
	var diags []diag.Diagnostic
	mediaTypes := make([]string, 0, len(content))
	for mediaType := range content {
		mediaTypes = append(mediaTypes, mediaType)
	}
	sort.Strings(mediaTypes)
	for _, mediaType := range mediaTypes {
		if raw := content[mediaType].Schema; len(raw) > 0 {
			_, schemaDiags := ParseAndValidateSchema(raw)
			diags = append(diags, schemaDiags...)
		}
	}
	return diags
}

func assertFixtureCoverage(t *testing.T, dir string, expected []string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
			found = append(found, entry.Name())
		}
	}
	sort.Strings(found)
	want := append([]string(nil), expected...)
	sort.Strings(want)
	if !reflect.DeepEqual(found, want) {
		t.Fatalf("%s fixtures = %v, want %v", dir, found, want)
	}
}

func TestSchemaPreservesExplicitNullDefaultAndLargeIntegerBounds(t *testing.T) {
	spectest.Proves(t, "client-contract/first-party-generated-clients", "strict-provider-authority", "exact-json-semantics-round-trip-without-loss")
	const input = `{"type":"integer","format":"uint64","nullable":true,"default":null,"minimum":0,"maximum":18446744073709551615}`
	schema, diags := ParseAndValidateSchema([]byte(input))
	if diag.HasErrors(diags) {
		t.Fatalf("schema produced diagnostics: %v", diags)
	}
	encoded, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	for _, exact := range [][]byte{[]byte(`"default":null`), []byte(`"maximum":18446744073709551615`)} {
		if !bytes.Contains(encoded, exact) {
			t.Fatalf("round trip %s lost exact token %s", encoded, exact)
		}
	}
}

func TestSchemaComparesLargeIntegerBoundsExactly(t *testing.T) {
	const input = `{"type":"integer","format":"uint64","minimum":18446744073709551615,"maximum":18446744073709551614}`
	_, diags := ParseAndValidateSchema([]byte(input))
	if !diag.HasErrors(diags) {
		t.Fatal("adjacent large integer bounds in descending order must fail")
	}
}

func TestSemanticArraysPreserveProviderOrder(t *testing.T) {
	const schemaInput = `{"type":"array","items":{"type":"string"},"default":["z","a"],"enum":[["b","a"],["a","b"]]}`
	schema, diags := ParseAndValidateSchema([]byte(schemaInput))
	if diag.HasErrors(diags) {
		t.Fatalf("ordered schema values produced diagnostics: %v", diags)
	}
	encodedSchema, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	for _, exact := range [][]byte{[]byte(`"default":["z","a"]`), []byte(`"enum":[["b","a"],["a","b"]]`)} {
		if !bytes.Contains(encodedSchema, exact) {
			t.Fatalf("schema round trip reordered %s: %s", exact, encodedSchema)
		}
	}

	const operationInput = `{"stream":"server","messages":{"output":{"type":"string"}},"transports":[{"protocol":"sse","path":"/events","encoding":"json"},{"protocol":"websocket","path":"/events","encoding":"json","websocket":{"subprotocol":"putnami.service.v1","resume":false}}],"security":{"alternatives":[{"allOf":[{"profile":"first"},{"profile":"second"}]},{"allOf":[]}]},"errors":[],"idempotency":{"kind":"safe"}}`
	document := &DocumentV1{
		ProtocolVersion: ProtocolVersion,
		Service:         Service{ID: "ordered", Audience: "ordered"},
		Credentials: map[string]CredentialProfile{
			"first":  {Kind: CredentialAPIKey, Header: "X-First"},
			"second": {Kind: CredentialNamedHeader, Header: "X-Second"},
		},
	}
	operation, diags := ParseAndValidateOperation([]byte(operationInput), document)
	if diag.HasErrors(diags) {
		t.Fatalf("ordered operation produced diagnostics: %v", diags)
	}
	if operation.Transports[0].Protocol != TransportSSE || operation.Transports[1].Protocol != TransportWebSocket ||
		operation.Security.Alternatives[0].AllOf[0].Profile != "first" ||
		operation.Security.Alternatives[0].AllOf[1].Profile != "second" {
		t.Fatalf("operation parser changed provider order: %+v", operation)
	}
}

func TestProtobufWellKnownMessagesDoNotRequireInlineDescriptors(t *testing.T) {
	document := DocumentV1{
		ProtocolVersion: ProtocolVersion,
		Service:         Service{ID: "clock", Audience: "clock"},
		Credentials:     map[string]CredentialProfile{},
		Protobuf: &ProtobufDescriptor{
			Syntax:  "proto3",
			Package: "clock.v1",
			Services: []ProtobufService{{Name: "ClockService", Methods: []ProtobufMethod{{
				Name: "Now", Input: "google.protobuf.Empty", Output: "google.protobuf.Timestamp",
			}}}},
			Messages: []ProtobufMessage{},
			Enums:    []ProtobufEnum{},
		},
	}
	if diags := ValidateDocument(&document); diag.HasErrors(diags) {
		t.Fatalf("well-known protobuf messages produced diagnostics: %v", diags)
	}
}

func TestProtobufRejectsImpossibleDescriptorShapes(t *testing.T) {
	validMap := func() ProtobufField {
		return ProtobufField{
			Name: "labels", JSONName: "labels", Number: 1, TypeKind: "map", Type: "map",
			Map: &ProtobufMap{KeyType: "string", ValueKind: "scalar", ValueType: "string"},
		}
	}
	type descriptorCase struct {
		name       string
		descriptor ProtobufDescriptor
	}
	cases := make([]descriptorCase, 0, 6)
	cases = append(cases,
		descriptorCase{
			"proto3 enum begins above zero",
			ProtobufDescriptor{Syntax: "proto3", Package: "x.v1", Services: []ProtobufService{}, Messages: []ProtobufMessage{}, Enums: []ProtobufEnum{{Name: "State", Values: []ProtobufEnumValue{{Name: "STATE_ONE", Number: 1}}}}},
		},
		descriptorCase{
			"enum number exceeds int32",
			ProtobufDescriptor{Syntax: "proto3", Package: "x.v1", Services: []ProtobufService{}, Messages: []ProtobufMessage{}, Enums: []ProtobufEnum{{Name: "State", Values: []ProtobufEnumValue{{Name: "STATE_ZERO", Number: 0}, {Name: "STATE_BIG", Number: 2147483648}}}}},
		},
	)
	for _, mutate := range []struct {
		name string
		fn   func(*ProtobufField)
	}{
		{"map marked repeated", func(field *ProtobufField) { field.Repeated = true }},
		{"map marked optional", func(field *ProtobufField) { field.Optional = true }},
		{"map placed in oneof", func(field *ProtobufField) { field.OneOf = "choice" }},
		{"map has noncanonical type", func(field *ProtobufField) { field.Type = "labels" }},
	} {
		field := validMap()
		mutate.fn(&field)
		cases = append(cases, descriptorCase{
			mutate.name,
			ProtobufDescriptor{Syntax: "proto3", Package: "x.v1", Services: []ProtobufService{}, Messages: []ProtobufMessage{{Name: "Bag", Fields: []ProtobufField{field}, OneOfs: []string{"choice"}}}, Enums: []ProtobufEnum{}},
		})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			document := &DocumentV1{
				ProtocolVersion: ProtocolVersion,
				Service:         Service{ID: "x", Audience: "x"},
				Credentials:     map[string]CredentialProfile{},
				Protobuf:        &tc.descriptor,
			}
			assertDiagnosticCode(t, ValidateDocument(document), ErrorCodeInvalidProtobuf)
		})
	}
}

func TestIdempotencyHeaderCannotCollideWithCredentialInjection(t *testing.T) {
	document := &DocumentV1{
		ProtocolVersion: ProtocolVersion,
		Service:         Service{ID: "x", Audience: "x"},
		Credentials: map[string]CredentialProfile{
			"request-key": {Kind: CredentialNamedHeader, Header: "X-Request-Key"},
		},
	}
	operation := &OperationV1{
		Stream:     StreamUnary,
		Transports: []Transport{{Protocol: TransportRESTJSON, Path: "/items", Encoding: EncodingJSON}},
		Security: Security{Alternatives: []SecurityAlternative{{AllOf: []SecurityRequirement{{
			Profile: "request-key",
		}}}}},
		Errors:      []DeclaredError{},
		Idempotency: Idempotency{Kind: IdempotencyIdempotent, KeyHeader: "x-request-key"},
	}
	assertDiagnosticCode(t, ValidateOperation(operation, document), ErrorCodeInvalidIdempotency)
}

func assertDiagnosticCode(t *testing.T, diagnostics []diag.Diagnostic, want string) {
	t.Helper()
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == want {
			return
		}
	}
	t.Fatalf("diagnostics %v do not contain %q", diagnostics, want)
}

// TestStreamReconnectNeedsAFirstPartyResumableTransport pins the consumer half
// of the resume agreement. resilience.stream.reconnect counts from the
// operation itself or, on a server stream only, from the document default. An
// effective reconnect needs a first-party websocket transport that declares
// resume, and each mistake yields exactly one diagnostic.
func TestStreamReconnectNeedsAFirstPartyResumableTransport(t *testing.T) {
	spectest.Proves(t, "client-contract/first-party-generated-clients", "strict-provider-authority", "unsupported-or-ambiguous-semantics-are-rejected-by-the-go-reader")
	unresumable := diag.Errorf(ErrorCodeInvalidResilience, "resilience.stream.reconnect",
		"stream reconnect requires a websocket transport with resume support or an sse transport that declares a continuation")
	misplacedResume := diag.Errorf(ErrorCodeInvalidResilience, "transports[0].websocket.resume",
		"websocket resume is valid only for safe server streams in protocol v1")

	t.Run("sse-only corpus fixture", func(t *testing.T) {
		diags := validateOpenAPIFixture(t, "fixtures/openapi/invalid/reconnect-without-resumable-transport.openapi.json")
		if !reflect.DeepEqual(diags, []diag.Diagnostic{unresumable}) {
			t.Fatalf("diagnostics = %v, want exactly %v", diags, unresumable)
		}
	})

	reconnect := func(value bool) *ResiliencePolicy {
		return &ResiliencePolicy{Stream: &StreamPolicy{Reconnect: &value}}
	}
	rest := Transport{Protocol: TransportRESTJSON, Path: "/clock", Encoding: EncodingJSON}
	sse := Transport{Protocol: TransportSSE, Path: "/clock/ticks", Encoding: EncodingJSON}
	websocket := func(resume bool) Transport {
		return Transport{Protocol: TransportWebSocket, Path: "/clock/ticks", Encoding: EncodingJSON,
			WebSocket: &WebSocketTransport{Subprotocol: WebSocketSubprotocolV1, Resume: resume}}
	}
	operation := func(stream StreamMode, kind IdempotencyKind, policy *ResiliencePolicy, transports ...Transport) *OperationV1 {
		declared := &OperationV1{
			Stream:      stream,
			Transports:  transports,
			Security:    Security{Alternatives: []SecurityAlternative{{AllOf: []SecurityRequirement{}}}},
			Errors:      []DeclaredError{},
			Idempotency: Idempotency{Kind: kind},
			Resilience:  policy,
		}
		if stream == StreamServer {
			declared.Messages = &MessageShapes{Output: &Schema{Type: "string"}}
		}
		return declared
	}
	cases := []struct {
		name      string
		operation *OperationV1
		defaults  *ResiliencePolicy
		want      []diag.Diagnostic
	}{
		{"a document default leaves a unary operation valid",
			operation(StreamUnary, IdempotencySafe, nil, rest), reconnect(true), nil},
		{"a server stream relying on the document default needs a resumable transport",
			operation(StreamServer, IdempotencySafe, nil, sse, websocket(false)), reconnect(true), []diag.Diagnostic{unresumable}},
		{"a server stream honors the document default through a resumable transport",
			operation(StreamServer, IdempotencySafe, nil, sse, websocket(true)), reconnect(true), nil},
		{"an operation's own reconnect counts on a unary operation",
			operation(StreamUnary, IdempotencySafe, reconnect(true), rest), nil, []diag.Diagnostic{unresumable}},
		{"an operation's own false overrides the document default",
			operation(StreamServer, IdempotencySafe, reconnect(false), sse), reconnect(true), nil},
		{"a first-party websocket without resume is named once",
			operation(StreamServer, IdempotencySafe, reconnect(true), websocket(false)), nil, []diag.Diagnostic{unresumable}},
		{"a resume on an unsafe server stream is named once",
			operation(StreamServer, IdempotencyNonIdempotent, reconnect(true), websocket(true)), nil, []diag.Diagnostic{misplacedResume}},
		{"a resume on a unary operation is named once",
			operation(StreamUnary, IdempotencySafe, reconnect(true), websocket(true)), nil, []diag.Diagnostic{misplacedResume}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			document := &DocumentV1{
				ProtocolVersion: ProtocolVersion,
				Service:         Service{ID: "clock", Audience: "urn:clock"},
				Credentials:     map[string]CredentialProfile{},
			}
			if tc.defaults != nil {
				document.Defaults = &Defaults{Resilience: tc.defaults}
			}
			diags := ValidateOperation(tc.operation, document)
			if len(diags) != len(tc.want) || (len(tc.want) > 0 && !reflect.DeepEqual(diags, tc.want)) {
				t.Fatalf("diagnostics = %v, want %v", diags, tc.want)
			}
		})
	}

	// A provider-owned wire resumes by its own protocol, so its resume never
	// satisfies a reconnect: the reconnect is named beside the wire's own refusal.
	t.Run("a provider-owned wire never counts as resumable", func(t *testing.T) {
		wire := providerWireOperation(EncodingBinary, "")
		wire.Transports[0].WebSocket.Resume = true
		wire.Resilience = reconnect(true)
		diags := ValidateOperation(wire, providerWireDocument())
		for _, diagnostic := range diags {
			if reflect.DeepEqual(diagnostic, unresumable) {
				return
			}
		}
		t.Fatalf("diagnostics %v do not contain %v", diags, unresumable)
	})
}

func TestStrictParserRejectsDuplicateKeysAndUnexpectedNulls(t *testing.T) {
	spectest.Proves(t, "client-contract/first-party-generated-clients", "strict-provider-authority", "unsupported-or-ambiguous-semantics-are-rejected-by-the-go-reader")
	cases := []struct {
		name  string
		input []byte
		parse func([]byte) bool
	}{
		{
			"duplicate document key",
			[]byte(`{"protocolVersion":1,"protocolVersion":1,"service":{"id":"x","audience":"a"},"credentials":{}}`),
			func(input []byte) bool { _, diags := ParseDocument(input); return diag.HasErrors(diags) },
		},
		{
			"null credentials",
			[]byte(`{"protocolVersion":1,"service":{"id":"x","audience":"a"},"credentials":null}`),
			func(input []byte) bool { _, diags := ParseDocument(input); return diag.HasErrors(diags) },
		},
		{
			"null nullable",
			[]byte(`{"type":"string","nullable":null}`),
			func(input []byte) bool { _, diags := ParseAndValidateSchema(input); return diag.HasErrors(diags) },
		},
		{
			"null additionalProperties",
			[]byte(`{"type":"object","additionalProperties":null}`),
			func(input []byte) bool { _, diags := ParseAndValidateSchema(input); return diag.HasErrors(diags) },
		},
		{
			"property named default does not weaken nested null rejection",
			[]byte(`{"type":"object","properties":{"default":{"type":"string","nullable":null}}}`),
			func(input []byte) bool { _, diags := ParseAndValidateSchema(input); return diag.HasErrors(diags) },
		},
		{
			"property named enum does not weaken nested null rejection",
			[]byte(`{"type":"object","properties":{"enum":{"type":"string","nullable":null}}}`),
			func(input []byte) bool { _, diags := ParseAndValidateSchema(input); return diag.HasErrors(diags) },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.parse(tc.input) {
				t.Errorf("input should fail strict parsing: %s", tc.input)
			}
		})
	}
}

func TestProtocolVersionCompatibilityWindow(t *testing.T) {
	spectest.Proves(t, "client-contract/first-party-generated-clients", "strict-provider-authority", "only-version-1-is-accepted")
	for _, tc := range []struct {
		name  string
		input string
	}{
		{"missing", `{"service":{"id":"x","audience":"a"},"credentials":{}}`},
		{"version zero", `{"protocolVersion":0,"service":{"id":"x","audience":"a"},"credentials":{}}`},
		{"future version", `{"protocolVersion":2,"service":{"id":"x","audience":"a"},"credentials":{}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, diags := ParseAndValidateDocument([]byte(tc.input))
			for _, diagnostic := range diags {
				if diagnostic.Code == ErrorCodeInvalidVersion {
					return
				}
			}
			t.Fatalf("version outside the v1 window produced diagnostics %v", diags)
		})
	}
}

func TestFullFixtureMetadataRoundTripsWithoutSemanticLoss(t *testing.T) {
	data, err := os.ReadFile("fixtures/openapi/valid/full.openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture openAPIFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	document, diags := ParseAndValidateDocument(fixture.Client)
	if diag.HasErrors(diags) {
		t.Fatal(diags)
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, diags := ParseAndValidateDocument(encoded)
	if diag.HasErrors(diags) || !reflect.DeepEqual(document, roundTrip) {
		t.Fatalf("document round trip lost metadata: diags=%v\nwant=%#v\n got=%#v", diags, document, roundTrip)
	}
}
