// Package api defines transport-agnostic endpoint definitions and orchestration for HTTP
// APIs. Endpoints declare validation, response shape, security, and middleware once; the
// api.Plugin dispatches them to a transport (today: *http.ServerPlugin) and exposes route
// metadata for OpenAPI / proto / gRPC / typed-client codegen consumers.
package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"sort"
	"strings"

	perrors "go.putnami.dev/errors"
	phttp "go.putnami.dev/http"
	"go.putnami.dev/inject"
	"go.putnami.dev/logger"
	"go.putnami.dev/schema"
)

// EndpointBuilder fluent-builds an endpoint definition with input validation, DI injection,
// security, response metadata, and middleware composition. Created via Endpoint().
type EndpointBuilder struct {
	method             string
	path               string
	description        string
	paramsSchema       reflect.Type
	querySchema        reflect.Type
	bodySchema         reflect.Type
	returns            reflect.Type
	returnsStatus      int
	returnsDescription string
	returnsStatusSet   bool
	bodyStream         bool
	returnsStream      bool
	bodyBinary         *BinaryMeta
	returnsBinary      *BinaryMeta
	bodyBytes          bool
	returnsBytes       bool
	subprotocol        string
	subprotocolSet     bool
	responses          []ResponseMeta
	errorCodes         []perrors.Code
	errorPolicies      map[perrors.Code]ErrorOptions
	errorDetails       map[perrors.Code]reflect.Type
	throws             []ResponseMeta
	injectTokens       map[string]inject.Token
	security           phttp.SecurityRule
	securityMeta       any
	corsMeta           any
	rateLimitMeta      any
	cacheMeta          any
	clientOptions      *ClientOperationOptions
	firstParty         bool
	middlewares        []phttp.Middleware
	handler            any
	log                *logger.Logger
}

// Endpoint creates a new endpoint builder for the given method and path. Equivalent to
// TypeScript's `endpoint()` from `@putnami/application`.
//
//	api.Endpoint("GET", "/users/{id}").
//	    Params(api.Type[UserParams]()).
//	    Returns(api.Type[User]()).
//	    Handle(getUser)
func Endpoint(method, path string) *EndpointBuilder {
	return &EndpointBuilder{
		method:        method,
		path:          path,
		injectTokens:  make(map[string]inject.Token),
		errorPolicies: make(map[perrors.Code]ErrorOptions),
		errorDetails:  make(map[perrors.Code]reflect.Type),
	}
}

// Description sets a human-readable description for the endpoint. Surfaces as the OpenAPI
// operation description.
func (b *EndpointBuilder) Description(desc string) *EndpointBuilder {
	b.description = desc
	return b
}

// Params sets the schema (struct type with `validate:` tags) for path parameters. Combine
// with the api.Type[T]() helper:
//
//	.Params(api.Type[UserParams]())
func (b *EndpointBuilder) Params(schemaType reflect.Type) *EndpointBuilder {
	b.paramsSchema = schemaType
	return b
}

// Query sets the schema for query parameters.
func (b *EndpointBuilder) Query(schemaType reflect.Type) *EndpointBuilder {
	b.querySchema = schemaType
	return b
}

// Body sets the schema for the JSON request body. Pass api.Stream(...) or
// api.StreamOf[T]() to declare a stream of incoming WebSocket messages,
// api.ByteStream() to declare raw octets in binary WebSocket messages, or
// api.Binary(mediaType, maxBytes) to declare a bounded raw octet body. Use
// api.BinaryStream(maxBytes) for an unbuffered HTTP body with a sender-chosen media type.
func (b *EndpointBuilder) Body(schemaType any) *EndpointBuilder {
	typ, stream, binary := schemaTypeFrom(schemaType, "Body")
	_, bytes := schemaType.(ByteStreamSchema)
	b.bodySchema = typ
	b.bodyStream = stream
	b.bodyBinary = binary
	b.bodyBytes = bytes
	return b
}

