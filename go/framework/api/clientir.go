package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"

	"go.putnami.dev/errors"
	phttp "go.putnami.dev/http"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/diagnostic"
)

// clientIRVersion is the language-neutral generated-client IR version shared
// by the Go and TypeScript readers and emitters.
const clientIRVersion = 1

// ParameterIR preserves an OpenAPI parameter's wire location and full schema.
type ParameterIR struct {
	Name     string                `json:"name"`
	Location string                `json:"location"`
	Required bool                  `json:"required"`
	Schema   clientcontract.Schema `json:"schema"`
}

// ContentIR is one media representation of a request or response body.
type ContentIR struct {
	MediaType string                 `json:"mediaType"`
	Schema    *clientcontract.Schema `json:"schema,omitempty"`
	// MaxBytes is the declared byte bound of a raw octet representation, read
	// from the media type's `x-putnami-max-bytes`. Zero on every JSON
	// representation. Streamed binary representations keep the bound while
	// enforcing it incrementally instead of buffering.
	MaxBytes int64 `json:"maxBytes,omitempty"`
	Streamed bool  `json:"streamed,omitempty"`
}

// IsBinary reports a raw octet representation: the payload is the HTTP body
// itself, not a value inside a JSON document. It is the distinction the two
// readers have to make — `format: byte` is base64 text inside JSON, and
// `format: binary` at the root of a non-JSON media type is the body.
func (content ContentIR) IsBinary() bool {
	return content.Schema != nil && content.Schema.Type == "string" && content.Schema.Format == "binary"
}

// RequestIR preserves body requiredness and every declared media type.
type RequestIR struct {
	Required bool        `json:"required"`
	Content  []ContentIR `json:"content"`
}

// ResponseHeaderIR is one typed response header.
type ResponseHeaderIR struct {
	Name     string                `json:"name"`
	Required bool                  `json:"required"`
	Schema   clientcontract.Schema `json:"schema"`
}

// SuccessIR is one declared 2xx result, including bodyless variants.
type SuccessIR struct {
	Status      int                `json:"status"`
	Description string             `json:"description"`
	Content     []ContentIR        `json:"content"`
	Headers     []ResponseHeaderIR `json:"headers,omitempty"`
}

// Intermediate representation for client code generation.
//
// This mirrors the TypeScript IR (`typescript/framework/client/src/generator/
// ir.type.ts`) field-for-field so the two per-language emitters share one
// contract: the OpenAPI spec. The Go reader ([ReadOpenAPISpec]) and the TS
// reader run against the SAME `*.openapi.json` fixtures (the golden parity
// harness) so they can never drift on operation set, naming, optionality, or
// $ref/named-type structure.
//
// The one intentional divergence is the per-field type token: the TS IR stores a
// TypeScript type string (`tsType`), the Go IR stores a Go type token
// ([FieldIR.GoType]) — `string`, `int64`, `float64`, `bool`, `map[string]any`,
// or a shared model name. Structural parity (everything except the language
// primitive) is enforced by the cross-language test in clientir_test.go.

// SpecIR is the full output of a spec reader — every service plus metadata.
type SpecIR struct {
	// IRVersion pins the neutral IR shape consumed by both language emitters.
	IRVersion int `json:"irVersion"`
	// Contract is present for strict first-party provider specifications.
	Contract *clientcontract.DocumentV1 `json:"contract,omitempty"`
	// Schemas preserves complete neutral component schemas by OpenAPI name.
	Schemas map[string]clientcontract.Schema `json:"schemas,omitempty"`
	// Transport is the detected transport mode. Always "http" for the OpenAPI reader.
	Transport string `json:"transport"`
	// Services holds one entry per first-path-segment group.
	Services []ServiceIR `json:"services"`
	// NamedTypes are shared models lifted from components.schemas, keyed by name
	// (e.g. "Model1"). A model reused across operations is represented once here
	// and referenced by name from method body/response types and field types.
	NamedTypes map[string][]FieldIR `json:"namedTypes,omitempty"`
	// Enums preserves closed string-value components instead of degrading them
	// to bare strings in generated clients.
	Enums map[string][]string `json:"enums,omitempty"`
	// Unions preserves tagged OpenAPI oneOf components and their discriminator.
	Unions map[string]UnionIR `json:"unions,omitempty"`
	// SpecHash is a truncated SHA-256 of the source spec bytes, for drift
	// detection. Excluded from golden comparisons.
	SpecHash string `json:"specHash,omitempty"`
}

// UnionIR is a tagged union lifted from an OpenAPI component.
type UnionIR struct {
	Discriminator string           `json:"discriminator"`
	Variants      []UnionVariantIR `json:"variants"`
}

// UnionVariantIR is one discriminator value and its payload fields.
type UnionVariantIR struct {
	Tag    string    `json:"tag"`
	Fields []FieldIR `json:"fields,omitempty"`
}

// ServiceIR is a group of operations sharing a first path segment.
type ServiceIR struct {
	// Name is the service name (e.g. "UsersService").
	Name string `json:"name"`
	// ClassName is the generated client class name (e.g. "UsersClient").
	ClassName string `json:"className"`
	// Methods are the endpoint methods in this service.
	Methods []MethodIR `json:"methods"`
}

// MethodIR is a single endpoint operation.
type MethodIR struct {
	// Name is the method name in camelCase (e.g. "getUser"), derived from the
	// operationId. The Go emitter computes its own PascalCase Go method name from
	// the HTTP method + path; this field carries the spec's operationId-derived
	// name for parity with the TS IR.
	Name string `json:"name"`
	// OperationID is the operation id from the spec (synthesized when absent).
	OperationID string `json:"operationId"`
	// HTTPMethod is the upper-case HTTP method (GET, POST, …).
	HTTPMethod string `json:"httpMethod"`
	// Path is the URL path with {param} placeholders.
	Path string `json:"path"`
	// Parameters preserves path, query, and header parameters without reducing
	// their schemas to legacy language-specific field tokens.
	Parameters []ParameterIR `json:"parameters,omitempty"`
	// Request preserves root bodies and all media types.
	Request *RequestIR `json:"request,omitempty"`
	// Successes retains every declared 2xx variant, including void variants.
	Successes []SuccessIR `json:"successes,omitempty"`
	// Client carries the strict first-party operation contract.
	Client *clientcontract.OperationV1 `json:"client,omitempty"`
	// Params are path parameters.
	Params []FieldIR `json:"params,omitempty"`
	// Query are query parameters.
	Query []FieldIR `json:"query,omitempty"`
	// Body are inline request-body fields (anonymous body shape). Mutually
	// exclusive with BodyType.
	Body []FieldIR `json:"body,omitempty"`
	// BodyType is the named request-body type — set when the body is a $ref to a
	// shared model in [SpecIR.NamedTypes].
	BodyType string `json:"bodyType,omitempty"`
	// Response are inline response fields (anonymous response shape). Mutually
	// exclusive with ResponseType.
	Response []FieldIR `json:"response,omitempty"`
	// ResponseType is the named response type — set when the chosen 2xx response
	// is a $ref to a shared model in [SpecIR.NamedTypes].
	ResponseType string `json:"responseType,omitempty"`
	// Streaming is the streaming mode, if any. Always empty for v1 (REST).
	Streaming string `json:"streaming,omitempty"`
}

// MarshalJSON keeps strict first-party success inventory explicit even when an
// upgrade-only client or bidirectional stream has no HTTP 2xx response. The TS
// neutral IR represents that semantic as successes:[]; omitting it would make
// the same provider contract serialize differently across generators. Legacy
// third-party IR retains its historical omitempty shape.
func (method MethodIR) MarshalJSON() ([]byte, error) {
	type methodAlias MethodIR
	if method.Client == nil {
		return json.Marshal(methodAlias(method))
	}
	successes := method.Successes
	if successes == nil {
		successes = []SuccessIR{}
	}
	return json.Marshal(struct {
		methodAlias
		Successes []SuccessIR `json:"successes"`
	}{methodAlias: methodAlias(method), Successes: successes})
}

// FieldIR is a single field in a request/response/model type.
type FieldIR struct {
	// Name is the field name as it appears on the wire (the JSON key).
	Name string `json:"name"`
	// GoType is the Go type token: a primitive ("string", "int64", "float64",
	// "bool"), the name of a shared model in [SpecIR.NamedTypes] (when the field
	// is a $ref), or "map[string]any" for an inline nested object that was not
	// promoted to a named type (the degrade rule). For arrays this is the element
	// type and [FieldIR.Array] is true.
	GoType string `json:"goType"`
	// Optional reports whether the field is absent from the schema's `required`.
	Optional bool `json:"optional"`
	// Array reports whether the field is an array of GoType.
	Array bool `json:"array"`
}

