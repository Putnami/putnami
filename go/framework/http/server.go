package http

import (
	"context"
	stderrors "errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.putnami.dev/app"
	"go.putnami.dev/errors"
	"go.putnami.dev/inject"
	"go.putnami.dev/logger"
	httproutes "go.putnami.dev/protocol/http-routes"
	runtimeproto "go.putnami.dev/protocol/runtime"
)

// defaultShutdownTimeout bounds the graceful drain when
// ServerConfig.ShutdownTimeout is left unset, mirroring the struct tag's
// documented default so both construction paths behave identically.
const defaultShutdownTimeout = 10 * time.Second

// defaultIdleTimeout bounds an idle keep-alive connection when
// ServerConfig.IdleTimeout is left unset. It exceeds the fixed 600-second
// backend keepalive timeout of Google Cloud application load balancers, so a
// proxy that pools connections to the server closes an idle one first.
const defaultIdleTimeout = 620 * time.Second

// ServerConfig holds HTTP server configuration.
type ServerConfig struct {
	// Port is the TCP port to listen on. Default: 8080. Overridden by PORT env var.
	Port int `json:"port" default:"8080" env:"PORT"`
	// ReadTimeout is the maximum duration for reading the entire request. Default: 30s.
	ReadTimeout time.Duration `json:"readTimeout" default:"30s"`
	// WriteTimeout is the maximum duration for writing the response. Default: 30s.
	WriteTimeout time.Duration `json:"writeTimeout" default:"30s"`
	// IdleTimeout is how long a keep-alive connection, HTTP/1.1 or HTTP/2 (h2c
	// included), stays open while it waits for its next request. Keep it above
	// the idle timeout of every proxy in front of the server, so the proxy
	// closes an idle connection first. Zero means the documented 620s default:
	// unlike net/http, an unset IdleTimeout never falls back to ReadTimeout.
	// Default: 620s.
	IdleTimeout time.Duration `json:"idleTimeout" default:"620s"`
	// ShutdownTimeout is the maximum duration to wait for in-flight requests
	// during graceful shutdown. Zero means the documented 10s default, so a
	// directly constructed ServerConfig drains exactly like a config-loaded one.
	// Set a negative value to drain under the caller's context deadline alone.
	ShutdownTimeout time.Duration `json:"shutdownTimeout" default:"10s"`
	// MaxBodySize is the maximum request body size in bytes. Requests exceeding this are rejected. Default: 1 MiB.
	MaxBodySize int64 `json:"maxBodySize" default:"1048576"`
	// MaxHeaderBytes is the maximum size of request headers in bytes. Default: 64 KiB.
	MaxHeaderBytes int `json:"maxHeaderBytes" default:"65536"`
	// WebSocketIdleTimeout bounds how long a hijacked WebSocket connection may
	// stay idle. The server arms a read deadline of this duration and pings the
	// client at half the interval; a connection that neither sends data nor
	// answers a ping within the window is dropped. This prevents idle/slow
	// clients from holding goroutines and file descriptors open indefinitely
	// (Server.ReadTimeout does not apply to hijacked connections). Default: 60s.
	// Set to a negative value to disable (not recommended).
	WebSocketIdleTimeout time.Duration `json:"webSocketIdleTimeout" default:"60s"`
	// StreamWriteTimeout bounds each individual SSE write. The server disables
	// the server-wide WriteTimeout for the stream's lifetime (so it does not
	// kill a long-lived stream) and instead re-arms a write deadline of this
	// duration before flushing every event. A client that stops reading trips
	// this deadline rather than blocking the handler's write indefinitely and
	// pinning a goroutine and file descriptor. Default: 30s. Set to a negative
	// value to disable (not recommended).
	StreamWriteTimeout time.Duration `json:"streamWriteTimeout" default:"30s"`
}

