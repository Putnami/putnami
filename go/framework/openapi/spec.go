// Package openapi generates OpenAPI 3.0.3 specifications from registered
// HTTP endpoints. It introspects endpoint metadata (schemas, responses,
// security) to produce a complete API document.
package openapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.putnami.dev/api"
	perrors "go.putnami.dev/errors"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/contracts"
	validation "go.putnami.dev/schema"
)

// catchAllTokenRE matches {name...} catch-all path tokens from the api package's
// path syntax. The captured group is the parameter name.
var catchAllTokenRE = regexp.MustCompile(`\{([A-Za-z_][A-Za-z0-9_]*)\.\.\.\}`)

// Document represents an OpenAPI 3.0.3 specification.
type Document struct {
	OpenAPI        string                     `json:"openapi"`
	Info           Info                       `json:"info"`
	Servers        []Server                   `json:"servers,omitempty"`
	Paths          map[string]PathItem        `json:"paths"`
	Components     *Components                `json:"components,omitempty"`
	ClientContract *clientcontract.DocumentV1 `json:"x-putnami-client,omitempty"`
	generationErr  error
}

// MarshalJSON fails when schema generation found a declaration that cannot be
// represented faithfully. The invalid value is never written into an OpenAPI
// document and callers keep the existing GenerateSpec API.
func (d Document) MarshalJSON() ([]byte, error) {
	if d.generationErr != nil {
		return nil, d.generationErr
	}
	type document Document
	return json.Marshal(document(d))
}

