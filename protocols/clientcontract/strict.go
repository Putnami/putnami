package clientcontract

import (
	"encoding/json"
	"fmt"
	"math/big"
	"net/url"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// Stable diagnostic codes returned by strict client contract parsing and validation.
const (
	ErrorCodeParseError               = "client_contract.parse_error"
	ErrorCodeUnknownField             = "client_contract.unknown_field"
	ErrorCodeInvalidVersion           = "client_contract.invalid_protocol_version"
	ErrorCodeRequired                 = "client_contract.required"
	ErrorCodeInvalidEnum              = "client_contract.invalid_enum"
	ErrorCodeInvalidCredential        = "client_contract.invalid_credential"
	ErrorCodeUnknownProfile           = "client_contract.unknown_profile"
	ErrorCodeDuplicate                = "client_contract.duplicate"
	ErrorCodeInvalidTransport         = "client_contract.invalid_transport"
	ErrorCodeInvalidSecurity          = "client_contract.invalid_security"
	ErrorCodeInvalidError             = "client_contract.invalid_error"
	ErrorCodeInvalidIdempotency       = "client_contract.invalid_idempotency"
	ErrorCodeInvalidResilience        = "client_contract.invalid_resilience"
	ErrorCodeInvalidSchema            = "client_contract.invalid_schema"
	ErrorCodeInvalidProtobuf          = "client_contract.invalid_protobuf"
	ErrorCodeInvalidGeneratedManifest = "client_contract.invalid_generated_manifest"
	// ErrorCodeUnsupportedRuntimeCapability names a generated target that
	// declares a behavior its runtime requirement does not cover, or requires a
	// runtime capability the runtimes of this protocol version do not implement.
	ErrorCodeUnsupportedRuntimeCapability = "client_contract.unsupported_runtime_capability"
)

// ValidErrorCodes is the closed set of diagnostic codes emitted by this package.
var ValidErrorCodes = map[string]bool{
	ErrorCodeParseError: true, ErrorCodeUnknownField: true,
	ErrorCodeInvalidVersion: true, ErrorCodeRequired: true,
	ErrorCodeInvalidEnum: true, ErrorCodeInvalidCredential: true,
	ErrorCodeUnknownProfile: true, ErrorCodeDuplicate: true,
	ErrorCodeInvalidTransport: true, ErrorCodeInvalidSecurity: true,
	ErrorCodeInvalidError: true, ErrorCodeInvalidIdempotency: true,
	ErrorCodeInvalidResilience: true, ErrorCodeInvalidSchema: true,
	ErrorCodeInvalidProtobuf:              true,
	ErrorCodeInvalidGeneratedManifest:     true,
	ErrorCodeUnsupportedRuntimeCapability: true,
}

// ParseDocument decodes document metadata and rejects unknown or ambiguous JSON.
func ParseDocument(data []byte) (*DocumentV1, []diag.Diagnostic) {
	if err := requireDocumentJSONMembers(data); err != nil {
		return nil, []diag.Diagnostic{decodeDiagnostic(err)}
	}
	var document DocumentV1
	if err := decodeStrict(data, &document); err != nil {
		return nil, []diag.Diagnostic{decodeDiagnostic(err)}
	}
	return &document, nil
}

// ParseOperation decodes operation metadata and rejects unknown or ambiguous JSON.
func ParseOperation(data []byte) (*OperationV1, []diag.Diagnostic) {
	if err := requireOperationJSONMembers(data); err != nil {
		return nil, []diag.Diagnostic{decodeDiagnostic(err)}
	}
	var operation OperationV1
	if err := decodeStrict(data, &operation); err != nil {
		return nil, []diag.Diagnostic{decodeDiagnostic(err)}
	}
	return &operation, nil
}

func requireDocumentJSONMembers(data []byte) error {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(data, &document); err != nil {
		return err
	}
	protobufRaw, exists := document["protobuf"]
	if !exists {
		return nil
	}
	var protobuf map[string]json.RawMessage
	if err := json.Unmarshal(protobufRaw, &protobuf); err != nil {
		return err
	}
	var services []map[string]json.RawMessage
	if err := json.Unmarshal(protobuf["services"], &services); err != nil {
		return err
	}
	for serviceIndex, service := range services {
		var methods []map[string]json.RawMessage
		if err := json.Unmarshal(service["methods"], &methods); err != nil {
			return err
		}
		for methodIndex, method := range methods {
			for _, member := range []string{"clientStreaming", "serverStreaming"} {
				if _, ok := method[member]; !ok {
					return fmt.Errorf("protobuf.services[%d].methods[%d].%s: field is required", serviceIndex, methodIndex, member)
				}
			}
		}
	}
	var enums []map[string]json.RawMessage
	if err := json.Unmarshal(protobuf["enums"], &enums); err != nil {
		return err
	}
	for enumIndex, enum := range enums {
		var values []map[string]json.RawMessage
		if err := json.Unmarshal(enum["values"], &values); err != nil {
			return err
		}
		for valueIndex, value := range values {
			if _, ok := value["number"]; !ok {
				return fmt.Errorf("protobuf.enums[%d].values[%d].number: field is required", enumIndex, valueIndex)
			}
		}
	}
	return nil
}

func requireOperationJSONMembers(data []byte) error {
	var operation map[string]json.RawMessage
	if err := json.Unmarshal(data, &operation); err != nil {
		return err
	}
	var transports []map[string]json.RawMessage
	if err := json.Unmarshal(operation["transports"], &transports); err != nil {
		return err
	}
	for index, transport := range transports {
		var protocol TransportProtocol
		if err := json.Unmarshal(transport["protocol"], &protocol); err != nil {
			return err
		}
		if protocol != TransportWebSocket {
			continue
		}
		websocketRaw, ok := transport["websocket"]
		if !ok {
			return fmt.Errorf("transports[%d].websocket: field is required", index)
		}
		var websocket map[string]json.RawMessage
		if err := json.Unmarshal(websocketRaw, &websocket); err != nil {
			return err
		}
		if _, ok := websocket["resume"]; !ok {
			return fmt.Errorf("transports[%d].websocket.resume: field is required", index)
		}
	}
	return nil
}

// ParseAndValidateDocument strictly decodes and validates document metadata.
func ParseAndValidateDocument(data []byte) (*DocumentV1, []diag.Diagnostic) {
	document, diags := ParseDocument(data)
	if document == nil {
		return nil, diags
	}
	diags = append(diags, ValidateDocument(document)...)
	return document, diags
}

// ParseAndValidateOperation strictly decodes and validates operation metadata.
func ParseAndValidateOperation(data []byte, document *DocumentV1) (*OperationV1, []diag.Diagnostic) {
	operation, diags := ParseOperation(data)
	if operation == nil {
		return nil, diags
	}
	diags = append(diags, ValidateOperation(operation, document)...)
	return operation, diags
}

// ParseAndValidateSchema strictly decodes and validates an authored schema.
func ParseAndValidateSchema(data []byte) (*Schema, []diag.Diagnostic) {
	var schema Schema
	if err := decodeStrict(data, &schema); err != nil {
		return nil, []diag.Diagnostic{decodeDiagnostic(err)}
	}
	diags := ValidateSchema(&schema)
	return &schema, diags
}

// ValidateSchema validates a standard request, response, component, or error
// schema against the first-party subset. Readers use it for every schema, not
// only schemas nested in x-putnami-client error metadata.
func ValidateSchema(schema *Schema) []diag.Diagnostic {
	return validateSchema("schema", schema)
}

func decodeDiagnostic(err error) diag.Diagnostic {
	message := err.Error()
	if strings.Contains(message, "unknown field") {
		return diag.Errorf(ErrorCodeUnknownField, "", "%s", message)
	}
	return diag.Errorf(ErrorCodeParseError, "", "%s", message)
}

// ValidateDocument validates the document-level first-party client contract.
func ValidateDocument(document *DocumentV1) []diag.Diagnostic {
	if document == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "document metadata is nil")}
	}
	var diags []diag.Diagnostic
	if document.ProtocolVersion != ProtocolVersion {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidVersion, "protocolVersion",
			"protocolVersion %d is unsupported; expected %d", document.ProtocolVersion, ProtocolVersion))
	}
	if blank(document.Service.ID) {
		diags = append(diags, required("service.id"))
	}
	if blank(document.Service.Audience) {
		diags = append(diags, required("service.audience"))
	}
	if document.Credentials == nil {
		diags = append(diags, required("credentials"))
	}
	names := make([]string, 0, len(document.Credentials))
	for name := range document.Credentials {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		field := fmt.Sprintf("credentials[%q]", name)
		profile := document.Credentials[name]
		if blank(name) {
			diags = append(diags, required(field))
		}
		diags = append(diags, validateCredential(field, profile)...)
	}
	if document.Defaults != nil && document.Defaults.Resilience != nil {
		diags = append(diags, validateResilience("defaults.resilience", document.Defaults.Resilience)...)
		if document.Defaults.Resilience.Cache != nil {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidResilience, "defaults.resilience.cache",
				"a response cache is declared per operation; a document default would cache operations that never asked for it"))
		}
	}
	if document.Protobuf != nil {
		diags = append(diags, validateProtobuf("protobuf", document.Protobuf)...)
	}
	return diags
}