// ServerPlugin implements the HTTP server as a framework plugin.
// It manages route registration, middleware composition, and server lifecycle.
type ServerPlugin struct {
	config          ServerConfig
	routes          *RouteController
	routeBodyLimits map[string]int64
	httpRouteFacts  []httpRouteFact
	projectName     string
	middlewares     []Middleware
	composedChain   func(Handler) Handler
	log             *logger.Logger
	server          *http.Server
	cc              *inject.ContainerContext
	pending         []*InjectedHandler
	routesWrapped   bool
	drain           chan struct{}
	drainOnce       sync.Once
	mu              sync.RWMutex
}

// drainSignal returns the channel that closes when this server begins a
// graceful stop. Hijacked connections are outside http.Server's own drain, so a
// long-lived socket watches this to end its conversation deliberately instead
// of being cut mid-frame.
func (p *ServerPlugin) drainSignal() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.drain == nil {
		p.drain = make(chan struct{})
	}
	return p.drain
}

// beginDrain announces the graceful stop to every hijacked connection. It is
// idempotent.
func (p *ServerPlugin) beginDrain() {
	p.mu.Lock()
	if p.drain == nil {
		p.drain = make(chan struct{})
	}
	signal := p.drain
	p.mu.Unlock()
	p.drainOnce.Do(func() { close(signal) })
}

// NewServerPlugin creates a new HTTP server plugin.
func NewServerPlugin(config ServerConfig) *ServerPlugin {
	return &ServerPlugin{
		config:          config,
		routes:          newRouteController(),
		routeBodyLimits: make(map[string]int64),
		log:             logger.Default().Named("http"),
	}
}

// Name returns the plugin name.
func (p *ServerPlugin) Name() string { return "http" }

// SingleServer returns the one ServerPlugin of the module tree owner belongs
// to. A plugin that contributes routes calls it from Configure to mount itself
// when the application passed it no server with RegisterOn. plugin is the name
// of the calling plugin.
//
// A tree that holds no ServerPlugin returns a CodeNoServer error, and a tree
// that holds several returns a CodeAmbiguousServer error. Both messages name
// plugin and RegisterOn, the call that chooses a server explicitly.
func SingleServer(owner *app.Module, plugin string) (*ServerPlugin, error) {
	var servers []*ServerPlugin
	if owner != nil {
		servers = app.Collect[*ServerPlugin](owner.Root())
	}
	switch len(servers) {
	case 1:
		return servers[0], nil
	case 0:
		return nil, errors.Newf(CodeNoServer,
			"the %s plugin has no HTTP server to mount its routes on: add an http.ServerPlugin to the application, or call RegisterOn(server)",
			plugin)
	default:
		return nil, errors.Newf(CodeAmbiguousServer,
			"the %s plugin finds %d HTTP servers in the application: call RegisterOn(server) to choose one",
			plugin, len(servers))
	}
}

// Route registers a handler for the given method and path.
// handler can be a Handler (func(*Context) *Response) or an *InjectedHandler from http.Inject().
func (p *ServerPlugin) Route(method, path string, handler any, opts ...RouteOption) *ServerPlugin {
	h := p.toHandler(handler)
	cfg := routeConfig{}
	for _, opt := range opts {
		opt(&cfg)
	}
	h = applyRouteOptions(h, cfg)
	p.routes.Add(method, path, h)
	if cfg.maxBodySize > 0 {
		p.routeBodyLimits[routeBodyLimitKey(method, path)] = cfg.maxBodySize
	}
	p.httpRouteFacts = append(p.httpRouteFacts, httpRouteFact{method: method, path: path, source: routeSourceManual})
	return p
}