// Info provides metadata about the API.
type Info struct {
	Title       string `json:"title"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
}

// Server represents a target server.
type Server struct {
	URL         string `json:"url"`
	Description string `json:"description,omitempty"`
}

// Components holds reusable schema and security definitions.
type Components struct {
	Schemas         map[string]SchemaObject         `json:"schemas,omitempty"`
	SecuritySchemes map[string]SecuritySchemeObject `json:"securitySchemes,omitempty"`
}

// PathItem groups operations by HTTP method for a single path.
type PathItem map[string]Operation

// Operation describes a single API operation.
type Operation struct {
	OperationID    string                      `json:"operationId,omitempty"`
	Summary        string                      `json:"summary,omitempty"`
	Description    string                      `json:"description,omitempty"`
	Tags           []string                    `json:"tags,omitempty"`
	Parameters     []Parameter                 `json:"parameters,omitempty"`
	RequestBody    *RequestBody                `json:"requestBody,omitempty"`
	Responses      map[string]Response         `json:"responses"`
	Security       []SecurityReq               `json:"security,omitempty"`
	ClientContract *clientcontract.OperationV1 `json:"x-putnami-client,omitempty"`
	// ExternalContract names the external authority that owns this
	// operation's wire contract (api.ClientOperationOptions.External). An
	// operation of a first-party document carries it instead of
	// ClientContract, and every first-party reader skips the operation.
	ExternalContract string `json:"x-putnami-external-contract,omitempty"`
}

// Parameter describes a single operation parameter.
type Parameter struct {
	Name        string        `json:"name"`
	In          string        `json:"in"` // "path", "query", "header"
	Description string        `json:"description,omitempty"`
	Required    bool          `json:"required"`
	Schema      *SchemaObject `json:"schema,omitempty"`
}

// RequestBody describes a request body.
type RequestBody struct {
	Description string               `json:"description,omitempty"`
	Required    bool                 `json:"required"`
	Content     map[string]MediaType `json:"content"`
}

// MediaType describes a media type with its schema.
type MediaType struct {
	Schema *SchemaObject `json:"schema,omitempty"`
	// MaxBytes is the declared byte bound of a raw octet payload. It is the one
	// fact a binary representation carries that no JSON Schema keyword can:
	// `format: binary` says the payload is octets, not how many a peer may send.
	// Absent on every JSON representation.
	MaxBytes *int64 `json:"x-putnami-max-bytes,omitempty"`
	Streamed bool   `json:"x-putnami-streamed,omitempty"`
}

// Response describes a single API response.
type Response struct {
	Description string               `json:"description"`
	Content     map[string]MediaType `json:"content,omitempty"`
}

// SchemaObject represents an OpenAPI schema. AnyOf documents a value several
// schemas may describe at once; only the error envelope's `details` uses it,
// when codes sharing a status declare different details types. The client
// contract carries each code's own schema instead, so no projection into it
// meets an AnyOf.
type SchemaObject struct {
	Type                 string                      `json:"type,omitempty"`
	Format               string                      `json:"format,omitempty"`
	Nullable             *bool                       `json:"nullable,omitempty"`
	Description          string                      `json:"description,omitempty"`
	Properties           map[string]SchemaObject     `json:"properties,omitempty"`
	Required             []string                    `json:"required,omitempty"`
	Items                *SchemaObject               `json:"items,omitempty"`
	Enum                 []string                    `json:"enum,omitempty"`
	OneOf                []SchemaObject              `json:"oneOf,omitempty"`
	AnyOf                []SchemaObject              `json:"anyOf,omitempty"`
	Discriminator        *DiscriminatorObject        `json:"discriminator,omitempty"`
	AdditionalProperties *SchemaAdditionalProperties `json:"additionalProperties,omitempty"`
	Ref                  string                      `json:"$ref,omitempty"`
	Default              json.RawMessage             `json:"default,omitempty"`
	Minimum              *json.Number                `json:"minimum,omitempty"`
	Maximum              *json.Number                `json:"maximum,omitempty"`
	ExclusiveMinimum     *bool                       `json:"exclusiveMinimum,omitempty"`
	ExclusiveMaximum     *bool                       `json:"exclusiveMaximum,omitempty"`
	MinLength            *int                        `json:"minLength,omitempty"`
	MaxLength            *int                        `json:"maxLength,omitempty"`
	Pattern              string                      `json:"pattern,omitempty"`
	MinItems             *int                        `json:"minItems,omitempty"`
	MaxItems             *int                        `json:"maxItems,omitempty"`
	UniqueItems          *bool                       `json:"uniqueItems,omitempty"`
	ReadOnly             *bool                       `json:"readOnly,omitempty"`
	WriteOnly            *bool                       `json:"writeOnly,omitempty"`
	// OpaqueJSON declares a value that may be any JSON document
	// (clientcontract.OpaqueJSONKey). It stands alone; see opaqueJSONSchema.
	OpaqueJSON string `json:"x-putnami-json,omitempty"`
}

// opaqueJSONSchema is the one declaration of a value the provider does not
// interpret: json.RawMessage, the empty interface, and the error envelope's
// details. The empty schema is not that declaration — it is what a projection
// writes when it cannot describe a type, so the strict reader refuses it.
func opaqueJSONSchema() *SchemaObject {
	return &SchemaObject{OpaqueJSON: clientcontract.OpaqueJSONAny}
}

// freeFormObjectSchema is a JSON object whose members are opaque JSON values:
// a string-keyed map of json.RawMessage or of the empty interface. It is the
// standard OpenAPI spelling, and the only one the strict reader accepts.
func freeFormObjectSchema() *SchemaObject {
	allowed := true
	return &SchemaObject{Type: "object", AdditionalProperties: &SchemaAdditionalProperties{Allowed: &allowed}}
}

// SchemaAdditionalProperties preserves both OpenAPI forms: a boolean controls
// whether undeclared keys are accepted, while a schema describes typed map
// values. Keeping the union explicit prevents map[string]T from degrading to an
// untyped object in first-party clients.
type SchemaAdditionalProperties struct {
	Allowed *bool
	Schema  *SchemaObject
}

// MarshalJSON emits either the boolean or typed-schema OpenAPI representation.
func (a SchemaAdditionalProperties) MarshalJSON() ([]byte, error) {
	if a.Allowed != nil && a.Schema == nil {
		return json.Marshal(*a.Allowed)
	}
	if a.Schema != nil && a.Allowed == nil {
		return json.Marshal(a.Schema)
	}
	return nil, fmt.Errorf("additionalProperties must hold exactly one of boolean or schema")
}

// UnmarshalJSON accepts either the boolean or typed-schema OpenAPI representation.
func (a *SchemaAdditionalProperties) UnmarshalJSON(data []byte) error {
	var allowed bool
	if err := json.Unmarshal(data, &allowed); err == nil {
		a.Allowed = &allowed
		a.Schema = nil
		return nil
	}
	var schema SchemaObject
	if err := json.Unmarshal(data, &schema); err != nil {
		return fmt.Errorf("additionalProperties must be a boolean or schema: %w", err)
	}
	a.Allowed = nil
	a.Schema = &schema
	return nil
}

func additionalPropertiesForbidden() *SchemaAdditionalProperties {
	value := false
	return &SchemaAdditionalProperties{Allowed: &value}
}

func additionalPropertiesSchema(schema *SchemaObject) *SchemaAdditionalProperties {
	return &SchemaAdditionalProperties{Schema: schema}
}

// DiscriminatorObject describes the property that selects a tagged-union arm.
type DiscriminatorObject struct {
	PropertyName string            `json:"propertyName"`
	Mapping      map[string]string `json:"mapping,omitempty"`
}

// SecuritySchemeObject describes a security scheme.
type SecuritySchemeObject struct {
	Type         string `json:"type"`
	Scheme       string `json:"scheme,omitempty"`
	BearerFormat string `json:"bearerFormat,omitempty"`
	Description  string `json:"description,omitempty"`
}

// SecurityReq is a map of security scheme names to required scopes.
type SecurityReq map[string][]string

// --- Route Discovery ---

// DiscoveredRoute holds metadata about a registered endpoint for spec generation.
type DiscoveredRoute struct {
	Method        string
	Path          string
	StreamMode    string // "server", "client", or "bidirectional" for SSE/WebSocket streams
	Description   string
	Tags          []string
	Params        reflect.Type    // Struct type for path parameters
	Query         reflect.Type    // Struct type for query parameters
	Body          reflect.Type    // Struct type for request body
	BodyBinary    *api.BinaryMeta // Raw octet request payload, when declared
	Returns       reflect.Type    // Struct type for primary success response body
	ReturnsBinary *api.BinaryMeta // Raw octet success payload, when declared
	// ProviderWire declares a provider-owned WebSocket wire: raw octets, or
	// JSON values of the declared messages under the provider's subprotocol.
	ProviderWire       *api.ProviderWireMeta
	ReturnsStatus      int    // Exact primary 2xx status; zero defaults to 200
	ReturnsDescription string // Primary response description; empty uses the default
	// AdditionalReturns documents extra success responses (e.g. 202 Accepted, 204 No
	// Content) declared via api.Endpoint().Response(status, …). An entry whose status
	// matches ReturnsStatus takes precedence over Returns.
	AdditionalReturns []ThrowsMeta
	ErrorCodes        []perrors.Code
	ErrorRetryability map[perrors.Code]bool
	// ErrorDetails maps a declared error code to the type of its envelope's
	// `details` member (api.EndpointBuilder.MayThrowDetails). It becomes that
	// error's x-putnami-client schema and documents `details` on its response.
	ErrorDetails  map[perrors.Code]reflect.Type
	Throws        []ThrowsMeta
	Security      *SecurityMeta
	ClientOptions *api.ClientOperationOptions
}

// ThrowsMeta describes an error response.
type ThrowsMeta struct {
	Status      int
	Description string
	Schema      reflect.Type
}

// SecurityMeta describes security requirements for an endpoint.
type SecurityMeta struct {
	Roles         []string
	Scopes        []string
	Authorization *clientcontract.Authorization
	ExcludePaths  []string
	Representable bool
	// Optional records that the route's rule serves a request that presents no
	// credential (phttp.SecurityOptionalAuthentication). Roles, Scopes and
	// Authorization then apply to an authenticated caller only, and the
	// first-party contract offers an anonymous alternative after the
	// credentialed ones.
	Optional bool
}

// --- Spec Generation ---

// Options configures OpenAPI document generation.
type Options struct {
	Title           string
	Version         string
	Description     string
	Servers         []Server
	SecuritySchemes map[string]SecuritySchemeObject
	// Contract projects the canonical enum/DTO/union vocabulary into reusable
	// OpenAPI components. Named Go endpoint types with the same contract node name
	// reference these components instead of being reconstructed through reflection.
	Contract *contracts.Manifest
	// ClientContract marks the generated document as a strict first-party client
	// contract. Operation extensions are projected mechanically from route facts.
	ClientContract *clientcontract.DocumentV1
	// ConnectMethods binds "<METHOD> <path>" to the canonical protobuf method
	// identity the published descriptor declares for that route. It comes from
	// the proto plugin, so the contract never recomputes an RPC name.
	ConnectMethods map[string]string
	// ConnectEncodings lists the payload encodings a mounted Connect server
	// actually serves, in provider preference order. Empty means no Connect
	// server is mounted and no Connect transport is advertised.
	ConnectEncodings []clientcontract.Encoding
}

// connectProjection resolves the Connect transports of one route. Both halves
// are required: the method binding proves the descriptor declares the route, the
// encodings prove a mounted server answers it.
type connectProjection struct {
	methods   map[string]string
	encodings []clientcontract.Encoding
}

func (c connectProjection) transportsFor(route DiscoveredRoute) []clientcontract.Transport {
	// A Connect call carries one request message and one response message,
	// both encoded by the published descriptor. A declared raw octet payload has
	// no descriptor and no encoding: carrying it would mean base64 inside the
	// envelope, which is the silent JSON re-wrapping a binary declaration exists
	// to refuse. The route keeps its REST transport and declares no Connect one.
	if route.BodyBinary != nil || route.ReturnsBinary != nil {
		return nil
	}
	if len(c.methods) == 0 || len(c.encodings) == 0 {
		return nil
	}
	// A unary route and a declared server stream are advertised; a client or
	// bidirectional stream is not. Connect carries at most one request message
	// per call, so advertising it for a duplex stream would hand a generated
	// client a URL that answers a declared conversation with one buffered reply.
	if route.StreamMode != "" && route.StreamMode != "server" {
		return nil
	}
	method, declared := c.methods[connectRouteKey(route.Method, route.Path)]
	if !declared {
		return nil
	}
	transports := make([]clientcontract.Transport, 0, len(c.encodings))
	for _, encoding := range c.encodings {
		transports = append(transports, clientcontract.Transport{
			Protocol:       clientcontract.TransportConnect,
			Path:           method,
			Encoding:       encoding,
			ProtobufMethod: method,
		})
	}
	return transports
}

// connectRouteKey is the shared route identity the proto projection keys on.
func connectRouteKey(method, path string) string {
	return strings.ToUpper(method) + " " + path
}

// GenerateSpec produces an OpenAPI 3.0.3 document from discovered routes.
func GenerateSpec(routes []DiscoveredRoute, opts Options) *Document {
	routes = sortedRoutes(routes)
	doc := &Document{
		OpenAPI:        "3.0.3",
		ClientContract: opts.ClientContract,
		Info: Info{
			Title:       opts.Title,
			Version:     opts.Version,
			Description: opts.Description,
		},
		Servers: opts.Servers,
		Paths:   make(map[string]PathItem),
	}

	// One generator shared across all routes so every named struct is emitted
	// once into components and referenced via $ref everywhere (dedup across ops,
	// and self-referential types terminate at the shared back-edge).
	gen := newSchemaGen(opts.Contract)
	connect := connectProjection{methods: opts.ConnectMethods, encodings: opts.ConnectEncodings}

	hasSecurity := false
	for _, route := range routes {
		if route.Security != nil {
			hasSecurity = true
			break
		}
	}
	var secReqs []SecurityReq
	if hasSecurity {
		schemes := opts.SecuritySchemes
		if len(schemes) == 0 {
			schemes = map[string]SecuritySchemeObject{
				"bearerAuth": {
					Type:         "http",
					Scheme:       "bearer",
					BearerFormat: "JWT",
				},
			}
		}
		doc.Components = &Components{
			SecuritySchemes: schemes,
		}
		// Operations must reference the schemes that actually exist in
		// components.securitySchemes; hardcoding "bearerAuth" produced an invalid
		// spec whenever the caller overrode SecuritySchemes with a custom key.
		secReqs = securityRequirements(schemes)
	}

	for _, route := range routes {
		oaPath := frameworkPathToOpenAPI(route.Path)

		pathItem, ok := doc.Paths[oaPath]
		if !ok {
			pathItem = make(PathItem)
		}

		checkExternalContract(gen, route, opts.ClientContract != nil)
		// A route an external authority owns is documented the way a provider
		// without a client contract documents it: the standard, not the
		// first-party contract, owns its request and error shapes.
		external := route.ClientOptions.IsExternal()
		firstParty := opts.ClientContract != nil && !external
		op := buildOperation(gen, route, secReqs, firstParty)
		switch {
		case firstParty:
			op.ClientContract = buildClientOperation(gen, route, opts.ClientContract, connect)
			validateDeclaredSSECursor(gen, route, op.ClientContract, op.Parameters)
		case opts.ClientContract != nil:
			op.ExternalContract = route.ClientOptions.External
		}
		pathItem[strings.ToLower(route.Method)] = op
		doc.Paths[oaPath] = pathItem
	}

	// Named struct types collected during generation become reusable component
	// schemas referenced via $ref. Only create Components for them when the
	// security block above did not already.
	if len(gen.components) > 0 {
		if doc.Components == nil {
			doc.Components = &Components{}
		}
		doc.Components.Schemas = gen.components
	}
	if opts.ClientContract != nil {
		closeFirstPartySchemas(gen, doc)
	}
	doc.generationErr = gen.generationErr

	return doc
}

// checkExternalContract records a generation failure for an External
// declaration that contradicts itself or the document: External leaves a route
// out of a first-party contract, so a document without one has nothing to
// leave it out of. The api plugin refuses the same declarations at Configure;
// this covers routes handed to GenerateSpec directly.
func checkExternalContract(g *schemaGen, route DiscoveredRoute, firstPartyDocument bool) {
	if g.generationErr != nil {
		return
	}
	authority, err := route.ClientOptions.ExternalAuthority()
	if err != nil {
		g.generationErr = fmt.Errorf("openapi: %s %s %s", strings.ToUpper(route.Method), route.Path, err.Error())
		return
	}
	if authority != "" && !firstPartyDocument {
		g.generationErr = fmt.Errorf(
			"openapi: %s %s declares External %q, and the document publishes no first-party client contract; External leaves a route out of the contract api.WithClientService publishes",
			strings.ToUpper(route.Method), route.Path, authority)
	}
}

func closeFirstPartySchemas(g *schemaGen, document *Document) {
	if document.Components != nil {
		for name, schema := range document.Components.Schemas {
			closeFirstPartySchema(g, &schema)
			document.Components.Schemas[name] = schema
		}
	}
	for path, item := range document.Paths {
		for method, operation := range item {
			// An operation an external authority owns keeps the schemas the
			// standard publishes. Components stay closed: they are shared by
			// the whole document, first-party operations included.
			if operation.ExternalContract != "" {
				continue
			}
			for i := range operation.Parameters {
				closeFirstPartySchema(g, operation.Parameters[i].Schema)
			}
			if operation.RequestBody != nil {
				for mediaType, content := range operation.RequestBody.Content {
					closeFirstPartySchema(g, content.Schema)
					operation.RequestBody.Content[mediaType] = content
				}
			}
			for status, response := range operation.Responses {
				for mediaType, content := range response.Content {
					closeFirstPartySchema(g, content.Schema)
					response.Content[mediaType] = content
				}
				operation.Responses[status] = response
			}
			item[method] = operation
		}
		document.Paths[path] = item
	}
}

func closeFirstPartySchema(g *schemaGen, schema *SchemaObject) {
	if schema == nil || schema.Ref != "" {
		return
	}
	for name, property := range schema.Properties {
		closeFirstPartySchema(g, &property)
		schema.Properties[name] = property
	}
	closeFirstPartySchema(g, schema.Items)
	for i := range schema.OneOf {
		closeFirstPartySchema(g, &schema.OneOf[i])
	}
	for i := range schema.AnyOf {
		closeFirstPartySchema(g, &schema.AnyOf[i])
	}
	if schema.AdditionalProperties != nil && schema.AdditionalProperties.Schema != nil {
		closeFirstPartySchema(g, schema.AdditionalProperties.Schema)
	}
	if schema.Type == "object" && schema.AdditionalProperties == nil {
		schema.AdditionalProperties = additionalPropertiesForbidden()
	}
	boundFirstPartyInteger(g, schema)
}

// integerFormatRange is the exact inclusive range of every integer width the
// first-party contract accepts. The bounds travel as decimal text so uint64 and
// int64 extremes survive a reader that has no 64-bit float (D0.2).
var integerFormatRange = map[string][2]string{
	"int32":  {"-2147483648", "2147483647"},
	"int64":  {"-9223372036854775808", "9223372036854775807"},
	"uint32": {"0", "4294967295"},
	"uint64": {"0", "18446744073709551615"},
}

// boundFirstPartyInteger states an integer's width twice: as the declared
// format, and as explicit minimum/maximum. A reader that ignores format still
// sees the range, and an author bound narrower than the natural range wins.
// An integer with no declared format is a generation failure, never a silently
// inferred int64.
func boundFirstPartyInteger(g *schemaGen, schema *SchemaObject) {
	if schema.Type != "integer" {
		return
	}
	bounds, known := integerFormatRange[schema.Format]
	if !known {
		if g.generationErr == nil {
			g.generationErr = fmt.Errorf(
				"openapi: integer schema declares format %q; first-party integers declare int32, int64, uint32 or uint64", schema.Format)
		}
		return
	}
	schema.Minimum = narrowerIntegerBound(schema.Minimum, bounds[0], true)
	schema.Maximum = narrowerIntegerBound(schema.Maximum, bounds[1], false)
}

// narrowerIntegerBound keeps the tighter of an author bound and the format's
// natural bound. Comparison is exact decimal: big.Int, not float64, because
// int64 and uint64 extremes are not representable as a float.
func narrowerIntegerBound(authored *json.Number, natural string, lower bool) *json.Number {
	naturalValue := json.Number(natural)
	if authored == nil {
		return &naturalValue
	}
	authoredInt, okAuthored := new(big.Int).SetString(strings.TrimSpace(authored.String()), 10)
	naturalInt, okNatural := new(big.Int).SetString(natural, 10)
	if !okAuthored || !okNatural {
		return authored
	}
	comparison := authoredInt.Cmp(naturalInt)
	if (lower && comparison > 0) || (!lower && comparison < 0) {
		return authored
	}
	return &naturalValue
}

// JSON returns the spec serialized as canonical, indented JSON with a trailing
// newline. Re-decoding through an untyped object makes encoding/json order every
// object key, while the semantically meaningful arrays already normalized by
// GenerateSpec retain their declared order.
func (d *Document) JSON() ([]byte, error) {
	encoded, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}

	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var document map[string]any
	if err := decoder.Decode(&document); err != nil {
		return nil, err
	}
	canonical, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(canonical, '\n'), nil
}

// --- Internal helpers ---

func sortedRoutes(routes []DiscoveredRoute) []DiscoveredRoute {
	out := append([]DiscoveredRoute(nil), routes...)
	sort.SliceStable(out, func(i, j int) bool {
		return routeSortKey(out[i]) < routeSortKey(out[j])
	})
	return out
}

func routeSortKey(route DiscoveredRoute) string {
	return strings.Join([]string{
		frameworkPathToOpenAPI(route.Path),
		strings.ToLower(route.Method),
		route.Description,
		strings.Join(route.Tags, ","),
		typeSortKey(route.Params),
		typeSortKey(route.Query),
		typeSortKey(route.Body),
		typeSortKey(route.Returns),
		errorCodesSortKey(route.ErrorCodes),
		route.StreamMode,
	}, "\x00")
}

func errorCodesSortKey(codes []perrors.Code) string {
	if len(codes) == 0 {
		return ""
	}
	parts := make([]string, 0, len(codes))
	for _, code := range codes {
		parts = append(parts, code.String())
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func typeSortKey(t reflect.Type) string {
	if t == nil {
		return ""
	}
	return t.PkgPath() + "." + t.String()
}

func frameworkPathToOpenAPI(path string) string {
	// Convert /users/{id} to /users/{id} (already in OpenAPI format)
	// Also handle /users/[id] → /users/{id}
	result := strings.ReplaceAll(path, "[", "{")
	result = strings.ReplaceAll(result, "]", "}")
	// Strip the multi-segment marker from catch-all params ({name...} → {name}).
	// OpenAPI 3.0 has no native multi-segment path-param syntax — the parameter is
	// rendered as a normal string and the multi-segment semantics are documented in
	// the parameter description (see buildPathParameters).
	result = catchAllTokenRE.ReplaceAllString(result, "{$1}")
	return result
}

func buildOperation(g *schemaGen, route DiscoveredRoute, secReqs []SecurityReq, firstParty bool) Operation {
	op := Operation{
		OperationID: generateOperationID(route.Method, route.Path),
		Description: route.Description,
		Tags:        route.Tags,
		Responses:   make(map[string]Response),
	}

	op.Parameters = append(op.Parameters, buildPathParameters(g, route)...)
	op.Parameters = append(op.Parameters, buildQueryParameters(g, route)...)
	if route.StreamMode != "" {
		buildStreamOperation(g, route, &op, firstParty)
	} else {
		op.RequestBody = buildRequestBody(g, route)
		if firstParty && op.RequestBody != nil && bodylessRequestMethod(route.Method) && g.generationErr == nil {
			// A first-party contract is what generated clients send. Publishing a
			// GET or HEAD request body would make every client send a payload the
			// endpoint pipeline never reads and intermediaries may drop.
			g.generationErr = fmt.Errorf(
				"openapi: %s %s declares a request body and %s carries none; move the input to query or path parameters",
				route.Method, route.Path, strings.ToUpper(route.Method))
		}
		buildResponses(g, route, op.Responses, firstParty)
	}
	buildSecurity(route, &op, secReqs)
	sortParameters(op.Parameters)

	return op
}

func buildClientOperation(g *schemaGen, route DiscoveredRoute, document *clientcontract.DocumentV1, connect connectProjection) *clientcontract.OperationV1 {
	idempotency := clientIdempotency(route)
	operation := &clientcontract.OperationV1{
		Stream:      clientStreamMode(route.StreamMode),
		Messages:    clientMessageShapes(g, route),
		Transports:  clientTransportsFor(g, route, connect, idempotency),
		Security:    clientSecurity(g, route, document),
		Errors:      clientErrors(g, route),
		Idempotency: idempotency,
	}
	if route.ClientOptions != nil {
		operation.Resilience = cloneClientResilience(route.ClientOptions.Resilience)
	}
	return operation
}

func clientMessageShapes(g *schemaGen, route DiscoveredRoute) *clientcontract.MessageShapes {
	schema := func(value reflect.Type) *clientcontract.Schema {
		if value == nil {
			return nil
		}
		return schemaObjectToClient(g.schema(value))
	}
	if route.ProviderWire != nil && route.ProviderWire.Bytes {
		// Raw octets have no schema: a byte stream declares no messages.
		return nil
	}
	switch route.StreamMode {
	case "server":
		return &clientcontract.MessageShapes{Output: schema(route.Returns)}
	case "client", "bidirectional":
		return &clientcontract.MessageShapes{Input: schema(route.Body), Output: schema(route.Returns)}
	default:
		return nil
	}
}

func clientStreamMode(mode string) clientcontract.StreamMode {
	switch mode {
	case "server":
		return clientcontract.StreamServer
	case "client":
		return clientcontract.StreamClient
	case "bidirectional":
		return clientcontract.StreamBidirectional
	default:
		return clientcontract.StreamUnary
	}
}

// clientTransports publishes only transports this provider's own server can
// honor. go/framework/http/websocket.go now negotiates the first-party
// subprotocol and reassembles continuation frames, and go/framework/api drives
// the published admission state machine over it, so a stream route publishes
// the websocket transport again. A server stream declares SSE first and
// WebSocket second: that declared order is the dispatch order a generated
// client follows.
func clientTransports(route DiscoveredRoute, connect connectProjection) []clientcontract.Transport {
	path := frameworkPathToOpenAPI(route.Path)
	if wire := route.ProviderWire; wire != nil {
		// A provider-owned wire is the route's only wire: the bound server
		// negotiates its declared token and nothing else, so no other transport
		// can be published beside it.
		encoding := clientcontract.EncodingJSON
		if wire.Bytes {
			encoding = clientcontract.EncodingBinary
		}
		return []clientcontract.Transport{{
			Protocol: clientcontract.TransportWebSocket, Path: path, Encoding: encoding,
			WebSocket: &clientcontract.WebSocketTransport{
				Subprotocol: wire.Subprotocol, Wire: clientcontract.WebSocketWireProvider,
			},
		}}
	}
	webSocket := clientcontract.Transport{
		Protocol: clientcontract.TransportWebSocket, Path: path, Encoding: clientcontract.EncodingJSON,
		WebSocket: &clientcontract.WebSocketTransport{Subprotocol: clientcontract.WebSocketSubprotocolV1},
	}
	switch route.StreamMode {
	case "":
		// REST first: it is the endpoint's own URL, and the Connect bridge
		// re-enters the same pipeline through one more hop. Connect follows as
		// the declared alternative for a consumer that wants one transport
		// family across a whole provider.
		return append([]clientcontract.Transport{{
			Protocol: clientcontract.TransportRESTJSON,
			Path:     path,
			Encoding: clientcontract.EncodingJSON,
		}}, connect.transportsFor(route)...)
	case "server":
		// SSE first, WebSocket second, Connect last: that declared order is the
		// dispatch order a generated client follows, and it keeps the transport
		// every browser already speaks in front.
		return append([]clientcontract.Transport{
			{Protocol: clientcontract.TransportSSE, Path: path, Encoding: clientcontract.EncodingJSON},
			webSocket,
		}, connect.transportsFor(route)...)
	case "client", "bidirectional":
		return []clientcontract.Transport{webSocket}
	default:
		// A stream shape this projection does not know has no transport. Falling
		// back to REST JSON here would publish a unary transport for a stream and
		// hand a generated client a call the provider never serves.
		return nil
	}
}

// clientTransportsFor resolves the transports of one route, applies the
// operation's declared preference order and its declared resume capability, and
// records why a route can publish none. The document then fails to serialize
// with a named route instead of publishing an operation contract no client
// could satisfy.
func clientTransportsFor(g *schemaGen, route DiscoveredRoute, connect connectProjection,
	idempotency clientcontract.Idempotency) []clientcontract.Transport {
	transports := clientTransports(route, connect)
	if len(transports) == 0 {
		if g.generationErr == nil {
			g.generationErr = fmt.Errorf(
				"openapi: %s %s declares a %s shape and this provider has no transport that can carry it",
				route.Method, route.Path, routeShapeName(route))
		}
		return transports
	}
	ordered, err := orderDeclaredTransports(route, transports)
	if err == nil {
		ordered, err = orderDeclaredConnectEncodings(route, ordered)
	}
	if err != nil {
		if g.generationErr == nil {
			g.generationErr = err
		}
		return nil
	}
	if err := applyDeclaredResume(route, ordered, idempotency); err != nil {
		if g.generationErr == nil {
			g.generationErr = err
		}
		return nil
	}
	if err := applyDeclaredSSEContinuation(route, ordered, idempotency); err != nil {
		if g.generationErr == nil {
			g.generationErr = err
		}
		return nil
	}
	return ordered
}

// orderDeclaredTransports narrows and reorders the transports this provider can
// carry to the order the operation declared.
//
// The declaration can drop and reorder transports; it cannot add one.
func orderDeclaredTransports(route DiscoveredRoute, available []clientcontract.Transport) ([]clientcontract.Transport, error) {
	if route.ClientOptions == nil || len(route.ClientOptions.Transports) == 0 {
		return available, nil
	}
	ordered := make([]clientcontract.Transport, 0, len(available))
	seen := make(map[clientcontract.TransportProtocol]bool, len(route.ClientOptions.Transports))
	for _, protocol := range route.ClientOptions.Transports {
		if seen[protocol] {
			return nil, fmt.Errorf("openapi: %s %s declares transport %q twice in its client transport order",
				route.Method, route.Path, protocol)
		}
		seen[protocol] = true
		matched := false
		for i := range available {
			if available[i].Protocol != protocol {
				continue
			}
			ordered = append(ordered, available[i])
			matched = true
		}
		if !matched {
			return nil, fmt.Errorf(
				"openapi: %s %s declares transport %q, which this provider does not serve for a %s shape",
				route.Method, route.Path, protocol, routeShapeName(route))
		}
	}
	return ordered, nil
}

// orderDeclaredConnectEncodings narrows and reorders the Connect entries of an
// operation's published transports to the encoding order it declared.
//
// The contract carries one Connect entry per encoding, and a generated client
// dispatches the first one it can carry, so without this declaration the
// bridge's own order decides and the second encoding is never dispatched. The
// declared entries take the place of the first Connect entry; the transports
// around them keep the order the operation already declared. As with the
// transport order, a declaration can drop an encoding and can never add one
// the mounted bridge does not serve.
func orderDeclaredConnectEncodings(route DiscoveredRoute, transports []clientcontract.Transport) ([]clientcontract.Transport, error) {
	if route.ClientOptions == nil || len(route.ClientOptions.ConnectEncodings) == 0 {
		return transports, nil
	}
	served := make(map[clientcontract.Encoding]clientcontract.Transport)
	for _, transport := range transports {
		if transport.Protocol == clientcontract.TransportConnect {
			served[transport.Encoding] = transport
		}
	}
	if len(served) == 0 {
		return nil, fmt.Errorf("openapi: %s %s declares a Connect encoding order and publishes no Connect transport",
			route.Method, route.Path)
	}
	declared := make([]clientcontract.Transport, 0, len(route.ClientOptions.ConnectEncodings))
	seen := make(map[clientcontract.Encoding]bool, len(route.ClientOptions.ConnectEncodings))
	for _, encoding := range route.ClientOptions.ConnectEncodings {
		if seen[encoding] {
			return nil, fmt.Errorf("openapi: %s %s declares Connect encoding %q twice in its client encoding order",
				route.Method, route.Path, encoding)
		}
		seen[encoding] = true
		transport, ok := served[encoding]
		if !ok {
			return nil, fmt.Errorf("openapi: %s %s declares Connect encoding %q, which the mounted bridge does not serve",
				route.Method, route.Path, encoding)
		}
		declared = append(declared, transport)
	}
	ordered := make([]clientcontract.Transport, 0, len(transports))
	for _, transport := range transports {
		if transport.Protocol != clientcontract.TransportConnect {
			ordered = append(ordered, transport)
			continue
		}
		ordered = append(ordered, declared...)
		declared = nil
	}
	return ordered, nil
}

// applyDeclaredResume marks the operation's WebSocket transport resume-capable.
//
// Resume continues a stream after the last sequence the consumer completely
// delivered, so it is only sound where re-reading the same position has no
// effect: a server stream, declared safe, carried by the first-party WebSocket
// wire. Every other shape is refused here rather than published as a capability
// no provider could honor without a gap or a duplicate.
func applyDeclaredResume(route DiscoveredRoute, transports []clientcontract.Transport,
	idempotency clientcontract.Idempotency) error {
	if route.ClientOptions == nil || !route.ClientOptions.Resume {
		return nil
	}
	if route.StreamMode != "server" {
		return fmt.Errorf("openapi: %s %s declares stream resume on a %s; only a server stream can be resumed",
			route.Method, route.Path, routeShapeName(route))
	}
	if idempotency.Kind != clientcontract.IdempotencySafe {
		return fmt.Errorf("openapi: %s %s declares stream resume with idempotency %q; only a safe stream can be resumed",
			route.Method, route.Path, idempotency.Kind)
	}
	marked := false
	for i := range transports {
		if transports[i].Protocol != clientcontract.TransportWebSocket || transports[i].WebSocket == nil {
			continue
		}
		websocket := *transports[i].WebSocket
		websocket.Resume = true
		transports[i].WebSocket = &websocket
		marked = true
	}
	if !marked {
		return fmt.Errorf("openapi: %s %s declares stream resume and publishes no websocket transport to carry it",
			route.Method, route.Path)
	}
	return nil
}

// applyDeclaredSSEContinuation publishes the operation's declared continuation
// on its SSE transport (clientcontract ADR 0013).
//
// A reopened connection reads the stream again from a position, so only a
// safe server stream carried by SSE can declare one. Every other shape, and a
// declaration the strict readers would refuse, fails here with the route
// named. What a cursor names is checked against the route's own schemas by
// validateDeclaredSSECursor.
func applyDeclaredSSEContinuation(route DiscoveredRoute, transports []clientcontract.Transport,
	idempotency clientcontract.Idempotency) error {
	if route.ClientOptions == nil || route.ClientOptions.SSEContinuation == nil {
		return nil
	}
	continuation := route.ClientOptions.SSEContinuation
	if route.StreamMode != "server" {
		return fmt.Errorf("openapi: %s %s declares an sse continuation on a %s; only a server stream can be continued",
			route.Method, route.Path, routeShapeName(route))
	}
	if idempotency.Kind != clientcontract.IdempotencySafe {
		return fmt.Errorf("openapi: %s %s declares an sse continuation with idempotency %q; only a safe stream can be continued",
			route.Method, route.Path, idempotency.Kind)
	}
	switch continuation.Mode {
	case clientcontract.SSEContinuationCursor:
		if continuation.Cursor == nil || strings.TrimSpace(continuation.Cursor.OutputField) == "" ||
			strings.TrimSpace(continuation.Cursor.QueryParameter) == "" {
			return fmt.Errorf("openapi: %s %s declares a cursor continuation that does not name both its output field and its query parameter",
				route.Method, route.Path)
		}
	case clientcontract.SSEContinuationBestEffort:
		if continuation.Cursor != nil {
			return fmt.Errorf("openapi: %s %s declares a best-effort continuation with a cursor; best-effort reopens the original selector and carries no position",
				route.Method, route.Path)
		}
	default:
		return fmt.Errorf("openapi: %s %s declares sse continuation mode %q; the modes are %q and %q",
			route.Method, route.Path, continuation.Mode, clientcontract.SSEContinuationCursor, clientcontract.SSEContinuationBestEffort)
	}
	marked := false
	for i := range transports {
		if transports[i].Protocol != clientcontract.TransportSSE {
			continue
		}
		declared := *continuation
		declared.Cursor = cloneValue(continuation.Cursor)
		transports[i].SSE = &clientcontract.SSETransport{Continuation: &declared}
		marked = true
	}
	if !marked {
		return fmt.Errorf("openapi: %s %s declares an sse continuation and publishes no sse transport to carry it",
			route.Method, route.Path)
	}
	return nil
}

// validateDeclaredSSECursor checks what a cursor continuation names against the
// route's own output message and query parameters, with the rule every strict
// reader applies (clientcontract.ValidateSSEContinuationReferences): the
// output field is a required plain-string property and the query parameter a
// declared plain-string one. The provider refuses the declaration with the
// route named instead of publishing a contract no reader accepts.
func validateDeclaredSSECursor(g *schemaGen, route DiscoveredRoute, operation *clientcontract.OperationV1, parameters []Parameter) {
	if g.generationErr != nil || operation == nil || route.ClientOptions == nil ||
		route.ClientOptions.SSEContinuation == nil || route.ClientOptions.SSEContinuation.Mode != clientcontract.SSEContinuationCursor {
		return
	}
	query := map[string]clientcontract.Schema{}
	for _, parameter := range parameters {
		if parameter.In == "query" && parameter.Schema != nil {
			query[parameter.Name] = *schemaObjectToClient(parameter.Schema)
		}
	}
	components := make(map[string]clientcontract.Schema, len(g.components))
	for name, schema := range g.components {
		components[name] = *schemaObjectToClient(&schema)
	}
	if diags := clientcontract.ValidateSSEContinuationReferences(operation, query, components); len(diags) > 0 {
		g.generationErr = fmt.Errorf("openapi: %s %s declares a cursor continuation its schemas cannot carry: %s",
			route.Method, route.Path, diags[0].String())
	}
}

// routeShapeName names the declared shape of a route for a refusal message.
func routeShapeName(route DiscoveredRoute) string {
	if route.StreamMode == "" {
		return "unary"
	}
	return route.StreamMode + " stream"
}

// clientSecurity projects the credential alternatives a generated client may
// present for one route. The alternatives never offer less than the route's own
// rule demands: an anonymous alternative requires a rule that serves a request
// presenting no credential (SecurityMeta.Optional), and it comes last, because
// a client takes the first alternative its binding satisfies and an empty one
// is always satisfied. An empty Security fails strict validation; the refusals
// this function can explain record their reason on the generator first.
func clientSecurity(g *schemaGen, route DiscoveredRoute, document *clientcontract.DocumentV1) clientcontract.Security {
	anonymous := clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{}}}}
	if route.Security == nil {
		if route.ClientOptions == nil || len(route.ClientOptions.Security.Alternatives) == 0 {
			return anonymous
		}
		// Advertising credentials for an endpoint that the server left anonymous
		// would make the generated client contract disagree with its provider.
		refuseClientSecurity(g, route, "declares client security alternatives and the route has no security rule; "+
			"secure the route, with a rule that reports OptionalAuthentication() true when it also serves anonymous callers")
		return clientcontract.Security{}
	}
	if !route.Security.Representable {
		return clientcontract.Security{}
	}
	if route.ClientOptions != nil && len(route.ClientOptions.Security.Alternatives) > 0 {
		security := cloneClientSecurity(route.ClientOptions.Security)
		security.Authorization = cloneClientAuthorization(route.Security.Authorization)
		last := len(security.Alternatives) - 1
		for i, alternative := range security.Alternatives {
			if len(alternative.AllOf) == 0 {
				if !route.Security.Optional {
					// A route that requires authentication cannot have an anonymous alternative.
					refuseClientSecurity(g, route, "declares an anonymous security alternative and its security rule requires authentication; "+
						"only a rule that reports OptionalAuthentication() true serves anonymous callers")
					return clientcontract.Security{}
				}
				if i != last {
					refuseClientSecurity(g, route, "declares the anonymous security alternative before a credentialed one; "+
						"a client takes the first alternative it satisfies and an empty one always is, so it must come last")
					return clientcontract.Security{}
				}
				// The rule's claims bind an authenticated caller only: the
				// anonymous alternative stays empty.
				continue
			}
			for j := range alternative.AllOf {
				security.Alternatives[i].AllOf[j].Scopes = mergeClientStrings(
					security.Alternatives[i].AllOf[j].Scopes, route.Security.Scopes)
				security.Alternatives[i].AllOf[j].Roles = mergeClientStrings(
					security.Alternatives[i].AllOf[j].Roles, route.Security.Roles)
			}
		}
		return security
	}
	if document == nil || len(document.Credentials) != 1 {
		// More than one profile requires the provider to choose explicit OR/AND
		// semantics with Endpoint.Client; guessing would weaken authentication.
		// An optional rule is no exception: which credential a client presents
		// when it holds several is the same guess.
		return clientcontract.Security{}
	}
	for profile := range document.Credentials {
		security := clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{
			AllOf: []clientcontract.SecurityRequirement{{
				Profile: profile,
				Scopes:  append([]string(nil), route.Security.Scopes...),
				Roles:   append([]string(nil), route.Security.Roles...),
			}},
		}}, Authorization: cloneClientAuthorization(route.Security.Authorization)}
		if route.Security.Optional {
			// The one credential when the binding holds it, anonymous otherwise.
			security.Alternatives = append(security.Alternatives, clientcontract.SecurityAlternative{AllOf: []clientcontract.SecurityRequirement{}})
		}
		return security
	}
	return clientcontract.Security{}
}

// refuseClientSecurity records why a route's client security cannot be
// published, unless an earlier route already failed generation.
func refuseClientSecurity(g *schemaGen, route DiscoveredRoute, reason string) {
	if g != nil && g.generationErr == nil {
		g.generationErr = fmt.Errorf("openapi: %s %s %s (%s)",
			route.Method, route.Path, reason, clientcontract.ErrorCodeInvalidSecurity)
	}
}

func cloneClientSecurity(in clientcontract.Security) clientcontract.Security {
	out := clientcontract.Security{
		Alternatives:  make([]clientcontract.SecurityAlternative, len(in.Alternatives)),
		Authorization: cloneClientAuthorization(in.Authorization),
	}
	for i, alternative := range in.Alternatives {
		out.Alternatives[i].AllOf = make([]clientcontract.SecurityRequirement, len(alternative.AllOf))
		for j, requirement := range alternative.AllOf {
			copy := requirement
			copy.Scopes = append([]string(nil), requirement.Scopes...)
			copy.Roles = append([]string(nil), requirement.Roles...)
			out.Alternatives[i].AllOf[j] = copy
		}
	}
	return out
}

func cloneClientAuthorization(in *clientcontract.Authorization) *clientcontract.Authorization {
	if in == nil {
		return nil
	}
	out := *in
	out.Issuers = append([]string(nil), in.Issuers...)
	out.Audiences = append([]string(nil), in.Audiences...)
	out.PrincipalKinds = append([]string(nil), in.PrincipalKinds...)
	out.Clients = append([]string(nil), in.Clients...)
	out.ScopesAll = append([]string(nil), in.ScopesAll...)
	out.ScopesAny = append([]string(nil), in.ScopesAny...)
	out.RolesAll = append([]string(nil), in.RolesAll...)
	out.RolesAny = append([]string(nil), in.RolesAny...)
	out.ScopeClaims = append([]string(nil), in.ScopeClaims...)
	out.RoleClaims = append([]string(nil), in.RoleClaims...)
	return &out
}

func mergeClientStrings(authored, required []string) []string {
	out := append([]string(nil), authored...)
	seen := make(map[string]bool, len(out))
	for _, value := range out {
		seen[value] = true
	}
	for _, value := range required {
		if !seen[value] {
			out = append(out, value)
			seen[value] = true
		}
	}
	return out
}

func clientIdempotency(route DiscoveredRoute) clientcontract.Idempotency {
	if route.ClientOptions != nil && route.ClientOptions.Idempotency != nil {
		return *route.ClientOptions.Idempotency
	}
	switch strings.ToUpper(route.Method) {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe}
	case http.MethodPut, http.MethodDelete:
		return clientcontract.Idempotency{Kind: clientcontract.IdempotencyIdempotent}
	default:
		return clientcontract.Idempotency{Kind: clientcontract.IdempotencyNonIdempotent}
	}
}

// clientErrors projects the errors a first-party operation declares.
//
// ADR 0006: `errors[].schema` describes the `details` member of the error
// envelope, never the envelope itself. `{code, error, message, details?}` is
// what every first-party endpoint writes (ADR 0003), so re-declaring it per
// error would tell a generated client to validate that shape against `details`
// — and a client reading a well-formed error would reject it. An error with no
// declared details body therefore carries no schema at all; one declared with
// MayThrowDetails carries the schema of that details type and nothing more.
func clientErrors(g *schemaGen, route DiscoveredRoute) []clientcontract.DeclaredError {
	errorsByKey := map[string]clientcontract.DeclaredError{}
	add := func(status int, code string, retryable *bool, details *clientcontract.Schema) {
		key := strconv.Itoa(status) + "\x00" + code
		if existing, ok := errorsByKey[key]; ok {
			if retryable != nil {
				existing.Retryable = cloneValue(retryable)
			}
			if details != nil {
				existing.Schema = details
			}
			errorsByKey[key] = existing
			return
		}
		errorsByKey[key] = clientcontract.DeclaredError{Status: status, Code: code, Retryable: cloneValue(retryable), Schema: details}
	}

	// Request decoding/validation and the framework's sanitized catch-all are
	// transport facts for every bound endpoint, even when handlers declare no
	// additional domain errors. They carry the envelope and nothing else, so
	// they declare no details schema.
	add(http.StatusBadRequest, string(perrors.CodeBadRequest), nil, nil)
	add(http.StatusInternalServerError, string(perrors.CodeInternalServer), nil, nil)
	// A declared binary body adds exactly two refusals, and they are declared
	// rather than implicit: an endpoint that bounds its payload must publish
	// what it answers when the bound or the media type is violated, otherwise a
	// generated client reads its own refusal as an unattributed remote failure.
	if route.BodyBinary != nil {
		add(http.StatusRequestEntityTooLarge, string(perrors.CodePayloadTooLarge), boolPointer(false), nil)
		add(http.StatusUnsupportedMediaType, string(perrors.CodeUnsupportedMediaType), boolPointer(false), nil)
	}
	// Sorted codes register each details type in the same order whatever the
	// declaration order, so a component name two types would share is settled
	// the same way every time.
	for _, code := range sortedErrorCodes(route.ErrorCodes) {
		var retryable *bool
		if value, ok := route.ErrorRetryability[code]; ok {
			retryable = &value
		}
		var details *clientcontract.Schema
		if detailsType := route.ErrorDetails[code]; detailsType != nil {
			details = schemaObjectToClient(g.schema(detailsType))
		}
		add(declaredErrorStatus(code), string(code), retryable, details)
	}

	for _, declared := range sortedResponseMeta(route.Throws) {
		matches := 0
		for _, existing := range errorsByKey {
			if existing.Status == declared.Status {
				matches++
			}
		}
		// One declared code at this status or several, the implicit 400 and 500
		// pair included: the codes discriminate the error a client receives
		// there, and Throws only documents the status. Several codes on one
		// status are valid: a generated client selects a declared error by
		// code, not by status (ADR 0003). The TypeScript projection applies the
		// same rule.
		//
		// A Throws schema describes the whole error response body, which it keeps
		// documenting in `responses`. It is not a details schema, so it does not
		// become one here: MayThrowDetails is the declaration of a details body,
		// and inventing one from this schema would republish the defect ADR 0006
		// closes, one level down.
		if matches > 0 {
			continue
		}
		// No declared code shares this status, so Throws(status, ...) has no
		// stable wire error code. Record the status so strict first-party
		// validation fails explicitly on the empty code instead of letting an
		// undiscriminated error reach a generated client.
		add(declared.Status, "", nil, nil)
	}

	keys := make([]string, 0, len(errorsByKey))
	for key := range errorsByKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]clientcontract.DeclaredError, 0, len(keys))
	for _, key := range keys {
		out = append(out, errorsByKey[key])
	}
	return out
}

// sortedErrorCodes returns the declared codes in byte order, each once. A code
// declared twice — MayThrowDetails beside MayThrowWith — is one error.
func sortedErrorCodes(codes []perrors.Code) []perrors.Code {
	out := slices.Clone(codes)
	slices.Sort(out)
	return slices.Compact(out)
}

// declaredErrorStatus is the HTTP status a declared code answers with. It
// reads the table the provider itself writes errors with
// (errors.HTTPStatus), so the published status and the answered one cannot
// disagree; an unregistered code answers 500 on both sides.
func declaredErrorStatus(code perrors.Code) int {
	if status, ok := perrors.HTTPStatusForCode(code); ok {
		return status
	}
	return http.StatusInternalServerError
}

// boolPointer names a declared retry classification. A refusal the provider
// raises before it reads the body never becomes retryable: the same request
// would be refused again.
func boolPointer(value bool) *bool { return &value }

func schemaObjectToClient(schema *SchemaObject) *clientcontract.Schema {
	if schema == nil {
		return nil
	}
	out := &clientcontract.Schema{
		Type:             schema.Type,
		Format:           schema.Format,
		Nullable:         cloneValue(schema.Nullable),
		Ref:              schema.Ref,
		Required:         append([]string(nil), schema.Required...),
		Title:            "",
		Description:      schema.Description,
		Default:          append(json.RawMessage(nil), schema.Default...),
		Minimum:          cloneValue(schema.Minimum),
		Maximum:          cloneValue(schema.Maximum),
		ExclusiveMinimum: cloneValue(schema.ExclusiveMinimum),
		ExclusiveMaximum: cloneValue(schema.ExclusiveMaximum),
		MinLength:        cloneValue(schema.MinLength),
		MaxLength:        cloneValue(schema.MaxLength),
		Pattern:          schema.Pattern,
		MinItems:         cloneValue(schema.MinItems),
		MaxItems:         cloneValue(schema.MaxItems),
		UniqueItems:      cloneValue(schema.UniqueItems),
		ReadOnly:         cloneValue(schema.ReadOnly),
		WriteOnly:        cloneValue(schema.WriteOnly),
		OpaqueJSON:       schema.OpaqueJSON,
	}
	if len(schema.Enum) > 0 {
		out.Enum = make([]json.RawMessage, len(schema.Enum))
		for i, value := range schema.Enum {
			out.Enum[i] = strconv.AppendQuote(nil, value)
		}
	}
	if len(schema.Properties) > 0 {
		out.Properties = make(map[string]clientcontract.Schema, len(schema.Properties))
		for name, property := range schema.Properties {
			out.Properties[name] = *schemaObjectToClient(&property)
		}
	}
	if schema.Items != nil {
		out.Items = schemaObjectToClient(schema.Items)
	}
	if schema.AdditionalProperties != nil {
		out.AdditionalProperties = &clientcontract.AdditionalProperties{
			Allowed: cloneValue(schema.AdditionalProperties.Allowed),
			Schema:  schemaObjectToClient(schema.AdditionalProperties.Schema),
		}
	}
	if len(schema.OneOf) > 0 {
		out.OneOf = make([]clientcontract.Schema, len(schema.OneOf))
		for i := range schema.OneOf {
			out.OneOf[i] = *schemaObjectToClient(&schema.OneOf[i])
		}
	}
	if schema.Discriminator != nil {
		out.Discriminator = &clientcontract.Discriminator{
			PropertyName: schema.Discriminator.PropertyName,
			Mapping:      make(map[string]string, len(schema.Discriminator.Mapping)),
		}
		for name, target := range schema.Discriminator.Mapping {
			out.Discriminator.Mapping[name] = target
		}
	}
	return out
}

func cloneValue[T any](value *T) *T {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneClientResilience(in *clientcontract.ResiliencePolicy) *clientcontract.ResiliencePolicy {
	if in == nil {
		return nil
	}
	out := *in
	out.TimeoutMs = cloneValue(in.TimeoutMs)
	out.AttemptTimeoutMs = cloneValue(in.AttemptTimeoutMs)
	out.MaxResponseBytes = cloneValue(in.MaxResponseBytes)
	if in.Retry != nil {
		retry := *in.Retry
		retry.MaxAttempts = cloneValue(in.Retry.MaxAttempts)
		retry.Statuses = append([]int(nil), in.Retry.Statuses...)
		retry.Codes = append([]string(nil), in.Retry.Codes...)
		out.Retry = &retry
	}
	if in.Circuit != nil {
		circuit := *in.Circuit
		circuit.FailureThreshold = cloneValue(in.Circuit.FailureThreshold)
		circuit.ResetTimeoutMs = cloneValue(in.Circuit.ResetTimeoutMs)
		out.Circuit = &circuit
	}
	if in.Stream != nil {
		stream := *in.Stream
		stream.IdleTimeoutMs = cloneValue(in.Stream.IdleTimeoutMs)
		stream.HeartbeatMs = cloneValue(in.Stream.HeartbeatMs)
		stream.Reconnect = cloneValue(in.Stream.Reconnect)
		stream.MaxBufferedMessages = cloneValue(in.Stream.MaxBufferedMessages)
		stream.MaxFrameBytes = cloneValue(in.Stream.MaxFrameBytes)
		out.Stream = &stream
	}
	if in.Cache != nil {
		cache := *in.Cache
		cache.StaleMs = cloneValue(in.Cache.StaleMs)
		cache.MaxEntries = cloneValue(in.Cache.MaxEntries)
		if in.Cache.KeyFields != nil {
			cache.KeyFields = append(make([]string, 0, len(in.Cache.KeyFields)), in.Cache.KeyFields...)
		}
		if in.Cache.InvalidationFields != nil {
			cache.InvalidationFields = append(make([]string, 0, len(in.Cache.InvalidationFields)), in.Cache.InvalidationFields...)
		}
		out.Cache = &cache
	}
	return &out
}

func buildStreamOperation(g *schemaGen, route DiscoveredRoute, op *Operation, firstParty bool) {
	protocols := "WebSocket"
	if route.StreamMode == "server" {
		protocols = "WebSocket, SSE"
	}
	desc := fmt.Sprintf("Stream endpoint (%s). %s", protocols, streamModeDescription(route.StreamMode))
	if wire := route.ProviderWire; wire != nil {
		desc = "Stream endpoint (WebSocket). " + providerWireDescription(*wire)
	}
	if route.Description != "" {
		op.Description = route.Description + "\n\n" + desc
	} else {
		op.Description = desc
	}

	if route.StreamMode == "server" {
		resp := Response{Description: "Server-Sent Events stream"}
		if route.Returns != nil {
			resp.Content = map[string]MediaType{
				"text/event-stream": {Schema: g.schema(route.Returns)},
			}
		}
		op.Responses["200"] = resp
	}
	op.Responses["101"] = Response{Description: "WebSocket upgrade"}
	addStandardErrorResponses(g, route, op.Responses, firstParty)
	addExplicitThrowResponses(g, route, op.Responses)
}

// providerWireDescription documents a provider-owned wire for a reader of the
// OpenAPI document who does not read x-putnami-client.
func providerWireDescription(wire api.ProviderWireMeta) string {
	negotiation := "Connect via WebSocket upgrade"
	if wire.Subprotocol != "" {
		negotiation += fmt.Sprintf(" with subprotocol %s", wire.Subprotocol)
	}
	if wire.Bytes {
		return negotiation + ". Raw octets in binary messages, both directions."
	}
	return negotiation + ". One JSON value per text message, both directions."
}

func streamModeDescription(mode string) string {
	switch mode {
	case "server":
		return "Connect via WebSocket upgrade or Accept: text/event-stream."
	case "client":
		return "Connect via WebSocket upgrade. Client sends messages, server returns a final response."
	default:
		return "Connect via WebSocket upgrade. Bidirectional message exchange."
	}
}

func buildPathParameters(g *schemaGen, route DiscoveredRoute) []Parameter {
	names := extractPathParams(route.Path)
	params := make([]Parameter, 0, len(names))
	for _, name := range names {
		p := Parameter{
			Name:     name,
			In:       "path",
			Required: true,
			Schema:   &SchemaObject{Type: "string"},
		}
		if route.Params != nil {
			if field, ok := findStructField(route.Params, name); ok {
				p.Schema = g.schema(field.Type)
				applyValidateConstraints(p.Schema, field.Tag.Get("validate"))
				if desc := field.Tag.Get("description"); desc != "" {
					p.Description = desc
				}
			}
		}
		if isCatchAllParam(route.Path, name) {
			// OpenAPI 3.0 has no native multi-segment path-param syntax. Document
			// the multi-segment semantics in the description so generated clients
			// and consumers know slashes are accepted in this value.
			note := "Multi-segment path parameter — captures one or more slash-separated segments."
			if p.Description == "" {
				p.Description = note
			} else {
				p.Description = p.Description + "\n\n" + note
			}
		}
		params = append(params, p)
	}
	return params
}

func buildQueryParameters(g *schemaGen, route DiscoveredRoute) []Parameter {
	if route.Query == nil {
		return nil
	}
	t := route.Query
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	var params []Parameter
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		name := jsonFieldName(field)
		if name == "" {
			continue // json:"-" — not part of the wire shape
		}
		p := Parameter{
			Name:     name,
			In:       "query",
			Required: hasValidateTag(field, "required"),
			Schema:   g.schema(field.Type),
		}
		applyValidateConstraints(p.Schema, field.Tag.Get("validate"))
		if desc := field.Tag.Get("description"); desc != "" {
			p.Description = desc
		}
		params = append(params, p)
	}
	return params
}

func sortParameters(params []Parameter) {
	sort.SliceStable(params, func(i, j int) bool {
		if params[i].In != params[j].In {
			return parameterLocationRank(params[i].In) < parameterLocationRank(params[j].In)
		}
		if params[i].Name != params[j].Name {
			return params[i].Name < params[j].Name
		}
		return params[i].Description < params[j].Description
	})
}

func parameterLocationRank(loc string) int {
	switch loc {
	case "path":
		return 0
	case "query":
		return 1
	case "header":
		return 2
	case "cookie":
		return 3
	default:
		return 9
	}
}

func buildRequestBody(g *schemaGen, route DiscoveredRoute) *RequestBody {
	if route.Body == nil {
		return nil
	}
	if route.BodyBinary != nil {
		return &RequestBody{Required: true, Content: binaryContent(*route.BodyBinary)}
	}
	return &RequestBody{
		Required: true,
		Content: map[string]MediaType{
			"application/json": {Schema: g.schema(route.Body)},
		},
	}
}

// binaryContent projects one declared raw octet payload. The schema is the
// OpenAPI vocabulary for "these are octets" and nothing more; the byte bound
// travels beside it because no schema keyword can carry it.
func binaryContent(declared api.BinaryMeta) map[string]MediaType {
	maxBytes := declared.MaxBytes
	return map[string]MediaType{
		declared.MediaType: {
			Schema:   &SchemaObject{Type: "string", Format: "binary"},
			MaxBytes: &maxBytes,
			Streamed: declared.Streamed,
		},
	}
}

func buildResponses(g *schemaGen, route DiscoveredRoute, responses map[string]Response, firstParty bool) {
	primaryStatus := route.ReturnsStatus
	if primaryStatus == 0 {
		primaryStatus = http.StatusOK
	}
	primaryDescription := route.ReturnsDescription
	if primaryDescription == "" {
		primaryDescription = "Successful response"
	}
	primaryKey := strconv.Itoa(primaryStatus)
	switch {
	case route.ReturnsBinary != nil:
		responses[primaryKey] = Response{
			Description: primaryDescription,
			Content:     binaryContent(*route.ReturnsBinary),
		}
	case route.Returns != nil:
		responses[primaryKey] = Response{
			Description: primaryDescription,
			Content: map[string]MediaType{
				"application/json": {Schema: g.schema(route.Returns)},
			},
		}
	default:
		responses[primaryKey] = Response{Description: primaryDescription}
	}

	// Additional success responses (e.g. 201 Created, 204 No Content) declared via
	// api.Endpoint().Response(status, …). A matching status overrides the primary.
	for _, extra := range sortedResponseMeta(route.AdditionalReturns) {
		statusKey := strconv.Itoa(extra.Status)
		desc := extra.Description
		if desc == "" {
			desc = "Successful response"
		}
		resp := Response{Description: desc}
		if extra.Schema != nil {
			resp.Content = map[string]MediaType{
				"application/json": {Schema: g.schema(extra.Schema)},
			}
		}
		responses[statusKey] = resp
	}

	addStandardErrorResponses(g, route, responses, firstParty)
	addExplicitThrowResponses(g, route, responses)
}

func addStandardErrorResponses(g *schemaGen, route DiscoveredRoute, responses map[string]Response, firstParty bool) {
	details := errorDetailsByStatus(route)
	for _, status := range []int{http.StatusBadRequest, http.StatusInternalServerError} {
		addStandardErrorResponse(g, responses, status, firstParty, details[status])
	}
	if route.BodyBinary != nil {
		addStandardErrorResponse(g, responses, http.StatusRequestEntityTooLarge, firstParty, details[http.StatusRequestEntityTooLarge])
		addStandardErrorResponse(g, responses, http.StatusUnsupportedMediaType, firstParty, details[http.StatusUnsupportedMediaType])
	}

	statuses := make([]int, 0, len(route.ErrorCodes))
	for _, code := range route.ErrorCodes {
		statuses = append(statuses, declaredErrorStatus(code))
	}
	sort.Ints(statuses)
	last := 0
	for i, status := range statuses {
		if i > 0 && status == last {
			continue
		}
		addStandardErrorResponse(g, responses, status, firstParty, details[status])
		last = status
	}
}

// errorDetailsByStatus groups the declared details types by the status their
// code answers with. Each group follows the stable error codes in byte order
// and names a type once, at its first code, so the documented response does
// not depend on declaration or map order. The TypeScript projection orders its
// variants the same way.
func errorDetailsByStatus(route DiscoveredRoute) map[int][]reflect.Type {
	if len(route.ErrorDetails) == 0 {
		return nil
	}
	// Only declared codes carry details, exactly as in clientErrors, so the
	// document never names a details body the contract does not publish.
	byStatus := map[int][]reflect.Type{}
	for _, code := range sortedErrorCodes(route.ErrorCodes) {
		details := route.ErrorDetails[code]
		if details == nil {
			continue
		}
		status := declaredErrorStatus(code)
		if !slices.Contains(byStatus[status], details) {
			byStatus[status] = append(byStatus[status], details)
		}
	}
	return byStatus
}

// addStandardErrorResponse documents the framework envelope for one status.
// When codes answering with that status declare a details type, the envelope
// documents `details` as that type, or `anyOf` the types when several codes
// share the status.
func addStandardErrorResponse(g *schemaGen, responses map[string]Response, status int, firstParty bool, details []reflect.Type) {
	statusKey := strconv.Itoa(status)
	if _, exists := responses[statusKey]; exists {
		return
	}
	desc := http.StatusText(status)
	if desc == "" {
		desc = fmt.Sprintf("Error %d", status)
	}
	schema := standardErrorSchemaFor(firstParty)
	switch len(details) {
	case 0:
	case 1:
		schema.Properties["details"] = *g.schema(details[0])
	default:
		variants := make([]SchemaObject, 0, len(details))
		for _, detailsType := range details {
			variants = append(variants, *g.schema(detailsType))
		}
		schema.Properties["details"] = SchemaObject{AnyOf: variants}
	}
	responses[statusKey] = Response{
		Description: desc,
		Content: map[string]MediaType{
			"application/json": {Schema: schema},
		},
	}
}

func standardErrorSchema() *SchemaObject {
	return &SchemaObject{
		Type: "object",
		Properties: map[string]SchemaObject{
			"code":    {Type: "string"},
			"error":   {Type: "string"},
			"message": {Type: "string"},
			// details carries whatever the error attached: declared, never the
			// empty schema a projection writes when it lost a type.
			"details": *opaqueJSONSchema(),
		},
		Required: []string{"error", "message"},
	}
}

func strictStandardErrorSchema() *SchemaObject {
	return &SchemaObject{
		Type: "object",
		Properties: map[string]SchemaObject{
			"code":    {Type: "string"},
			"error":   {Type: "string"},
			"message": {Type: "string"},
		},
		Required:             []string{"code", "error", "message"},
		AdditionalProperties: additionalPropertiesForbidden(),
	}
}

func standardErrorSchemaFor(firstParty bool) *SchemaObject {
	if firstParty {
		return strictStandardErrorSchema()
	}
	return standardErrorSchema()
}

func addExplicitThrowResponses(g *schemaGen, route DiscoveredRoute, responses map[string]Response) {
	for _, throws := range sortedResponseMeta(route.Throws) {
		responses[fmt.Sprintf("%d", throws.Status)] = responseFromMeta(g, throws)
	}
}

func responseFromMeta(g *schemaGen, meta ThrowsMeta) Response {
	resp := Response{Description: meta.Description}
	if meta.Schema != nil {
		resp.Content = map[string]MediaType{
			"application/json": {Schema: g.schema(meta.Schema)},
		}
	}
	return resp
}

func sortedResponseMeta(in []ThrowsMeta) []ThrowsMeta {
	out := append([]ThrowsMeta(nil), in...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Status != out[j].Status {
			return out[i].Status < out[j].Status
		}
		if out[i].Description != out[j].Description {
			return out[i].Description < out[j].Description
		}
		return typeSortKey(out[i].Schema) < typeSortKey(out[j].Schema)
	})
	return out
}

// securityRequirements turns the resolved securitySchemes into the per-operation
// security requirement list. Each scheme name becomes an alternative requirement
// so every op.Security key is guaranteed to exist in components.securitySchemes.
// The default (single "bearerAuth") reduces to the historical [{bearerAuth: []}].
func securityRequirements(schemes map[string]SecuritySchemeObject) []SecurityReq {
	names := make([]string, 0, len(schemes))
	for name := range schemes {
		names = append(names, name)
	}
	sort.Strings(names)
	reqs := make([]SecurityReq, 0, len(names))
	for _, name := range names {
		reqs = append(reqs, SecurityReq{name: {}})
	}
	return reqs
}

func buildSecurity(route DiscoveredRoute, op *Operation, secReqs []SecurityReq) {
	if route.Security == nil {
		return
	}
	op.Security = secReqs
	if route.Security.Optional {
		// The empty requirement is OpenAPI's anonymous alternative. Copy first:
		// secReqs is shared by every operation of the document.
		op.Security = append(append(make([]SecurityReq, 0, len(secReqs)+1), secReqs...), SecurityReq{})
	}
	var secParts []string
	if len(route.Security.Roles) > 0 {
		secParts = append(secParts, "Required roles: "+strings.Join(route.Security.Roles, ", "))
	}
	if len(route.Security.Scopes) > 0 {
		secParts = append(secParts, "Required scopes: "+strings.Join(route.Security.Scopes, ", "))
	}
	if len(secParts) > 0 {
		secNote := strings.Join(secParts, ". ") + "."
		if op.Description != "" {
			op.Description += "\n\n" + secNote
		} else {
			op.Description = secNote
		}
	}
}

// generateOperationID delegates to the api package's single canonical synthesis
// (GET /users/{id} → getUsers_Id). The spec's operationId is the key generated
// clients trace with and the design graph joins operations on, so it must not be
// a second, independently-drifting implementation.
func generateOperationID(method, path string) string {
	return api.CanonicalOperationID(method, path)
}

func extractPathParams(path string) []string {
	segs := strings.Split(path, "/")
	params := make([]string, 0, len(segs))
	for _, seg := range segs {
		var name string
		switch {
		case strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}"):
			name = seg[1 : len(seg)-1]
		case strings.HasPrefix(seg, "[") && strings.HasSuffix(seg, "]"):
			name = seg[1 : len(seg)-1]
		default:
			continue
		}
		// Strip the catch-all marker so the rest of the spec generator sees a
		// plain parameter name. The multi-segment semantics surface through the
		// parameter description, not the name.
		name = strings.TrimSuffix(name, "...")
		params = append(params, name)
	}
	return params
}

// isCatchAllParam reports whether the named parameter appears in the path
// using the catch-all marker `{name...}` rather than the single-segment `{name}`.
func isCatchAllParam(path, name string) bool {
	return strings.Contains(path, "{"+name+"...}")
}

func findStructField(t reflect.Type, name string) (reflect.StructField, bool) {
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return reflect.StructField{}, false
	}
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if jsonFieldName(field) == name {
			return field, true
		}
	}
	return reflect.StructField{}, false
}

// jsonFieldName returns the JSON property name for a struct field, matching
// encoding/json: json:"-" (exactly) omits the field entirely (returns ""), a
// tag with no name before the comma (json:",omitempty") falls back to the Go
// field name, and an explicit name is used as-is.
func jsonFieldName(field reflect.StructField) string {
	tag := field.Tag.Get("json")
	if tag == "-" {
		return ""
	}
	name := tag
	if idx := strings.IndexByte(tag, ','); idx != -1 {
		name = tag[:idx]
	}
	if name == "" {
		return field.Name
	}
	return name
}

func hasValidateTag(field reflect.StructField, constraint string) bool {
	tag := field.Tag.Get("validate")
	if tag == "" {
		return false
	}
	for _, part := range strings.Split(tag, ",") {
		if strings.TrimSpace(part) == constraint {
			return true
		}
	}
	return false
}

// --- Type mapping ---

// schemaGen builds OpenAPI schemas, promoting every named (non-anonymous) struct
// type to a reusable component referenced via $ref. Naming each struct once — top
// level and nested — lets downstream client generators emit a named type per
// struct instead of degrading anonymous inline objects to a generic map. Anonymous
// structs (no type name) cannot be named and stay inline. The stack of types
// currently being expanded doubles as the cycle guard: a self-referential model's
// back-edge resolves to the already-in-progress component's $ref rather than
// recursing until the goroutine stack overflows.
type schemaGen struct {
	// components holds the schema for every named struct promoted to a reusable
	// component, keyed by component name. Surfaced as Document.Components.Schemas.
	components map[string]SchemaObject
	// stack is the set of named struct types currently being expanded — the
	// recursion path. A type already on the stack signals a cycle back-edge and
	// resolves to a $ref before the component is registered.
	stack map[string]bool
	// compName maps a fully-qualified type key (PkgPath.Name) to the component
	// name chosen for it, and usedNames is the reverse (component name → owning
	// key). Together they give same-named types from different packages distinct,
	// collision-free component names instead of silently sharing one slot.
	compName  map[string]string
	usedNames map[string]string
	// contractTypes is the canonical type namespace projected from the contract
	// IR. Named reflected endpoint types with a matching name resolve to the
	// canonical component, which is installed lazily with its dependency closure.
	contractTypes      map[string]bool
	contractEnums      map[string]contracts.Enum
	contractUnions     map[string]contracts.Union
	contractStructs    map[string]contracts.Struct
	contractProjecting map[string]bool
	generationErr      error
}

func newSchemaGen(contract *contracts.Manifest) *schemaGen {
	g := &schemaGen{
		components:         map[string]SchemaObject{},
		stack:              map[string]bool{},
		compName:           map[string]string{},
		usedNames:          map[string]string{},
		contractTypes:      map[string]bool{},
		contractEnums:      map[string]contracts.Enum{},
		contractUnions:     map[string]contracts.Union{},
		contractStructs:    map[string]contracts.Struct{},
		contractProjecting: map[string]bool{},
	}
	if contract != nil {
		g.addContract(contract)
	}
	return g
}

// addContract indexes and reserves the contract type namespace. Components are
// projected lazily when an endpoint references a matching named type, so the
// document contains only the referenced dependency closure.
func (g *schemaGen) addContract(m *contracts.Manifest) {
	for _, e := range m.Enums {
		g.contractTypes[e.Name] = true
		g.contractEnums[e.Name] = e
		g.usedNames[e.Name] = "contract:" + e.Name
	}
	for _, u := range m.Unions {
		g.contractTypes[u.Name] = true
		g.contractUnions[u.Name] = u
		g.usedNames[u.Name] = "contract:" + u.Name
	}
	for _, s := range m.Structs {
		g.contractTypes[s.Name] = true
		g.contractStructs[s.Name] = s
		g.usedNames[s.Name] = "contract:" + s.Name
	}
}

// ensureContractType projects one referenced contract node and everything it
// references into OpenAPI 3.0 components. Tagged-union discriminator constants
// use one-value enums because OpenAPI 3.0 has no JSON Schema const keyword.
func (g *schemaGen) ensureContractType(name string) {
	if _, done := g.components[name]; done || g.contractProjecting[name] {
		return
	}
	g.contractProjecting[name] = true
	defer delete(g.contractProjecting, name)

	if e, ok := g.contractEnums[name]; ok {
		values := make([]string, 0, len(e.Values))
		for _, value := range e.Values {
			values = append(values, value.Value)
		}
		g.components[name] = SchemaObject{Type: "string", Enum: values, Description: e.Description}
		return
	}
	if u, ok := g.contractUnions[name]; ok {
		oneOf := make([]SchemaObject, 0, len(u.Variants))
		for _, variant := range u.Variants {
			fields := variant.Fields
			if variant.Struct != "" {
				fields = g.contractStructs[variant.Struct].Fields
			}
			props := map[string]SchemaObject{}
			required := []string{u.Discriminator}
			for _, field := range fields {
				if field.Name == u.Discriminator {
					continue
				}
				props[field.Name] = g.contractFieldSchema(field)
				if !field.Optional {
					required = append(required, field.Name)
				}
			}
			props[u.Discriminator] = SchemaObject{Type: "string", Enum: []string{variant.Tag}}
			sort.Strings(required)
			oneOf = append(oneOf, SchemaObject{
				Type:                 "object",
				Description:          variant.Description,
				Properties:           props,
				Required:             required,
				AdditionalProperties: additionalPropertiesForbidden(),
			})
		}
		g.components[name] = SchemaObject{
			Description:   u.Description,
			OneOf:         oneOf,
			Discriminator: &DiscriminatorObject{PropertyName: u.Discriminator},
		}
		return
	}
	if s, ok := g.contractStructs[name]; ok {
		props := make(map[string]SchemaObject, len(s.Fields))
		required := make([]string, 0, len(s.Fields))
		for _, field := range s.Fields {
			props[field.Name] = g.contractFieldSchema(field)
			if !field.Optional {
				required = append(required, field.Name)
			}
		}
		sort.Strings(required)
		g.components[name] = SchemaObject{
			Type:                 "object",
			Description:          s.Description,
			Properties:           props,
			Required:             required,
			AdditionalProperties: additionalPropertiesForbidden(),
		}
	}
}

func (g *schemaGen) contractFieldSchema(field contracts.Field) SchemaObject {
	var schema SchemaObject
	switch field.Type {
	case "string":
		schema.Type = "string"
	case "int":
		schema.Type, schema.Format = "integer", "int64"
	case "float":
		schema.Type, schema.Format = "number", "double"
	case "bool":
		schema.Type = "boolean"
	case "duration":
		schema.Type, schema.Format = "string", "duration"
	default:
		g.ensureContractType(field.Type)
		schema.Ref = schemaRef(field.Type)
	}
	if field.Repeated {
		item := schema
		schema = SchemaObject{Type: "array", Items: &item}
	}
	schema.Description = field.Description
	return schema
}

// typeKey is the fully-qualified identity of a named type (PkgPath.Name), used
// to track cycles and components without colliding across packages. Unnamed
// types return "".
func typeKey(t reflect.Type) string {
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Name() == "" {
		return ""
	}
	return t.PkgPath() + "." + t.Name()
}

// componentName returns the collision-free component name for t, deriving it
// from the bare type name and disambiguating with a numeric suffix when a
// different package already claimed that name.
func (g *schemaGen) componentName(t reflect.Type) string {
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	key := typeKey(t)
	if name, ok := g.compName[key]; ok {
		return name
	}
	base := t.Name()
	name := base
	for i := 2; ; i++ {
		owner, taken := g.usedNames[name]
		if !taken || owner == key {
			break
		}
		name = base + strconv.Itoa(i)
	}
	g.usedNames[name] = key
	g.compName[key] = name
	return name
}

// Well-known stdlib types that reflect.Kind cannot map correctly: time.Time is
// a struct of unexported fields (would render as an empty object) and []byte
// marshals as a base64 string (not an array of integers). Matched by type
// identity before the Kind switch.
var (
	timeType       = reflect.TypeOf(time.Time{})
	durationType   = reflect.TypeOf(time.Duration(0))
	rawMessageType = reflect.TypeOf(json.RawMessage(nil))
)

func wellKnownSchema(t reflect.Type) *SchemaObject {
	switch t {
	case timeType:
		return &SchemaObject{Type: "string", Format: "date-time"}
	case durationType:
		return &SchemaObject{Type: "integer", Format: "int64"}
	case rawMessageType:
		// RawMessage may contain any JSON scalar, array, or object: the provider
		// declares it opaque rather than advertising a deceptively closed shape.
		return opaqueJSONSchema()
	}
	// []byte marshals as a base64 string under encoding/json. (json.RawMessage,
	// also a []byte, is handled above before this generic check.)
	if t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8 {
		return &SchemaObject{Type: "string", Format: "byte"}
	}
	return nil
}

func (g *schemaGen) schema(t reflect.Type) *SchemaObject {
	if t == nil {
		return &SchemaObject{Type: "object"}
	}
	nullable := false
	for t.Kind() == reflect.Ptr {
		nullable = true
		t = t.Elem()
	}
	var schema *SchemaObject
	if wk := wellKnownSchema(t); wk != nil {
		schema = wk
	} else if t.Name() != "" && g.contractTypes[t.Name()] {
		g.ensureContractType(t.Name())
		schema = &SchemaObject{Ref: schemaRef(t.Name())}
	} else {
		switch t.Kind() {
		case reflect.String:
			schema = &SchemaObject{Type: "string"}
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			format := "int64"
			if t.Kind() >= reflect.Int8 && t.Kind() <= reflect.Int32 {
				format = "int32"
			}
			schema = &SchemaObject{Type: "integer", Format: format}
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			format := "uint64"
			if t.Kind() >= reflect.Uint8 && t.Kind() <= reflect.Uint32 {
				format = "uint32"
			}
			schema = &SchemaObject{Type: "integer", Format: format}
		case reflect.Float32:
			schema = &SchemaObject{Type: "number", Format: "float"}
		case reflect.Float64:
			schema = &SchemaObject{Type: "number", Format: "double"}
		case reflect.Bool:
			schema = &SchemaObject{Type: "boolean"}
		case reflect.Slice, reflect.Array:
			schema = &SchemaObject{Type: "array", Items: g.schema(t.Elem())}
		case reflect.Struct:
			schema = g.structSchemaRef(t)
		case reflect.Interface:
			if t.NumMethod() == 0 {
				// The empty interface holds whatever encoding/json decoded: any
				// JSON value, declared as such.
				schema = opaqueJSONSchema()
			} else {
				// A non-empty interface cannot be decoded into, so it declares
				// nothing a client could send; the strict reader refuses it.
				schema = &SchemaObject{}
			}
		case reflect.Map:
			if t.Key().Kind() != reflect.String {
				// encoding/json stringifies some non-string keys, but the neutral
				// schema cannot preserve that conversion rule mechanically.
				schema = &SchemaObject{}
			} else if value := g.schema(t.Elem()); value.OpaqueJSON != "" {
				schema = freeFormObjectSchema()
			} else {
				schema = &SchemaObject{Type: "object", AdditionalProperties: additionalPropertiesSchema(value)}
			}
		default:
			// Empty schema stays legal OpenAPI for low-level/external consumers,
			// while the first-party contract validator rejects it explicitly.
			schema = &SchemaObject{}
		}
	}
	// An opaque value already admits null, so a pointer to one declares nothing
	// more: the declaration keeps its one spelling.
	if nullable && schema.OpaqueJSON == "" {
		copy := *schema
		value := true
		copy.Nullable = &value
		return &copy
	}
	return schema
}

// structSchemaRef promotes every named (non-anonymous) struct type to a reusable
// component and returns a $ref to it, so downstream client generators can name the
// type instead of degrading an anonymous inline object to a generic map. Anonymous
// structs have no name to reference and stay inline. A type already published, or
// currently mid-expansion (a self-reference cycle's back-edge), resolves to its
// $ref without re-expanding.
func (g *schemaGen) structSchemaRef(t reflect.Type) *SchemaObject {
	key := typeKey(t)
	if key == "" {
		// Anonymous struct — cannot be named, so it must stay inline.
		schema := g.structSchema(t)
		return &schema
	}
	name := g.componentName(t)
	if _, done := g.components[name]; done || g.stack[key] {
		// Already published, or on the current expansion path (cycle back-edge):
		// reference the one component rather than inlining or recursing.
		return &SchemaObject{Ref: schemaRef(name)}
	}
	// structSchema sets/clears g.stack[key] via its own defer and recurses into
	// fields, where nested named structs promote themselves the same way. Register
	// only after expansion so back-edges above resolve to this component's $ref.
	schema := g.structSchema(t)
	g.components[name] = schema
	return &SchemaObject{Ref: schemaRef(name)}
}

func schemaRef(name string) string { return "#/components/schemas/" + name }

func (g *schemaGen) structSchema(t reflect.Type) SchemaObject {
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return *g.schema(t)
	}

	if key := typeKey(t); key != "" {
		g.stack[key] = true
		defer delete(g.stack, key)
	}

	schema := SchemaObject{
		Type:       "object",
		Properties: make(map[string]SchemaObject),
	}
	required := map[string]bool{}
	g.collectFields(t, schema.Properties, required)

	req := make([]string, 0, len(required))
	for name := range required {
		req = append(req, name)
	}
	sort.Strings(req)
	schema.Required = req
	return schema
}

// collectFields fills props/required from t's exported fields, matching
// encoding/json's field-selection ("dominance") rules:
//   - an untagged anonymous struct field is promoted (its fields flattened into
//     the parent) rather than nested;
//   - across embedding levels a SHALLOWER field wins a JSON-name collision over
//     a deeper one, regardless of declaration order;
//   - at the same shallowest depth an explicitly json-tagged field wins over an
//     untagged one, and any remaining tie (two tagged, or two untagged, fields
//     at equal depth) is AMBIGUOUS: encoding/json drops the name from the wire,
//     so it must not appear in the schema either.
//
// json:"-" fields are skipped. The shared schema.JSONFields selector owns cycle
// detection and the same ambiguity rules used by first-party body validation.
func (g *schemaGen) collectFields(t reflect.Type, props map[string]SchemaObject, required map[string]bool) {
	for _, selected := range validation.JSONFields(t) {
		field := selected.Field
		prop := *g.schema(field.Type)
		applyValidateConstraints(&prop, field.Tag.Get("validate"))
		if defaultTag := field.Tag.Get("default"); defaultTag != "" {
			defaultValue, err := validation.DefaultJSON(defaultTag, field.Type)
			if err != nil {
				if g.generationErr == nil {
					g.generationErr = fmt.Errorf("openapi: field %q: %w", selected.Name, err)
				}
			} else {
				prop.Default = defaultValue
			}
		}
		if desc := field.Tag.Get("description"); desc != "" {
			prop.Description = desc
		}
		props[selected.Name] = prop
		if hasValidateTag(field, "required") {
			required[selected.Name] = true
		}
	}
}

// structToSchema builds an object schema for t, expanding the OUTERMOST type
// inline (its Properties/Required are returned directly rather than as a $ref).
// Nested named structs still promote themselves to components on the throwaway
// schemaGen — those components are discarded, so only the outer object survives.
// Retained as a standalone helper for tests and ad-hoc callers; spec generation
// uses a shared schemaGen so $refs and components aggregate across routes.
func structToSchema(t reflect.Type) SchemaObject {
	return newSchemaGen(nil).structSchema(t)
}

// applyValidateConstraints reads numeric (min/max), length (minlen/maxlen),
// pattern, enum (oneof) and format (uuid/email/url) constraints from a validate tag
// and populates the matching SchemaObject fields, so the generated spec
// advertises the same rules the runtime validator enforces. Tokenized parsing
// (vs. strings.Contains) avoids matching substrings of unrelated constraints.
func applyValidateConstraints(prop *SchemaObject, validateTag string) {
	if prop == nil || validateTag == "" {
		return
	}
	for _, tok := range strings.Split(validateTag, ",") {
		key, val, hasVal := strings.Cut(strings.TrimSpace(tok), "=")
		switch key {
		case "uuid":
			prop.Format = "uuid"
		case "email":
			prop.Format = "email"
		case "url":
			prop.Format = "uri"
		case "min":
			if _, err := strconv.ParseFloat(val, 64); hasVal && err == nil {
				value := json.Number(val)
				prop.Minimum = &value
			}
		case "max":
			if _, err := strconv.ParseFloat(val, 64); hasVal && err == nil {
				value := json.Number(val)
				prop.Maximum = &value
			}
		case "minlen":
			if n, err := strconv.Atoi(val); hasVal && err == nil {
				prop.MinLength = &n
			}
		case "maxlen":
			if n, err := strconv.Atoi(val); hasVal && err == nil {
				prop.MaxLength = &n
			}
		case "pattern":
			if hasVal {
				prop.Pattern = val
			}
		case "oneof":
			if hasVal {
				prop.Enum = strings.Split(val, "|")
			}
		}
	}
}

// bodylessRequestMethod names the request methods whose payload RFC 9110 leaves
// undefined, so no first-party contract may declare one.
func bodylessRequestMethod(method string) bool {
	switch strings.ToUpper(method) {
	case "GET", "HEAD":
		return true
	default:
		return false
	}
}
