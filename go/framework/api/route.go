package api

import (
	"reflect"
	"strings"

	perrors "go.putnami.dev/errors"
	phttp "go.putnami.dev/http"
)

// ResponseMeta describes a single response entry: status code, optional description, and
// optional schema. Used for both additional success responses (declared via Response) and
// error responses (declared via Throws).
//
// Mirrors the TypeScript ResponseMeta exported from `@putnami/application/api`.
type ResponseMeta struct {
	// Status is the HTTP status code (e.g. 200, 404).
	Status int
	// Description is a human-readable description of the response.
	Description string
	// Schema is the response body type, or nil for empty responses.
	Schema reflect.Type
}

// ResponseDeclarations groups the success and error response metadata declared on an endpoint.
type ResponseDeclarations struct {
	// Returns lists additional success responses declared via Response. The primary
	// response declared by Returns or ReturnsStatus is stored directly on DiscoveredRoute.
	Returns []ResponseMeta
	// ErrorCodes lists framework-known errors declared via MayThrow.
	ErrorCodes []perrors.Code
	// ErrorRetryability retains explicit MayThrowWith retry classifications by
	// stable framework error code. Absence means the resilience policy decides.
	ErrorRetryability map[perrors.Code]bool
	// ErrorDetails retains the `details` body type declared via MayThrowDetails,
	// by stable framework error code. It describes the envelope's `details`
	// member only, never the envelope (ADR 0006). Absence means the error
	// carries no declared details.
	ErrorDetails map[perrors.Code]reflect.Type
	// Throws lists error responses declared via Throws / ThrowsAll.
	Throws []ResponseMeta
}

// ErrorOptions carries client-visible properties for one stable framework
// error declaration.
type ErrorOptions struct {
	// Retryable explicitly classifies this remote outcome. False is meaningful:
	// it blocks a broader retry status/code policy.
	Retryable bool
}

// EndpointMeta groups the cross-cutting metadata captured by per-endpoint builder methods
// (.Description, .Secure, .Cors, .RateLimit, .Cache). Used for documentation generation.
type EndpointMeta struct {
	// Description is the human-readable summary set via .Description.
	Description string
	// SecurityOptions, when non-nil, contains the structured security requirements declared
	// via .Secure(security.Options{...}). Guard functions are opaque and produce nil here.
	SecurityOptions any
	// CorsOptions, when non-nil, contains the CORS options declared via .Cors.
	CorsOptions any
	// RateLimitOptions, when non-nil, contains the rate-limit options declared via .RateLimit.
	RateLimitOptions any
	// CacheOptions, when non-nil, contains the cache options declared via .Cache.
	CacheOptions any
	// ClientOptions carries the generated-client security, idempotency and
	// resilience policy declared on this operation. Transport, stream and error
	// metadata remain derived from adjacent route fields.
	ClientOptions *ClientOperationOptions
}

// DiscoveredRoute holds the metadata for a registered endpoint, lifted out of any HTTP-specific
// abstraction so OpenAPI, proto, gRPC, and client generators can consume it without depending
// on the HTTP transport package.
type DiscoveredRoute struct {
	// Method is the HTTP method (GET, POST, etc.).
	Method string
	// Path is the route path with the api plugin's prefix applied.
	Path string
	// StreamMode is set for streaming endpoints (SSE / WebSocket).
	StreamMode phttp.StreamMode
	// Description is the human-readable description from .Description (optional).
	Description string
	// ParamsSchema is the path-parameter struct type, or nil.
	ParamsSchema reflect.Type
	// QuerySchema is the query-parameter struct type, or nil.
	QuerySchema reflect.Type
	// BodySchema is the request body struct type, or nil.
	BodySchema reflect.Type
	// BodyBinary declares a raw-octet request payload: its media type and its
	// byte bound. Non-nil means BodySchema is a placeholder and no JSON schema
	// describes the body.
	BodyBinary *BinaryMeta
	// ReturnsSchema is the primary success response type, or nil.
	ReturnsSchema reflect.Type
	// ReturnsBinary declares a raw-octet success payload, mirroring BodyBinary.
	ReturnsBinary *BinaryMeta
	// ProviderWire declares a provider-owned WebSocket wire: the route's
	// messages are raw octets or JSON values of the declared message types, the
	// upgrade request is the admission, and there is no first-party
	// conversation. Nil for unary routes and first-party streams.
	ProviderWire *ProviderWireMeta
	// ReturnsStatus is the exact primary 2xx status. Zero preserves the
	// legacy default of 200 for routes constructed directly.
	ReturnsStatus int
	// ReturnsDescription documents the primary success response.
	ReturnsDescription string
	// Responses bundles additional response declarations (status-specific returns and throws).
	Responses ResponseDeclarations
	// Meta carries cross-cutting metadata (security, cors, rate limit, cache).
	Meta EndpointMeta
	// DocumentOnly is true when the endpoint was registered via Document() —
	// the api plugin records the route for OpenAPI/typed-client codegen but
	// does NOT bind a transport handler. Consumers that wire the route to a
	// transport-specific surface (gRPC bridge, proto Connect RPC) skip these
	// entries.
	DocumentOnly bool
}

// CanonicalOperationID is the single synthesis of an operation's canonical
// identity from its HTTP method and route path (GET /users/{id} → getUsers_Id).
//
// Every Go surface that names an operation resolves through it: the OpenAPI
// generator stamps it as the spec's operationId, generated clients embed it in
// their descriptor and trace key, and the design graph uses it for both the
// api.operation node and the generatedFrom edges. Keeping one implementation is
// what makes those keys join — two independent synthesizers would silently
// attribute the same operation under two different ids.
//
// Both parameter syntaxes are accepted so a TypeScript-authored path ([id]) and
// a Go-authored one ({id}) name the same operation. The catch-all marker is
// stripped: multi-segment semantics belong to the parameter, not to its id.
func CanonicalOperationID(method, path string) string {
	replacer := strings.NewReplacer("{", "", "}", "", "[", "", "]", "", "...", "")
	clean := replacer.Replace(path)

	parts := strings.Split(strings.Trim(clean, "/"), "/")
	for i, part := range parts {
		if part == "" {
			continue
		}
		parts[i] = strings.ToUpper(part[:1]) + part[1:]
	}
	// Root path: the method alone is the whole identity.
	if len(parts) == 1 && parts[0] == "" {
		return strings.ToLower(method)
	}
	return strings.ToLower(method) + strings.Join(parts, "_")
}