// RouteMount registers handler as the owner of an entire path prefix: it serves
// every request whose method matches and whose path begins with prefix. prefix
// must be absolute and end in "/". Unlike Route, the route inventory records the
// mount as a single MatchPrefix / static-mount route at prefix rather than the
// named catch-all used internally — the only shape the http-routes describe surface accepts for a mount
// that owns a subtree. At runtime the router still matches the whole subtree.
func (p *ServerPlugin) RouteMount(method, prefix string, handler any, opts ...RouteOption) *ServerPlugin {
	h := p.toHandler(handler)
	cfg := routeConfig{}
	for _, opt := range opts {
		opt(&cfg)
	}
	h = applyRouteOptions(h, cfg)
	// prefix ends in "/", so the internal named catch-all owns the subtree while
	// the public inventory records one prefix mount.
	pattern := prefix + "{mountPath...}"
	p.routes.Add(method, pattern, h)
	if cfg.maxBodySize > 0 {
		p.routeBodyLimits[routeBodyLimitKey(method, pattern)] = cfg.maxBodySize
	}
	p.httpRouteFacts = append(p.httpRouteFacts, httpRouteFact{
		method: method, path: prefix, source: routeSourceStaticMount, match: httproutes.MatchPrefix,
	})
	return p
}

// GET registers a GET handler.
func (p *ServerPlugin) GET(path string, handler any) *ServerPlugin {
	return p.Route("GET", path, handler)
}

// POST registers a POST handler.
func (p *ServerPlugin) POST(path string, handler any) *ServerPlugin {
	return p.Route("POST", path, handler)
}

// PUT registers a PUT handler.
func (p *ServerPlugin) PUT(path string, handler any) *ServerPlugin {
	return p.Route("PUT", path, handler)
}

// DELETE registers a DELETE handler.
func (p *ServerPlugin) DELETE(path string, handler any) *ServerPlugin {
	return p.Route("DELETE", path, handler)
}

// PATCH registers a PATCH handler.
func (p *ServerPlugin) PATCH(path string, handler any) *ServerPlugin {
	return p.Route("PATCH", path, handler)
}

// Handle registers a typed Handler for the given method and path. It is the structural
// counterpart to Route that satisfies the api.Server contract: an api.Plugin (or any other
// transport-agnostic consumer) can register routes without depending on the variadic
// RouteOption shape.
func (p *ServerPlugin) Handle(method, path string, handler Handler) {
	p.routes.Add(method, path, handler)
	p.httpRouteFacts = append(p.httpRouteFacts, httpRouteFact{method: method, path: path, source: routeSourceTypedAPI})
}

// HandleWithBodyLimit registers a typed API handler whose request body has its
// own byte bound. This lets a binary endpoint accept its declared payload size
// without weakening the global JSON-body guard for unrelated routes.
func (p *ServerPlugin) HandleWithBodyLimit(method, path string, handler Handler, maxBytes int64) {
	if maxBytes <= 0 {
		panic(fmt.Sprintf("http: route %s %s needs a strictly positive body limit, got %d", method, path, maxBytes))
	}
	p.Handle(method, path, handler)
	p.routeBodyLimits[routeBodyLimitKey(method, path)] = maxBytes
}

// AddPendingInjectedHandler registers an InjectedHandler for DI finalization. If the
// server plugin has not yet configured (no container yet acquired), the handler is
// queued and finalized when Configure runs. If the container is already available, the
// handler is finalized immediately so the request path never sees an unfinalised handler.
//
// Used by api.Plugin to forward handlers built by api.Endpoint().Inject(...). Because
// plugins configure in registration order, an api plugin registered after the http
// server will hit the immediate-finalization path.
func (p *ServerPlugin) AddPendingInjectedHandler(ih *InjectedHandler) error {
	if ih == nil {
		return nil
	}
	p.mu.Lock()
	p.pending = append(p.pending, ih)
	cc := p.cc
	p.mu.Unlock()
	if cc != nil {
		return ih.Finalize(cc)
	}
	return nil
}

