package clientcontract

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

const (
	sseFeature     = "client-contract/first-party-generated-clients"
	sseRequirement = "sse-continuation"
)

func cursorContinuation() *SSETransport {
	return &SSETransport{Continuation: &SSEContinuation{
		Mode:   SSEContinuationCursor,
		Cursor: &SSECursor{OutputField: "cursor", QueryParameter: "cursor"},
	}}
}

func bestEffortContinuation() *SSETransport {
	return &SSETransport{Continuation: &SSEContinuation{Mode: SSEContinuationBestEffort}}
}

func sseTransport(metadata *SSETransport) Transport {
	return Transport{Protocol: TransportSSE, Path: "/tail", Encoding: EncodingJSON, SSE: metadata}
}

// sseOperation is a server stream carried by the given transports; the caller
// adjusts idempotency and policy.
func sseOperation(kind IdempotencyKind, policy *ResiliencePolicy, transports ...Transport) *OperationV1 {
	return &OperationV1{
		Stream:      StreamServer,
		Messages:    &MessageShapes{Output: &Schema{Type: "string"}},
		Transports:  transports,
		Security:    Security{Alternatives: []SecurityAlternative{{AllOf: []SecurityRequirement{}}}},
		Errors:      []DeclaredError{},
		Idempotency: Idempotency{Kind: kind},
		Resilience:  policy,
	}
}

func sseDocument(defaults *ResiliencePolicy) *DocumentV1 {
	document := &DocumentV1{
		ProtocolVersion: ProtocolVersion,
		Service:         Service{ID: "logs", Audience: "urn:logs"},
		Credentials:     map[string]CredentialProfile{},
	}
	if defaults != nil {
		document.Defaults = &Defaults{Resilience: defaults}
	}
	return document
}

func reconnectPolicy(value bool) *ResiliencePolicy {
	return &ResiliencePolicy{Stream: &StreamPolicy{Reconnect: &value}}
}

