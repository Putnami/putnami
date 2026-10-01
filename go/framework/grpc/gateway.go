package grpc

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"go.putnami.dev/app"
	phttp "go.putnami.dev/http"
	"go.putnami.dev/logger"
	protocolplatform "go.putnami.dev/protocol/platform"
)

// GatewayConfig holds HTTP-gRPC gateway configuration.
type GatewayConfig struct {
	// PathPrefix is the HTTP path prefix for Connect handlers.
	// Default: "/" (handles all paths matching registered services).
	PathPrefix string
}

// GatewayPlugin bridges HTTP and gRPC using the Connect protocol.
// It mounts Connect service handlers on the HTTP server, allowing
// gRPC services to be called over HTTP/1.1 with JSON or proto encoding.
type GatewayPlugin struct {
	config   GatewayConfig
	handlers []ServiceHandler
	log      *logger.Logger
	// mounted records whether RegisterOn has been called. Handlers are only
	// reachable once RegisterOn mounts them on the HTTP server; the lifecycle
	// hooks do not mount anything themselves.
	mounted bool
}

// ServiceHandler represents a Connect service handler to be mounted.
type ServiceHandler struct {
	// Path is the base path for the service (e.g., "/package.Service/").
	Path string
	// Handler is the HTTP handler for this service.
	Handler http.Handler
}

// NewGatewayPlugin creates a new HTTP-gRPC gateway plugin.
func NewGatewayPlugin(config GatewayConfig) *GatewayPlugin {
	if config.PathPrefix == "" {
		config.PathPrefix = "/"
	}
	return &GatewayPlugin{
		config: config,
		log:    logger.Default().Named("grpc.gateway"),
	}
}

// Name returns the plugin name.
func (p *GatewayPlugin) Name() string { return "grpc-gateway" }

// Mount registers a Connect service handler.
//
//	gateway.Mount(grpc.ServiceHandler{
//	    Path:    userconnect.UserServiceHandler,  // from generated connect code
//	    Handler: connectHandler,
//	})
func (p *GatewayPlugin) Mount(handler ServiceHandler) *GatewayPlugin {
	p.handlers = append(p.handlers, handler)
	return p
}

// MountHandler is a convenience for mounting a path/handler pair directly.
//
//	path, handler := userconnect.NewUserServiceHandler(svc)
//	gateway.MountHandler(path, handler)
func (p *GatewayPlugin) MountHandler(path string, handler http.Handler) *GatewayPlugin {
	return p.Mount(ServiceHandler{Path: path, Handler: handler})
}

// Configure implements the plugin lifecycle.
func (p *GatewayPlugin) Configure(_ context.Context, _ *app.Module) error {
	return nil
}

// Start implements the plugin lifecycle. The gateway mounts its handlers via
// RegisterOn(httpServer), not as part of the lifecycle, so adding it with
// Use() alone registers no routes. Rather than log a misleading "started"
// message, warn loudly when there are handlers that were never mounted — that
// is otherwise a silent failure surfacing only as 404s in production.
func (p *GatewayPlugin) Start(_ context.Context, _ *app.Module) error {
	switch {
	case len(p.handlers) == 0:
		// Nothing to mount.
	case !p.mounted:
		p.log.Warn(fmt.Sprintf(
			"grpc gateway has %d service handler(s) but RegisterOn(httpServer) was never called; "+
				"these services are NOT reachable — call gateway.RegisterOn(httpServer)",
			len(p.handlers)))
	default:
		p.log.Debug(fmt.Sprintf("grpc gateway mounted %d service handler(s)", len(p.handlers)))
	}
	return nil
}

// Stop implements the plugin lifecycle.
func (p *GatewayPlugin) Stop(_ context.Context, _ *app.Module) error {
	return nil
}

// RegisterOn mounts the Connect handlers on the HTTP server.
// The gateway mounts each service as a prefix mount that forwards every request
// under the service subtree to the appropriate Connect handler. Handlers are
// mounted under the configured PathPrefix, so a non-default prefix routes
// services at "<PathPrefix>/<package.Service>/<method>".
func (p *GatewayPlugin) RegisterOn(server *phttp.ServerPlugin) {
	prefix := protocolplatform.NormalizePrefix(p.config.PathPrefix)
	for _, h := range p.handlers {
		handler := h
		// Mount the service subtree at a "/"-terminated prefix so the route
		// inventory can represent it as a MatchPrefix / static-mount route; the
		// HTTP router still matches the whole subtree ("<mount>*" is byte-identical
		// to the old "<path>/*" wildcard).
		mount := prefix + "/" + strings.Trim(handler.Path, "/") + "/"
		server.RouteMount("POST", mount, connectBridge(handler.Handler))
	}
	p.mounted = true
}

// connectBridge adapts an http.Handler to a phttp.Handler by writing
// directly to the response writer. This is necessary because Connect
// handlers manage their own serialization.
func connectBridge(h http.Handler) phttp.Handler {
	return func(ctx *phttp.Context) *phttp.Response {
		// Let the Connect handler write directly
		h.ServeHTTP(ctx.Writer, ctx.Request)
		// Return nil since the Connect handler already wrote the response
		return nil
	}
}