func validateCredential(field string, profile CredentialProfile) []diag.Diagnostic {
	var diags []diag.Diagnostic
	switch profile.Kind {
	case CredentialServiceToken:
		if profile.Header != "" {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidCredential, field+".header",
				"service-token obtains and injects its bearer token through the framework; header is not configurable"))
		}
		diags = append(diags, validateStrings(field+".scopes", profile.Scopes)...)
	case CredentialForwardedUserToken:
		if profile.Audience != "" || profile.Header != "" || len(profile.Scopes) > 0 {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidCredential, field,
				"forwarded-user-token has no profile fields; operation requirements declare any scopes"))
		}
	case CredentialAPIKey, CredentialNamedHeader:
		if blank(profile.Header) {
			diags = append(diags, required(field+".header"))
		} else if !validHeaderName(profile.Header) || reservedCredentialHeader(profile.Header) {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidCredential, field+".header",
				"header %q is not an allowed credential header name", profile.Header))
		}
		if profile.Audience != "" || len(profile.Scopes) > 0 {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidCredential, field,
				"%s accepts only a header name; credential values come from the consumer binding", profile.Kind))
		}
	default:
		diags = append(diags, diag.Errorf(ErrorCodeInvalidEnum, field+".kind",
			"credential kind %q is unsupported", profile.Kind))
	}
	return diags
}

// ValidateOperation validates operation metadata against its provider document.
func ValidateOperation(operation *OperationV1, document *DocumentV1) []diag.Diagnostic {
	if operation == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "operation metadata is nil")}
	}
	var diags []diag.Diagnostic
	if !validStream(operation.Stream) {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidEnum, "stream",
			"stream mode %q is unsupported", operation.Stream))
	}
	byteStream := false
	for _, transport := range operation.Transports {
		byteStream = byteStream || transport.ByteStream()
	}
	diags = append(diags, validateMessageShapes(operation.Stream, operation.Messages, byteStream)...)
	diags = append(diags, validateTransports(operation.Stream, operation.Transports)...)
	needsProtobuf := false
	for _, transport := range operation.Transports {
		if transport.Protocol == TransportConnect || transport.Encoding == EncodingProto {
			needsProtobuf = true
		}
	}
	if needsProtobuf && (document == nil || document.Protobuf == nil) {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidProtobuf, "transports",
			"Connect and proto transports require the document protobuf descriptor projection"))
	}
	diags = append(diags, validateSecurity(operation.Security, document)...)
	diags = append(diags, validateErrors(operation.Errors)...)
	diags = append(diags, validateIdempotency(operation.Idempotency)...)
	diags = append(diags, validateIdempotencyCredentialCollision(operation, document)...)
	if operation.Resilience != nil {
		diags = append(diags, validateResilience("resilience", operation.Resilience)...)
		diags = append(diags, validateCacheScope(operation)...)
	}
	diags = append(diags, validateStreamContinuation(operation, document)...)
	return diags
}

// validateCacheScope admits a response cache only where replaying a stored
// answer is indistinguishable from calling again: one request, one response,
// and no side effect. A stream has no single answer to store, and a
// non-idempotent operation called twice is not the same as called once.
func validateCacheScope(operation *OperationV1) []diag.Diagnostic {
	if operation.Resilience == nil || operation.Resilience.Cache == nil {
		return nil
	}
	var diags []diag.Diagnostic
	if operation.Stream != StreamUnary {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidResilience, "resilience.cache",
			"a response cache requires a unary operation; stream mode %q has no single answer to store", operation.Stream))
	}
	if operation.Idempotency.Kind != IdempotencySafe && operation.Idempotency.Kind != IdempotencyIdempotent {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidResilience, "resilience.cache",
			"a response cache requires a safe or idempotent operation; idempotency kind %q repeats effects a stored answer would skip",
			operation.Idempotency.Kind))
	}
	return diags
}

// validateCachePolicy checks the values of one cache declaration. Its scope
// (unary, safe or idempotent, operation-level) is checked by the callers that
// know the operation.
func validateCachePolicy(field string, policy *CachePolicy) []diag.Diagnostic {
	var diags []diag.Diagnostic
	if policy.FreshMs <= 0 {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidResilience, field+".freshMs", "value must be positive"))
	}
	if policy.StaleMs != nil {
		if *policy.StaleMs <= 0 {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidResilience, field+".staleMs", "value must be positive"))
		} else if *policy.StaleMs <= policy.FreshMs {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidResilience, field+".staleMs",
				"staleMs must exceed freshMs; omit it to never serve a stale answer"))
		}
	}
	if policy.MaxEntries != nil && *policy.MaxEntries <= 0 {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidResilience, field+".maxEntries", "value must be positive"))
	}
	if policy.KeyFields != nil && len(policy.KeyFields) == 0 {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidResilience, field+".keyFields",
			"keyFields must name at least one field; omit it to key on the whole request"))
	}
	diags = append(diags, validateStrings(field+".keyFields", policy.KeyFields)...)
	for i, keyField := range policy.KeyFields {
		if _, _, err := ParseCacheKeyField(keyField); err != nil {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidResilience, fmt.Sprintf("%s.keyFields[%d]", field, i), "%v", err))
		}
	}
	if policy.InvalidationFields != nil && len(policy.InvalidationFields) == 0 {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidResilience, field+".invalidationFields",
			"invalidationFields must name at least one field; omit it to drop answers by key prefix only"))
	}
	diags = append(diags, validateStrings(field+".invalidationFields", policy.InvalidationFields)...)
	for i, invalidationField := range policy.InvalidationFields {
		if err := ParseCacheInvalidationField(invalidationField); err != nil {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidResilience, fmt.Sprintf("%s.invalidationFields[%d]", field, i), "%v", err))
		}
	}
	return diags
}

// ParseCacheInvalidationField checks one declared invalidation field: the name
// of a top-level property of the success body, as it appears on the wire, with
// no leading or trailing space and no control character. The rule names bytes,
// not a language's idea of white space, so both readers apply it identically.
func ParseCacheInvalidationField(value string) error {
	if value == "" || value[0] == ' ' || value[len(value)-1] == ' ' {
		return fmt.Errorf("invalidation field %q must name a top-level response property", value)
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("invalidation field %q must not contain control characters", value)
		}
	}
	return nil
}

// CacheResponseProperties lists the top-level properties an operation's JSON
// success bodies declare, each mapped to whether every body that declares it
// declares a string that is not octets, an integer or a boolean — the values
// both runtimes compare exactly (ADR 0007). It follows one local component reference for a body
// and for each property. A body that is not an object with declared
// properties contributes nothing.
func CacheResponseProperties(bodies []*Schema, components map[string]Schema) map[string]bool {
	properties := map[string]bool{}
	for _, body := range bodies {
		resolved := resolveCacheSchema(body, components)
		if resolved == nil {
			continue
		}
		for name, property := range resolved.Properties {
			scalar := cacheComparableScalar(resolveCacheSchema(&property, components))
			if previous, seen := properties[name]; seen {
				scalar = scalar && previous
			}
			properties[name] = scalar
		}
	}
	return properties
}

// resolveCacheSchema follows one local component reference. Nullability, the
// only thing a reference may add beside it, never changes the value kind.
func resolveCacheSchema(schema *Schema, components map[string]Schema) *Schema {
	if schema == nil || schema.Ref == "" {
		return schema
	}
	name, found := strings.CutPrefix(schema.Ref, "#/components/schemas/")
	resolved, ok := components[name]
	if !found || !ok {
		return nil
	}
	return &resolved
}

// cacheComparableScalar reports a schema whose values both runtimes render to
// the same text: a string, an integer or a boolean. A number is refused — a
// TypeScript runtime holds a double, which keeps no lexeme — and so is every
// composite, union or opaque value. So are octets: their JSON form is base64
// text, which Go would tag, but a TypeScript runtime decodes a `byte` or
// `binary` string to a byte array no invalidation value can equal, so it would
// tag nothing and drop nothing.
func cacheComparableScalar(schema *Schema) bool {
	if schema == nil || schema.Ref != "" || len(schema.OneOf) > 0 || schema.OpaqueJSON != "" {
		return false
	}
	switch schema.Type {
	case "string":
		return schema.Format != "byte" && schema.Format != "binary"
	case "integer", "boolean":
		return true
	default:
		return false
	}
}