// Returns sets the primary success response type, used for OpenAPI's default
// 200 response. Pass api.Stream(...) or api.StreamOf[T]() to declare a stream
// of outgoing messages, or api.ByteStream() to declare raw octets in binary
// WebSocket messages. api.Binary(mediaType, maxBytes) declares bounded octets;
// api.BinaryStream(maxBytes) declares an unbuffered HTTP body with a sender-chosen label.
func (b *EndpointBuilder) Returns(schemaType any) *EndpointBuilder {
	typ, stream, binary := schemaTypeFrom(schemaType, "Returns")
	_, bytes := schemaType.(ByteStreamSchema)
	b.returns = typ
	b.returnsStream = stream
	b.returnsBinary = binary
	b.returnsBytes = bytes
	b.returnsStatus = 200
	b.returnsDescription = "Successful response"
	b.returnsStatusSet = false
	return b
}

// ReturnsStatus sets the primary 2xx response status, description, and type for
// a unary endpoint. Use it when success is not HTTP 200, such as a create
// operation returning 201. It panics for a non-2xx status or when the endpoint
// is finalized as a stream.
func (b *EndpointBuilder) ReturnsStatus(status int, description string, schemaType any) *EndpointBuilder {
	if status < 200 || status > 299 {
		panic(fmt.Sprintf("api.EndpointBuilder.ReturnsStatus: status must be between 200 and 299, got %d", status))
	}
	typ, stream, binary := schemaTypeFrom(schemaType, "ReturnsStatus")
	_, bytes := schemaType.(ByteStreamSchema)
	b.returns = typ
	b.returnsStream = stream
	b.returnsBinary = binary
	b.returnsBytes = bytes
	b.returnsStatus = status
	b.returnsDescription = description
	b.returnsStatusSet = true
	return b
}

// Response declares an additional status-specific success response for documentation.
// Pass nil for `schemaType` if the response has no body (e.g. 204 No Content).
//
//	.Response(201, "Created", api.Type[Created]())
//	.Response(204, "No content", nil)
func (b *EndpointBuilder) Response(status int, description string, schemaType reflect.Type) *EndpointBuilder {
	b.responses = append(b.responses, ResponseMeta{
		Status:      status,
		Description: description,
		Schema:      schemaType,
	})
	return b
}

// MayThrow declares framework-known error codes this endpoint may emit. OpenAPI maps
// each code through the errors.HTTPStatusForCode table and documents the standard error
// response envelope. Use MayThrowDetails when the error carries a typed `details` body,
// and Throws / ThrowsAll for custom response descriptions.
//
//	.MayThrow(errors.CodeNotFound, errors.CodeConflict)
func (b *EndpointBuilder) MayThrow(codes ...perrors.Code) *EndpointBuilder {
	b.errorCodes = append(b.errorCodes, codes...)
	return b
}

// MayThrowWith declares a framework-known stable error code and its explicit
// generated-client retry classification. Use MayThrow when the operation's
// resilience policy alone should decide whether the outcome is retried.
func (b *EndpointBuilder) MayThrowWith(code perrors.Code, options ErrorOptions) *EndpointBuilder {
	b.errorCodes = append(b.errorCodes, code)
	b.errorPolicies[code] = options
	return b
}

// MayThrowDetails declares a stable error code this endpoint may emit and the
// type of the `details` member its error envelope carries. The first-party
// contract publishes that type as the error's schema, and generated clients
// decode `details` onto the typed error for that code (ADR 0006).
//
// It declares no retry classification; chain MayThrowWith for the same code
// to add one. The HTTP status comes from errors.HTTPStatusForCode, the same
// table the provider answers with, so register a custom code once with
// errors.RegisterHTTPStatus before the api plugin configures. The handler
// attaches the value with errors.Any("details", value) on a client-safe
// error (errors.User or a security error): other categories never expose it.
//
//	errors.RegisterHTTPStatus("deploy_rejected", http.StatusBadRequest)
//	.MayThrowDetails("deploy_rejected", api.Type[DeployRejection]())
//
// It panics on a nil details type, and on errors.CodeBadRequest or
// errors.CodeInternalServer: every endpoint answers those two implicit codes,
// and the framework owns their `details` (a request validation failure
// carries its field errors there), so a declared type would describe bodies
// the endpoint never writes. A later declaration for the same code replaces
// the earlier one.
func (b *EndpointBuilder) MayThrowDetails(code perrors.Code, details reflect.Type) *EndpointBuilder {
	if details == nil {
		panic(fmt.Sprintf("api.EndpointBuilder.MayThrowDetails: details type for %q is nil; use MayThrow for an error without details", code))
	}
	if code == perrors.CodeBadRequest || code == perrors.CodeInternalServer {
		panic(fmt.Sprintf("api.EndpointBuilder.MayThrowDetails: %q is a framework-owned implicit error whose details the framework writes (request validation puts its field errors there); declare a code of your own", code))
	}
	if b.errorDetails == nil {
		b.errorDetails = make(map[perrors.Code]reflect.Type)
	}
	b.errorCodes = append(b.errorCodes, code)
	b.errorDetails[code] = details
	return b
}