// toHandler converts any to a Handler, tracking InjectedHandlers for finalization.
func (p *ServerPlugin) toHandler(handler any) Handler {
	switch h := handler.(type) {
	case Handler:
		return h
	case func(*Context) *Response:
		return h
	case *InjectedHandler:
		p.pending = append(p.pending, h)
		return h.Handle
	default:
		panic(fmt.Sprintf("http: unsupported handler type %T, expected Handler or *InjectedHandler", handler))
	}
}

// Use adds a middleware to the chain.
func (p *ServerPlugin) Use(mw Middleware) *ServerPlugin {
	p.middlewares = append(p.middlewares, mw)
	return p
}

// Configure implements the Configurer interface. Finalizes injected handlers
// by resolving their DI dependencies from the container.
func (p *ServerPlugin) Configure(_ context.Context, owner *app.Module) error {
	// Acquire the DI container from the owning module (set by auto-wire phase).
	if owner != nil {
		p.projectName = owner.Root().Name()
		if cc := owner.Container(); cc != nil {
			p.mu.Lock()
			p.cc = cc
			p.mu.Unlock()
		}
	}

	// Finalize injected handlers.
	// Always attempt finalization — Finalize handles nil cc gracefully
	// for context-only handlers and returns a clear error for handlers
	// that need DI deps but have no container.
	// Finalize is idempotent, so re-running Configure (e.g., after Validate)
	// safely skips already-finalized handlers.
	if len(p.pending) > 0 {
		p.mu.RLock()
		cc := p.cc
		p.mu.RUnlock()
		for _, ih := range p.pending {
			if err := ih.Finalize(cc); err != nil {
				return err
			}
		}
		p.log.Debug(fmt.Sprintf("finalized %d injected handlers", len(p.pending)))
	}

	return nil
}

// Start implements the Starter interface — starts the HTTP server.
func (p *ServerPlugin) Start(_ context.Context, owner *app.Module) error {
	startedAt := time.Now()

	// Pre-compose the middleware chain with all registered route handlers.
	p.wrapRoutesOnce()

	mux := http.NewServeMux()
	mux.HandleFunc("/", p.buildHandler())

	addr, server := p.buildServer(mux)
	p.server = server

	// Start listening
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return errors.Wrapf(err, CodeListen, "listen failed", errors.String("addr", addr))
	}

	durationMs := time.Since(startedAt).Milliseconds()
	p.log.Info(fmt.Sprintf("⚡️ listening http://localhost%s", addr),
		slog.Int64("durationMs", durationMs),
		slog.Any(runtimeproto.ReadyLogKey, readyMarker(ln, durationMs)))

	go func() {
		if err := p.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			p.log.Error("server error", err)
		}
	}()

	return nil
}

// readyMarker builds the machine-readable readiness payload attached to the
// listening log record under the reserved key runtimeproto.ReadyLogKey.
//
// A served workload's stdout is a LOG stream — the extension that spawned it
// re-emits each line as a log event — so this is how the framework reaches the
// runtime event stream it does not own: the extension's forwarder recognizes
// the key and emits the typed `ready` event
// (protocols/runtime/ready_marker.go). The human message is untouched, so the
// marker is the ONLY readiness path since B6b deleted the log probe.
//
// The port comes from the LISTENER, not from the configured address: `PORT=0`
// binds an ephemeral port, and a readiness claim carrying the requested port
// instead of the bound one would send a client to the wrong address. When the
// bound address is not addressable, the marker degrades to a workload claim —
// startup did finish — rather than emitting a server claim nobody can act on.
func readyMarker(ln net.Listener, durationMs int64) runtimeproto.ReadyData {
	data := runtimeproto.ReadyData{Target: runtimeproto.ReadyTargetWorkload, DurationMs: durationMs}
	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if !ok || tcpAddr.Port < 1 || tcpAddr.Port > 65535 {
		return data
	}
	data.Target = runtimeproto.ReadyTargetServer
	// "localhost" matches the address the log message prints and the one a
	// developer can actually open; the listener binds every interface.
	data.Endpoints = []runtimeproto.ReadyEndpoint{{
		Scheme: runtimeproto.ReadySchemeHTTP,
		Host:   "localhost",
		Port:   tcpAddr.Port,
	}}
	return runtimeproto.ReadyMarker(data)
}