// ValidateCacheInvalidationFields checks that every declared invalidation
// field names a string (not octets), integer or boolean property of the operation's JSON
// success body. A field that names nothing would tag no answer, so the
// consumer's invalidation would silently drop nothing; it is refused rather
// than tolerated.
func ValidateCacheInvalidationFields(field string, policy *CachePolicy, properties map[string]bool) []diag.Diagnostic {
	if policy == nil {
		return nil
	}
	var diags []diag.Diagnostic
	for i, invalidationField := range policy.InvalidationFields {
		entry := fmt.Sprintf("%s.invalidationFields[%d]", field, i)
		if err := ParseCacheInvalidationField(invalidationField); err != nil {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidResilience, entry, "%v", err))
			continue
		}
		scalar, declared := properties[invalidationField]
		switch {
		case !declared:
			diags = append(diags, diag.Errorf(ErrorCodeInvalidResilience, entry,
				"invalidation field %q names no top-level property of a JSON success body this operation declares", invalidationField))
		case !scalar:
			diags = append(diags, diag.Errorf(ErrorCodeInvalidResilience, entry,
				"invalidation field %q must name a string, integer or boolean property; the runtimes compare no other value", invalidationField))
		}
	}
	return diags
}

// ParseCacheKeyField splits a declared key field into its section and name.
// The whole body is ("body", ""). Header names are returned as declared; the
// runtimes compare them case-insensitively.
func ParseCacheKeyField(value string) (section, name string, err error) {
	if value == CacheKeySectionBody {
		return CacheKeySectionBody, "", nil
	}
	section, name, found := strings.Cut(value, ".")
	if !found || name == "" || strings.TrimSpace(name) != name {
		return "", "", fmt.Errorf("key field %q must be body or <path|query|header|body>.<name>", value)
	}
	switch section {
	case CacheKeySectionPath, CacheKeySectionQuery, CacheKeySectionBody:
	case CacheKeySectionHeader:
		if !validHeaderName(name) {
			return "", "", fmt.Errorf("key field %q does not name a valid header", value)
		}
	default:
		return "", "", fmt.Errorf("key field %q must be body or <path|query|header|body>.<name>", value)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return "", "", fmt.Errorf("key field %q must not contain control characters", value)
		}
	}
	return section, name, nil
}

// CacheKeyInputs names the request inputs an operation declares, so a cache
// declaration can be checked against the fields it would key on.
type CacheKeyInputs struct {
	// Path, Query and Header list declared parameter names.
	Path   []string
	Query  []string
	Header []string
	// Body reports whether the operation declares a request body.
	Body bool
	// BodyProperties lists the top-level properties of a JSON object body.
	// Nil when the body is not a closed JSON object, in which case only the
	// whole body can be a key field.
	BodyProperties []string
}

// CacheKeyBodyProperties lists the top-level properties of a JSON request body
// schema, following one local component reference. It returns nil when the
// schema is not an object with declared properties, so only the whole body can
// then be a key field.
func CacheKeyBodyProperties(schema *Schema, components map[string]Schema) []string {
	if schema == nil {
		return nil
	}
	if schema.Ref != "" {
		name, found := strings.CutPrefix(schema.Ref, "#/components/schemas/")
		resolved, ok := components[name]
		if !found || !ok {
			return nil
		}
		schema = &resolved
	}
	if len(schema.Properties) == 0 {
		return nil
	}
	names := make([]string, 0, len(schema.Properties))
	for name := range schema.Properties {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ValidateCacheKeyFields checks that every declared key field names an input
// the operation declares. A field that names nothing would key every request
// on the same absent value and hand one caller another caller's answer, so it
// is refused rather than tolerated.
func ValidateCacheKeyFields(field string, policy *CachePolicy, inputs CacheKeyInputs) []diag.Diagnostic {
	if policy == nil {
		return nil
	}
	var diags []diag.Diagnostic
	for i, keyField := range policy.KeyFields {
		entry := fmt.Sprintf("%s.keyFields[%d]", field, i)
		section, name, err := ParseCacheKeyField(keyField)
		if err != nil {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidResilience, entry, "%v", err))
			continue
		}
		declared := false
		switch section {
		case CacheKeySectionPath:
			declared = contains(inputs.Path, name)
		case CacheKeySectionQuery:
			declared = contains(inputs.Query, name)
		case CacheKeySectionHeader:
			for _, header := range inputs.Header {
				if strings.EqualFold(header, name) {
					declared = true
				}
			}
		case CacheKeySectionBody:
			declared = inputs.Body && (name == "" || contains(inputs.BodyProperties, name))
		}
		if !declared {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidResilience, entry,
				"key field %q names no request input this operation declares", keyField))
		}
	}
	return diags
}

func validateIdempotencyCredentialCollision(operation *OperationV1, document *DocumentV1) []diag.Diagnostic {
	if operation.Idempotency.Kind != IdempotencyIdempotent || operation.Idempotency.KeyHeader == "" || document == nil {
		return nil
	}
	want := strings.ToLower(operation.Idempotency.KeyHeader)
	for i, alternative := range operation.Security.Alternatives {
		for j, requirement := range alternative.AllOf {
			profile, ok := document.Credentials[requirement.Profile]
			if !ok || strings.ToLower(credentialHeader(profile)) != want {
				continue
			}
			return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidIdempotency, "idempotency.keyHeader",
				"keyHeader conflicts with credential profile %q in security.alternatives[%d].allOf[%d]",
				requirement.Profile, i, j)}
		}
	}
	return nil
}

func validateMessageShapes(stream StreamMode, messages *MessageShapes, byteStream bool) []diag.Diagnostic {
	var diags []diag.Diagnostic
	if byteStream {
		// Raw octets have no schema: a message schema beside them would describe
		// a value the wire never carries.
		if messages != nil {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidSchema, "messages",
				"a provider-owned byte stream carries raw octets and must omit messages"))
		}
		return diags
	}
	switch stream {
	case StreamUnary:
		if messages != nil {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidSchema, "messages",
				"unary operations use standard OpenAPI request and response schemas and must omit messages"))
		}
	case StreamServer:
		if messages == nil || messages.Output == nil {
			diags = append(diags, required("messages.output"))
		} else {
			diags = append(diags, validateSchema("messages.output", messages.Output)...)
		}
		if messages != nil && messages.Input != nil {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidSchema, "messages.input",
				"server streams use the standard OpenAPI request schema and must omit messages.input"))
		}
	case StreamClient:
		if messages == nil || messages.Input == nil {
			diags = append(diags, required("messages.input"))
		} else {
			diags = append(diags, validateSchema("messages.input", messages.Input)...)
		}
		if messages != nil && messages.Output != nil {
			diags = append(diags, validateSchema("messages.output", messages.Output)...)
		}
	case StreamBidirectional:
		if messages == nil || messages.Input == nil {
			diags = append(diags, required("messages.input"))
		} else {
			diags = append(diags, validateSchema("messages.input", messages.Input)...)
		}
		if messages == nil || messages.Output == nil {
			diags = append(diags, required("messages.output"))
		} else {
			diags = append(diags, validateSchema("messages.output", messages.Output)...)
		}
	}
	return diags
}

// ValidateOperationForID also joins an operation to its protobuf descriptor
// entry. OpenAPI readers should use this form because the operationId is the
// stable key shared by standards artifacts and the generated descriptor.
func ValidateOperationForID(operationID string, operation *OperationV1, document *DocumentV1) []diag.Diagnostic {
	diags := ValidateOperation(operation, document)
	if operation == nil || document == nil || document.Protobuf == nil {
		return diags
	}
	needsProtobuf := false
	for _, transport := range operation.Transports {
		if transport.Protocol == TransportConnect || transport.Encoding == EncodingProto {
			needsProtobuf = true
			break
		}
	}
	if !needsProtobuf {
		return diags
	}
	wantClient := operation.Stream == StreamClient || operation.Stream == StreamBidirectional
	wantServer := operation.Stream == StreamServer || operation.Stream == StreamBidirectional
	methods := protobufMethods(document.Protobuf)
	for i, transport := range operation.Transports {
		if transport.Protocol != TransportConnect && transport.Encoding != EncodingProto {
			continue
		}
		method, ok := methods[transport.ProtobufMethod]
		if !ok {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidProtobuf,
				fmt.Sprintf("transports[%d].protobufMethod", i),
				"operation %q references protobuf method %q which is not declared", operationID, transport.ProtobufMethod))
			continue
		}
		if method.ClientStreaming != wantClient || method.ServerStreaming != wantServer {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidProtobuf,
				fmt.Sprintf("transports[%d].protobufMethod", i),
				"protobuf streaming flags do not match operation stream mode %q", operation.Stream))
		}
	}
	return diags
}

