package api

import (
	"context"
	"maps"
	"reflect"
	"strings"
	"sync"

	"go.putnami.dev/app"
	"go.putnami.dev/errors"
	phttp "go.putnami.dev/http"
	"go.putnami.dev/logger"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	protofeatures "go.putnami.dev/protocol/features"
	protocolplatform "go.putnami.dev/protocol/platform"
)

// Plugin is the api orchestrator: it owns endpoint registrations, hands them off to a
// transport (Server) at Configure time, and exposes route metadata via DiscoveredRoutes
// so OpenAPI / proto / gRPC / typed-client plugins can consume them.
//
// Endpoints register through Plugin.Register; the actual transport binding happens during
// the Configure phase, which means Discovery is complete before openapi/proto/grpc plugins
// run their own Configure step.
type Plugin struct {
	server        Server
	prefix        string
	clientService *clientcontract.DocumentV1
	// protobufMethods binds "<METHOD> <path>" to the canonical protobuf method
	// identity the published descriptor declares for that route.
	protobufMethods map[string]string
	// connectEncodings lists the payload encodings a mounted Connect server
	// serves, empty when no Connect server is mounted.
	connectEncodings []clientcontract.Encoding
	pending          []EndpointDefinition
	dispatched       []EndpointDefinition
	routes           []DiscoveredRoute
	log              *logger.Logger
	mu               sync.Mutex
}

// Option configures the api.Plugin via functional options.
type Option func(*Plugin)

// WithPrefix prefixes every registered route path. The prefix is normalized with the
// platform protocol's rule (protocolplatform.NormalizePrefix): whitespace and slashes at
// either end are dropped and one leading slash is added.
func WithPrefix(prefix string) Option {
	return func(p *Plugin) {
		p.prefix = protocolplatform.NormalizePrefix(prefix)
	}
}