func (p *ServerPlugin) wrapRoutesOnce() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.routesWrapped {
		return
	}
	chain := Chain(p.middlewares...)
	p.routes.wrapAll(chain)
	// Retain the composed chain so synthesized handlers (e.g. the OPTIONS
	// preflight fallback in buildHandler) run the same middleware stack —
	// crucially the CORS middleware — as registered routes.
	p.composedChain = chain
	p.routesWrapped = true
}

// Handler returns this server's composed request handler, with the registered
// middleware chain applied. A host that owns its own listener — an embedding
// runtime, or a test that needs a real port — mounts this instead of calling
// Start.
func (p *ServerPlugin) Handler() http.Handler {
	p.wrapRoutesOnce()
	return p.buildHandler()
}

func (p *ServerPlugin) buildHandler() http.HandlerFunc {
	maxBody := p.config.MaxBodySize
	if maxBody == 0 {
		maxBody = 1 << 20 // 1 MiB default
	}

	return func(w http.ResponseWriter, r *http.Request) {
		ctx := NewContext(w, r)

		// Match route
		matchedMethod := r.Method
		match := p.routes.lookup(r.Method, r.URL.Path)
		if match == nil {
			if r.Method == "HEAD" {
				match = p.routes.lookup("GET", r.URL.Path)
				matchedMethod = "GET"
			}
			// An OPTIONS request to a known path with no explicit OPTIONS
			// handler must still run the middleware chain so the CORS
			// middleware can answer the preflight (analogous to HEAD→GET).
			// Without this, routing is method-keyed and the preflight 404s
			// before any middleware runs, silently breaking CORS.
			if match == nil && r.Method == "OPTIONS" {
				if allowed := p.routes.allowedMethods(r.URL.Path); len(allowed) > 0 {
					match = &routeMatch[Handler]{
						Handlers: []Handler{p.preflightHandler(allowed)},
						Pattern:  r.URL.Path,
					}
				}
			}
			if match == nil {
				if err := NotFound().WriteTo(w); err != nil {
					p.log.Error("write response", err)
				}
				return
			}
		}

		ctx.Route = match.Pattern
		ctx.Params = match.Params
		bodyLimit := maxBody
		if routeLimit := p.routeBodyLimits[routeBodyLimitKey(matchedMethod, match.Pattern)]; routeLimit > 0 {
			bodyLimit = routeLimit
		}
		body := &maxBytesBody{ReadCloser: http.MaxBytesReader(w, r.Body, bodyLimit)}
		r.Body = body

		// Create scoped DI context if container is available
		p.mu.RLock()
		cc := p.cc
		p.mu.RUnlock()
		var scope *inject.DetachedScope
		var scopeCtx context.Context
		if cc != nil {
			s, err := cc.CreateScope()
			if err != nil {
				p.log.Error("create DI scope", err)
				resp := InternalError("Internal Server Error")
				if writeErr := resp.WriteTo(w); writeErr != nil {
					p.log.Error("write error response", writeErr)
				}
				return
			}
			scope = s
			scopeCtx = scope.Context(r.Context())
			ctx = ctx.WithContext(scopeCtx)
		}

		// finalize reconciles a request-scoped UnitOfWork with the handler outcome
		// (commit on success, roll back on error/panic) and then closes the scope.
		// It is a no-op when there is no DI scope, and idempotent so the panic path
		// and the normal path never double-finalize. When the handler succeeded but
		// committing its work failed it records that in commitErr so the caller can
		// downgrade the success response to a 500 rather than lie to the client.
		finalized := false
		deferScopeClose := false
		var commitErr error
		closeScope := func() {
			if scope != nil {
				if cerr := scope.Close(); cerr != nil {
					p.log.Error("close scope", cerr)
				}
			}
		}
		finalize := func(outcome error) {
			if scope == nil || finalized {
				return
			}
			finalized = true
			if ferr := scope.Finalize(scopeCtx, outcome); ferr != nil {
				p.log.Error("finalize request scope", ferr)
				if outcome == nil {
					commitErr = ferr
				}
			}
			if !deferScopeClose {
				closeScope()
			}
		}
		defer func() {
			if rec := recover(); rec != nil {
				// No middleware converted this panic to a response: roll the request
				// scope back (releasing every open transaction/connection) before
				// re-panicking so the existing recovery behavior stands.
				finalize(errRequestPanic)
				panic(rec)
			}
		}()

		// Execute pre-composed handler (middleware chain applied at Start time)
		resp := match.Handlers[0](ctx)
		if body.exceeded.Load() {
			if resp != nil && resp.stream != nil {
				if err := resp.stream.Close(); err != nil {
					p.log.Error("close refused response stream", err)
				}
			}
			resp = payloadTooLargeResponse()
		}
		if resp != nil && resp.stream != nil {
			// Commit before headers so a failed transaction still answers 500,
			// but retain scoped resources until the last body read completes.
			deferScopeClose = true
			defer closeScope()
			stream := resp.stream
			closeStream := func() {
				if err := stream.Close(); err != nil {
					p.log.Error("close response stream", err)
				}
			}
			defer closeStream()
		}
		finalize(requestOutcome(r.Context(), resp))
		if commitErr != nil {
			// The handler succeeded but committing its work failed: never report
			// success. Replace the response with a sanitized 500.
			resp = scopeFailureResponse(scopeCtx, commitErr)
		}
		if resp != nil {
			if err := resp.WriteTo(w); err != nil {
				p.log.Error("write response", err)
			}
		}
	}
}