func validateTransports(mode StreamMode, transports []Transport) []diag.Diagnostic {
	if len(transports) == 0 {
		return []diag.Diagnostic{required("transports")}
	}
	var diags []diag.Diagnostic
	seen := map[string]bool{}
	hasWebSocket := false
	providerWires := 0
	for i, transport := range transports {
		field := fmt.Sprintf("transports[%d]", i)
		if err := validateTransportPath(transport.Path); err != nil {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidTransport, field+".path",
				"%v", err))
		}
		if transport.SSE != nil && transport.Protocol != TransportSSE {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidTransport, field+".sse",
				"sse metadata is valid only for sse transports"))
		}
		switch transport.Protocol {
		case TransportRESTJSON:
			if transport.Encoding != EncodingJSON || mode != StreamUnary {
				diags = append(diags, diag.Errorf(ErrorCodeInvalidTransport, field,
					"rest-json requires json encoding and unary stream mode"))
			}
			if transport.WebSocket != nil {
				diags = append(diags, diag.Errorf(ErrorCodeInvalidTransport, field+".websocket",
					"websocket metadata is valid only for websocket transports"))
			}
		case TransportSSE:
			if transport.Encoding != EncodingJSON || mode != StreamServer {
				diags = append(diags, diag.Errorf(ErrorCodeInvalidTransport, field,
					"sse requires json encoding and server stream mode"))
			}
			if transport.WebSocket != nil {
				diags = append(diags, diag.Errorf(ErrorCodeInvalidTransport, field+".websocket",
					"websocket metadata is valid only for websocket transports"))
			}
			if transport.SSE != nil {
				diags = append(diags, validateSSETransport(field+".sse", transport.SSE)...)
			}
		case TransportConnect:
			if transport.Encoding != EncodingJSON && transport.Encoding != EncodingProto {
				diags = append(diags, diag.Errorf(ErrorCodeInvalidTransport, field+".encoding",
					"connect encoding must be json or proto"))
			}
			if transport.WebSocket != nil {
				diags = append(diags, diag.Errorf(ErrorCodeInvalidTransport, field+".websocket",
					"websocket metadata is valid only for websocket transports"))
			}
		case TransportWebSocket:
			hasWebSocket = true
			if transport.ProviderWire() {
				providerWires++
			}
			diags = append(diags, validateWebSocketTransport(mode, field, transport)...)
		default:
			diags = append(diags, diag.Errorf(ErrorCodeInvalidEnum, field+".protocol",
				"transport protocol %q is unsupported", transport.Protocol))
		}
		needsMethod := transport.Protocol == TransportConnect || transport.Encoding == EncodingProto
		if needsMethod && blank(transport.ProtobufMethod) {
			diags = append(diags, required(field+".protobufMethod"))
		} else if !needsMethod && transport.ProtobufMethod != "" {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidTransport, field+".protobufMethod",
				"protobufMethod is valid only for Connect or proto transports"))
		}
		key := fmt.Sprintf("%s\x00%s\x00%s\x00%s", transport.Protocol, transport.Path, transport.Encoding, transport.ProtobufMethod)
		if seen[key] {
			diags = append(diags, diag.Errorf(ErrorCodeDuplicate, field,
				"transport repeats an earlier protocol, path, and encoding"))
		}
		seen[key] = true
	}
	if (mode == StreamClient || mode == StreamBidirectional) && !hasWebSocket {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidTransport, "transports",
			"%s streaming requires a websocket transport in protocol v1", mode))
	}
	// A route serves one wire. A provider-owned wire beside any other transport
	// would ask a generated client to pick between two handler shapes for one
	// operation, and a fallback between them would re-open a conversation whose
	// frames mean something else.
	if providerWires > 0 && len(transports) != 1 {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidTransport, "transports",
			"a provider-owned websocket wire must be the operation's only transport"))
	}
	return diags
}

// validateWebSocketTransport checks one websocket transport against the wire
// it declares. The first-party conversation keeps its fixed subprotocol and its
// json or proto payloads; a provider-owned wire carries JSON values of the
// declared message types under the subprotocol its provider names, or raw
// octets, and never borrows the conversation's namespace or its resume.
func validateWebSocketTransport(mode StreamMode, field string, transport Transport) []diag.Diagnostic {
	var diags []diag.Diagnostic
	if transport.WebSocket == nil {
		if transport.Encoding != EncodingJSON && transport.Encoding != EncodingProto {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidTransport, field+".encoding",
				"websocket encoding must be json or proto"))
		}
		return append(diags, required(field+".websocket"))
	}
	websocket := transport.WebSocket
	switch websocket.Wire {
	case "":
		switch transport.Encoding {
		case EncodingJSON, EncodingProto:
		case EncodingBinary:
			diags = append(diags, diag.Errorf(ErrorCodeInvalidTransport, field+".encoding",
				"binary payloads travel only on a provider-owned websocket wire"))
		default:
			diags = append(diags, diag.Errorf(ErrorCodeInvalidTransport, field+".encoding",
				"websocket encoding must be json or proto"))
		}
		if websocket.Subprotocol != WebSocketSubprotocolV1 {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidTransport, field+".websocket.subprotocol",
				"subprotocol %q is unsupported; expected %q", websocket.Subprotocol, WebSocketSubprotocolV1))
		}
	case WebSocketWireProvider:
		if transport.Encoding != EncodingJSON && transport.Encoding != EncodingBinary {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidTransport, field+".encoding",
				"a provider-owned websocket wire carries json or binary messages"))
		}
		if mode != StreamBidirectional {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidTransport, field+".websocket.wire",
				"a provider-owned websocket wire carries a bidirectional stream, not a %s one", mode))
		}
		switch {
		case websocket.Subprotocol == "" && transport.Encoding == EncodingJSON:
			// JSON frames mean something only under the vocabulary that names
			// them, so a typed provider wire must say which one it speaks.
			diags = append(diags, required(field+".websocket.subprotocol"))
		case websocket.Subprotocol == "":
		case !ValidWebSocketSubprotocol(websocket.Subprotocol):
			diags = append(diags, diag.Errorf(ErrorCodeInvalidTransport, field+".websocket.subprotocol",
				"subprotocol %q is not a negotiable websocket token", websocket.Subprotocol))
		case strings.HasPrefix(websocket.Subprotocol, WebSocketReservedSubprotocolPrefix):
			diags = append(diags, diag.Errorf(ErrorCodeInvalidTransport, field+".websocket.subprotocol",
				"subprotocol %q is in the first-party namespace %q, which a provider-owned wire cannot declare",
				websocket.Subprotocol, WebSocketReservedSubprotocolPrefix))
		}
		if websocket.Resume {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidResilience, field+".websocket.resume",
				"resume is the first-party conversation's continuation; a provider-owned wire resumes by its own protocol"))
		}
	default:
		diags = append(diags, diag.Errorf(ErrorCodeInvalidEnum, field+".websocket.wire",
			"websocket wire %q is unsupported", websocket.Wire))
	}
	return diags
}

// validateSSETransport checks the shape of one SSE continuation declaration.
// Where it may appear (a safe server stream) is checked with the operation,
// and what its cursor names is checked against the document by
// ValidateSSEContinuationReferences.
func validateSSETransport(field string, sse *SSETransport) []diag.Diagnostic {
	continuation := sse.Continuation
	field += ".continuation"
	if continuation == nil {
		return []diag.Diagnostic{required(field)}
	}
	if blank(string(continuation.Mode)) {
		return []diag.Diagnostic{required(field + ".mode")}
	}
	var diags []diag.Diagnostic
	switch continuation.Mode {
	case SSEContinuationCursor:
		if continuation.Cursor == nil {
			diags = append(diags, required(field+".cursor"))
			break
		}
		if blank(continuation.Cursor.OutputField) {
			diags = append(diags, required(field+".cursor.outputField"))
		}
		if blank(continuation.Cursor.QueryParameter) {
			diags = append(diags, required(field+".cursor.queryParameter"))
		}
	case SSEContinuationBestEffort:
		if continuation.Cursor != nil {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidResilience, field+".cursor",
				"best-effort continuation reopens the original selector and never carries a cursor"))
		}
	default:
		diags = append(diags, diag.Errorf(ErrorCodeInvalidEnum, field+".mode",
			"sse continuation mode %q is unsupported", continuation.Mode))
	}
	return diags
}