// --- OpenAPI document parsing structs (the subset the reader consumes) ---

type oaDocument struct {
	Paths          map[string]oaPathItem `json:"paths"`
	Components     *oaComponents         `json:"components"`
	ClientContract json.RawMessage       `json:"x-putnami-client"`
}

type oaPathItem struct {
	Parameters []oaParameter
	Operations map[string]oaOperation
}

func (p *oaPathItem) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if paramsRaw, ok := raw["parameters"]; ok {
		if err := json.Unmarshal(paramsRaw, &p.Parameters); err != nil {
			return err
		}
	}
	p.Operations = make(map[string]oaOperation)
	for _, method := range httpMethodOrder {
		opRaw, ok := raw[method]
		if !ok {
			continue
		}
		var op oaOperation
		if err := json.Unmarshal(opRaw, &op); err != nil {
			return err
		}
		p.Operations[method] = op
	}
	return nil
}

type oaComponents struct {
	Schemas map[string]oaSchema `json:"schemas"`
}

type oaOperation struct {
	OperationID    string                      `json:"operationId"`
	Parameters     []oaParameter               `json:"parameters"`
	RequestBody    *oaRequestBody              `json:"requestBody"`
	Responses      map[string]oaResponse       `json:"responses"`
	ClientContract json.RawMessage             `json:"x-putnami-client"`
	ParsedClient   *clientcontract.OperationV1 `json:"-"`
	// ExternalContract names the external authority that owns this
	// operation's wire contract. A strict reader skips such an operation.
	ExternalContract json.RawMessage `json:"x-putnami-external-contract"`
}

type oaParameter struct {
	Name     string    `json:"name"`
	In       string    `json:"in"`
	Required bool      `json:"required"`
	Schema   *oaSchema `json:"schema"`
}

type oaRequestBody struct {
	Required bool                   `json:"required"`
	Content  map[string]oaMediaType `json:"content"`
}

type oaResponse struct {
	Description string                 `json:"description"`
	Content     map[string]oaMediaType `json:"content"`
	Headers     map[string]oaHeader    `json:"headers"`
}

type oaHeader struct {
	Required bool      `json:"required"`
	Schema   *oaSchema `json:"schema"`
}

type oaMediaType struct {
	Schema   *oaSchema `json:"schema"`
	MaxBytes *int64    `json:"x-putnami-max-bytes"`
	Streamed *bool     `json:"x-putnami-streamed"`
}

type oaSchema struct {
	Raw           json.RawMessage     `json:"-"`
	Ref           string              `json:"$ref"`
	Type          string              `json:"type"`
	Format        string              `json:"format"`
	Nullable      *bool               `json:"nullable"`
	Properties    map[string]oaSchema `json:"properties"`
	Required      []string            `json:"required"`
	Items         *oaSchema           `json:"items"`
	Enum          []json.RawMessage   `json:"enum"`
	OneOf         []oaSchema          `json:"oneOf"`
	Discriminator *oaDiscriminator    `json:"discriminator"`
}

func (s *oaSchema) UnmarshalJSON(data []byte) error {
	type schemaAlias oaSchema
	var decoded schemaAlias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*s = oaSchema(decoded)
	s.Raw = append(json.RawMessage(nil), data...)
	return nil
}

type oaDiscriminator struct {
	PropertyName string            `json:"propertyName"`
	Mapping      map[string]string `json:"mapping"`
}

// httpMethodOrder is the canonical, deterministic order the reader walks the
// methods of a path item. Go map iteration is randomized, so the reader must
// impose a stable order or the generated IR (and golden tests) would flap.
var httpMethodOrder = []string{"get", "post", "put", "patch", "delete", "head", "options", "trace"}

// ReadOpenAPISpec converts an OpenAPI 3.0.x document (the raw JSON the openapi
// plugin emits) into the [SpecIR]. It groups operations into services by their
// first path segment, converts each operation into a typed method (preferring
// the 200 → 201 → lowest-2xx JSON response), and lifts components.schemas into
// shared named types so a model reused across operations is represented once.
//
// The walk is fully deterministic: paths are visited in sorted order, methods in
// [httpMethodOrder], and object properties alphabetically — so the same spec
// always yields byte-identical IR.
func ReadOpenAPISpec(specJSON []byte) (SpecIR, error) {
	var doc oaDocument
	if err := json.Unmarshal(specJSON, &doc); err != nil {
		return SpecIR{}, errors.Newf(CodeClientGenConfig, "api: parse openapi spec: %v", err)
	}
	strict := len(doc.ClientContract) > 0
	var documentContract *clientcontract.DocumentV1
	if strict {
		if err := validateFirstPartyOpenAPISource(specJSON); err != nil {
			return SpecIR{}, err
		}
		var diags []diagnostic.Diagnostic
		documentContract, diags = clientcontract.ParseAndValidateDocument(doc.ClientContract)
		if len(diags) > 0 {
			return SpecIR{}, clientContractReadError("document x-putnami-client", diags)
		}
	}
	var schemas map[string]clientcontract.Schema
	if strict {
		var err error
		schemas, err = neutralComponentSchemas(doc.Components, true)
		if err != nil {
			return SpecIR{}, err
		}
	}

	type svcAccum struct {
		name    string
		methods []MethodIR
	}
	order := []string{}
	byName := map[string]*svcAccum{}
	seenOperationIDs := map[string]string{}
	seenMethodNames := map[string]string{}

	for _, path := range sortedKeys(doc.Paths) {
		pathItem := doc.Paths[path]
		for _, httpMethod := range orderedMethods(pathItem.Operations) {
			op := pathItem.Operations[httpMethod]
			if strict {
				// An operation an external authority owns is served and
				// documented, never generated: it contributes no method, and its
				// own schemas are the standard's, not the first-party subset.
				authority, externalDiags := clientcontract.ExternalContractAuthority(op.ClientContract, op.ExternalContract)
				if len(externalDiags) > 0 {
					return SpecIR{}, clientContractReadError(strings.ToUpper(httpMethod)+" "+path+" "+clientcontract.ExternalContractKey, externalDiags)
				}
				if authority != "" {
					continue
				}
				if len(op.ClientContract) == 0 {
					// Carry the shared corpus diagnostic code: the same missing
					// marker must be reportable identically by both readers.
					return SpecIR{}, errors.Newf(CodeClientGenConfig,
						"api: strict first-party operation %s %s is missing x-putnami-client (%s)",
						strings.ToUpper(httpMethod), path, clientcontract.ErrorCodeRequired)
				}
				var diags []diagnostic.Diagnostic
				op.ParsedClient, diags = clientcontract.ParseOperation(op.ClientContract)
				if op.ParsedClient != nil {
					operationID := op.OperationID
					if operationID == "" {
						operationID = buildOperationID(strings.ToUpper(httpMethod), path)
					}
					diags = append(diags, clientcontract.ValidateOperationForID(operationID, op.ParsedClient, documentContract)...)
				}
				if len(diags) > 0 {
					return SpecIR{}, clientContractReadError(strings.ToUpper(httpMethod)+" "+path+" x-putnami-client", diags)
				}
			}
			method, err := operationToMethod(path, strings.ToUpper(httpMethod), op, pathItem.Parameters, strict)
			if err != nil {
				return SpecIR{}, err
			}
			if strict {
				if diags := validateMethodCacheKeyFields(method, schemas); len(diags) > 0 {
					return SpecIR{}, clientContractReadError(strings.ToUpper(httpMethod)+" "+path+" x-putnami-client", diags)
				}
				if diags := validateMethodSSEContinuation(method, schemas); len(diags) > 0 {
					return SpecIR{}, clientContractReadError(strings.ToUpper(httpMethod)+" "+path+" x-putnami-client", diags)
				}
				location := strings.ToUpper(httpMethod) + " " + path
				if previous, exists := seenOperationIDs[method.OperationID]; exists {
					return SpecIR{}, errors.Newf(CodeClientGenConfig,
						"api: strict first-party operations %s and %s share operationId %q", previous, location, method.OperationID)
				}
				seenOperationIDs[method.OperationID] = location
				if previous, exists := seenMethodNames[method.Name]; exists {
					return SpecIR{}, errors.Newf(CodeClientGenConfig,
						"api: strict first-party operations %s and %s normalize to method name %q", previous, location, method.Name)
				}
				seenMethodNames[method.Name] = location
			}
			svc := inferServiceName(path)
			acc, ok := byName[svc]
			if !ok {
				acc = &svcAccum{name: svc}
				byName[svc] = acc
				order = append(order, svc)
			}
			acc.methods = append(acc.methods, method)
		}
	}

	services := make([]ServiceIR, 0, len(order))
	for _, name := range order {
		acc := byName[name]
		services = append(services, ServiceIR{
			Name:      name,
			ClassName: strings.TrimSuffix(name, "Service") + "Client",
			Methods:   acc.methods,
		})
	}

	var namedTypes map[string][]FieldIR
	var enums map[string][]string
	var unions map[string]UnionIR
	if !strict {
		namedTypes, enums, unions = buildComponentTypes(doc.Components)
	}
	spec := SpecIR{
		IRVersion:  clientIRVersion,
		Contract:   documentContract,
		Schemas:    schemas,
		Transport:  "http",
		Services:   services,
		NamedTypes: namedTypes,
		Enums:      enums,
		Unions:     unions,
		SpecHash:   specHash(specJSON),
	}
	if strict {
		if err := validateClientSchemaReferences(spec); err != nil {
			return SpecIR{}, err
		}
		if err := validateFirstPartyConnectContract(spec); err != nil {
			return SpecIR{}, err
		}
	}
	return spec, nil
}

