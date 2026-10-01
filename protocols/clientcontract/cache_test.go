package clientcontract

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

const cacheFeature = "client-contract/first-party-generated-clients"

func cachedOperation(stream StreamMode, kind IdempotencyKind, policy *CachePolicy) *OperationV1 {
	operation := &OperationV1{
		Stream:      stream,
		Transports:  []Transport{{Protocol: TransportRESTJSON, Path: "/accounts/{id}", Encoding: EncodingJSON}},
		Security:    Security{Alternatives: []SecurityAlternative{{AllOf: []SecurityRequirement{}}}},
		Errors:      []DeclaredError{},
		Idempotency: Idempotency{Kind: kind},
		Resilience:  &ResiliencePolicy{Cache: policy},
	}
	if stream == StreamServer {
		operation.Messages = &MessageShapes{Output: &Schema{Type: "string"}}
		operation.Transports = []Transport{{Protocol: TransportSSE, Path: "/accounts/{id}", Encoding: EncodingJSON}}
	}
	return operation
}

func intPointer(value int) *int { return &value }

func hasCode(diags []diag.Diagnostic, code string) bool {
	for _, diagnostic := range diags {
		if diagnostic.Code == code {
			return true
		}
	}
	return false
}

func TestACachePolicyIsAcceptedOnlyOnAUnarySafeOrIdempotentOperation(t *testing.T) {
	spectest.Proves(t, cacheFeature, "response-cache-policy", "a-cache-policy-is-accepted-only-on-a-unary-safe-or-idempotent-operation")
	document := &DocumentV1{ProtocolVersion: ProtocolVersion, Service: Service{ID: "accounts", Audience: "accounts"}, Credentials: map[string]CredentialProfile{}}
	policy := &CachePolicy{FreshMs: 5000, StaleMs: intPointer(300000)}

	for _, kind := range []IdempotencyKind{IdempotencySafe, IdempotencyIdempotent} {
		if diags := ValidateOperation(cachedOperation(StreamUnary, kind, policy), document); diag.HasErrors(diags) {
			t.Fatalf("a unary %s operation declaring a cache was refused: %v", kind, diags)
		}
	}
	if diags := ValidateOperation(cachedOperation(StreamUnary, IdempotencyNonIdempotent, policy), document); !hasCode(diags, ErrorCodeInvalidResilience) {
		t.Fatalf("a non-idempotent operation declaring a cache was accepted: %v", diags)
	}
	if diags := ValidateOperation(cachedOperation(StreamServer, IdempotencySafe, policy), document); !hasCode(diags, ErrorCodeInvalidResilience) {
		t.Fatalf("a server stream declaring a cache was accepted: %v", diags)
	}
	withDefault := *document
	withDefault.Defaults = &Defaults{Resilience: &ResiliencePolicy{Cache: policy}}
	if diags := ValidateDocument(&withDefault); !hasCode(diags, ErrorCodeInvalidResilience) {
		t.Fatalf("a document-level cache default was accepted: %v", diags)
	}
}