// validateStreamContinuation checks both halves of the continuation agreement.
// The consumer half is the effective resilience.stream.reconnect; the provider
// half is websocket.resume on a first-party transport or a continuation on an
// SSE transport. A provider-owned wire resumes by its own protocol, so it never
// counts. Each mistake yields one diagnostic: a reconnect nothing can honor is
// named once for the operation, and a resume or a continuation on the wrong
// operation once per transport.
func validateStreamContinuation(operation *OperationV1, document *DocumentV1) []diag.Diagnostic {
	var diags []diag.Diagnostic
	continuable := false
	for _, transport := range operation.Transports {
		if transport.Protocol == TransportWebSocket && transport.WebSocket != nil && !transport.ProviderWire() &&
			transport.WebSocket.Resume {
			continuable = true
		}
		if transport.Continuation() != nil {
			continuable = true
		}
	}
	if streamReconnect(operation, document) && !continuable {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidResilience, "resilience.stream.reconnect",
			"stream reconnect requires a websocket transport with resume support or an sse transport that declares a continuation"))
	}
	safeServerStream := operation.Stream == StreamServer && operation.Idempotency.Kind == IdempotencySafe
	for i, transport := range operation.Transports {
		if transport.Protocol == TransportWebSocket && transport.WebSocket != nil && transport.WebSocket.Resume && !safeServerStream {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidResilience, fmt.Sprintf("transports[%d].websocket.resume", i),
				"websocket resume is valid only for safe server streams in protocol v1"))
		}
		if transport.Continuation() != nil && !safeServerStream {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidResilience, fmt.Sprintf("transports[%d].sse.continuation", i),
				"sse continuation is valid only for safe server streams in protocol v1"))
		}
	}
	return diags
}

// ValidateSSEContinuationReferences checks what a cursor continuation names
// against what the operation declares, once a reader has resolved the
// operation's query parameters (name to schema) and the document's component
// schemas. The output field is a required property of the output message and
// the query parameter a declared query parameter, and both are a plain string:
// no format, no enum, no null, because the runtime copies a position verbatim
// from one to the other and never interprets it. One local component
// reference is followed for the message and for each schema.
func ValidateSSEContinuationReferences(operation *OperationV1, query map[string]Schema, components map[string]Schema) []diag.Diagnostic {
	if operation == nil {
		return nil
	}
	var output *Schema
	if operation.Messages != nil {
		output = resolveCacheSchema(operation.Messages.Output, components)
	}
	var diags []diag.Diagnostic
	for i, transport := range operation.Transports {
		continuation := transport.Continuation()
		if continuation == nil || continuation.Mode != SSEContinuationCursor || continuation.Cursor == nil {
			continue
		}
		field := fmt.Sprintf("transports[%d].sse.continuation.cursor", i)
		name := continuation.Cursor.OutputField
		var property *Schema
		if output != nil {
			if declared, ok := output.Properties[name]; ok {
				property = &declared
			}
		}
		switch {
		case output == nil || output.Type != "object" || property == nil:
			diags = append(diags, diag.Errorf(ErrorCodeInvalidResilience, field+".outputField",
				"output field %q names no property of the declared output message", name))
		case !contains(output.Required, name):
			diags = append(diags, diag.Errorf(ErrorCodeInvalidResilience, field+".outputField",
				"output field %q must be required: every message carries the position after it", name))
		case !sseCursorText(resolveCacheSchema(property, components)):
			diags = append(diags, diag.Errorf(ErrorCodeInvalidResilience, field+".outputField",
				"output field %q must be a plain string: a position is opaque text", name))
		}
		parameter, declared := query[continuation.Cursor.QueryParameter]
		switch {
		case !declared:
			diags = append(diags, diag.Errorf(ErrorCodeInvalidResilience, field+".queryParameter",
				"query parameter %q is not declared by this operation", continuation.Cursor.QueryParameter))
		case !sseCursorText(resolveCacheSchema(&parameter, components)):
			diags = append(diags, diag.Errorf(ErrorCodeInvalidResilience, field+".queryParameter",
				"query parameter %q must be a plain string: a position is opaque text", continuation.Cursor.QueryParameter))
		}
	}
	return diags
}

// sseCursorText reports a schema whose values a runtime can carry verbatim as
// a position: a string with no format, no enum, no union and no null.
func sseCursorText(schema *Schema) bool {
	return schema != nil && schema.Ref == "" && schema.Type == "string" && schema.Format == "" &&
		(schema.Nullable == nil || !*schema.Nullable) && len(schema.Enum) == 0 && len(schema.OneOf) == 0 &&
		schema.OpaqueJSON == ""
}

// streamReconnect returns the operation's effective
// resilience.stream.reconnect. The operation's own value always counts. The
// document default reaches only server streams, the one mode a reconnect can
// continue, so a document that turns reconnect on for its streams leaves its
// unary operations valid.
func streamReconnect(operation *OperationV1, document *DocumentV1) bool {
	if operation.Resilience != nil && operation.Resilience.Stream != nil && operation.Resilience.Stream.Reconnect != nil {
		return *operation.Resilience.Stream.Reconnect
	}
	if operation.Stream != StreamServer || document == nil || document.Defaults == nil ||
		document.Defaults.Resilience == nil || document.Defaults.Resilience.Stream == nil ||
		document.Defaults.Resilience.Stream.Reconnect == nil {
		return false
	}
	return *document.Defaults.Resilience.Stream.Reconnect
}

func validateSecurity(security Security, document *DocumentV1) []diag.Diagnostic {
	if len(security.Alternatives) == 0 {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidSecurity, "security.alternatives",
			"at least one security alternative is required; use an empty allOf for anonymous")}
	}
	var diags []diag.Diagnostic
	for i, alternative := range security.Alternatives {
		if alternative.AllOf == nil {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidSecurity,
				fmt.Sprintf("security.alternatives[%d].allOf", i),
				"allOf is required; use an explicit empty array for anonymous"))
			continue
		}
		seen := map[string]bool{}
		headers := map[string]string{}
		for j, requirement := range alternative.AllOf {
			field := fmt.Sprintf("security.alternatives[%d].allOf[%d]", i, j)
			if blank(requirement.Profile) {
				diags = append(diags, required(field+".profile"))
			} else if document == nil {
				diags = append(diags, diag.Errorf(ErrorCodeUnknownProfile, field+".profile",
					"credential profile %q cannot be resolved without document metadata", requirement.Profile))
			} else if _, ok := document.Credentials[requirement.Profile]; !ok {
				diags = append(diags, diag.Errorf(ErrorCodeUnknownProfile, field+".profile",
					"credential profile %q is not declared by the provider", requirement.Profile))
			} else {
				header := credentialHeader(document.Credentials[requirement.Profile])
				folded := strings.ToLower(header)
				if prior, exists := headers[folded]; header != "" && exists {
					diags = append(diags, diag.Errorf(ErrorCodeInvalidSecurity, field+".profile",
						"credential profiles %q and %q both target header %q in one allOf", prior, requirement.Profile, header))
				}
				headers[folded] = requirement.Profile
			}
			if seen[requirement.Profile] {
				diags = append(diags, diag.Errorf(ErrorCodeDuplicate, field+".profile",
					"credential profile %q appears twice in one allOf", requirement.Profile))
			}
			seen[requirement.Profile] = true
			diags = append(diags, validateStrings(field+".scopes", requirement.Scopes)...)
			diags = append(diags, validateStrings(field+".roles", requirement.Roles)...)
		}
	}
	if security.Authorization != nil {
		auth := security.Authorization
		fields := []struct {
			name   string
			values []string
		}{
			{"issuers", auth.Issuers}, {"audiences", auth.Audiences},
			{"principalKinds", auth.PrincipalKinds}, {"clients", auth.Clients},
			{"scopesAll", auth.ScopesAll}, {"scopesAny", auth.ScopesAny},
			{"rolesAll", auth.RolesAll}, {"rolesAny", auth.RolesAny},
			{"scopeClaims", auth.ScopeClaims}, {"roleClaims", auth.RoleClaims},
		}
		for _, entry := range fields {
			diags = append(diags, validateStrings("security.authorization."+entry.name, entry.values)...)
		}
	}
	return diags
}

func validateErrors(errors []DeclaredError) []diag.Diagnostic {
	var diags []diag.Diagnostic
	if errors == nil {
		diags = append(diags, required("errors"))
	}
	seen := map[string]bool{}
	for i, declared := range errors {
		field := fmt.Sprintf("errors[%d]", i)
		if declared.Status < 400 || declared.Status > 599 {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidError, field+".status",
				"error HTTP status must be between 400 and 599"))
		}
		if blank(declared.Code) {
			diags = append(diags, required(field+".code"))
		}
		grpc := 0
		if declared.GRPCCode != nil {
			grpc = *declared.GRPCCode
			if grpc < 1 || grpc > 16 {
				diags = append(diags, diag.Errorf(ErrorCodeInvalidError, field+".grpcCode",
					"error gRPC code must be between 1 and 16"))
			}
		}
		key := fmt.Sprintf("%d\x00%s\x00%d", declared.Status, declared.Code, grpc)
		if seen[key] {
			diags = append(diags, diag.Errorf(ErrorCodeDuplicate, field,
				"declared error repeats an earlier status, code, and grpcCode"))
		}
		seen[key] = true
		if declared.Schema != nil {
			diags = append(diags, validateSchema(field+".schema", declared.Schema)...)
		}
	}
	return diags
}