type maxBytesBody struct {
	io.ReadCloser
	exceeded atomic.Bool
}

func (body *maxBytesBody) Read(value []byte) (int, error) {
	n, err := body.ReadCloser.Read(value)
	var maxErr *http.MaxBytesError
	if stderrors.As(err, &maxErr) {
		body.exceeded.Store(true)
	}
	return n, err
}

func payloadTooLargeResponse() *Response {
	return ErrorResponse(errors.New(errors.CodePayloadTooLarge, "Request body exceeds the declared bound"))
}

func routeBodyLimitKey(method, path string) string {
	return strings.ToUpper(method) + "\x00" + path
}

// requestOutcome classifies a handler's result for the request-scope
// transaction boundary: a canceled/timed-out request context, or a server-error
// (5xx) response, is a failure that rolls back; anything else is a success that
// commits. Client errors (4xx) commit — a 4xx is a normal, handled outcome, and
// any write the handler chose to persist alongside it (an audit row, a rate
// counter) is kept; a handler that wants a 4xx to roll back marks the unit of
// work rollback-only. A nil response (the handler wrote to the ResponseWriter
// directly) is a success unless the request context was canceled.
func requestOutcome(reqCtx context.Context, resp *Response) error {
	if err := reqCtx.Err(); err != nil {
		return err
	}
	if resp != nil && resp.Status >= http.StatusInternalServerError {
		return errServerErrorResponse
	}
	return nil
}

// preflightHandler builds the fallback OPTIONS handler for a known path that
// has no explicit OPTIONS route. It runs the composed middleware chain so a
// configured CORS middleware intercepts and emits the Access-Control-* preflight
// response; absent CORS, it answers 204 with an Allow header listing the
// registered methods plus OPTIONS.
func (p *ServerPlugin) preflightHandler(allowed []string) Handler {
	allow := strings.Join(append(allowed, "OPTIONS"), ", ")
	base := func(ctx *Context) *Response {
		ctx.SetHeader("Allow", allow)
		return NoContent()
	}
	if p.composedChain != nil {
		return p.composedChain(base)
	}
	return base
}