// Throws declares an error response for documentation. Schema is optional — pass nil to
// document the status without a body schema.
//
//	.Throws(404, "Not found", nil)
//	.Throws(400, "Validation failed", api.Type[ValidationError]())
func (b *EndpointBuilder) Throws(status int, description string, schemaType reflect.Type) *EndpointBuilder {
	b.throws = append(b.throws, ResponseMeta{
		Status:      status,
		Description: description,
		Schema:      schemaType,
	})
	return b
}

// ThrowsAll appends a batch of error responses, useful for spreading a reusable bundle:
//
//	authErrors := []api.ResponseMeta{
//	    {Status: 401, Description: "Unauthorized"},
//	    {Status: 403, Description: "Forbidden"},
//	}
//	.ThrowsAll(authErrors...)
func (b *EndpointBuilder) ThrowsAll(specs ...ResponseMeta) *EndpointBuilder {
	b.throws = append(b.throws, specs...)
	return b
}

// Inject declares a DI token to resolve into ctx.Injected[name] before the handler runs.
// For typed DI parameters, use http.Inject() with a function-shaped handler instead.
func (b *EndpointBuilder) Inject(name string, token inject.Token) *EndpointBuilder {
	b.injectTokens[name] = token
	return b
}

// Secure enforces an authorization rule (security.Options or security.Guard) on this
// endpoint. The rule's middleware is applied after generic middleware in the chain.
func (b *EndpointBuilder) Secure(rule phttp.SecurityRule) *EndpointBuilder {
	b.security = rule
	b.securityMeta = rule
	return b
}

// Use appends a middleware to the per-endpoint chain. Generic middleware (CORS, rate
// limiting, logging) typically lives on the server or api plugin, not on the endpoint.
func (b *EndpointBuilder) Use(mw phttp.Middleware) *EndpointBuilder {
	b.middlewares = append(b.middlewares, mw)
	return b
}

// Cors attaches a CORS middleware to this endpoint. Equivalent to .Use(http.CORS(opts))
// but also captures the options as documentation metadata.
func (b *EndpointBuilder) Cors(opts phttp.CORSOptions) *EndpointBuilder {
	b.middlewares = append(b.middlewares, phttp.CORS(opts))
	b.corsMeta = opts
	return b
}

// RateLimit attaches a rate-limiting middleware to this endpoint.
func (b *EndpointBuilder) RateLimit(opts phttp.RateLimitOptions) *EndpointBuilder {
	b.middlewares = append(b.middlewares, phttp.RateLimit(opts))
	b.rateLimitMeta = opts
	return b
}

// Cache attaches middleware that sets the Cache-Control response header,
// mirroring .Cors() and .RateLimit(), and records opts as endpoint metadata.
func (b *EndpointBuilder) Cache(opts phttp.CacheOptions) *EndpointBuilder {
	b.middlewares = append(b.middlewares, phttp.Cache(opts))
	b.cacheMeta = opts
	return b
}

// Client declares the operation-specific client policy that cannot be inferred
// from the route itself. The framework derives stream mode, transport paths and
// declared errors from the endpoint definition when it publishes the contract.
func (b *EndpointBuilder) Client(opts ClientOperationOptions) *EndpointBuilder {
	b.clientOptions = cloneClientOperationOptions(&opts)
	return b
}