func validateIdempotency(idempotency Idempotency) []diag.Diagnostic {
	switch idempotency.Kind {
	case IdempotencySafe, IdempotencyNonIdempotent:
		if idempotency.KeyHeader != "" {
			return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidIdempotency, "idempotency.keyHeader",
				"keyHeader is valid only when kind is idempotent")}
		}
	case IdempotencyIdempotent:
		if idempotency.KeyHeader != "" &&
			(!validHeaderName(idempotency.KeyHeader) || reservedCredentialHeader(idempotency.KeyHeader)) {
			return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidIdempotency, "idempotency.keyHeader",
				"keyHeader %q is not an allowed request header name", idempotency.KeyHeader)}
		}
	default:
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidEnum, "idempotency.kind",
			"idempotency kind %q is unsupported", idempotency.Kind)}
	}
	return nil
}

func validateResilience(field string, policy *ResiliencePolicy) []diag.Diagnostic {
	var diags []diag.Diagnostic
	positiveInt := func(path string, value *int) {
		if value != nil && *value <= 0 {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidResilience, path, "value must be positive"))
		}
	}
	positiveInt(field+".timeoutMs", policy.TimeoutMs)
	positiveInt(field+".attemptTimeoutMs", policy.AttemptTimeoutMs)
	if policy.MaxResponseBytes != nil && *policy.MaxResponseBytes <= 0 {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidResilience, field+".maxResponseBytes", "value must be positive"))
	}
	if policy.TimeoutMs != nil && policy.AttemptTimeoutMs != nil && *policy.AttemptTimeoutMs > *policy.TimeoutMs {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidResilience, field+".attemptTimeoutMs",
			"attemptTimeoutMs must not exceed timeoutMs"))
	}
	if policy.Retry != nil {
		positiveInt(field+".retry.maxAttempts", policy.Retry.MaxAttempts)
		diags = append(diags, validateIntSet(field+".retry.statuses", policy.Retry.Statuses, 400, 599)...)
		diags = append(diags, validateStrings(field+".retry.codes", policy.Retry.Codes)...)
	}
	if policy.Circuit != nil {
		positiveInt(field+".circuit.failureThreshold", policy.Circuit.FailureThreshold)
		positiveInt(field+".circuit.resetTimeoutMs", policy.Circuit.ResetTimeoutMs)
	}
	if policy.Stream != nil {
		positiveInt(field+".stream.handshakeTimeoutMs", policy.Stream.HandshakeTimeoutMs)
		positiveInt(field+".stream.idleTimeoutMs", policy.Stream.IdleTimeoutMs)
		positiveInt(field+".stream.heartbeatMs", policy.Stream.HeartbeatMs)
		positiveInt(field+".stream.maxBufferedMessages", policy.Stream.MaxBufferedMessages)
		if policy.Stream.MaxFrameBytes != nil && *policy.Stream.MaxFrameBytes <= 0 {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidResilience, field+".stream.maxFrameBytes", "value must be positive"))
		}
	}
	if policy.Cache != nil {
		diags = append(diags, validateCachePolicy(field+".cache", policy.Cache)...)
	}
	return diags
}

func validateSchema(field string, schema *Schema) []diag.Diagnostic {
	if schema == nil {
		return nil
	}
	var diags []diag.Diagnostic
	validTypes := map[string]bool{"string": true, "number": true, "integer": true, "boolean": true, "object": true, "array": true}
	validFormats := map[string]bool{
		"int32": true, "int64": true, "uint32": true, "uint64": true,
		"float": true, "double": true,
		"byte": true, "binary": true, "date": true, "date-time": true,
		"uuid": true, "email": true, "uri": true,
	}
	if schema.Type != "" && !validTypes[schema.Type] {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidSchema, field+".type", "schema type %q is unsupported", schema.Type))
	}
	if schema.Format != "" && !validFormats[schema.Format] {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidSchema, field+".format", "schema format %q is unsupported", schema.Format))
	}
	if schema.IsOpaqueJSON() {
		return validateOpaqueJSONSchema(field, schema)
	}
	if schema.Ref != "" {
		if !strings.HasPrefix(schema.Ref, "#/components/schemas/") {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidSchema, field+".$ref",
				"only local component schema references are supported"))
		}
		if schemaHasRefShapeSiblings(schema) {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidSchema, field,
				"$ref permits only nullable, title, and description siblings"))
		}
		return diags
	}
	if schema.Type == "" && len(schema.OneOf) == 0 {
		// The empty schema stays refused: "any JSON value" is a declaration
		// (x-putnami-json), never the absence of one.
		diags = append(diags, diag.Errorf(ErrorCodeInvalidSchema, field, "schema must declare type, $ref, oneOf, or "+OpaqueJSONKey))
	}
	if schema.Type != "object" && (len(schema.Properties) > 0 || len(schema.Required) > 0 || schema.AdditionalProperties != nil) {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidSchema, field, "properties, required, and additionalProperties require object type"))
	}
	if schema.Type == "array" && schema.Items == nil {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidSchema, field+".items", "array schema requires items"))
	}
	if schema.Type != "array" && (schema.Items != nil || schema.MinItems != nil || schema.MaxItems != nil || schema.UniqueItems != nil) {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidSchema, field, "items and array constraints require array type"))
	}
	if schema.Type != "string" && (schema.MinLength != nil || schema.MaxLength != nil || schema.Pattern != "") {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidSchema, field, "string constraints require string type"))
	}
	if schema.Type != "number" && schema.Type != "integer" && (schema.Minimum != nil || schema.Maximum != nil || schema.ExclusiveMinimum != nil || schema.ExclusiveMaximum != nil) {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidSchema, field, "numeric constraints require number or integer type"))
	}
	diags = append(diags, nonNegativeBounds(field, schema)...)
	required := map[string]bool{}
	for i, name := range schema.Required {
		if _, ok := schema.Properties[name]; !ok {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidSchema, fmt.Sprintf("%s.required[%d]", field, i),
				"required property %q is not declared in properties", name))
		}
		if required[name] {
			diags = append(diags, diag.Errorf(ErrorCodeDuplicate, fmt.Sprintf("%s.required[%d]", field, i),
				"required property %q appears more than once", name))
		}
		required[name] = true
	}
	propertyNames := make([]string, 0, len(schema.Properties))
	for name := range schema.Properties {
		propertyNames = append(propertyNames, name)
	}
	sort.Strings(propertyNames)
	for _, name := range propertyNames {
		child := schema.Properties[name]
		diags = append(diags, validateSchema(fmt.Sprintf("%s.properties[%q]", field, name), &child)...)
	}
	if schema.Items != nil {
		diags = append(diags, validateSchema(field+".items", schema.Items)...)
	}
	if schema.AdditionalProperties != nil {
		if schema.AdditionalProperties.Schema != nil && schema.AdditionalProperties.Schema.IsOpaqueJSON() {
			// One declaration, one spelling: a free-form object is
			// `additionalProperties: true`, which every OpenAPI reader already
			// understands. A second spelling would let two providers publish
			// the same shape as two different contracts.
			diags = append(diags, diag.Errorf(ErrorCodeInvalidSchema, field+".additionalProperties",
				"declare a free-form object as additionalProperties: true, not as an %s value", OpaqueJSONKey))
		} else if schema.AdditionalProperties.Schema != nil {
			diags = append(diags, validateSchema(field+".additionalProperties", schema.AdditionalProperties.Schema)...)
		} else if schema.AdditionalProperties.Allowed == nil {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidSchema, field+".additionalProperties",
				"additionalProperties must be a boolean or schema"))
		}
	}
	if len(schema.OneOf) > 0 && len(schema.OneOf) < 2 {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidSchema, field+".oneOf", "oneOf requires at least two variants"))
	}
	for i := range schema.OneOf {
		diags = append(diags, validateSchema(fmt.Sprintf("%s.oneOf[%d]", field, i), &schema.OneOf[i])...)
	}
	if schema.Discriminator != nil {
		if len(schema.OneOf) < 2 || blank(schema.Discriminator.PropertyName) {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidSchema, field+".discriminator",
				"discriminator requires a propertyName and at least two oneOf variants"))
		}
		for i, variant := range schema.OneOf {
			if variant.Ref != "" || variant.Type != "object" || len(variant.Properties) == 0 {
				continue
			}
			name := schema.Discriminator.PropertyName
			if _, ok := variant.Properties[name]; !ok || !contains(variant.Required, name) {
				diags = append(diags, diag.Errorf(ErrorCodeInvalidSchema, fmt.Sprintf("%s.oneOf[%d]", field, i),
					"direct object discriminator property %q must be present and required", name))
			}
		}
	}
	return diags
}