// New creates an api.Plugin that dispatches endpoints onto the given Server. In typical
// applications the server is `*http.ServerPlugin`; future edge / in-memory transports can
// implement Server directly without the http dependency.
//
//	httpServer := http.NewServerPlugin(http.ServerConfig{})
//	api := api.New(httpServer)
//	api.Register(api.Endpoint("GET", "/health").Handle(healthHandler))
//	app.New("myapp").Use(httpServer).Use(api).ListenAndServe()
func New(server Server, opts ...Option) *Plugin {
	p := &Plugin{
		server: server,
		log:    logger.Default().Named("api"),
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Name returns the plugin name. Implements app.Plugin.
func (p *Plugin) Name() string { return "api" }

// CapabilitySchemas exposes the configured route inventory to the app-owned
// capability emitter after Configure has completed.
func (p *Plugin) CapabilitySchemas() []app.CapabilitySchema {
	routes := p.DiscoveredRoutes()
	out := make([]app.CapabilitySchema, 0, len(routes))
	for _, route := range routes {
		out = append(out, app.CapabilitySchema{
			Name: route.Method + " " + route.Path,
			Kind: "route",
			Path: route.Path,
		})
	}
	return out
}

// CapabilityDiscoverers completes app.CapabilityInventoryContributor. Route
// discovery is represented by the concrete route schemas above.
func (*Plugin) CapabilityDiscoverers() []app.CapabilityDiscoverer { return nil }

// ContributeDesign derives the API surface from the same endpoint definitions
// used by transport binding and client generation. Feature metadata is supplied
// by the owning module through the scoped builder.
func (p *Plugin) ContributeDesign(builder *app.DesignBuilder) error {
	for _, definition := range p.Definitions() {
		method := strings.ToUpper(definition.Method())
		path := p.PrefixPath(definition.Path())
		operationID := "api.operation:" + method + ":" + path
		properties := map[string]string{
			"method":      method,
			"operationId": CanonicalOperationID(method, path),
			"path":        path,
		}
		if description := definition.Description(); description != "" {
			properties["description"] = description
		}
		if err := builder.AddNode(protofeatures.DesignNode{
			ID: operationID, Kind: protofeatures.DesignNodeAPIOperation,
			Name: method + " " + path, Properties: properties,
		}); err != nil {
			return err
		}
		if err := builder.RelateFromModule(operationID, protofeatures.DesignEdgeExposes, protofeatures.DesignAuthorityExact); err != nil {
			return err
		}
		for _, token := range definition.injectionTokens() {
			serviceID := "service:" + token.Key()
			if err := builder.AddNode(protofeatures.DesignNode{ID: serviceID, Kind: protofeatures.DesignNodeService, Name: token.Name()}); err != nil {
				return err
			}
			if err := builder.AddEdge(protofeatures.DesignEdge{From: operationID, To: serviceID, Kind: protofeatures.DesignEdgeInjects, Authority: protofeatures.DesignAuthorityExact}); err != nil {
				return err
			}
		}
		if err := contributeAPISchema(builder, operationID, "params", definition.ParamsSchema(), protofeatures.DesignEdgeAccepts); err != nil {
			return err
		}
		if err := contributeAPISchema(builder, operationID, "query", definition.QuerySchema(), protofeatures.DesignEdgeAccepts); err != nil {
			return err
		}
		if err := contributeAPISchema(builder, operationID, "body", definition.BodySchema(), protofeatures.DesignEdgeAccepts); err != nil {
			return err
		}
		if err := contributeAPISchema(builder, operationID, "response", definition.ReturnsSchema(), protofeatures.DesignEdgeReturns); err != nil {
			return err
		}
	}
	return nil
}

func contributeAPISchema(builder *app.DesignBuilder, operationID, role string, typ reflect.Type, edgeKind protofeatures.DesignEdgeKind) error {
	if typ == nil {
		return nil
	}
	for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Array {
		typ = typ.Elem()
	}
	key := typ.String()
	if typ.PkgPath() != "" && typ.Name() != "" {
		key = typ.PkgPath() + "." + typ.Name()
	}
	// The role is part of the node identity, not just the edge properties.
	schemaID := "api.schema:" + key + ":" + role
	if err := builder.AddNode(protofeatures.DesignNode{
		ID: schemaID, Kind: protofeatures.DesignNodeAPISchema, Name: typ.String(),
		Properties: map[string]string{"goType": key, "role": role},
	}); err != nil {
		return err
	}
	return builder.AddEdge(protofeatures.DesignEdge{
		From: operationID, To: schemaID, Kind: edgeKind, Authority: protofeatures.DesignAuthorityExact,
		Properties: map[string]string{"role": role},
	})
}

// Prefix returns the configured route prefix (without trailing slash).
func (p *Plugin) Prefix() string { return p.prefix }

// ClientServiceContract returns the first-party client contract declared with
// [WithClientService]. The result is a defensive copy so callers cannot mutate
// the provider declaration while OpenAPI and client generation read it.
func (p *Plugin) ClientServiceContract() *clientcontract.DocumentV1 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return cloneClientDocument(p.clientService)
}

// PublishClientProtobuf joins the exact generated proto wire shape to this
// provider's first-party client contract. Proto plugins call it during
// Configure; it is a no-op for providers that did not opt into first-party
// generation with WithClientService.
//
// The descriptor and the route binding travel together on purpose: a contract
// that published one without the other would either name a protobuf method no
// descriptor declares, or declare methods no route reaches.
func (p *Plugin) PublishClientProtobuf(projection ClientProtobufProjection) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.clientService == nil {
		return
	}
	p.clientService.Protobuf = cloneProtobufDescriptor(projection.Descriptor)
	p.protobufMethods = cloneRouteMethods(projection.RouteMethods)
}

// ClientProtobufMethods returns the published route → protobuf method binding.
// The result is a defensive copy: the contract projection reads it while the
// provider is still configuring sibling plugins.
func (p *Plugin) ClientProtobufMethods() map[string]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return cloneRouteMethods(p.protobufMethods)
}

// PublishClientConnectTransport records that a Connect server is mounted on this
// provider and which payload encodings it actually serves. The contract
// advertises a Connect transport only for a provider that published both this
// and a protobuf projection: announcing Connect without a mounted bridge would
// hand every generated client a URL nothing answers.
func (p *Plugin) PublishClientConnectTransport(encodings []clientcontract.Encoding) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.connectEncodings = append([]clientcontract.Encoding(nil), encodings...)
}

// ClientConnectEncodings returns the payload encodings the mounted Connect
// server serves, in the provider's declared preference order.
func (p *Plugin) ClientConnectEncodings() []clientcontract.Encoding {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]clientcontract.Encoding(nil), p.connectEncodings...)
}