// Handle finalizes the endpoint with a typed handler that receives an *EndpointContext
// (validated params/query/body, resolved DI deps).
//
// Accepted shapes:
//   - func(ctx *http.EndpointContext) *http.Response — typed handler
//   - *http.InjectedHandler from http.Inject() — DI-injected handler with *EndpointContext
func (b *EndpointBuilder) Handle(handler any) EndpointDefinition {
	b.validateReturnsStatus("Handle")
	b.validateProviderWire("Handle")
	if b.isStream() {
		// .Inject() resolves into ctx.Injected, which only exists on the unary
		// pipeline's *EndpointContext. The stream pipeline has no place to surface
		// resolved tokens, so accepting them would be a silent no-op. Fail loudly
		// instead and point at the supported pattern.
		if len(b.injectTokens) > 0 {
			panic("api.EndpointBuilder.Handle: .Inject() is not supported on stream endpoints; resolve dependencies with inject.Resolve inside the stream handler or capture them via closure")
		}
		streamHandler, ok := handler.(StreamHandler)
		if !ok {
			panic(fmt.Sprintf("api.EndpointBuilder.Handle: stream endpoint requires api.ServerStream, api.ClientStream, or api.BidiStream handler, got %T", handler))
		}
		if strings.ToUpper(b.method) != "GET" {
			panic("api.EndpointBuilder.Handle: stream endpoints must use GET for SSE/WebSocket transport")
		}
		if got, want := streamHandler.streamMode(), b.streamMode(); got != want {
			panic(fmt.Sprintf("api.EndpointBuilder.Handle: stream handler mode %q does not match endpoint mode %q", got, want))
		}
		if wire := b.providerWire(); handlerCarriesBytes(streamHandler) != (wire != nil && wire.Bytes) {
			panic("api.EndpointBuilder.Handle: a byte stream endpoint requires api.ByteTunnel, and api.ByteTunnel requires Body(api.ByteStream()) and Returns(api.ByteStream())")
		}
		b.handler = streamHandler
		return EndpointDefinition{builder: b}
	}

	switch handler.(type) {
	case func(ctx *phttp.EndpointContext) *phttp.Response:
		b.handler = handler
	case *phttp.InjectedHandler:
		b.handler = handler
	default:
		panic(fmt.Sprintf("api.EndpointBuilder.Handle: unsupported handler type %T", handler))
	}
	return EndpointDefinition{builder: b}
}

// HandleRaw finalizes the endpoint with a low-level handler that receives the base
// *http.Context (no automatic validation, no injected map). Use when the validation
// pipeline is unwanted.
func (b *EndpointBuilder) HandleRaw(handler phttp.Handler) EndpointDefinition {
	b.validateReturnsStatus("HandleRaw")
	b.validateProviderWire("HandleRaw")
	b.handler = handler
	return EndpointDefinition{builder: b, raw: true}
}

// Document finalizes the endpoint as documentation-only: api.Plugin records it
// in DiscoveredRoutes but registers no transport handler. Use this when the
// route is already mounted by some other path — e.g. the caller calls
// server.GET / POST directly — and the api plugin only publishes the schema.
//
// Consumers split on whether they describe the route or map a handler:
//
//   - OpenAPI and the typed-client generators INCLUDE it.
//
//   - The proto generator and the gRPC Connect bridge SKIP it.
//
// Typical use:
//
//	api.Register(api.Endpoint("GET", "/.well-known/putnami/events").
//	    Description("Canonical events endpoint").
//	    Returns(api.Type[Manifest]()).
//	    Document())
//	server.GET("/.well-known/putnami/events", manifestHandler)
//
// Document is mutually exclusive with Handle / HandleRaw. Streaming endpoints
// (Body / Returns declared via api.Stream / api.StreamOf) are supported and
// produce StreamMode metadata in the discovered route just like handler-bound
// streams; only the transport binding is skipped.
func (b *EndpointBuilder) Document() EndpointDefinition {
	b.validateReturnsStatus("Document")
	b.validateProviderWire("Document")
	return EndpointDefinition{builder: b, documentOnly: true}
}