func TestCacheValuesAndKeyFieldsAreValidatedStrictly(t *testing.T) {
	spectest.Proves(t, cacheFeature, "response-cache-policy", "cache-values-and-key-fields-are-validated-strictly")
	document := &DocumentV1{ProtocolVersion: ProtocolVersion, Service: Service{ID: "accounts", Audience: "accounts"}, Credentials: map[string]CredentialProfile{}}
	invalid := map[string]*CachePolicy{
		"zero fresh":         {FreshMs: 0},
		"stale equals fresh": {FreshMs: 5000, StaleMs: intPointer(5000)},
		"stale below fresh":  {FreshMs: 5000, StaleMs: intPointer(10)},
		"zero max entries":   {FreshMs: 5000, MaxEntries: intPointer(0)},
		"empty key fields":   {FreshMs: 5000, KeyFields: []string{}},
		"unknown section":    {FreshMs: 5000, KeyFields: []string{"cookie.session"}},
		"bare name":          {FreshMs: 5000, KeyFields: []string{"principal"}},
		"empty name":         {FreshMs: 5000, KeyFields: []string{"path."}},
		"bad header":         {FreshMs: 5000, KeyFields: []string{"header.X Tenant"}},
		"duplicate field":    {FreshMs: 5000, KeyFields: []string{"path.id", "path.id"}},
	}
	for name, policy := range invalid {
		if diags := ValidateOperation(cachedOperation(StreamUnary, IdempotencySafe, policy), document); !diag.HasErrors(diags) {
			t.Errorf("%s: invalid cache policy was accepted", name)
		}
	}
	valid := &CachePolicy{FreshMs: 1, StaleMs: intPointer(2), MaxEntries: intPointer(1),
		KeyFields: []string{"path.id", "query.limit", "header.X-Tenant", "body", "body.principal"}}
	if diags := ValidateOperation(cachedOperation(StreamUnary, IdempotencySafe, valid), document); diag.HasErrors(diags) {
		t.Fatalf("a valid cache policy was refused: %v", diags)
	}

	inputs := CacheKeyInputs{Path: []string{"id"}, Query: []string{"limit"}, Header: []string{"x-tenant"}, Body: true, BodyProperties: []string{"principal"}}
	if diags := ValidateCacheKeyFields("resilience.cache", valid, inputs); diag.HasErrors(diags) {
		t.Fatalf("declared key fields were refused: %v", diags)
	}
	for _, keyField := range []string{"path.accountId", "query.offset", "header.X-Region", "body.action"} {
		policy := &CachePolicy{FreshMs: 1, KeyFields: []string{keyField}}
		if diags := ValidateCacheKeyFields("resilience.cache", policy, inputs); !hasCode(diags, ErrorCodeInvalidResilience) {
			t.Errorf("key field %q names no declared input and was accepted", keyField)
		}
	}
	noBody := CacheKeyInputs{Path: []string{"id"}}
	if diags := ValidateCacheKeyFields("resilience.cache", &CachePolicy{FreshMs: 1, KeyFields: []string{"body"}}, noBody); !diag.HasErrors(diags) {
		t.Fatal("a body key field on an operation without a body was accepted")
	}

	properties := CacheKeyBodyProperties(&Schema{Ref: "#/components/schemas/Query"}, map[string]Schema{
		"Query": {Type: "object", Properties: map[string]Schema{"principal": {Type: "string"}, "action": {Type: "string"}}},
	})
	if strings.Join(properties, ",") != "action,principal" {
		t.Fatalf("body properties = %v", properties)
	}
}

func TestACachePolicyRoundTripsThroughTheStrictReader(t *testing.T) {
	spectest.Proves(t, cacheFeature, "response-cache-policy", "cache-values-and-key-fields-are-validated-strictly")
	source := `{"stream":"unary","transports":[{"protocol":"rest-json","path":"/a","encoding":"json"}],"security":{"alternatives":[{"allOf":[]}]},"errors":[],"idempotency":{"kind":"safe"},"resilience":{"cache":{"freshMs":5000,"staleMs":300000,"maxEntries":64,"keyFields":["query.q"]}}}`
	operation, diags := ParseAndValidateOperation([]byte(source), nil)
	if diag.HasErrors(diags) {
		t.Fatalf("diags = %v", diags)
	}
	encoded, err := json.Marshal(operation)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != source {
		t.Fatalf("cache policy did not round trip:\n got %s\nwant %s", encoded, source)
	}
	for _, unknown := range []string{
		`{"stream":"unary","transports":[{"protocol":"rest-json","path":"/a","encoding":"json"}],"security":{"alternatives":[{"allOf":[]}]},"errors":[],"idempotency":{"kind":"safe"},"resilience":{"cache":{"freshMs":5000,"mode":"lru"}}}`,
		`{"stream":"unary","transports":[{"protocol":"rest-json","path":"/a","encoding":"json"}],"security":{"alternatives":[{"allOf":[]}]},"errors":[],"idempotency":{"kind":"safe"},"resilience":{"cache":{"freshMs":null}}}`,
	} {
		if _, diags := ParseAndValidateOperation([]byte(unknown), nil); !diag.HasErrors(diags) {
			t.Fatalf("an unrepresentable cache declaration was accepted: %s", unknown)
		}
	}
}