// Register queues an endpoint for binding. The actual transport binding and route metadata
// capture happen during Configure, after the DI container is built.
//
// Returns the plugin for fluent chaining.
func (p *Plugin) Register(def EndpointDefinition) *Plugin {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pending = append(p.pending, def)
	return p
}

// DiscoveredRoutes returns the metadata for every registered endpoint after Configure has
// run. Empty before Configure.
func (p *Plugin) DiscoveredRoutes() []DiscoveredRoute {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]DiscoveredRoute, len(p.routes))
	copy(out, p.routes)
	return out
}

// Definitions returns the queued endpoint definitions before Configure runs, plus any
// already-dispatched ones. Used by transport adapters (gRPC bridge, typed client codegen)
// that need access to the full builder metadata, not just the flattened DiscoveredRoute.
func (p *Plugin) Definitions() []EndpointDefinition {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]EndpointDefinition, 0, len(p.pending)+len(p.dispatched))
	out = append(out, p.dispatched...)
	out = append(out, p.pending...)
	return out
}

// PrefixPath returns the full path the api plugin would register an endpoint at for the
// given source path. Useful for adapters that need to mirror the prefixing logic.
func (p *Plugin) PrefixPath(path string) string {
	return p.prefixPath(path)
}

// Configure dispatches every pending endpoint onto the transport and captures its route
// metadata. Implements app.Configurer.
func (p *Plugin) Configure(_ context.Context, _ *app.Module) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.server == nil {
		return nil
	}

	// Refuse a contradictory external declaration before binding anything, so
	// a failed Configure never leaves part of the API mounted.
	for _, def := range p.pending {
		if err := p.checkExternalContract(def); err != nil {
			return err
		}
	}

	for _, def := range p.pending {
		// Thread the api-scoped framework logger into the endpoint so request-path
		// logging (body decode / DI resolution failures) flows through the
		// configured sinks instead of stdlib slog.
		def.builder.log = p.log
		// A route an external authority owns keeps the standard pipeline: the
		// standard, not the first-party contract, decides its request and error
		// shapes.
		external := def.builder.clientOptions.IsExternal()
		def.builder.firstParty = p.clientService != nil && !external
		path := p.prefixPath(def.Path())
		// Document-only endpoints contribute schema metadata to DiscoveredRoutes
		// but skip transport binding — the handler is mounted elsewhere (typically
		// directly via server.GET/POST). Stream-mode metadata is preserved so
		// codegen still sees the right shape.
		if def.IsDocumentOnly() {
			p.routes = append(p.routes, toDiscoveredRoute(def, path))
			p.dispatched = append(p.dispatched, def)
			continue
		}
		if def.IsStream() {
			streamServer, ok := p.server.(StreamServer)
			if !ok {
				return errors.Newf(CodeTransportUnsupported, "api: server %T does not support stream endpoints", p.server)
			}
			handler := def.BuildStreamHandler()
			// A provider-owned wire speaks its own vocabulary whether or not
			// this API declares a client contract: the upgrade is its admission.
			// A first-party provider negotiates the published service protocol
			// and admits in band; the route keeps the raw transport stream when
			// this API declares no client contract, or when an external
			// authority owns the route.
			document := p.clientService
			if external {
				document = nil
			}
			if wire := def.providerWireService(document, handler); wire != nil {
				handler.Subprotocol = wire.subprotocol
				handler.AdmitOnUpgrade = true
				handler.Serve = wire.serve
			} else if service := def.webSocketService(document, path, handler); service != nil {
				handler.Subprotocol = clientcontract.WebSocketSubprotocolV1
				handler.Serve = service.serve
			}
			handler.SSEWire = def.sseWire(document)
			streamServer.HandleStream(path, handler)
			p.routes = append(p.routes, toDiscoveredRoute(def, path))
			p.dispatched = append(p.dispatched, def)
			continue
		}

		handler := def.BuildHandler()

		// Forward injected handlers to the http server. The hook is responsible for
		// finalizing the handler against the DI container — either now (if the server
		// has already configured, which is the common Use(httpServer).Use(apiPlugin)
		// order) or queued for the server's own Configure pass.
		if ih := def.InjectedHandler(); ih != nil {
			if hookable, ok := p.server.(injectedHandlerHook); ok {
				if err := hookable.AddPendingInjectedHandler(ih); err != nil {
					return err
				}
			}
		}

		if binary := def.BodyBinary(); binary != nil {
			if limited, ok := p.server.(bodyLimitServer); ok {
				limited.HandleWithBodyLimit(def.Method(), path, handler, binary.MaxBytes)
			} else {
				p.server.Handle(def.Method(), path, handler)
			}
		} else {
			p.server.Handle(def.Method(), path, handler)
		}
		p.routes = append(p.routes, toDiscoveredRoute(def, path))
		p.dispatched = append(p.dispatched, def)
	}
	p.pending = nil
	return nil
}