// EndpointDefinition is a fully configured endpoint produced by EndpointBuilder.Handle,
// EndpointBuilder.HandleRaw, or EndpointBuilder.Document. Register it on an
// api.Plugin via Plugin.Register.
type EndpointDefinition struct {
	builder      *EndpointBuilder
	raw          bool
	documentOnly bool
}

// Method returns the HTTP method.
func (d EndpointDefinition) Method() string { return d.builder.method }

// Path returns the route path, before the api plugin's prefix is applied.
func (d EndpointDefinition) Path() string { return d.builder.path }

// Description returns the human-readable description set via .Description, or "".
func (d EndpointDefinition) Description() string { return d.builder.description }

// ParamsSchema returns the path-parameter schema, or nil.
func (d EndpointDefinition) ParamsSchema() reflect.Type { return d.builder.paramsSchema }

// QuerySchema returns the query-parameter schema, or nil.
func (d EndpointDefinition) QuerySchema() reflect.Type { return d.builder.querySchema }

// BodySchema returns the body schema, or nil.
func (d EndpointDefinition) BodySchema() reflect.Type { return d.builder.bodySchema }

// ReturnsSchema returns the primary return schema, or nil.
func (d EndpointDefinition) ReturnsSchema() reflect.Type { return d.builder.returns }

// BodyBinary returns the declared raw-octet request payload, or nil when the
// body is a JSON document.
func (d EndpointDefinition) BodyBinary() *BinaryMeta { return cloneBinaryMeta(d.builder.bodyBinary) }

// ReturnsBinary returns the declared raw-octet success payload, or nil when the
// primary response is a JSON document.
func (d EndpointDefinition) ReturnsBinary() *BinaryMeta {
	return cloneBinaryMeta(d.builder.returnsBinary)
}

// Responses returns the additional response declarations.
func (d EndpointDefinition) Responses() []ResponseMeta { return d.builder.responses }

// ErrorCodes returns the framework-known error codes declared via MayThrow,
// MayThrowWith and MayThrowDetails.
func (d EndpointDefinition) ErrorCodes() []perrors.Code { return d.builder.errorCodes }

// Throws returns the error response declarations.
func (d EndpointDefinition) Throws() []ResponseMeta { return d.builder.throws }

// IsStream reports whether this endpoint uses streaming request or response
// payloads.
func (d EndpointDefinition) IsStream() bool { return d.builder.isStream() }

// IsDocumentOnly reports whether this endpoint was finalized with Document() —
// i.e. api.Plugin records it in DiscoveredRoutes but skips transport
// registration.
func (d EndpointDefinition) IsDocumentOnly() bool { return d.documentOnly }

// StreamMode returns the inferred stream mode. Empty for non-stream endpoints.
func (d EndpointDefinition) StreamMode() phttp.StreamMode { return d.builder.streamMode() }

// ClientOptions returns a defensive copy of the operation-specific generated
// client policy, or nil when the endpoint did not declare one.
func (d EndpointDefinition) ClientOptions() *ClientOperationOptions {
	return cloneClientOperationOptions(d.builder.clientOptions)
}

// InjectedHandler returns the underlying *http.InjectedHandler if the endpoint was
// finalized with one. Used by the api plugin to register the handler for DI finalization.
func (d EndpointDefinition) InjectedHandler() *phttp.InjectedHandler {
	if ih, ok := d.builder.handler.(*phttp.InjectedHandler); ok {
		return ih
	}
	return nil
}

// injectionTokens returns every DI token the endpoint resolves through either
// EndpointBuilder.Inject or http.Inject. The result is de-duplicated and sorted
// by stable token key for deterministic design discovery.
func (d EndpointDefinition) injectionTokens() []inject.Token {
	byKey := make(map[string]inject.Token, len(d.builder.injectTokens))
	for _, token := range d.builder.injectTokens {
		byKey[token.Key()] = token
	}
	if handler := d.InjectedHandler(); handler != nil {
		for _, token := range handler.DependencyTokens() {
			byKey[token.Key()] = token
		}
	}
	keys := make([]string, 0, len(byKey))
	for key := range byKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	tokens := make([]inject.Token, 0, len(keys))
	for _, key := range keys {
		tokens = append(tokens, byKey[key])
	}
	return tokens
}