// validateOpaqueJSONSchema checks the one closed form of an opaque JSON value:
// the keyword with its only value, and nothing that would constrain or shape
// the value. Null is already one of the values it admits, so a nullable flag
// would be a second spelling of the same declaration; a type, a format or a
// bound would contradict "any JSON value". Title and description document it
// and change nothing a client decodes.
func validateOpaqueJSONSchema(field string, schema *Schema) []diag.Diagnostic {
	var diags []diag.Diagnostic
	if schema.OpaqueJSON != OpaqueJSONAny {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidEnum, field+"."+OpaqueJSONKey,
			"%s must be %q, got %q", OpaqueJSONKey, OpaqueJSONAny, schema.OpaqueJSON))
	}
	if schema.Ref != "" || schema.Nullable != nil || schemaHasRefShapeSiblings(schema) {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidSchema, field,
			"%s permits only title and description siblings", OpaqueJSONKey))
	}
	return diags
}

func schemaHasRefShapeSiblings(schema *Schema) bool {
	return schema.Type != "" || schema.Format != "" || len(schema.Properties) > 0 || len(schema.Required) > 0 ||
		schema.Items != nil || schema.AdditionalProperties != nil || len(schema.Enum) > 0 || len(schema.OneOf) > 0 ||
		schema.Discriminator != nil || schema.Default != nil || schema.Minimum != nil || schema.Maximum != nil ||
		schema.ExclusiveMinimum != nil || schema.ExclusiveMaximum != nil || schema.MinLength != nil ||
		schema.MaxLength != nil || schema.Pattern != "" || schema.MinItems != nil || schema.MaxItems != nil ||
		schema.UniqueItems != nil || schema.ReadOnly != nil || schema.WriteOnly != nil
}

func nonNegativeBounds(field string, schema *Schema) []diag.Diagnostic {
	var diags []diag.Diagnostic
	check := func(path string, value *int) {
		if value != nil && *value < 0 {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidSchema, path, "constraint must be non-negative"))
		}
	}
	check(field+".minLength", schema.MinLength)
	check(field+".maxLength", schema.MaxLength)
	check(field+".minItems", schema.MinItems)
	check(field+".maxItems", schema.MaxItems)
	if schema.MinLength != nil && schema.MaxLength != nil && *schema.MinLength > *schema.MaxLength {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidSchema, field, "minLength must not exceed maxLength"))
	}
	if schema.MinItems != nil && schema.MaxItems != nil && *schema.MinItems > *schema.MaxItems {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidSchema, field, "minItems must not exceed maxItems"))
	}
	if schema.Minimum != nil && schema.Maximum != nil {
		minimum, minOK := new(big.Rat).SetString(schema.Minimum.String())
		maximum, maxOK := new(big.Rat).SetString(schema.Maximum.String())
		if !minOK || !maxOK {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidSchema, field, "minimum and maximum must be JSON numbers"))
		} else if minimum.Cmp(maximum) > 0 {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidSchema, field, "minimum must not exceed maximum"))
		}
	}
	return diags
}

func validateStrings(field string, values []string) []diag.Diagnostic {
	var diags []diag.Diagnostic
	seen := map[string]bool{}
	for i, value := range values {
		entry := fmt.Sprintf("%s[%d]", field, i)
		if blank(value) {
			diags = append(diags, required(entry))
		}
		if seen[value] {
			diags = append(diags, diag.Errorf(ErrorCodeDuplicate, entry, "value %q appears more than once", value))
		}
		seen[value] = true
	}
	return diags
}

func validateIntSet(field string, values []int, min, max int) []diag.Diagnostic {
	var diags []diag.Diagnostic
	seen := map[int]bool{}
	for i, value := range values {
		entry := fmt.Sprintf("%s[%d]", field, i)
		if value < min || value > max {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidResilience, entry,
				"value must be between %d and %d", min, max))
		}
		if seen[value] {
			diags = append(diags, diag.Errorf(ErrorCodeDuplicate, entry, "value %d appears more than once", value))
		}
		seen[value] = true
	}
	return diags
}

func validateTransportPath(value string) error {
	if blank(value) || !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") {
		return fmt.Errorf("transport path must be an absolute same-authority path beginning with one '/'")
	}
	if strings.ContainsAny(value, "?#\\") || strings.Contains(value, "//") {
		return fmt.Errorf("transport path must not contain query, fragment, backslash, or repeated separators")
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("transport path must not contain control characters")
		}
	}
	decoded, err := url.PathUnescape(value)
	if err != nil {
		return fmt.Errorf("transport path has invalid percent encoding")
	}
	if strings.Contains(decoded, "//") || strings.Contains(decoded, "\\") {
		return fmt.Errorf("transport path must not encode separators or backslashes")
	}
	for _, segment := range strings.Split(decoded, "/") {
		if segment == "." || segment == ".." {
			return fmt.Errorf("transport path must not contain dot traversal segments")
		}
	}
	return nil
}

func validHeaderName(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			continue
		}
		switch r {
		case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
			continue
		default:
			return false
		}
	}
	return true
}

func reservedCredentialHeader(value string) bool {
	switch strings.ToLower(value) {
	case "authorization", "host", "content-length", "connection", "transfer-encoding",
		"upgrade", "cookie", "set-cookie", "traceparent", "tracestate", "baggage",
		"grpc-timeout", "connect-timeout-ms", "x-client-id", "x-request-id",
		"x-putnami-client-id", "x-putnami-service", "x-putnami-stream-wire":
		return true
	default:
		return false
	}
}

func credentialHeader(profile CredentialProfile) string {
	switch profile.Kind {
	case CredentialServiceToken, CredentialForwardedUserToken:
		return "Authorization"
	case CredentialAPIKey, CredentialNamedHeader:
		return profile.Header
	default:
		return ""
	}
}