func (p *ServerPlugin) buildServer(handler http.Handler) (string, *http.Server) {
	port := p.config.Port
	if envPort := os.Getenv("PORT"); envPort != "" {
		if v, err := strconv.Atoi(envPort); err == nil {
			port = v
		}
	}
	addr := fmt.Sprintf(":%d", port)
	maxHeaderBytes := p.config.MaxHeaderBytes
	if maxHeaderBytes == 0 {
		maxHeaderBytes = 64 << 10 // 64 KiB
	}
	readTimeout := p.config.ReadTimeout
	if readTimeout == 0 {
		readTimeout = 30 * time.Second
	}
	writeTimeout := p.config.WriteTimeout
	if writeTimeout == 0 {
		writeTimeout = 30 * time.Second
	}
	// Always enable h2c (HTTP/2 cleartext) alongside HTTP/1.1.
	// This is required for container platforms (Cloud Run, Fargate) that
	// forward HTTP/2 without TLS to the container. HTTP/1.1 clients are
	// unaffected — the server negotiates the protocol automatically.
	var protos http.Protocols
	protos.SetHTTP1(true)
	protos.SetHTTP2(true)
	protos.SetUnencryptedHTTP2(true)

	return addr, &http.Server{
		Addr:           addr,
		Handler:        handler,
		ReadTimeout:    readTimeout,
		WriteTimeout:   writeTimeout,
		IdleTimeout:    p.config.idleTimeout(),
		MaxHeaderBytes: maxHeaderBytes,
		Protocols:      &protos,
	}
}

// Stop implements the Stopper interface — gracefully shuts down the server.
//
// The drain deadline is resolved through shutdownTimeout so an unset
// ShutdownTimeout gets the documented default instead of an already-expired
// context. A zero-duration deadline would make Shutdown return
// context.DeadlineExceeded before any in-flight request could finish, which is
// the opposite of a graceful stop.
func (p *ServerPlugin) Stop(ctx context.Context, _ *app.Module) error {
	// Hijacked WebSocket connections are not drained by http.Server.Shutdown,
	// so they are told first and end their conversations themselves.
	p.beginDrain()
	if p.server == nil {
		return nil
	}
	p.log.Debug("shutting down HTTP server")
	if timeout := p.config.shutdownTimeout(); timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	return p.server.Shutdown(ctx)
}

// shutdownTimeout resolves the graceful-drain deadline. Zero — the value a
// directly constructed ServerConfig carries, because `default:"10s"` is only
// applied by config loading — resolves to defaultShutdownTimeout. A negative
// value opts out of the framework-owned bound and drains under the caller's
// context alone.
func (c ServerConfig) shutdownTimeout() time.Duration {
	if c.ShutdownTimeout == 0 {
		return defaultShutdownTimeout
	}
	return c.ShutdownTimeout
}

// idleTimeout resolves the keep-alive idle bound. Zero — the value a directly
// constructed ServerConfig carries, because `default:"620s"` is only applied
// by config loading — resolves to defaultIdleTimeout, never to ReadTimeout.
// net/http copies the resolved value into its HTTP/2 server, so one bound
// covers HTTP/1.1 and HTTP/2 connections alike.
func (c ServerConfig) idleTimeout() time.Duration {
	if c.IdleTimeout == 0 {
		return defaultIdleTimeout
	}
	return c.IdleTimeout
}

// setContainerContext provides the DI container for per-request scoping.
// Used in tests; production code acquires the container via owner.Container() in Configure.
func (p *ServerPlugin) setContainerContext(cc *inject.ContainerContext) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cc = cc
}

// --- RouteController ---

// RouteController manages per-method routers.
type RouteController struct {
	routers map[string]*Router[Handler]
}

// newRouteController creates a new route controller.
func newRouteController() *RouteController {
	return &RouteController{
		routers: make(map[string]*Router[Handler]),
	}
}