// BuildHandler composes the validation pipeline, DI lookups, and middleware chain into a
// single phttp.Handler ready for transport registration.
func (d EndpointDefinition) BuildHandler() phttp.Handler {
	b := d.builder
	if d.IsStream() {
		return func(_ *phttp.Context) *phttp.Response {
			return phttp.InternalError("stream endpoint cannot be built as a unary handler")
		}
	}

	if d.raw {
		rawHandler, ok := b.handler.(phttp.Handler)
		if !ok {
			return func(_ *phttp.Context) *phttp.Response { return phttp.InternalError("invalid handler type") }
		}
		handler := rawHandler
		if b.security != nil {
			handler = phttp.Chain(b.security.Middleware())(handler)
		}
		if len(b.middlewares) == 0 {
			return handler
		}
		return phttp.Chain(b.middlewares...)(handler)
	}

	var callHandler func(ectx *phttp.EndpointContext) *phttp.Response
	switch h := b.handler.(type) {
	case func(ctx *phttp.EndpointContext) *phttp.Response:
		callHandler = h
	case *phttp.InjectedHandler:
		callHandler = h.HandleEndpoint
	default:
		return func(_ *phttp.Context) *phttp.Response { return phttp.InternalError("invalid handler type") }
	}

	hasInjections := len(b.injectTokens) > 0
	handler := func(ctx *phttp.Context) *phttp.Response {
		ectx := &phttp.EndpointContext{Context: ctx}
		if hasInjections {
			ectx.Injected = make(map[string]any, len(b.injectTokens))
		}

		if resp := b.validateParams(ctx, ectx); resp != nil {
			return resp
		}
		if resp := b.validateQuery(ctx, ectx); resp != nil {
			return resp
		}
		if resp := b.validateBody(ctx, ectx); resp != nil {
			return resp
		}
		if resp := b.resolveInjections(ctx, ectx); resp != nil {
			return resp
		}

		return callHandler(ectx)
	}

	if b.security != nil {
		handler = phttp.Chain(b.security.Middleware())(handler)
	}
	if len(b.middlewares) > 0 {
		handler = phttp.Chain(b.middlewares...)(handler)
	}
	return handler
}

// BuildStreamHandler builds the HTTP transport stream handler for this
// endpoint.
func (d EndpointDefinition) BuildStreamHandler() phttp.StreamHandler {
	b := d.builder
	streamHandler, ok := b.handler.(StreamHandler)
	if !ok || !d.IsStream() {
		return phttp.StreamHandler{}
	}
	return phttp.StreamHandler{
		Mode:          b.streamMode(),
		BodySchema:    b.bodySchema,
		ReturnsSchema: b.returns,
		Before:        b.streamBefore(),
		Handle: func(ctx *phttp.StreamContext) error {
			return streamHandler.handleStream(ctx)
		},
	}
}

func (b *EndpointBuilder) streamBefore() phttp.Handler {
	var handler phttp.Handler = func(ctx *phttp.Context) *phttp.Response {
		ectx := &phttp.EndpointContext{Context: ctx}
		if resp := b.validateParams(ctx, ectx); resp != nil {
			return resp
		}
		return b.validateQuery(ctx, ectx)
	}
	if b.security != nil {
		handler = phttp.Chain(b.security.Middleware())(handler)
	}
	if len(b.middlewares) > 0 {
		handler = phttp.Chain(b.middlewares...)(handler)
	}
	return handler
}

// logger returns the framework logger threaded in by api.Plugin at Configure
// time, falling back to the default api-scoped logger when the endpoint is built
// outside a plugin (e.g. BuildHandler called directly in a test).
func (b *EndpointBuilder) logger() *logger.Logger {
	if b.log != nil {
		return b.log
	}
	return logger.Default().Named("api")
}