// --- Connect contract: descriptor, published schema and IR must agree ---

// validateFirstPartyConnectContract joins the three published views of one
// provider and refuses any disagreement between them: the protobuf descriptor a
// Connect client decodes with, the neutral component schemas a REST client
// decodes with, and the per-operation transports that say which of the two a
// call uses. Anything a Connect client would have to guess — a field the schema
// does not declare, a presence rule that differs by transport, an integer whose
// width narrows on one wire — is a read failure here, before a client is
// emitted from it.
func validateFirstPartyConnectContract(spec SpecIR) error {
	if err := validateConnectTransportIdentity(spec); err != nil {
		return err
	}
	if spec.Contract == nil || spec.Contract.Protobuf == nil {
		return nil
	}
	if err := validateProtobufEnumParity(spec, spec.Contract.Protobuf); err != nil {
		return err
	}
	return validateProtobufMessageParity(spec, spec.Contract.Protobuf)
}

// validateConnectTransportIdentity refuses a Connect transport whose request
// path is not the protobuf method identity. A Connect URL IS the method, so a
// path that says otherwise names an endpoint the descriptor never describes.
func validateConnectTransportIdentity(spec SpecIR) error {
	for _, service := range spec.Services {
		for _, method := range service.Methods {
			if method.Client == nil {
				continue
			}
			for i, transport := range method.Client.Transports {
				if transport.Protocol != clientcontract.TransportConnect {
					continue
				}
				if transport.Path == transport.ProtobufMethod {
					continue
				}
				return errors.Newf(CodeClientGenConfig,
					"api: strict first-party operation %s transport %d declares connect path %q for protobuf method %q; a Connect path is the method identity (%s)",
					method.OperationID, i, transport.Path, transport.ProtobufMethod, clientcontract.ErrorCodeInvalidTransport)
			}
		}
	}
	return nil
}

// validateProtobufEnumParity refuses a descriptor enum whose members do not
// match the published string enum of the same name. proto3 carries an enum as a
// number and JSON carries it as text, so the two encodings only describe one
// declaration when their member sets are the same one. The zero value is the
// proto3 absence marker and has no JSON member.
func validateProtobufEnumParity(spec SpecIR, descriptor *clientcontract.ProtobufDescriptor) error {
	for _, enum := range descriptor.Enums {
		schema, published := spec.Schemas[enum.Name]
		if !published {
			continue
		}
		declared, ok := stringEnumMembers(schema)
		if !ok {
			return errors.Newf(CodeClientGenConfig,
				"api: strict first-party protobuf enum %q is published as component schema %q, which declares no string enum members (%s)",
				enum.Name, enum.Name, clientcontract.ErrorCodeInvalidProtobuf)
		}
		projected := map[string]bool{}
		for i, value := range enum.Values {
			member := clientcontract.EnumMember(enum.Name, value.Name)
			if i == 0 {
				if declared[member] {
					return errors.Newf(CodeClientGenConfig,
						"api: strict first-party protobuf enum %q uses %q as its zero value, but %q is a declared member; the proto3 zero value is the absence marker (%s)",
						enum.Name, value.Name, member, clientcontract.ErrorCodeInvalidProtobuf)
				}
				continue
			}
			if !declared[member] {
				return errors.Newf(CodeClientGenConfig,
					"api: strict first-party protobuf enum %q declares value %q, which the published schema does not list as a member (%s)",
					enum.Name, value.Name, clientcontract.ErrorCodeInvalidProtobuf)
			}
			projected[member] = true
		}
		for member := range declared {
			if !projected[member] {
				return errors.Newf(CodeClientGenConfig,
					"api: strict first-party protobuf enum %q has no value for published member %q; a Connect client could not encode it (%s)",
					enum.Name, member, clientcontract.ErrorCodeInvalidProtobuf)
			}
		}
	}
	return nil
}

// stringEnumMembers reads the declared members of a published string enum.
func stringEnumMembers(schema clientcontract.Schema) (map[string]bool, bool) {
	if schema.Type != "string" || len(schema.Enum) == 0 {
		return nil, false
	}
	members := make(map[string]bool, len(schema.Enum))
	for _, raw := range schema.Enum {
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, false
		}
		members[value] = true
	}
	return members, true
}