func cachedManifest(capabilities []RuntimeCapability, cache *CachePolicy) *GeneratedClientManifestV1 {
	return &GeneratedClientManifestV1{
		ProtocolVersion: ProtocolVersion,
		GeneratedBy:     GeneratedBy,
		Language:        GeneratedLanguageTypeScript,
		Service:         Service{ID: "identity", Audience: "identity"},
		Binding: GeneratedBinding{ImportPath: "@example/identity-client", Clients: []GeneratedBindingClient{{
			Service: "identity", ClientSymbol: "IdentityClient", BindingSymbol: "registerIdentityClient",
		}}},
		ContractSHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Operations: []GeneratedOperation{{
			OperationID: "effectiveAccess", Service: "identity", MethodSymbol: "effectiveAccess", Stream: StreamUnary,
			Transports: []Transport{{Protocol: TransportRESTJSON, Path: "/internal/authorization/effective-access", Encoding: EncodingJSON}},
			Cache:      cache,
		}},
		RuntimeCapabilities: capabilities,
		Files:               []GeneratedFile{},
	}
}

func TestAManifestThatDeclaresACachePolicyRequiresTheResponseCacheRuntimeCapability(t *testing.T) {
	spectest.Proves(t, cacheFeature, "response-cache-policy", "a-manifest-that-declares-a-cache-policy-requires-the-response-cache-runtime-capability")
	policy := &CachePolicy{FreshMs: 5000, StaleMs: intPointer(300000)}
	manifest := cachedManifest([]RuntimeCapability{RuntimeCapabilityResponseCache}, policy)
	if got := RequiredRuntimeCapabilities(manifest.Operations); len(got) != 1 || got[0] != RuntimeCapabilityResponseCache {
		t.Fatalf("required capabilities = %v", got)
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, diags := ParseAndValidateGeneratedManifest(data); diag.HasErrors(diags) {
		t.Fatalf("a consistent cached manifest was refused: %v", diags)
	}

	if diags := ValidateGeneratedManifest(cachedManifest(nil, policy)); !hasCode(diags, ErrorCodeUnsupportedRuntimeCapability) {
		t.Fatalf("a cache policy without its runtime capability was accepted: %v", diags)
	}
	if diags := ValidateGeneratedManifest(cachedManifest([]RuntimeCapability{"response-cache-v2"}, policy)); !hasCode(diags, ErrorCodeUnsupportedRuntimeCapability) {
		t.Fatalf("a capability no runtime implements was accepted: %v", diags)
	}
	if diags := ValidateGeneratedManifest(cachedManifest([]RuntimeCapability{RuntimeCapabilityResponseCache}, nil)); !hasCode(diags, ErrorCodeInvalidGeneratedManifest) {
		t.Fatalf("a capability no operation needs was accepted: %v", diags)
	}
	if diags := ValidateGeneratedManifest(cachedManifest(nil, nil)); diag.HasErrors(diags) {
		t.Fatalf("an uncached manifest without capabilities was refused: %v", diags)
	}
	withUnknownField := strings.Replace(string(data), `"freshMs":5000`, `"freshMs":5000,"mode":"lru"`, 1)
	if _, diags := ParseAndValidateGeneratedManifest([]byte(withUnknownField)); !diag.HasErrors(diags) {
		t.Fatal("a cache policy field this runtime does not implement was accepted")
	}
	stream := cachedManifest([]RuntimeCapability{RuntimeCapabilityResponseCache}, policy)
	stream.Operations[0].Stream = StreamServer
	stream.Operations[0].Transports = []Transport{{Protocol: TransportSSE, Path: "/events", Encoding: EncodingJSON}}
	if diags := ValidateGeneratedManifest(stream); !hasCode(diags, ErrorCodeInvalidResilience) {
		t.Fatalf("a cached stream operation was accepted in a manifest: %v", diags)
	}
}

func TestInvalidationFieldsNameAComparableScalarOfTheSuccessBody(t *testing.T) {
	spectest.Proves(t, cacheFeature, "response-cache-policy", "invalidation-fields-name-a-string-integer-or-boolean-property-of-the-success-body")
	document := &DocumentV1{ProtocolVersion: ProtocolVersion, Service: Service{ID: "accounts", Audience: "accounts"}, Credentials: map[string]CredentialProfile{}}
	invalid := map[string]*CachePolicy{
		"empty list":         {FreshMs: 5000, InvalidationFields: []string{}},
		"duplicate field":    {FreshMs: 5000, InvalidationFields: []string{"principalId", "principalId"}},
		"blank field":        {FreshMs: 5000, InvalidationFields: []string{""}},
		"padded field":       {FreshMs: 5000, InvalidationFields: []string{" principalId"}},
		"control character":  {FreshMs: 5000, InvalidationFields: []string{"principal\u0000Id"}},
		"delete character":   {FreshMs: 5000, InvalidationFields: []string{"principal\u007fId"}},
		"second entry blank": {FreshMs: 5000, InvalidationFields: []string{"principalId", "\t"}},
	}
	for name, policy := range invalid {
		if diags := ValidateOperation(cachedOperation(StreamUnary, IdempotencySafe, policy), document); !diag.HasErrors(diags) {
			t.Errorf("%s: invalid invalidation fields were accepted", name)
		}
	}
	duplicate := ValidateOperation(cachedOperation(StreamUnary, IdempotencySafe, invalid["duplicate field"]), document)
	if !hasCode(duplicate, ErrorCodeDuplicate) {
		t.Fatalf("a duplicate invalidation field carried %v, want %s", duplicate, ErrorCodeDuplicate)
	}
	valid := &CachePolicy{FreshMs: 5000, InvalidationFields: []string{"principalId", "tenant.id", "version", "active"}}
	if diags := ValidateOperation(cachedOperation(StreamUnary, IdempotencySafe, valid), document); diag.HasErrors(diags) {
		t.Fatalf("valid invalidation fields were refused: %v", diags)
	}

	components := map[string]Schema{
		"Access": {Type: "object", Properties: map[string]Schema{
			"principalId": {Type: "string", Format: "uuid"},
			"tenant.id":   {Type: "string"},
			"version":     {Type: "integer", Format: "int64"},
			"active":      {Type: "boolean", Nullable: boolPointer(true)},
			"state":       {Ref: "#/components/schemas/State"},
			"score":       {Type: "number"},
			"scopes":      {Type: "array", Items: &Schema{Type: "string"}},
			"owner":       {Ref: "#/components/schemas/Owner"},
			"payload":     {OpaqueJSON: OpaqueJSONAny},
			"either":      {OneOf: []Schema{{Type: "string"}, {Type: "boolean"}}},
		}},
		"State": {Type: "string", Enum: []json.RawMessage{json.RawMessage(`"active"`)}},
		"Owner": {Type: "object", Properties: map[string]Schema{"id": {Type: "string"}}},
		// A second success variant declares one property as a number: a field
		// every body cannot compare is refused, whichever variant is stored.
		"Moved": {Type: "object", Properties: map[string]Schema{"principalId": {Type: "string"}, "version": {Type: "number"}}},
	}
	properties := CacheResponseProperties([]*Schema{
		{Ref: "#/components/schemas/Access", Nullable: boolPointer(false)},
		{Ref: "#/components/schemas/Moved"},
		{Type: "string"},
		{Ref: "#/components/schemas/Missing"},
		nil,
	}, components)
	for name, want := range map[string]bool{
		"principalId": true, "tenant.id": true, "active": true, "state": true,
		"version": false, "score": false, "scopes": false, "owner": false, "payload": false, "either": false,
	} {
		if got, ok := properties[name]; !ok || got != want {
			t.Errorf("property %q: comparable=%v declared=%v, want comparable=%v", name, got, ok, want)
		}
	}
	if _, ok := properties["id"]; ok {
		t.Fatal("a nested property was read as a top-level one")
	}
	accepted := &CachePolicy{FreshMs: 1, InvalidationFields: []string{"principalId", "tenant.id", "active", "state"}}
	if diags := ValidateCacheInvalidationFields("resilience.cache", accepted, properties); diag.HasErrors(diags) {
		t.Fatalf("declared scalar invalidation fields were refused: %v", diags)
	}
	for _, field := range []string{"principal", "id", "version", "score", "scopes", "owner", "payload", "either"} {
		policy := &CachePolicy{FreshMs: 1, InvalidationFields: []string{field}}
		if diags := ValidateCacheInvalidationFields("resilience.cache", policy, properties); !hasCode(diags, ErrorCodeInvalidResilience) {
			t.Errorf("invalidation field %q was accepted", field)
		}
	}
	if diags := ValidateCacheInvalidationFields("resilience.cache", nil, properties); diags != nil {
		t.Fatalf("an absent policy produced %v", diags)
	}
	if diags := ValidateCacheInvalidationFields("resilience.cache", &CachePolicy{FreshMs: 1, InvalidationFields: []string{" x"}}, properties); !hasCode(diags, ErrorCodeInvalidResilience) {
		t.Fatalf("a malformed field reached the schema check: %v", diags)
	}

	source := `{"stream":"unary","transports":[{"protocol":"rest-json","path":"/a","encoding":"json"}],"security":{"alternatives":[{"allOf":[]}]},"errors":[],"idempotency":{"kind":"safe"},"resilience":{"cache":{"freshMs":5000,"keyFields":["query.q"],"invalidationFields":["principalId"]}}}`
	operation, diags := ParseAndValidateOperation([]byte(source), nil)
	if diag.HasErrors(diags) {
		t.Fatalf("diags = %v", diags)
	}
	encoded, err := json.Marshal(operation)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != source {
		t.Fatalf("invalidation fields did not round trip:\n got %s\nwant %s", encoded, source)
	}
}

func boolPointer(value bool) *bool { return &value }

// TestInvalidationFieldComparabilityMatchesTheSharedVectors pins the property
// rule every contract check applies — both strict readers and both providers —
// to the vectors the TypeScript reader and provider consume too. A field
// whose schema is not comparable would tag an answer in one runtime and not in
// the other, so every check refuses it.
func TestInvalidationFieldComparabilityMatchesTheSharedVectors(t *testing.T) {
	spectest.Proves(t, cacheFeature, "response-cache-policy", "invalidation-fields-name-a-string-integer-or-boolean-property-of-the-success-body")
	data, err := os.ReadFile("fixtures/cache/invalidation.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors struct {
		PropertySchemas []struct {
			Name       string          `json:"name"`
			Schema     json.RawMessage `json:"schema"`
			Comparable bool            `json:"comparable"`
		} `json:"propertySchemas"`
	}
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors.PropertySchemas) == 0 {
		t.Fatal("the shared property schema vectors are empty")
	}
	for _, vector := range vectors.PropertySchemas {
		t.Run(vector.Name, func(t *testing.T) {
			var schema Schema
			if err := json.Unmarshal(vector.Schema, &schema); err != nil {
				t.Fatal(err)
			}
			inline := CacheResponseProperties([]*Schema{{Type: "object", Properties: map[string]Schema{"field": schema}}}, nil)
			referenced := CacheResponseProperties([]*Schema{{Type: "object", Properties: map[string]Schema{
				"field": {Ref: "#/components/schemas/Field"},
			}}}, map[string]Schema{"Field": schema})
			if inline["field"] != vector.Comparable || referenced["field"] != vector.Comparable {
				t.Fatalf("comparable inline=%v referenced=%v, want %v", inline["field"], referenced["field"], vector.Comparable)
			}
			diags := ValidateCacheInvalidationFields("resilience.cache", &CachePolicy{FreshMs: 1, InvalidationFields: []string{"field"}}, inline)
			if diag.HasErrors(diags) == vector.Comparable {
				t.Fatalf("diagnostics %v, want refused=%v", diags, !vector.Comparable)
			}
		})
	}
}