func (b *EndpointBuilder) isStream() bool {
	return b.bodyStream || b.returnsStream
}

func (b *EndpointBuilder) validateReturnsStatus(finalizer string) {
	if b.returnsStatusSet && b.isStream() {
		panic(fmt.Sprintf("api.EndpointBuilder.%s: .ReturnsStatus() is not supported on stream endpoints; use .Returns()", finalizer))
	}
}

// methodCarriesRequestBody reports the methods whose declared body the endpoint
// pipeline decodes and validates. DELETE is included: the contract publishes a
// DELETE request body, and a body the provider publishes but never validates is
// a body the handler reads unchecked.
func methodCarriesRequestBody(method string) bool {
	switch strings.ToUpper(method) {
	case "POST", "PUT", "PATCH", "DELETE":
		return true
	default:
		return false
	}
}

// optionalRequestBodyMethod names the methods whose declared body may be absent
// on the wire. POST, PUT and PATCH state their payload; DELETE may carry one or
// none, and an absent one is not a malformed request.
func optionalRequestBodyMethod(method string) bool {
	return strings.ToUpper(method) == "DELETE"
}

func (b *EndpointBuilder) streamMode() phttp.StreamMode {
	switch {
	case b.bodyStream && b.returnsStream:
		return phttp.StreamModeBidirectional
	case b.bodyStream:
		return phttp.StreamModeClient
	case b.returnsStream:
		return phttp.StreamModeServer
	default:
		return ""
	}
}

func schemaTypeFrom(value any, method string) (reflect.Type, bool, *BinaryMeta) {
	switch v := value.(type) {
	case reflect.Type:
		return v, false, nil
	case StreamSchema:
		return v.Type(), true, nil
	case ByteStreamSchema:
		// Like a binary body, the placeholder type keeps "a payload is
		// declared" true; ProviderWireMeta is what every projection reads.
		return byteStreamPayloadType(), true, nil
	case BinarySchema:
		// The payload type keeps "this endpoint declares a payload" true for the
		// route facts that predate binary bodies. Nothing reads it as a JSON
		// schema: every projection short-circuits on the BinaryMeta beside it.
		return binaryPayloadType(), false, v.meta()
	default:
		panic(fmt.Sprintf("api.EndpointBuilder.%s: expected reflect.Type, api.StreamSchema, api.ByteStreamSchema or api.BinarySchema, got %T", method, value))
	}
}

// validateParams validates path parameters against the schema.
func (b *EndpointBuilder) validateParams(ctx *phttp.Context, ectx *phttp.EndpointContext) *phttp.Response {
	if b.paramsSchema == nil {
		return nil
	}
	paramsMap := make(map[string]any, len(ctx.Params))
	for k, v := range ctx.Params {
		paramsMap[k] = v
	}
	result := schema.Validate(b.paramsSchema, paramsMap, schema.WithCoerce(), schema.WithLabel("params"))
	if result.HasErrors() {
		return validationErrorResponse(result.Errors, b.firstParty)
	}
	ectx.ValidatedParams = result.Data
	return nil
}

// validateQuery validates query parameters against the schema.
func (b *EndpointBuilder) validateQuery(ctx *phttp.Context, ectx *phttp.EndpointContext) *phttp.Response {
	if b.querySchema == nil {
		return nil
	}
	queryMap := make(map[string]any)
	for k, v := range ctx.QueryParams() {
		if len(v) == 1 {
			queryMap[k] = v[0]
		} else {
			queryMap[k] = v
		}
	}
	result := schema.Validate(b.querySchema, queryMap, schema.WithCoerce(), schema.WithLabel("query"))
	if result.HasErrors() {
		return validationErrorResponse(result.Errors, b.firstParty)
	}
	ectx.ValidatedQuery = result.Data
	return nil
}