// validateProtobufMessageParity refuses a descriptor message that disagrees with
// the published schema of the same name. The join is by name because that is how
// the provider projection names a message after the Go type the schema component
// is named after; a synthesized request or reply wrapper has no component and is
// left alone. Oneof members are skipped: the schema carries a union as one
// property, so its arms have no field of their own to join.
func validateProtobufMessageParity(spec SpecIR, descriptor *clientcontract.ProtobufDescriptor) error {
	for _, message := range descriptor.Messages {
		schema, published := spec.Schemas[message.Name]
		if !published || schema.Type != "object" {
			continue
		}
		required := map[string]bool{}
		for _, name := range schema.Required {
			required[name] = true
		}
		for _, field := range message.Fields {
			if field.OneOf != "" {
				continue
			}
			scope := "protobuf message " + message.Name + " field " + field.JSONName
			property, declared := schema.Properties[field.JSONName]
			if !declared {
				return errors.Newf(CodeClientGenConfig,
					"api: strict first-party %s is not a property of the published schema %q; a Connect client would send a field no REST client can (%s)",
					scope, message.Name, clientcontract.ErrorCodeInvalidProtobuf)
			}
			if err := checkProtobufFieldPresence(scope, field, property, required[field.JSONName]); err != nil {
				return err
			}
			if err := checkProtobufFieldShape(scope, field, property, spec.Schemas); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkProtobufFieldPresence refuses a field whose descriptor and schema
// disagree on what "no value" means. A property the schema lets be absent or
// null needs explicit proto3 presence; without it a Connect client cannot tell
// an omitted value from a zero one, and the same declaration means two things
// depending on the transport. Repeated fields and maps are absent-as-empty on
// both wires and proto3 forbids marking them optional.
func checkProtobufFieldPresence(scope string, field clientcontract.ProtobufField, property clientcontract.Schema, required bool) error {
	if field.Repeated || field.TypeKind == "map" {
		return nil
	}
	absent := !required || (property.Nullable != nil && *property.Nullable)
	if field.Optional == absent {
		return nil
	}
	if absent {
		return errors.Newf(CodeClientGenConfig,
			"api: strict first-party %s declares no proto3 presence, but the published schema lets the property be absent or null; declare the field optional (%s)",
			scope, clientcontract.ErrorCodeInvalidProtobuf)
	}
	return errors.Newf(CodeClientGenConfig,
		"api: strict first-party %s declares proto3 presence, but the published schema requires the property and forbids null (%s)",
		scope, clientcontract.ErrorCodeInvalidProtobuf)
}

// protobufScalarShapes maps every proto3 scalar this contract accepts onto the
// exact JSON type and format the published schema must declare for it. An empty
// format means the schema must declare none of its own beyond a refinement the
// type already allows (a string may still be a uuid or a date-time).
var protobufScalarShapes = map[string]struct{ jsonType, format string }{
	"int32": {"integer", "int32"}, "sint32": {"integer", "int32"}, "sfixed32": {"integer", "int32"},
	"int64": {"integer", "int64"}, "sint64": {"integer", "int64"}, "sfixed64": {"integer", "int64"},
	"uint32": {"integer", "uint32"}, "fixed32": {"integer", "uint32"},
	"uint64": {"integer", "uint64"}, "fixed64": {"integer", "uint64"},
	"double": {"number", "double"}, "float": {"number", "float"},
	"bool":   {"boolean", ""},
	"string": {"string", ""},
	"bytes":  {"string", "byte"},
}

// protobufWellKnownShapes is the closed correspondence between a protobuf
// well-known message and the JSON shape the published schema declares for it.
// A well-known type is the one place where the two wires legitimately carry
// different representations of one value, so the correspondence is declared here
// and a Connect codec converts across it — it is never inferred.
var protobufWellKnownShapes = map[string]struct{ jsonType, format string }{
	"google.protobuf.Timestamp": {"string", "date-time"},
	// Go's time.Duration is int64 nanoseconds in JSON; proto3 carries it as
	// seconds and nanos. Same quantity, two representations.
	"google.protobuf.Duration": {"integer", "int64"},
	"google.protobuf.Empty":    {"object", ""},
	"google.protobuf.Any":      {"object", ""},
}

// checkProtobufFieldShape refuses a descriptor field whose wire shape is not the
// shape the published schema declares — the case D0.2 named: an integer that is
// 64 bits on REST and 32 bits on Connect, narrowed by the transport rather than
// by a declaration.
func checkProtobufFieldShape(scope string, field clientcontract.ProtobufField, property clientcontract.Schema, components map[string]clientcontract.Schema) error {
	resolved := resolveClientSchema(property, components)
	if field.TypeKind == "map" {
		if resolved.Type != "object" || resolved.AdditionalProperties == nil || resolved.AdditionalProperties.Schema == nil {
			return errors.Newf(CodeClientGenConfig,
				"api: strict first-party %s is a protobuf map, but the published schema is not an object with a typed additionalProperties value (%s)",
				scope, clientcontract.ErrorCodeInvalidProtobuf)
		}
		return nil
	}
	// A repeated field of any kind is a JSON array of its element; compare the
	// element so a repeated message is not read as a single object.
	if field.Repeated {
		if resolved.Type != "array" || resolved.Items == nil {
			return errors.Newf(CodeClientGenConfig,
				"api: strict first-party %s is a repeated protobuf field, but the published schema is not an array (%s)",
				scope, clientcontract.ErrorCodeInvalidProtobuf)
		}
		resolved = resolveClientSchema(*resolved.Items, components)
	}
	switch field.TypeKind {
	case "message":
		shape, wellKnown := protobufWellKnownShapes[field.Type]
		if !wellKnown {
			if resolved.Type != "object" && len(resolved.OneOf) == 0 {
				return errors.Newf(CodeClientGenConfig,
					"api: strict first-party %s references protobuf message %q, but the published schema is not an object (%s)",
					scope, field.Type, clientcontract.ErrorCodeInvalidProtobuf)
			}
			return nil
		}
		return checkPublishedJSONShape(scope, field.Type, shape.jsonType, shape.format, resolved)
	case "enum":
		if _, ok := stringEnumMembers(resolved); !ok {
			return errors.Newf(CodeClientGenConfig,
				"api: strict first-party %s references protobuf enum %q, but the published schema declares no string enum members (%s)",
				scope, field.Type, clientcontract.ErrorCodeInvalidProtobuf)
		}
		return nil
	}
	shape, known := protobufScalarShapes[field.Type]
	if !known {
		return errors.Newf(CodeClientGenConfig,
			"api: strict first-party %s declares protobuf scalar %q, which has no published JSON shape (%s)",
			scope, field.Type, clientcontract.ErrorCodeInvalidProtobuf)
	}
	return checkPublishedJSONShape(scope, field.Type, shape.jsonType, shape.format, resolved)
}

// checkPublishedJSONShape compares one resolved property against the JSON type
// and format its protobuf type corresponds to.
func checkPublishedJSONShape(scope, protoType, jsonType, format string, resolved clientcontract.Schema) error {
	if resolved.Type != jsonType {
		return errors.Newf(CodeClientGenConfig,
			"api: strict first-party %s is protobuf %q, but the published schema declares JSON type %q instead of %q (%s)",
			scope, protoType, resolved.Type, jsonType, clientcontract.ErrorCodeInvalidProtobuf)
	}
	if format != "" && resolved.Format != format {
		return errors.Newf(CodeClientGenConfig,
			"api: strict first-party %s is protobuf %q, but the published schema declares format %q instead of %q; a width is declared, never narrowed by a transport (%s)",
			scope, protoType, resolved.Format, format, clientcontract.ErrorCodeInvalidProtobuf)
	}
	return nil
}

// resolveClientSchema follows one level of local component reference so a
// $ref property is compared as the schema it names. References are already
// proven resolvable by validateClientSchemaReferences.
func resolveClientSchema(schema clientcontract.Schema, components map[string]clientcontract.Schema) clientcontract.Schema {
	seen := map[string]bool{}
	for schema.Ref != "" {
		name := strings.TrimPrefix(schema.Ref, "#/components/schemas/")
		if seen[name] {
			return schema
		}
		seen[name] = true
		resolved, ok := components[name]
		if !ok {
			return schema
		}
		schema = resolved
	}
	return schema
}

func validateFirstPartyOpenAPISource(specJSON []byte) error {
	if err := validateNoDuplicateJSONKeys(specJSON); err != nil {
		return err
	}
	var document struct {
		Paths map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(specJSON, &document); err != nil {
		return err
	}
	for _, path := range sortedKeys(document.Paths) {
		var item map[string]json.RawMessage
		if err := json.Unmarshal(document.Paths[path], &item); err != nil {
			return errors.Newf(CodeClientGenConfig, "api: parse path item %q: %v", path, err)
		}
		if err := validateKnownJSONKeys("path "+path, item,
			"summary", "description", "parameters", "get", "post", "put", "patch", "delete", "head", "options", "trace"); err != nil {
			return err
		}
		if raw := item["parameters"]; len(raw) > 0 {
			if err := validateFirstPartyParameters("path "+path+" parameters", raw); err != nil {
				return err
			}
		}
		for _, method := range httpMethodOrder {
			if raw := item[method]; len(raw) > 0 {
				if err := validateFirstPartyOperation(strings.ToUpper(method)+" "+path, raw); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func validateNoDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var walk func(string) error
	walk = func(scope string) error {
		token, err := decoder.Token()
		if err != nil {
			return errors.Newf(CodeClientGenConfig, "api: parse first-party OpenAPI source at %s: %v", scope, err)
		}
		delimiter, compound := token.(json.Delim)
		if !compound {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				keyToken, keyErr := decoder.Token()
				if keyErr != nil {
					return errors.Newf(CodeClientGenConfig, "api: parse first-party OpenAPI source at %s: %v", scope, keyErr)
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.Newf(CodeClientGenConfig, "api: invalid object key at %s", scope)
				}
				if seen[key] {
					return errors.Newf(CodeClientGenConfig, "api: duplicate JSON key %q at %s", key, scope)
				}
				seen[key] = true
				if err := walk(scope + "." + key); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		case '[':
			for decoder.More() {
				if err := walk(scope + "[]"); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		default:
			return errors.Newf(CodeClientGenConfig, "api: unexpected JSON delimiter %q at %s", delimiter, scope)
		}
	}
	return walk("document")
}

func validateKnownJSONKeys(scope string, object map[string]json.RawMessage, allowed ...string) error {
	known := make(map[string]bool, len(allowed))
	for _, key := range allowed {
		known[key] = true
	}
	for _, key := range sortedKeys(object) {
		if !known[key] {
			return errors.Newf(CodeClientGenConfig,
				"api: strict first-party %s has unsupported OpenAPI field %q", scope, key)
		}
	}
	return nil
}

func validateFirstPartyOperation(scope string, raw json.RawMessage) error {
	var operation map[string]json.RawMessage
	if err := json.Unmarshal(raw, &operation); err != nil {
		return err
	}
	if err := validateKnownJSONKeys(scope, operation,
		"tags", "summary", "description", "operationId", "parameters", "requestBody", "responses",
		"deprecated", "security", "externalDocs", clientcontract.ExtensionKey, clientcontract.ExternalContractKey); err != nil {
		return err
	}
	if rawParameters := operation["parameters"]; len(rawParameters) > 0 {
		if err := validateFirstPartyParameters(scope+" parameters", rawParameters); err != nil {
			return err
		}
	}
	if request := operation["requestBody"]; len(request) > 0 {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(request, &object); err != nil {
			return err
		}
		if err := validateKnownJSONKeys(scope+" requestBody", object, "description", "required", "content"); err != nil {
			return err
		}
		if err := validateFirstPartyContent(scope+" requestBody content", object["content"]); err != nil {
			return err
		}
	}
	if responses := operation["responses"]; len(responses) > 0 {
		var byStatus map[string]json.RawMessage
		if err := json.Unmarshal(responses, &byStatus); err != nil {
			return err
		}
		for _, status := range sortedKeys(byStatus) {
			var response map[string]json.RawMessage
			if err := json.Unmarshal(byStatus[status], &response); err != nil {
				return err
			}
			if err := validateKnownJSONKeys(scope+" response "+status, response, "description", "headers", "content"); err != nil {
				return err
			}
			if err := validateFirstPartyContent(scope+" response "+status+" content", response["content"]); err != nil {
				return err
			}
			if err := validateFirstPartyHeaders(scope+" response "+status+" headers", response["headers"]); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateFirstPartyParameters(scope string, raw json.RawMessage) error {
	var parameters []json.RawMessage
	if err := json.Unmarshal(raw, &parameters); err != nil {
		return err
	}
	for i := range parameters {
		var parameter map[string]json.RawMessage
		if err := json.Unmarshal(parameters[i], &parameter); err != nil {
			return err
		}
		if err := validateKnownJSONKeys(scope, parameter,
			"name", "in", "description", "required", "deprecated", "schema", "example", "examples"); err != nil {
			return err
		}
	}
	return nil
}

func validateFirstPartyContent(scope string, raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var content map[string]json.RawMessage
	if err := json.Unmarshal(raw, &content); err != nil {
		return err
	}
	for _, mediaType := range sortedKeys(content) {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(content[mediaType], &object); err != nil {
			return err
		}
		if err := validateKnownJSONKeys(scope+" "+mediaType, object, "schema", "example", "examples", binaryMaxBytesKey, binaryStreamedKey); err != nil {
			return err
		}
		if marker, exists := object[binaryStreamedKey]; exists {
			if string(marker) != "true" || object[binaryMaxBytesKey] == nil {
				return errors.Newf(CodeClientGenUnsupportedSemantic, "api: %s streamed octets require %s true and a positive byte bound", scope, binaryStreamedKey)
			}
		}
	}
	return nil
}

func validateFirstPartyHeaders(scope string, raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var headers map[string]json.RawMessage
	if err := json.Unmarshal(raw, &headers); err != nil {
		return err
	}
	for _, name := range sortedKeys(headers) {
		var header map[string]json.RawMessage
		if err := json.Unmarshal(headers[name], &header); err != nil {
			return err
		}
		if err := validateKnownJSONKeys(scope+" "+name, header,
			"description", "required", "deprecated", "schema", "example", "examples"); err != nil {
			return err
		}
	}
	return nil
}

// validateClientSchemaReferences closes the last gap left by validating each
// Schema Object independently: every local component reference must resolve in
// the same first-party document. Third-party documents retain the historical
// permissive behavior through the caller's strict gate.
func validateClientSchemaReferences(spec SpecIR) error {
	check := func(scope string, schema *clientcontract.Schema) error {
		return walkClientSchema(scope, schema, func(scope string, schema *clientcontract.Schema) error {
			if err := checkClientSchemaReference(scope, schema, spec.Schemas); err != nil {
				return err
			}
			if err := checkFirstPartyIntegerWidth(scope, schema); err != nil {
				return err
			}
			return refuseBinaryInsideJSON(scope, schema)
		})
	}
	// A content entry is the one place a raw octet payload is representable, so
	// it is checked as a whole — media type, schema and bound together — instead
	// of letting the schema walk see a `format: binary` it must refuse.
	checkContent := func(scope string, content *ContentIR) error {
		if content.IsBinary() {
			return checkBinaryRepresentation(scope, *content)
		}
		if content.MaxBytes != 0 || content.Streamed {
			return errors.Newf(CodeClientGenUnsupportedSemantic,
				"api: strict first-party %s declares %s on a %s representation; the bound describes raw octets only",
				scope, binaryMaxBytesKey, content.MediaType)
		}
		if content.Schema == nil {
			return nil
		}
		return check(scope, content.Schema)
	}
	for _, name := range sortedKeys(spec.Schemas) {
		schema := spec.Schemas[name]
		if err := check("component schema "+name, &schema); err != nil {
			return err
		}
	}
	for _, service := range spec.Services {
		for _, method := range service.Methods {
			scope := "operation " + method.OperationID
			if method.Client != nil && method.Client.Resilience != nil && method.Client.Resilience.Cache != nil {
				request, response := binaryRequest(method), binaryResponse(method)
				if (request != nil && request.Streamed) || (response != nil && response.Streamed) {
					return errors.Newf(CodeClientGenUnsupportedSemantic, "api: %s cannot cache a streamed octet body", scope)
				}
			}
			for i := range method.Parameters {
				if err := check(scope+" parameter "+method.Parameters[i].Name, &method.Parameters[i].Schema); err != nil {
					return err
				}
			}
			if method.Request != nil {
				for i := range method.Request.Content {
					if err := checkContent(scope+" request "+method.Request.Content[i].MediaType, &method.Request.Content[i]); err != nil {
						return err
					}
				}
			}
			for i := range method.Successes {
				for j := range method.Successes[i].Content {
					if err := checkContent(scope+" response "+method.Successes[i].Content[j].MediaType, &method.Successes[i].Content[j]); err != nil {
						return err
					}
				}
				for j := range method.Successes[i].Headers {
					if err := check(scope+" response header "+method.Successes[i].Headers[j].Name, &method.Successes[i].Headers[j].Schema); err != nil {
						return err
					}
				}
			}
			if method.Client != nil {
				if method.Client.Messages != nil {
					if err := check(scope+" input message", method.Client.Messages.Input); err != nil {
						return err
					}
					if err := check(scope+" output message", method.Client.Messages.Output); err != nil {
						return err
					}
				}
				for i := range method.Client.Errors {
					if method.Client.Errors[i].Schema != nil {
						if err := check(scope+" error "+method.Client.Errors[i].Code, method.Client.Errors[i].Schema); err != nil {
							return err
						}
					}
				}
			}
		}
	}
	return nil
}

// walkClientSchema visits schema and every schema nested under it, in a
// deterministic order, applying one rule per node. Callers add rules instead of
// adding traversals, so a rule can never be applied to a smaller part of the
// document than the rule before it.
func walkClientSchema(scope string, schema *clientcontract.Schema, visit func(string, *clientcontract.Schema) error) error {
	if schema == nil {
		return nil
	}
	if err := visit(scope, schema); err != nil {
		return err
	}
	for _, name := range sortedKeys(schema.Properties) {
		child := schema.Properties[name]
		if err := walkClientSchema(scope+" property "+name, &child, visit); err != nil {
			return err
		}
	}
	if err := walkClientSchema(scope+" items", schema.Items, visit); err != nil {
		return err
	}
	if schema.AdditionalProperties != nil {
		if err := walkClientSchema(scope+" additionalProperties", schema.AdditionalProperties.Schema, visit); err != nil {
			return err
		}
	}
	for i := range schema.OneOf {
		if err := walkClientSchema(scope+" oneOf", &schema.OneOf[i], visit); err != nil {
			return err
		}
	}
	return nil
}

// binaryMaxBytesKey is the media-type extension that carries a raw octet
// payload's declared byte bound.
const binaryMaxBytesKey = "x-putnami-max-bytes"

const binaryStreamedKey = "x-putnami-streamed"

// refuseBinaryInsideJSON rejects `format: binary` in every position a JSON
// document reaches. JSON has no octet literal: a reader that accepted this
// would have to invent an encoding, and the two languages would invent
// different ones. Base64 bytes inside a JSON document are `format: byte`, which
// stays supported everywhere.
func refuseBinaryInsideJSON(scope string, schema *clientcontract.Schema) error {
	if schema.Type == "string" && schema.Format == "binary" {
		return errors.Newf(CodeClientGenUnsupportedSemantic,
			"api: strict first-party %s declares format \"binary\" inside a JSON document; declare base64 bytes as format \"byte\", or declare the whole body binary with its own media type",
			scope)
	}
	return nil
}

// checkBinaryRepresentation validates the one legitimate raw octet position:
// the root schema of a fixed non-JSON media type with a positive bound, or */*
// with explicit streaming. Neither permits JSON document vocabulary.
func checkBinaryRepresentation(scope string, content ContentIR) error {
	if isJSONMediaType(content.MediaType) {
		return errors.Newf(CodeClientGenUnsupportedSemantic,
			"api: strict first-party %s declares raw octets under a JSON media type", scope)
	}
	switch {
	case content.Streamed:
		if content.MediaType != "*/*" || content.MaxBytes <= 0 {
			return errors.Newf(CodeClientGenUnsupportedSemantic, "api: strict first-party %s streamed octets require */* and a positive byte bound", scope)
		}
	case content.MaxBytes <= 0:
		return errors.Newf(CodeClientGenUnsupportedSemantic,
			"api: strict first-party %s declares raw octets without a positive %s bound", scope, binaryMaxBytesKey)
	case !phttp.ConcreteMediaType(content.MediaType):
		return errors.Newf(CodeClientGenUnsupportedSemantic, "api: strict first-party %s bounded octets require a concrete media type", scope)
	}
	schema := *content.Schema
	if schema.Nullable != nil && *schema.Nullable {
		return errors.Newf(CodeClientGenUnsupportedSemantic,
			"api: strict first-party %s declares nullable raw octets; an absent body is an empty one", scope)
	}
	if len(schema.Enum) > 0 || len(schema.OneOf) > 0 || len(schema.Properties) > 0 || schema.Items != nil {
		return errors.Newf(CodeClientGenUnsupportedSemantic,
			"api: strict first-party %s constrains raw octets with JSON schema vocabulary", scope)
	}
	return nil
}

func checkClientSchemaReference(scope string, schema *clientcontract.Schema, components map[string]clientcontract.Schema) error {
	if schema.Ref != "" {
		name := strings.TrimPrefix(schema.Ref, "#/components/schemas/")
		if _, ok := components[name]; !ok {
			return errors.Newf(CodeClientGenConfig,
				"api: strict first-party %s references missing component schema %q", scope, name)
		}
	}
	if schema.Discriminator == nil {
		return nil
	}
	for _, tag := range sortedKeys(schema.Discriminator.Mapping) {
		ref := schema.Discriminator.Mapping[tag]
		name := strings.TrimPrefix(ref, "#/components/schemas/")
		if _, ok := components[name]; !ok {
			return errors.Newf(CodeClientGenConfig,
				"api: strict first-party %s discriminator %q references missing component schema %q", scope, tag, name)
		}
	}
	return nil
}

// firstPartyIntegerFormats is the closed set of integer widths a first-party
// contract may declare (D0.2). It is the same set the provider projection bounds
// and the strict emitter maps to a language type.
var firstPartyIntegerFormats = map[string]bool{"int32": true, "int64": true, "uint32": true, "uint64": true}

// checkFirstPartyIntegerWidth refuses an integer whose width is not declared.
// A reader that accepted it would have to pick a width, and the two language
// emitters picked different ones — Go int64, TypeScript number — from the same
// document. The width is a declaration, so its absence is a read failure.
func checkFirstPartyIntegerWidth(scope string, schema *clientcontract.Schema) error {
	if schema.Type != "integer" {
		return nil
	}
	if firstPartyIntegerFormats[schema.Format] {
		return nil
	}
	return errors.Newf(CodeClientGenConfig,
		"api: strict first-party %s declares integer format %q; declare int32, int64, uint32 or uint64 (%s)",
		scope, schema.Format, clientcontract.ErrorCodeRequired)
}

// orderedMethods returns the HTTP methods present on a path item in canonical
// order, skipping any non-method keys (parameters, summary, …).
func orderedMethods(pathItem map[string]oaOperation) []string {
	out := make([]string, 0, len(pathItem))
	for _, m := range httpMethodOrder {
		if _, ok := pathItem[m]; ok {
			out = append(out, m)
		}
	}
	return out
}

// operationToMethod converts a single OpenAPI operation into a MethodIR.
func operationToMethod(path, httpMethod string, op oaOperation, pathParameters []oaParameter, strict bool) (MethodIR, error) {
	operationID := op.OperationID
	if operationID == "" {
		operationID = buildOperationID(httpMethod, path)
	}

	method := MethodIR{
		Name:        toCamelCase(operationID),
		OperationID: operationID,
		HTTPMethod:  httpMethod,
		Path:        path,
		Client:      op.ParsedClient,
	}

	var params, query []FieldIR
	parameters := mergedParameters(pathParameters, op.Parameters)
	if strict {
		sort.Slice(parameters, func(i, j int) bool {
			left, right := parameterLocationRank(parameters[i].In), parameterLocationRank(parameters[j].In)
			if left != right {
				return left < right
			}
			return parameters[i].Name < parameters[j].Name
		})
	}
	for _, p := range parameters {
		if !strict {
			field := parameterToField(p)
			switch p.In {
			case "path":
				params = append(params, field)
			case "query":
				query = append(query, field)
			}
		}
		if strict && (p.In == "path" || p.In == "query" || p.In == "header") {
			parameter, err := neutralParameter(p, strict)
			if err != nil {
				return MethodIR{}, errors.Newf(CodeClientGenConfig, "api: %s %s parameter %q: %v", httpMethod, path, p.Name, err)
			}
			method.Parameters = append(method.Parameters, parameter)
		} else if strict {
			return MethodIR{}, errors.Newf(CodeClientGenConfig,
				"api: strict first-party operation %s %s has unsupported parameter location %q", httpMethod, path, p.In)
		}
	}
	if len(params) > 0 {
		method.Params = params
	}
	if len(query) > 0 {
		method.Query = query
	}

	// Request body: a $ref body is a named type; an inline object becomes
	// per-operation fields.
	if op.RequestBody != nil {
		if strict {
			request, err := neutralRequest(op.RequestBody, true)
			if err != nil {
				return MethodIR{}, errors.Newf(CodeClientGenConfig, "api: %s %s request body: %v", httpMethod, path, err)
			}
			method.Request = request
		}
		if !strict {
			if bodySchema := jsonSchema(op.RequestBody.Content); bodySchema != nil {
				if bodySchema.Ref != "" {
					method.BodyType = refName(bodySchema.Ref)
				} else if fields := schemaToFields(*bodySchema); len(fields) > 0 {
					method.Body = fields
				}
			}
		}
	}

	// Response body from the chosen 2xx JSON response.
	if !strict {
		if respSchema := pickResponseSchema(op.Responses); respSchema != nil {
			if respSchema.Ref != "" {
				method.ResponseType = refName(respSchema.Ref)
			} else if fields := schemaToFields(*respSchema); len(fields) > 0 {
				method.Response = fields
			}
		}
	}
	if strict {
		successes, err := neutralSuccesses(op.Responses, true)
		if err != nil {
			return MethodIR{}, errors.Newf(CodeClientGenConfig, "api: %s %s responses: %v", httpMethod, path, err)
		}
		method.Successes = successes
		if len(successes) == 0 && op.ParsedClient.Stream != clientcontract.StreamClient &&
			op.ParsedClient.Stream != clientcontract.StreamBidirectional {
			return MethodIR{}, errors.Newf(CodeClientGenConfig,
				"api: strict first-party operation %s %s declares no 2xx success response", httpMethod, path)
		}
		if (op.ParsedClient.Stream == clientcontract.StreamClient || op.ParsedClient.Stream == clientcontract.StreamBidirectional) &&
			!hasResponseStatus(op.Responses, "101") {
			return MethodIR{}, errors.Newf(CodeClientGenConfig,
				"api: strict first-party WebSocket operation %s %s declares no 101 upgrade response", httpMethod, path)
		}
	}

	return method, nil
}

func parameterLocationRank(location string) int {
	switch location {
	case "path":
		return 0
	case "query":
		return 1
	case "header":
		return 2
	default:
		return 3
	}
}

func hasResponseStatus(responses map[string]oaResponse, status string) bool {
	_, ok := responses[status]
	return ok
}

// validateMethodCacheKeyFields refuses a cache declaration whose key fields
// name an input the operation does not declare, or whose invalidation fields
// name no string, integer or boolean property of its JSON success body — a
// byte or binary string is octets, not a string. The
// provider fails the same way, because it re-reads what it publishes through
// this reader.
func validateMethodCacheKeyFields(method MethodIR, schemas map[string]clientcontract.Schema) []diagnostic.Diagnostic {
	if method.Client == nil || method.Client.Resilience == nil || method.Client.Resilience.Cache == nil {
		return nil
	}
	var inputs clientcontract.CacheKeyInputs
	for _, parameter := range method.Parameters {
		switch parameter.Location {
		case "path":
			inputs.Path = append(inputs.Path, parameter.Name)
		case "query":
			inputs.Query = append(inputs.Query, parameter.Name)
		case "header":
			inputs.Header = append(inputs.Header, parameter.Name)
		}
	}
	if method.Request != nil {
		inputs.Body = true
		for _, content := range method.Request.Content {
			if strings.EqualFold(content.MediaType, "application/json") {
				inputs.BodyProperties = clientcontract.CacheKeyBodyProperties(content.Schema, schemas)
			}
		}
	}
	diags := clientcontract.ValidateCacheKeyFields("resilience.cache", method.Client.Resilience.Cache, inputs)
	var bodies []*clientcontract.Schema
	for _, success := range method.Successes {
		for _, content := range success.Content {
			if !strings.EqualFold(content.MediaType, "application/json") {
				continue
			}
			if content.Schema != nil && content.Schema.Ref != "" {
				if _, ok := schemas[strings.TrimPrefix(content.Schema.Ref, "#/components/schemas/")]; !ok {
					// A dangling body reference is the reader's own refusal, which
					// names the missing component; a body it cannot read has no
					// properties to judge an invalidation field by.
					return diags
				}
			}
			bodies = append(bodies, content.Schema)
		}
	}
	return append(diags, clientcontract.ValidateCacheInvalidationFields("resilience.cache", method.Client.Resilience.Cache,
		clientcontract.CacheResponseProperties(bodies, schemas))...)
}

// validateMethodSSEContinuation refuses a cursor continuation whose output
// field or query parameter is not a plain string the operation declares
// (clientcontract ADR 0013). The TypeScript reader refuses the same documents
// with the same code.
func validateMethodSSEContinuation(method MethodIR, schemas map[string]clientcontract.Schema) []diagnostic.Diagnostic {
	if method.Client == nil {
		return nil
	}
	query := map[string]clientcontract.Schema{}
	for _, parameter := range method.Parameters {
		if parameter.Location == "query" {
			query[parameter.Name] = parameter.Schema
		}
	}
	return clientcontract.ValidateSSEContinuationReferences(method.Client, query, schemas)
}

func clientContractReadError(scope string, diags []diagnostic.Diagnostic) error {
	if len(diags) == 0 {
		return nil
	}
	if len(diags) == 1 {
		return errors.Newf(CodeClientGenConfig, "api: invalid %s: %s", scope, diags[0].String())
	}
	return errors.Newf(CodeClientGenConfig, "api: invalid %s: %s (and %d more diagnostics)", scope, diags[0].String(), len(diags)-1)
}

func neutralComponentSchemas(components *oaComponents, strict bool) (map[string]clientcontract.Schema, error) {
	if components == nil || len(components.Schemas) == 0 {
		return nil, nil
	}
	out := make(map[string]clientcontract.Schema, len(components.Schemas))
	for _, name := range sortedKeys(components.Schemas) {
		schema, err := neutralSchema(components.Schemas[name], strict)
		if err != nil {
			return nil, errors.Newf(CodeClientGenConfig, "api: component schema %q: %v", name, err)
		}
		out[name] = schema
	}
	return out, nil
}

func neutralParameter(parameter oaParameter, strict bool) (ParameterIR, error) {
	if parameter.Schema == nil {
		if strict {
			return ParameterIR{}, errors.New(CodeClientGenConfig, "strict first-party parameter is missing a schema")
		}
		return ParameterIR{Name: parameter.Name, Location: parameter.In, Required: parameter.Required}, nil
	}
	schema, err := neutralSchema(*parameter.Schema, strict)
	if err != nil {
		return ParameterIR{}, err
	}
	return ParameterIR{
		Name:     parameter.Name,
		Location: parameter.In,
		Required: parameter.Required,
		Schema:   schema,
	}, nil
}

func neutralRequest(request *oaRequestBody, strict bool) (*RequestIR, error) {
	content, err := neutralContent(request.Content, strict)
	if err != nil {
		return nil, err
	}
	if strict && len(content) == 0 {
		return nil, errors.New(CodeClientGenConfig, "strict first-party request body has no content representations")
	}
	return &RequestIR{Required: request.Required, Content: content}, nil
}

func neutralSuccesses(responses map[string]oaResponse, strict bool) ([]SuccessIR, error) {
	statuses := make([]int, 0, len(responses))
	byStatus := make(map[int]oaResponse, len(responses))
	for status, response := range responses {
		if len(status) != 3 || status[0] != '2' || !isDigit(status[1]) || !isDigit(status[2]) {
			continue
		}
		value := int(status[0]-'0')*100 + int(status[1]-'0')*10 + int(status[2]-'0')
		statuses = append(statuses, value)
		byStatus[value] = response
	}
	sort.Ints(statuses)
	out := make([]SuccessIR, 0, len(statuses))
	for _, status := range statuses {
		response := byStatus[status]
		content, err := neutralContent(response.Content, strict)
		if err != nil {
			return nil, errors.Newf(CodeClientGenConfig, "response %d: %v", status, err)
		}
		headers := make([]ResponseHeaderIR, 0, len(response.Headers))
		for _, name := range sortedKeys(response.Headers) {
			header := response.Headers[name]
			if header.Schema == nil {
				if strict {
					return nil, errors.Newf(CodeClientGenConfig, "response %d header %q is missing a schema", status, name)
				}
				continue
			}
			schema, err := neutralSchema(*header.Schema, strict)
			if err != nil {
				return nil, errors.Newf(CodeClientGenConfig, "response %d header %q: %v", status, name, err)
			}
			headers = append(headers, ResponseHeaderIR{Name: name, Required: header.Required, Schema: schema})
		}
		out = append(out, SuccessIR{
			Status:      status,
			Description: response.Description,
			Content:     content,
			Headers:     headers,
		})
	}
	return out, nil
}

func neutralContent(content map[string]oaMediaType, strict bool) ([]ContentIR, error) {
	mediaTypes := sortedKeys(content)
	out := make([]ContentIR, 0, len(mediaTypes))
	for _, mediaType := range mediaTypes {
		entry := ContentIR{MediaType: mediaType}
		if streamed := content[mediaType].Streamed; streamed != nil {
			entry.Streamed = *streamed
		}
		if bound := content[mediaType].MaxBytes; bound != nil {
			entry.MaxBytes = *bound
		}
		if content[mediaType].Schema == nil {
			if strict {
				return nil, errors.Newf(CodeClientGenConfig, "media type %q is missing a schema", mediaType)
			}
		} else {
			schema, err := neutralSchema(*content[mediaType].Schema, strict)
			if err != nil {
				return nil, errors.Newf(CodeClientGenConfig, "media type %q: %v", mediaType, err)
			}
			entry.Schema = &schema
		}
		out = append(out, entry)
	}
	return out, nil
}

func neutralSchema(schema oaSchema, strict bool) (clientcontract.Schema, error) {
	if len(schema.Raw) == 0 {
		if strict {
			return clientcontract.Schema{}, errors.New(CodeClientGenConfig, "strict first-party schema is missing its source representation")
		}
		return clientcontract.Schema{}, nil
	}
	if strict {
		parsed, diags := clientcontract.ParseAndValidateSchema(schema.Raw)
		if len(diags) > 0 {
			return clientcontract.Schema{}, clientContractReadError("schema", diags)
		}
		return *parsed, nil
	}
	var parsed clientcontract.Schema
	if err := json.Unmarshal(schema.Raw, &parsed); err != nil {
		return clientcontract.Schema{}, errors.Newf(CodeClientGenConfig, "parse schema: %v", err)
	}
	return parsed, nil
}

func mergedParameters(pathParameters, operationParameters []oaParameter) []oaParameter {
	if len(pathParameters) == 0 {
		return operationParameters
	}
	out := append([]oaParameter(nil), pathParameters...)
	for _, p := range operationParameters {
		replaced := false
		for i := range out {
			if out[i].Name == p.Name && out[i].In == p.In {
				out[i] = p
				replaced = true
				break
			}
		}
		if !replaced {
			out = append(out, p)
		}
	}
	return out
}

// pickResponseSchema selects the schema of the first JSON 2xx response,
// preferring 200, then 201, then the lowest remaining 2xx status with a JSON
// body.
func pickResponseSchema(responses map[string]oaResponse) *oaSchema {
	if len(responses) == 0 {
		return nil
	}
	var twoXx []string
	for code := range responses {
		if len(code) == 3 && code[0] == '2' && isDigit(code[1]) && isDigit(code[2]) {
			twoXx = append(twoXx, code)
		}
	}
	sort.Strings(twoXx)
	ordered := make([]string, 0, len(twoXx))
	for _, c := range twoXx {
		if c == "200" {
			ordered = append(ordered, c)
		}
	}
	for _, c := range twoXx {
		if c == "201" {
			ordered = append(ordered, c)
		}
	}
	for _, c := range twoXx {
		if c != "200" && c != "201" {
			ordered = append(ordered, c)
		}
	}
	for _, code := range ordered {
		if schema := jsonSchema(responses[code].Content); schema != nil {
			return schema
		}
	}
	return nil
}

// jsonSchema extracts the application/json schema from a content map, or nil.
func jsonSchema(content map[string]oaMediaType) *oaSchema {
	if content == nil {
		return nil
	}
	mt, ok := content["application/json"]
	if !ok {
		return nil
	}
	return mt.Schema
}

// parameterToField converts an OpenAPI parameter to a FieldIR.
func parameterToField(p oaParameter) FieldIR {
	goType := "string"
	isArray := false
	if p.Schema != nil {
		goType, isArray = resolveFieldType(*p.Schema)
	}
	return FieldIR{
		Name:     p.Name,
		GoType:   goType,
		Optional: !p.Required,
		Array:    isArray,
	}
}

// buildComponentTypes classifies components.schemas into object models, closed
// enums, and tagged unions so no contract type degrades to a scalar or empty
// interface on its way to a generated client.
func buildComponentTypes(components *oaComponents) (map[string][]FieldIR, map[string][]string, map[string]UnionIR) {
	if components == nil || len(components.Schemas) == 0 {
		return nil, nil, nil
	}
	named := make(map[string][]FieldIR, len(components.Schemas))
	enums := make(map[string][]string)
	unions := make(map[string]UnionIR)
	for _, name := range sortedKeys(components.Schemas) {
		schema := components.Schemas[name]
		if values, ok := stringEnumValues(schema.Enum); ok && len(values) > 0 {
			enums[name] = values
			continue
		}
		if len(schema.OneOf) > 0 && schema.Discriminator != nil {
			unions[name] = schemaToUnion(schema, components.Schemas)
			continue
		}
		fields := schemaToFields(schema)
		if fields == nil {
			// A named model with no usable properties is still a declared type;
			// keep it as an empty (non-nil) field set so the value serializes as
			// [] rather than null, matching the TS reader.
			fields = []FieldIR{}
		}
		named[name] = fields
	}
	if len(named) == 0 {
		named = nil
	}
	if len(enums) == 0 {
		enums = nil
	}
	if len(unions) == 0 {
		unions = nil
	}
	return named, enums, unions
}

func schemaToUnion(schema oaSchema, components map[string]oaSchema) UnionIR {
	discriminator := schema.Discriminator.PropertyName
	union := UnionIR{Discriminator: discriminator, Variants: make([]UnionVariantIR, 0, len(schema.OneOf))}
	for _, arm := range schema.OneOf {
		resolved := arm
		if arm.Ref != "" {
			if component, ok := components[refName(arm.Ref)]; ok {
				resolved = component
			}
		}
		tag := ""
		if discSchema, ok := resolved.Properties[discriminator]; ok {
			if values, stringsOnly := stringEnumValues(discSchema.Enum); stringsOnly && len(values) > 0 {
				tag = values[0]
			}
		}
		if tag == "" && arm.Ref != "" {
			for _, candidate := range sortedKeys(schema.Discriminator.Mapping) {
				if refName(schema.Discriminator.Mapping[candidate]) == refName(arm.Ref) {
					tag = candidate
					break
				}
			}
		}
		fields := schemaToFields(resolved)
		filtered := fields[:0]
		for _, field := range fields {
			if field.Name != discriminator {
				filtered = append(filtered, field)
			}
		}
		union.Variants = append(union.Variants, UnionVariantIR{Tag: tag, Fields: filtered})
	}
	return union
}

func stringEnumValues(raw []json.RawMessage) ([]string, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	values := make([]string, len(raw))
	for i := range raw {
		if err := json.Unmarshal(raw[i], &values[i]); err != nil {
			return nil, false
		}
	}
	return values, true
}

// schemaToFields converts an OpenAPI object schema to a deterministic, sorted
// []FieldIR.
func schemaToFields(schema oaSchema) []FieldIR {
	if len(schema.Properties) == 0 {
		return nil
	}
	required := make(map[string]bool, len(schema.Required))
	for _, r := range schema.Required {
		required[r] = true
	}
	fields := make([]FieldIR, 0, len(schema.Properties))
	for _, name := range sortedSchemaKeys(schema.Properties) {
		// Skip names that could be dangerous if reflected into prototype-based
		// languages, keeping the IR field set identical to the TS reader's.
		if name == "__proto__" || name == "constructor" || name == "prototype" {
			continue
		}
		goType, array := resolveFieldType(schema.Properties[name])
		fields = append(fields, FieldIR{
			Name:     name,
			GoType:   goType,
			Optional: !required[name],
			Array:    array,
		})
	}
	if len(fields) == 0 {
		return nil
	}
	return fields
}

// resolveFieldType resolves a property schema to a Go type token + array flag,
// mirroring the TS reader's rules:
//   - $ref → the referenced model name.
//   - array → the element type with array=true.
//   - inline object (no $ref) → "map[string]any" (the degrade rule).
//   - everything else → a scalar.
func resolveFieldType(prop oaSchema) (goType string, array bool) {
	if prop.Ref != "" {
		return refName(prop.Ref), false
	}
	if prop.Type == "array" {
		items := prop.Items
		if items == nil {
			return "any", true
		}
		if items.Ref != "" {
			return refName(items.Ref), true
		}
		if items.Type == "object" {
			return "map[string]any", true
		}
		return scalarGoType(items.Type, items.Format), true
	}
	if prop.Type == "object" {
		return "map[string]any", false
	}
	return scalarGoType(prop.Type, prop.Format), false
}

// scalarGoType maps an OpenAPI scalar type + format to a Go type token. Unlike
// the TS reader (which collapses integer/number to `number`), the Go reader
// preserves integer vs. floating-point and honors the int32/float formats so the
// generated client uses precise Go types. An unknown/absent type degrades to
// `string`, matching the TS reader's default.
func scalarGoType(typ, format string) string {
	switch typ {
	case "integer":
		if format == "int32" {
			return "int32"
		}
		return "int64"
	case "number":
		if format == "float" {
			return "float32"
		}
		return "float64"
	case "boolean":
		return "bool"
	case "string":
		return "string"
	default:
		return "string"
	}
}

// inferServiceName derives a service name from the first non-parameter path
// segment, e.g. "/users/{id}" → "UsersService".
func inferServiceName(path string) string {
	first := "api"
	for seg := range strings.SplitSeq(path, "/") {
		if seg == "" || strings.HasPrefix(seg, "{") {
			continue
		}
		first = seg
		break
	}
	// A path segment may carry characters no identifier can ("/.well-known/...").
	// Every one of them separates words, not only '-' and '_', so the class name
	// is a legal identifier. The TypeScript reader (openapi-reader.ts) applies
	// the same rule.
	name := pascalCase(strings.Map(func(r rune) rune {
		if ('0' <= r && r <= '9') || ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z') {
			return r
		}
		return '-'
	}, first))
	if name == "" {
		name = "Api"
	}
	if isDigit(name[0]) {
		name = "_" + name
	}
	return name + "Service"
}

// refName extracts the schema name from a local component $ref pointer.
func refName(ref string) string {
	idx := strings.LastIndex(ref, "/")
	if idx < 0 {
		return ref
	}
	return ref[idx+1:]
}

// buildOperationID synthesizes an operation id for a SPEC operation that
// declares none, mirroring the TS reader (`openapi-reader.ts`) so the IR matches
// across languages on the same fixture.
//
// It is deliberately not [CanonicalOperationID]: that one names an operation the
// Go framework itself owns (and stamps into the spec it generates), while this
// one is a last-resort reconstruction for a foreign spec that omitted the id.
// Every spec this repository produces declares an operationId, so the two never
// name the same operation.
func buildOperationID(httpMethod, path string) string {
	var b strings.Builder
	b.WriteString(strings.ToLower(httpMethod))
	for seg := range strings.SplitSeq(path, "/") {
		if seg == "" {
			continue
		}
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			b.WriteString("_" + seg[1:len(seg)-1])
			continue
		}
		b.WriteString(strings.ToUpper(seg[:1]) + seg[1:])
	}
	return b.String()
}

// toCamelCase normalizes OpenAPI operation identifiers into the shared
// Go-TypeScript generated method form.
func toCamelCase(s string) string {
	if s == "" {
		return s
	}
	pascal := pascalCase(s)
	if pascal == "" {
		return ""
	}
	return strings.ToLower(pascal[:1]) + pascal[1:]
}

// pascalCase splits on hyphens/underscores and upper-cases each part.
func pascalCase(s string) string {
	var b strings.Builder
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == '-' || r == '_' }) {
		if part == "" {
			continue
		}
		b.WriteString(strings.ToUpper(part[:1]) + part[1:])
	}
	return b.String()
}

func specHash(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])[:16]
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedSchemaKeys(m map[string]oaSchema) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