// checkExternalContract refuses an External declaration that contradicts
// itself or the API: External marks a route inside a first-party contract, so
// an API that publishes none has nothing to leave the route out of.
func (p *Plugin) checkExternalContract(def EndpointDefinition) error {
	authority, err := def.builder.clientOptions.ExternalAuthority()
	if err != nil {
		return errors.Newf(CodeClientGenConfig, "api: %s %s %s", def.Method(), p.prefixPath(def.Path()), err.Error())
	}
	if authority != "" && p.clientService == nil {
		return errors.Newf(CodeClientGenConfig,
			"api: %s %s declares External %q, and this API publishes no first-party client contract; External leaves a route out of the contract api.WithClientService publishes, so declare that contract or drop External",
			def.Method(), p.prefixPath(def.Path()), authority)
	}
	return nil
}

// injectedHandlerHook is satisfied by transports that need to track *http.InjectedHandler
// instances for DI finalization. *http.ServerPlugin implements this via
// AddPendingInjectedHandler, which now finalizes the handler immediately when the DI
// container is already available (the Use(httpServer).Use(apiPlugin) order) so the
// request path never sees an unfinalised handler.
type injectedHandlerHook interface {
	AddPendingInjectedHandler(ih *phttp.InjectedHandler) error
}

func (p *Plugin) prefixPath(path string) string {
	if p.prefix == "" {
		if path == "" {
			return "/"
		}
		if !strings.HasPrefix(path, "/") {
			return "/" + path
		}
		return path
	}
	if path == "" || path == "/" {
		return p.prefix
	}
	if !strings.HasPrefix(path, "/") {
		return p.prefix + "/" + path
	}
	return p.prefix + path
}

func toDiscoveredRoute(def EndpointDefinition, fullPath string) DiscoveredRoute {
	b := def.builder
	return DiscoveredRoute{
		Method:             def.Method(),
		Path:               fullPath,
		StreamMode:         def.StreamMode(),
		Description:        b.description,
		ParamsSchema:       b.paramsSchema,
		QuerySchema:        b.querySchema,
		BodySchema:         b.bodySchema,
		BodyBinary:         cloneBinaryMeta(b.bodyBinary),
		ReturnsSchema:      b.returns,
		ReturnsBinary:      cloneBinaryMeta(b.returnsBinary),
		ProviderWire:       b.providerWire(),
		ReturnsStatus:      b.returnsStatus,
		ReturnsDescription: b.returnsDescription,
		Responses: ResponseDeclarations{
			Returns:           append([]ResponseMeta(nil), b.responses...),
			ErrorCodes:        append([]errors.Code(nil), b.errorCodes...),
			ErrorRetryability: cloneErrorPolicies(b.errorPolicies),
			ErrorDetails:      maps.Clone(b.errorDetails),
			Throws:            append([]ResponseMeta(nil), b.throws...),
		},
		Meta: EndpointMeta{
			Description:      b.description,
			SecurityOptions:  b.securityMeta,
			CorsOptions:      b.corsMeta,
			RateLimitOptions: b.rateLimitMeta,
			CacheOptions:     b.cacheMeta,
			ClientOptions:    cloneClientOperationOptions(b.clientOptions),
		},
		DocumentOnly: def.documentOnly,
	}
}

func cloneErrorPolicies(source map[errors.Code]ErrorOptions) map[errors.Code]bool {
	if source == nil {
		return nil
	}
	result := make(map[errors.Code]bool, len(source))
	for code, options := range source {
		result[code] = options.Retryable
	}
	return result
}

var _ app.DesignContributor = (*Plugin)(nil)