// validateBody validates the JSON request body against the schema, skipping methods that
// do not carry a body.
func (b *EndpointBuilder) validateBody(ctx *phttp.Context, ectx *phttp.EndpointContext) *phttp.Response {
	if b.bodySchema == nil || !methodCarriesRequestBody(ctx.Method) {
		return nil
	}
	if b.bodyBinary != nil {
		if b.bodyBinary.Streamed {
			if !phttp.ConcreteMediaType(ctx.Request.Header.Get("Content-Type")) {
				return phttp.ErrorResponse(perrors.New(perrors.CodeUnsupportedMediaType, "Unsupported request content type"))
			}
			if announced := ctx.Request.ContentLength; announced > b.bodyBinary.MaxBytes {
				return phttp.ErrorResponse(perrors.New(perrors.CodePayloadTooLarge, "Request body exceeds the declared bound"))
			}
			ectx.DecodedBody = ctx.Request.Body
			return nil
		}
		// A declared binary body never reaches the JSON pipeline: no decode, no
		// schema validation, no base64. The declaration's own two rules — the
		// media type and the byte bound — are the whole contract.
		data, refusal := readDeclaredBinaryBody(ctx, *b.bodyBinary)
		if refusal != nil {
			return refusal
		}
		ectx.DecodedBody = data
		return nil
	}
	raw, err := ctx.RawBody()
	if err != nil {
		return phttp.ErrorResponse(perrors.BadRequest("Invalid request body"))
	}
	if optionalRequestBodyMethod(ctx.Method) && len(bytes.TrimSpace(raw)) == 0 {
		// A DELETE may legitimately carry nothing. Only a DELETE that does send a
		// body is validated — the alternative is a published request schema the
		// handler then reads unchecked.
		return nil
	}
	if b.firstParty {
		target := reflect.New(b.bodySchema)
		strict := json.NewDecoder(bytes.NewReader(raw))
		strict.DisallowUnknownFields()
		if err := strict.Decode(target.Interface()); err != nil {
			return phttp.ErrorResponse(perrors.BadRequest("Invalid request body"))
		}
		var trailing any
		if err := strict.Decode(&trailing); err != io.EOF {
			return phttp.ErrorResponse(perrors.BadRequest("Invalid request body"))
		}
		result := schema.ValidateDecoded(b.bodySchema, target.Interface(), raw, schema.WithLabel("body"))
		if result.HasErrors() {
			return validationErrorResponse(result.Errors, true)
		}
		ectx.ValidatedBody = result.Data
		ectx.DecodedBody = target.Elem().Interface()
		return nil
	}

	var bodyMap map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&bodyMap); err != nil {
		b.logger().WarnCtx(ctx.Context(), "request body decode failed",
			slog.String("method", ctx.Method),
			slog.String("path", ctx.Path),
			slog.Any("error", err),
		)
		return phttp.JSONStatus(400, map[string]string{
			"error":   "Bad Request",
			"message": "Invalid request body",
		})
	}
	result := schema.Validate(b.bodySchema, bodyMap, schema.WithLabel("body"))
	if result.HasErrors() {
		return validationErrorResponse(result.Errors, b.firstParty)
	}
	ectx.ValidatedBody = result.Data
	return nil
}

// resolveInjections resolves DI tokens registered via .Inject(name, token).
func (b *EndpointBuilder) resolveInjections(ctx *phttp.Context, ectx *phttp.EndpointContext) *phttp.Response {
	for name, token := range b.injectTokens {
		val, err := inject.Resolve[any](ctx.Context(), token)
		if err != nil {
			b.logger().ErrorCtx(ctx.Context(), "DI resolution failed", err,
				slog.String("token", name),
				slog.String("method", ctx.Method),
				slog.String("path", ctx.Path),
			)
			// Carry the real DI failure onto the request's terminal record; the
			// Logging middleware reads it back via logger.RequestError.
			logger.SetRequestError(ctx.Context(), err)
			return phttp.InternalError("Internal Server Error")
		}
		ectx.Injected[name] = val
	}
	return nil
}

func validationErrorResponse(errors []schema.FieldError, firstParty bool) *phttp.Response {
	if firstParty {
		return phttp.ErrorResponse(perrors.BadRequest("Validation failed"))
	}
	return phttp.JSONStatus(400, map[string]any{
		"error":   "Bad Request",
		"message": "Validation failed",
		"details": errors,
	})
}