func validateProtobuf(field string, descriptor *ProtobufDescriptor) []diag.Diagnostic {
	var diags []diag.Diagnostic
	if descriptor.Syntax != "proto3" {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidProtobuf, field+".syntax",
			"protobuf syntax must be proto3"))
	}
	if blank(descriptor.Package) {
		diags = append(diags, required(field+".package"))
	}
	if descriptor.Services == nil {
		diags = append(diags, required(field+".services"))
	}
	if descriptor.Messages == nil {
		diags = append(diags, required(field+".messages"))
	}
	if descriptor.Enums == nil {
		diags = append(diags, required(field+".enums"))
	}
	knownMessages := map[string]bool{}
	knownEnums := map[string]bool{}
	for i, message := range descriptor.Messages {
		entry := fmt.Sprintf("%s.messages[%d].name", field, i)
		if blank(message.Name) {
			diags = append(diags, required(entry))
		} else if knownMessages[message.Name] {
			diags = append(diags, diag.Errorf(ErrorCodeDuplicate, entry,
				"protobuf message %q is declared more than once", message.Name))
		}
		knownMessages[message.Name] = true
	}
	for i, enum := range descriptor.Enums {
		entry := fmt.Sprintf("%s.enums[%d].name", field, i)
		if blank(enum.Name) {
			diags = append(diags, required(entry))
		} else if knownEnums[enum.Name] {
			diags = append(diags, diag.Errorf(ErrorCodeDuplicate, entry,
				"protobuf enum %q is declared more than once", enum.Name))
		}
		knownEnums[enum.Name] = true
	}

	serviceNames := map[string]bool{}
	rpcs := map[string]bool{}
	for i, service := range descriptor.Services {
		entry := fmt.Sprintf("%s.services[%d]", field, i)
		if blank(service.Name) {
			diags = append(diags, required(entry+".name"))
		} else if serviceNames[service.Name] {
			diags = append(diags, diag.Errorf(ErrorCodeDuplicate, entry+".name",
				"protobuf service %q is declared more than once", service.Name))
		}
		serviceNames[service.Name] = true
		if service.Methods == nil {
			diags = append(diags, required(entry+".methods"))
		}
		methodNames := map[string]bool{}
		for j, method := range service.Methods {
			mf := fmt.Sprintf("%s.methods[%d]", entry, j)
			for _, member := range []struct{ name, value string }{
				{"name", method.Name}, {"input", method.Input}, {"output", method.Output},
			} {
				if blank(member.value) {
					diags = append(diags, required(mf+"."+member.name))
				}
			}
			if methodNames[method.Name] {
				diags = append(diags, diag.Errorf(ErrorCodeDuplicate, mf+".name",
					"protobuf method %q is declared more than once in service %q", method.Name, service.Name))
			}
			methodNames[method.Name] = true
			fullMethod := "/" + descriptor.Package + "." + service.Name + "/" + method.Name
			if rpcs[fullMethod] {
				diags = append(diags, diag.Errorf(ErrorCodeDuplicate, mf,
					"protobuf method identity %q is declared more than once", fullMethod))
			}
			rpcs[fullMethod] = true
			if !isKnownProtobufMessage(method.Input, knownMessages) {
				diags = append(diags, diag.Errorf(ErrorCodeInvalidProtobuf, mf+".input",
					"input message %q is not declared", method.Input))
			}
			if !isKnownProtobufMessage(method.Output, knownMessages) {
				diags = append(diags, diag.Errorf(ErrorCodeInvalidProtobuf, mf+".output",
					"output message %q is not declared", method.Output))
			}
		}
	}

	for messageIndex, message := range descriptor.Messages {
		entry := fmt.Sprintf("%s.messages[%d]", field, messageIndex)
		if message.Fields == nil {
			diags = append(diags, required(entry+".fields"))
		}
		diags = append(diags, validateStrings(entry+".oneofs", message.OneOfs)...)
		oneofs := map[string]bool{}
		for _, oneof := range message.OneOfs {
			oneofs[oneof] = true
		}
		numbers := map[int]bool{}
		names := map[string]bool{}
		jsonNames := map[string]bool{}
		lastNumber := 0
		for i, protoField := range message.Fields {
			ff := fmt.Sprintf("%s.fields[%d]", entry, i)
			if blank(protoField.Name) {
				diags = append(diags, required(ff+".name"))
			}
			if blank(protoField.JSONName) {
				diags = append(diags, required(ff+".jsonName"))
			}
			if protoField.Number <= 0 || protoField.Number > 536870911 ||
				protoField.Number >= 19000 && protoField.Number <= 19999 {
				diags = append(diags, diag.Errorf(ErrorCodeInvalidProtobuf, ff+".number",
					"protobuf field number is outside the usable range"))
			}
			if i > 0 && protoField.Number <= lastNumber {
				diags = append(diags, diag.Errorf(ErrorCodeInvalidProtobuf, ff+".number",
					"protobuf fields must be in ascending field-number order"))
			}
			lastNumber = protoField.Number
			if numbers[protoField.Number] || names[protoField.Name] || jsonNames[protoField.JSONName] {
				diags = append(diags, diag.Errorf(ErrorCodeDuplicate, ff,
					"protobuf field number, name, and jsonName must be unique within a message"))
			}
			numbers[protoField.Number] = true
			names[protoField.Name] = true
			jsonNames[protoField.JSONName] = true
			diags = append(diags, validateProtobufField(ff, protoField, knownMessages, knownEnums, oneofs)...)
		}
	}

	for enumIndex, enum := range descriptor.Enums {
		entry := fmt.Sprintf("%s.enums[%d]", field, enumIndex)
		if len(enum.Values) == 0 {
			diags = append(diags, required(entry+".values"))
			continue
		}
		names := map[string]bool{}
		numbers := map[int]bool{}
		for i, value := range enum.Values {
			vf := fmt.Sprintf("%s.values[%d]", entry, i)
			if blank(value.Name) {
				diags = append(diags, required(vf+".name"))
			}
			if value.Number < -2147483648 || value.Number > 2147483647 {
				diags = append(diags, diag.Errorf(ErrorCodeInvalidProtobuf, vf+".number",
					"protobuf enum number must fit signed int32"))
			}
			if names[value.Name] || numbers[value.Number] {
				diags = append(diags, diag.Errorf(ErrorCodeDuplicate, vf,
					"protobuf enum names and numeric values must be unique"))
			}
			names[value.Name] = true
			numbers[value.Number] = true
		}
		if enum.Values[0].Number != 0 {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidProtobuf, entry+".values[0].number",
				"the first proto3 enum value must be zero"))
		}
	}
	return diags
}

func validateProtobufField(field string, protoField ProtobufField, messages, enums, oneofs map[string]bool) []diag.Diagnostic {
	var diags []diag.Diagnostic
	scalars := map[string]bool{
		"double": true, "float": true, "int64": true, "uint64": true,
		"int32": true, "fixed64": true, "fixed32": true, "bool": true,
		"string": true, "bytes": true, "uint32": true, "sfixed32": true,
		"sfixed64": true, "sint32": true, "sint64": true,
	}
	switch protoField.TypeKind {
	case "message":
		if !isKnownProtobufMessage(protoField.Type, messages) {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidProtobuf, field+".type",
				"message type %q is not declared", protoField.Type))
		}
	case "enum":
		if !enums[protoField.Type] {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidProtobuf, field+".type",
				"enum type %q is not declared", protoField.Type))
		}
	case "map":
		if protoField.Map == nil {
			diags = append(diags, required(field+".map"))
		}
		if protoField.Type != "map" {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidProtobuf, field+".type",
				"protobuf map fields must use canonical type %q", "map"))
		}
	case "scalar":
		if !scalars[protoField.Type] {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidProtobuf, field+".type",
				"protobuf scalar type %q is unsupported", protoField.Type))
		}
	default:
		diags = append(diags, diag.Errorf(ErrorCodeInvalidProtobuf, field+".typeKind",
			"protobuf typeKind %q is unsupported", protoField.TypeKind))
	}
	if protoField.TypeKind != "map" && protoField.Map != nil {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidProtobuf, field+".map",
			"map metadata is valid only when typeKind is map"))
	}
	if protoField.Map != nil {
		keys := map[string]bool{"int32": true, "int64": true, "uint32": true, "uint64": true, "sint32": true, "sint64": true, "fixed32": true, "fixed64": true, "sfixed32": true, "sfixed64": true, "bool": true, "string": true}
		if !keys[protoField.Map.KeyType] {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidProtobuf, field+".map.keyType",
				"protobuf map key type %q is unsupported", protoField.Map.KeyType))
		}
		switch protoField.Map.ValueKind {
		case "message":
			if !isKnownProtobufMessage(protoField.Map.ValueType, messages) {
				diags = append(diags, diag.Errorf(ErrorCodeInvalidProtobuf, field+".map.valueType",
					"map value message %q is not declared", protoField.Map.ValueType))
			}
		case "enum":
			if !enums[protoField.Map.ValueType] {
				diags = append(diags, diag.Errorf(ErrorCodeInvalidProtobuf, field+".map.valueType",
					"map value enum %q is not declared", protoField.Map.ValueType))
			}
		case "scalar":
			if !scalars[protoField.Map.ValueType] {
				diags = append(diags, diag.Errorf(ErrorCodeInvalidProtobuf, field+".map",
					"map scalar value type %q is unsupported", protoField.Map.ValueType))
			}
		default:
			diags = append(diags, diag.Errorf(ErrorCodeInvalidProtobuf, field+".map.valueKind",
				"map valueKind %q is unsupported", protoField.Map.ValueKind))
		}
	}
	if protoField.TypeKind == "map" && (protoField.Repeated || protoField.Optional || protoField.OneOf != "") {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidProtobuf, field,
			"map cannot be combined with repeated, optional, or oneof"))
	} else if protoField.Repeated && (protoField.Optional || protoField.OneOf != "") {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidProtobuf, field,
			"repeated cannot be combined with optional or oneof"))
	}
	if protoField.Optional && protoField.OneOf != "" {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidProtobuf, field,
			"optional cannot be combined with an explicit oneof"))
	}
	if protoField.OneOf != "" && !oneofs[protoField.OneOf] {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidProtobuf, field+".oneof",
			"oneof %q is not declared by the message", protoField.OneOf))
	}
	return diags
}

func isKnownProtobufMessage(name string, declared map[string]bool) bool {
	if declared[name] {
		return true
	}
	switch name {
	case "google.protobuf.Any", "google.protobuf.Duration", "google.protobuf.Empty", "google.protobuf.Timestamp":
		return true
	default:
		return false
	}
}

func protobufMethods(descriptor *ProtobufDescriptor) map[string]ProtobufMethod {
	methods := map[string]ProtobufMethod{}
	for _, service := range descriptor.Services {
		for _, method := range service.Methods {
			methods["/"+descriptor.Package+"."+service.Name+"/"+method.Name] = method
		}
	}
	return methods
}

func required(field string) diag.Diagnostic {
	return diag.Errorf(ErrorCodeRequired, field, "field is required and must be non-empty")
}

func blank(value string) bool { return strings.TrimSpace(value) == "" }

func validStream(mode StreamMode) bool {
	return mode == StreamUnary || mode == StreamServer || mode == StreamClient || mode == StreamBidirectional
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