// Add registers a handler for a method and path.
func (rc *RouteController) Add(method, path string, handler Handler) {
	r, ok := rc.routers[method]
	if !ok {
		r = NewRouter[Handler]()
		rc.routers[method] = r
	}
	r.Add(path, handler)
}

// wrapAll applies the middleware chain to all registered handlers in-place.
// This pre-composes the chain once per route so the middleware composition
// loop does not re-run per request — only the next() closure inherent to the
// Middleware signature is allocated per request.
func (rc *RouteController) wrapAll(chain func(Handler) Handler) {
	for _, r := range rc.routers {
		r.wrapAll(chain)
	}
}

// lookup finds a handler for the given method and path.
func (rc *RouteController) lookup(method, path string) *routeMatch[Handler] {
	r, ok := rc.routers[method]
	if !ok {
		return nil
	}
	return r.lookup(path)
}

// allowedMethods returns the sorted set of HTTP methods registered for path,
// across all per-method routers. It is empty when the path matches no route,
// which lets callers distinguish an unknown path (404) from a known path that
// merely lacks a handler for the requested method.
func (rc *RouteController) allowedMethods(path string) []string {
	var methods []string
	for method, r := range rc.routers {
		if r.lookup(path) != nil {
			methods = append(methods, method)
		}
	}
	sort.Strings(methods)
	return methods
}

// --- Options ---

// RouteOption configures route registration.
type RouteOption func(*routeConfig)

type routeConfig struct {
	accept      []string
	statusCode  int
	maxBodySize int64
}

// WithAccept sets acceptable content types for this route.
// Requests with Content-Type not matching any of the given types receive 415 Unsupported Media Type.
func WithAccept(types ...string) RouteOption {
	return func(c *routeConfig) {
		c.accept = types
	}
}

// WithStatusCode sets the default response status code for this route.
// If the handler returns a 200 response, the status code is replaced with this value.
// Useful for POST endpoints that should return 201 Created.
func WithStatusCode(code int) RouteOption {
	return func(c *routeConfig) {
		c.statusCode = code
	}
}

// WithMaxBodySize sets the request-body limit for one manually registered
// route without changing ServerConfig.MaxBodySize for every other endpoint.
func WithMaxBodySize(maxBytes int64) RouteOption {
	if maxBytes <= 0 {
		panic(fmt.Sprintf("http: route body limit must be strictly positive, got %d", maxBytes))
	}
	return func(c *routeConfig) {
		c.maxBodySize = maxBytes
	}
}

// applyRouteOptions wraps a handler to enforce route-level options.
func applyRouteOptions(handler Handler, cfg routeConfig) Handler {
	if len(cfg.accept) == 0 && cfg.statusCode == 0 {
		return handler
	}
	return func(ctx *Context) *Response {
		// Check Content-Type against accepted types.
		if len(cfg.accept) > 0 {
			ct := ctx.Request.Header.Get("Content-Type")
			if ct != "" && !matchesAccept(ct, cfg.accept) {
				return NewResponse(415).WithHeader("Accept", strings.Join(cfg.accept, ", "))
			}
		}
		resp := handler(ctx)
		// Apply default status code.
		if cfg.statusCode != 0 && resp != nil && resp.Status == 200 {
			resp.Status = cfg.statusCode
		}
		return resp
	}
}

// matchesAccept checks if a content type matches any of the accepted types.
func matchesAccept(contentType string, accepted []string) bool {
	// Strip parameters (e.g., "application/json; charset=utf-8" → "application/json")
	ct := strings.TrimSpace(contentType)
	if idx := strings.IndexByte(ct, ';'); idx >= 0 {
		ct = strings.TrimSpace(ct[:idx])
	}
	for _, a := range accepted {
		if strings.EqualFold(ct, a) {
			return true
		}
	}
	return false
}