func TestAnSSETransportDeclaresACursorOrBestEffortContinuation(t *testing.T) {
	spectest.Proves(t, sseFeature, sseRequirement, "an-sse-transport-declares-a-cursor-or-best-effort-continuation")
	if diags := validateOpenAPIFixture(t, "fixtures/openapi/valid/sse-continuation.openapi.json"); len(diags) > 0 {
		t.Fatalf("the valid continuation fixture produced diagnostics: %v", diags)
	}
	cases := []struct {
		name      string
		transport Transport
		wire      string
		mode      SSEContinuationMode
	}{
		{"cursor", sseTransport(cursorContinuation()),
			`{"protocol":"sse","path":"/tail","encoding":"json","sse":{"continuation":{"mode":"cursor","cursor":{"outputField":"cursor","queryParameter":"cursor"}}}}`,
			SSEContinuationCursor},
		{"best effort", sseTransport(bestEffortContinuation()),
			`{"protocol":"sse","path":"/tail","encoding":"json","sse":{"continuation":{"mode":"best-effort"}}}`,
			SSEContinuationBestEffort},
		{"no continuation keeps the exact bytes of an sse transport", sseTransport(nil),
			`{"protocol":"sse","path":"/tail","encoding":"json"}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			operation := sseOperation(IdempotencySafe, reconnectPolicy(tc.mode != ""), tc.transport)
			if diags := ValidateOperation(operation, sseDocument(nil)); diags != nil {
				t.Fatalf("a valid declaration was refused: %v", diags)
			}
			encoded, err := json.Marshal(tc.transport)
			if err != nil {
				t.Fatal(err)
			}
			if string(encoded) != tc.wire {
				t.Fatalf("published transport = %s\nwant %s", encoded, tc.wire)
			}
			parsed, diags := ParseAndValidateOperation(mustMarshal(t, operation), sseDocument(nil))
			if diag.HasErrors(diags) || !reflect.DeepEqual(parsed, operation) {
				t.Fatalf("round trip lost a fact: diags=%v\n got %#v\nwant %#v", diags, parsed, operation)
			}
			continuation := parsed.Transports[0].Continuation()
			if (continuation == nil) != (tc.mode == "") || (continuation != nil && continuation.Mode != tc.mode) {
				t.Fatalf("Continuation() = %#v, want mode %q", continuation, tc.mode)
			}
		})
	}
	stray := Transport{Protocol: TransportWebSocket, Path: "/tail", Encoding: EncodingJSON,
		WebSocket: &WebSocketTransport{Subprotocol: WebSocketSubprotocolV1}, SSE: bestEffortContinuation()}
	if stray.Continuation() != nil {
		t.Fatal("a websocket transport reported the sse metadata it must not carry as a continuation")
	}
}

func TestAReconnectNeedsAResumableWebSocketOrAContinuableSSETransport(t *testing.T) {
	spectest.Proves(t, sseFeature, sseRequirement, "a-reconnect-needs-a-resumable-websocket-or-a-continuable-sse-transport")
	unresumable := diag.Errorf(ErrorCodeInvalidResilience, "resilience.stream.reconnect",
		"stream reconnect requires a websocket transport with resume support or an sse transport that declares a continuation")
	misplaced := diag.Errorf(ErrorCodeInvalidResilience, "transports[0].sse.continuation",
		"sse continuation is valid only for safe server streams in protocol v1")
	resumableWebSocket := Transport{Protocol: TransportWebSocket, Path: "/tail", Encoding: EncodingJSON,
		WebSocket: &WebSocketTransport{Subprotocol: WebSocketSubprotocolV1, Resume: true}}
	cases := []struct {
		name      string
		operation *OperationV1
		defaults  *ResiliencePolicy
		want      []diag.Diagnostic
	}{
		{"a cursor continuation carries the operation's own reconnect",
			sseOperation(IdempotencySafe, reconnectPolicy(true), sseTransport(cursorContinuation())), nil, nil},
		{"a best-effort continuation carries the document default",
			sseOperation(IdempotencySafe, nil, sseTransport(bestEffortContinuation())), reconnectPolicy(true), nil},
		{"a continuation the consumer did not opt into is still a valid declaration",
			sseOperation(IdempotencySafe, nil, sseTransport(cursorContinuation())), nil, nil},
		{"a continuation beside a resumable websocket transport",
			sseOperation(IdempotencySafe, reconnectPolicy(true), sseTransport(cursorContinuation()), resumableWebSocket), nil, nil},
		{"an sse transport without a continuation still refuses reconnect",
			sseOperation(IdempotencySafe, reconnectPolicy(true), sseTransport(nil)), nil, []diag.Diagnostic{unresumable}},
		{"the document default still needs a continuable transport",
			sseOperation(IdempotencySafe, nil, sseTransport(nil)), reconnectPolicy(true), []diag.Diagnostic{unresumable}},
		{"a continuation on an idempotent stream is named once",
			sseOperation(IdempotencyIdempotent, nil, sseTransport(cursorContinuation())), nil, []diag.Diagnostic{misplaced}},
		{"a continuation on a non-idempotent stream is named once",
			sseOperation(IdempotencyNonIdempotent, nil, sseTransport(bestEffortContinuation())), nil, []diag.Diagnostic{misplaced}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			diags := ValidateOperation(tc.operation, sseDocument(tc.defaults))
			if !reflect.DeepEqual(diags, tc.want) {
				t.Fatalf("diagnostics = %v, want %v", diags, tc.want)
			}
		})
	}

	// sse metadata on another transport is refused where it stands and never
	// counts as a continuation, so the reconnect is named beside it.
	stray := Transport{Protocol: TransportWebSocket, Path: "/tail", Encoding: EncodingJSON,
		WebSocket: &WebSocketTransport{Subprotocol: WebSocketSubprotocolV1}, SSE: bestEffortContinuation()}
	diags := ValidateOperation(sseOperation(IdempotencySafe, reconnectPolicy(true), stray), sseDocument(nil))
	want := []diag.Diagnostic{
		diag.Errorf(ErrorCodeInvalidTransport, "transports[0].sse", "sse metadata is valid only for sse transports"),
		unresumable,
	}
	if !reflect.DeepEqual(diags, want) {
		t.Fatalf("diagnostics = %v, want %v", diags, want)
	}
}

func TestAMalformedMisplacedOrMismatchedContinuationIsRefused(t *testing.T) {
	spectest.Proves(t, sseFeature, sseRequirement, "a-malformed-misplaced-or-mismatched-continuation-is-refused")

	// Each corpus fixture carries exactly one defect, which is what lets the
	// TypeScript reader — which stops at the first — name the same code.
	expected := loadExpectations(t)
	seen := 0
	for name, code := range expected.Invalid {
		if !strings.HasPrefix(name, "sse-") && name != "stream-wire-credential-header.openapi.json" {
			continue
		}
		seen++
		t.Run("corpus/"+name, func(t *testing.T) {
			diags := validateOpenAPIFixture(t, "fixtures/openapi/invalid/"+name)
			if len(diags) != 1 || diags[0].Code != code {
				t.Fatalf("diagnostics = %v, want exactly one %s", diags, code)
			}
		})
	}
	if seen < 14 {
		t.Fatalf("the corpus names %d continuation fixtures; the refusals below would pass vacuously", seen)
	}

	t.Run("the strict parser refuses values of the wrong JSON type", func(t *testing.T) {
		for _, transport := range []string{
			`{"protocol":"sse","path":"/tail","encoding":"json","sse":null}`,
			`{"protocol":"sse","path":"/tail","encoding":"json","sse":{"continuation":null}}`,
			`{"protocol":"sse","path":"/tail","encoding":"json","sse":{"continuation":{"mode":5}}}`,
			`{"protocol":"sse","path":"/tail","encoding":"json","sse":{"continuation":{"mode":"cursor","cursor":{"outputField":["cursor"],"queryParameter":"cursor"}}}}`,
			`{"protocol":"sse","path":"/tail","encoding":"json","sse":{"continuation":{"mode":"best-effort","mode":"cursor"}}}`,
		} {
			source := `{"stream":"server","messages":{"output":{"type":"string"}},"transports":[` + transport +
				`],"security":{"alternatives":[{"allOf":[]}]},"errors":[],"idempotency":{"kind":"safe"}}`
			if _, diags := ParseOperation([]byte(source)); !hasCode(diags, ErrorCodeParseError) {
				t.Errorf("transport %s: diagnostics = %v, want %s", transport, diags, ErrorCodeParseError)
			}
		}
	})

	t.Run("a blank cursor carrier is required", func(t *testing.T) {
		transport := sseTransport(&SSETransport{Continuation: &SSEContinuation{
			Mode: SSEContinuationCursor, Cursor: &SSECursor{OutputField: " ", QueryParameter: ""},
		}})
		want := []diag.Diagnostic{
			required("transports[0].sse.continuation.cursor.outputField"),
			required("transports[0].sse.continuation.cursor.queryParameter"),
		}
		if diags := ValidateOperation(sseOperation(IdempotencySafe, nil, transport), sseDocument(nil)); !reflect.DeepEqual(diags, want) {
			t.Fatalf("diagnostics = %v, want %v", diags, want)
		}
	})

	t.Run("references must name a plain declared string", func(t *testing.T) {
		text := Schema{Type: "string"}
		frame := func(cursor Schema, required ...string) *Schema {
			return &Schema{Type: "object", Properties: map[string]Schema{"cursor": cursor, "line": text}, Required: required}
		}
		components := map[string]Schema{
			"Frame":    *frame(text, "cursor", "line"),
			"Position": {Type: "string", MinLength: intPointer(1)},
			"Stamp":    {Type: "string", Format: "date-time"},
		}
		query := map[string]Schema{"cursor": text}
		cases := []struct {
			name   string
			output *Schema
			query  map[string]Schema
			field  string
		}{
			{"a referenced message and a referenced position", &Schema{Ref: "#/components/schemas/Frame"},
				map[string]Schema{"cursor": {Ref: "#/components/schemas/Position"}}, ""},
			{"a position through a component", frame(Schema{Ref: "#/components/schemas/Position"}, "cursor"), query, ""},
			{"a message that is not an object", &Schema{Type: "string"}, query, "outputField"},
			{"a dangling message reference", &Schema{Ref: "#/components/schemas/Missing"}, query, "outputField"},
			{"a nullable position", frame(Schema{Type: "string", Nullable: boolPointer(true)}, "cursor"), query, "outputField"},
			{"an enumerated position", frame(Schema{Type: "string", Enum: []json.RawMessage{json.RawMessage(`"a"`)}}, "cursor"), query, "outputField"},
			{"an octet position", frame(Schema{Type: "string", Format: "byte"}, "cursor"), query, "outputField"},
			{"an opaque position", frame(Schema{OpaqueJSON: OpaqueJSONAny}, "cursor"), query, "outputField"},
			{"a formatted position through a component", frame(Schema{Ref: "#/components/schemas/Stamp"}, "cursor"), query, "outputField"},
			{"a nullable query parameter", frame(text, "cursor"),
				map[string]Schema{"cursor": {Type: "string", Nullable: boolPointer(true)}}, "queryParameter"},
			{"an array query parameter", frame(text, "cursor"),
				map[string]Schema{"cursor": {Type: "array", Items: &text}}, "queryParameter"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				operation := sseOperation(IdempotencySafe, nil, sseTransport(cursorContinuation()))
				operation.Messages.Output = tc.output
				diags := ValidateSSEContinuationReferences(operation, tc.query, components)
				if tc.field == "" {
					if diags != nil {
						t.Fatalf("valid references refused: %v", diags)
					}
					return
				}
				want := "transports[0].sse.continuation.cursor." + tc.field
				if len(diags) != 1 || diags[0].Code != ErrorCodeInvalidResilience || diags[0].Field != want {
					t.Fatalf("diagnostics = %v, want one %s at %s", diags, ErrorCodeInvalidResilience, want)
				}
			})
		}
		bestEffort := sseOperation(IdempotencySafe, nil, sseTransport(bestEffortContinuation()))
		if diags := ValidateSSEContinuationReferences(bestEffort, nil, nil); diags != nil {
			t.Fatalf("best-effort names nothing to check, got %v", diags)
		}
		if diags := ValidateSSEContinuationReferences(nil, nil, nil); diags != nil {
			t.Fatalf("a nil operation produced %v", diags)
		}
	})
}

func continuedManifest(capabilities []RuntimeCapability, metadata *SSETransport, cache *CachePolicy) *GeneratedClientManifestV1 {
	manifest := cachedManifest(capabilities, cache)
	manifest.Operations = append(manifest.Operations, GeneratedOperation{
		OperationID: "tailLogs", Service: "identity", MethodSymbol: "tailLogs", Stream: StreamServer,
		Transports: []Transport{sseTransport(metadata)},
	})
	return manifest
}

func TestAManifestThatDeclaresAContinuationRequiresTheSSEContinuationRuntimeCapability(t *testing.T) {
	spectest.Proves(t, sseFeature, sseRequirement, "a-manifest-that-declares-a-continuation-requires-the-sse-continuation-runtime-capability")
	policy := &CachePolicy{FreshMs: 5000}
	both := continuedManifest(nil, cursorContinuation(), policy)
	want := []RuntimeCapability{RuntimeCapabilityResponseCache, RuntimeCapabilitySSEContinuation}
	if got := RequiredRuntimeCapabilities(both.Operations); !reflect.DeepEqual(got, want) {
		t.Fatalf("required capabilities = %v, want %v", got, want)
	}
	if got := RequiredRuntimeCapabilities(continuedManifest(nil, nil, nil).Operations); got != nil {
		t.Fatalf("an sse transport without a continuation required %v", got)
	}

	// A TypeScript target that needs the capability must name it: one that
	// omits it is refused, so it never runs a declared continuation without
	// the runtime behavior.
	unnamed := ValidateGeneratedManifest(continuedManifest(nil, bestEffortContinuation(), nil))
	named := false
	for _, diagnostic := range unnamed {
		named = named || (diagnostic.Code == ErrorCodeUnsupportedRuntimeCapability &&
			strings.Contains(diagnostic.Message, `declare an sse continuation but the target does not require the "sse-continuation"`))
	}
	if !named {
		t.Fatalf("a continuation without its capability produced %v", unnamed)
	}
	listed := ValidateGeneratedManifest(continuedManifest([]RuntimeCapability{RuntimeCapabilitySSEContinuation}, cursorContinuation(), nil))
	if listed != nil {
		t.Fatalf("a TypeScript target that requires the capability its runtime implements was refused: %v", listed)
	}

	// The Go runtime implements it too: a Go target that names it is valid,
	// and one that omits it is still refused.
	goTarget := func(capabilities []RuntimeCapability) *GeneratedClientManifestV1 {
		manifest := continuedManifest(capabilities, cursorContinuation(), nil)
		manifest.Language = GeneratedLanguageGo
		manifest.Binding.ImportPath = "example.com/identityclient"
		return manifest
	}
	if diags := ValidateGeneratedManifest(goTarget([]RuntimeCapability{RuntimeCapabilitySSEContinuation})); diags != nil {
		t.Fatalf("a Go target that requires the capability its runtime implements was refused: %v", diags)
	}
	if diags := ValidateGeneratedManifest(goTarget(nil)); !hasCode(diags, ErrorCodeUnsupportedRuntimeCapability) {
		t.Fatalf("a Go target that omits a required capability was accepted: %v", diags)
	}
	// The shared set, which the manifest schema publishes, is what every
	// runtime implements: a capability one runtime leads with is not in it.
	var common []RuntimeCapability
	for _, capability := range RuntimeCapabilitiesImplementedBy(GeneratedLanguageGo) {
		if slices.Contains(RuntimeCapabilitiesImplementedBy(GeneratedLanguageTypeScript), capability) {
			common = append(common, capability)
		}
	}
	if !reflect.DeepEqual(ImplementedRuntimeCapabilities, common) {
		t.Fatalf("shared capabilities = %v, want the intersection %v", ImplementedRuntimeCapabilities, common)
	}
	// Both runtimes implement ADR 0013, so the shared set is the full set.
	if want := []RuntimeCapability{RuntimeCapabilityResponseCache, RuntimeCapabilitySSEContinuation}; !reflect.DeepEqual(ImplementedRuntimeCapabilities, want) {
		t.Fatalf("shared capabilities = %v, want %v", ImplementedRuntimeCapabilities, want)
	}
	if got := RuntimeCapabilitiesImplementedBy("rust"); got != nil {
		t.Fatalf("an unknown language implements %v", got)
	}
	if diags := ValidateGeneratedManifest(continuedManifest(nil, nil, nil)); diags != nil {
		t.Fatalf("a manifest without a continuation was refused: %v", diags)
	}
	malformed := continuedManifest(nil, &SSETransport{Continuation: &SSEContinuation{Mode: "replay"}}, nil)
	if diags := ValidateGeneratedManifest(malformed); !hasCode(diags, ErrorCodeInvalidEnum) {
		t.Fatalf("a manifest carrying an unknown continuation mode was accepted: %v", diags)
	}
}

func TestThePublishedSchemaCarriesTheContinuationDeclaration(t *testing.T) {
	spectest.Proves(t, sseFeature, sseRequirement, "the-published-schema-carries-the-continuation-declaration")
	defs := loadSchema(t)["$defs"].(map[string]any)
	transport := defs["transport"].(map[string]any)["properties"].(map[string]any)
	if ref := transport["sse"].(map[string]any)["$ref"]; ref != "#/$defs/sseTransport" {
		t.Fatalf("transport.sse = %v", ref)
	}
	continuation := defs["sseContinuation"].(map[string]any)
	modes := continuation["properties"].(map[string]any)["mode"].(map[string]any)["enum"].([]any)
	if !reflect.DeepEqual(modes, []any{string(SSEContinuationCursor), string(SSEContinuationBestEffort)}) {
		t.Fatalf("published modes = %v", modes)
	}
	for definition, want := range map[string][]any{
		"sseTransport":    {"continuation"},
		"sseContinuation": {"mode"},
		"sseCursor":       {"outputField", "queryParameter"},
	} {
		if got := defs[definition].(map[string]any)["required"].([]any); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s.required = %v, want %v", definition, got, want)
		}
	}
}
