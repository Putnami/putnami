// Package grpc provides a gRPC server plugin for the Putnami Go framework.
// It integrates with the application lifecycle and DI container, supporting
// both native gRPC and Connect protocol transports.
package grpc

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"time"

	"go.putnami.dev/app"
	"go.putnami.dev/errors"
	"go.putnami.dev/inject"
	"go.putnami.dev/logger"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
)

// Conservative transport-safety defaults applied when the matching Config
// field is left zero. grpc-go ships none of these for connection lifetime
// or stream count, so without them connections live forever and a single
// client can open unbounded streams — a connection-exhaustion / slow-client
// DoS surface that the sibling HTTP server already closes by default.
const (
	defaultMaxConnectionIdle     = 15 * time.Minute
	defaultMaxConnectionAge      = 30 * time.Minute
	defaultMaxConnectionAgeGrace = 5 * time.Minute
	defaultKeepaliveTime         = 2 * time.Minute
	defaultKeepaliveTimeout      = 20 * time.Second
	defaultMinClientPingInterval = 30 * time.Second
	defaultMaxConcurrentStreams  = uint32(1000)
	defaultShutdownTimeout       = 10 * time.Second
)

// Config holds gRPC server configuration. The keepalive and concurrency
// fields mirror the HTTP server's secure-by-default timeouts: each is
// optional and falls back to a conservative default when left zero, so a
// server is hardened out of the box without any extra wiring. An explicit
// WithServerOption(grpc.KeepaliveParams(...)) still overrides these.
type Config struct {
	Port int `json:"port" default:"9090" env:"GRPC_PORT"`

	// MaxConnectionIdle closes a connection after this much time with no
	// active RPCs, reaping idle clients. Zero uses defaultMaxConnectionIdle.
	MaxConnectionIdle time.Duration `json:"maxConnectionIdle"`
	// MaxConnectionAge caps a connection's total lifetime, forcing periodic
	// reconnects (and re-resolution through load balancers). Zero uses
	// defaultMaxConnectionAge.
	MaxConnectionAge time.Duration `json:"maxConnectionAge"`
	// MaxConnectionAgeGrace is the extra time in-flight RPCs may run after
	// MaxConnectionAge fires before the connection is forcibly closed. Zero
	// uses defaultMaxConnectionAgeGrace.
	MaxConnectionAgeGrace time.Duration `json:"maxConnectionAgeGrace"`
	// KeepaliveTime is how long the server waits with no activity before
	// pinging the client to confirm the transport is alive. Zero uses
	// defaultKeepaliveTime.
	KeepaliveTime time.Duration `json:"keepaliveTime"`
	// KeepaliveTimeout is how long the server waits for a ping ack before
	// declaring the connection dead. Zero uses defaultKeepaliveTimeout.
	KeepaliveTimeout time.Duration `json:"keepaliveTimeout"`
	// MinClientPingInterval is the smallest interval the server tolerates
	// between client keepalive pings; a client that pings faster is sent a
	// GOAWAY. Zero uses defaultMinClientPingInterval.
	MinClientPingInterval time.Duration `json:"minClientPingInterval"`
	// MaxConcurrentStreams caps concurrent streams per connection. Zero
	// uses defaultMaxConcurrentStreams (grpc-go itself imposes no limit).
	MaxConcurrentStreams uint32 `json:"maxConcurrentStreams"`
	// ShutdownTimeout bounds the graceful drain on Stop. GracefulStop blocks
	// until every in-flight RPC finishes; once this elapses the server is
	// force-closed so a single stuck or long-lived stream cannot hang
	// deployments and rolling restarts. Zero uses defaultShutdownTimeout
	// (mirroring the HTTP server). A shorter deadline on the passed context
	// still wins.
	ShutdownTimeout time.Duration `json:"shutdownTimeout"`
}

// keepaliveServerParameters resolves keepalive.ServerParameters from the
// Config, substituting a conservative default for every zero field.
func (c Config) keepaliveServerParameters() keepalive.ServerParameters {
	params := keepalive.ServerParameters{
		MaxConnectionIdle:     c.MaxConnectionIdle,
		MaxConnectionAge:      c.MaxConnectionAge,
		MaxConnectionAgeGrace: c.MaxConnectionAgeGrace,
		Time:                  c.KeepaliveTime,
		Timeout:               c.KeepaliveTimeout,
	}
	if params.MaxConnectionIdle == 0 {
		params.MaxConnectionIdle = defaultMaxConnectionIdle
	}
	if params.MaxConnectionAge == 0 {
		params.MaxConnectionAge = defaultMaxConnectionAge
	}
	if params.MaxConnectionAgeGrace == 0 {
		params.MaxConnectionAgeGrace = defaultMaxConnectionAgeGrace
	}
	if params.Time == 0 {
		params.Time = defaultKeepaliveTime
	}
	if params.Timeout == 0 {
		params.Timeout = defaultKeepaliveTimeout
	}
	return params
}

// keepaliveEnforcementPolicy resolves the keepalive.EnforcementPolicy that
// bounds how aggressively clients may ping. PermitWithoutStream stays true
// so clients that keepalive on idle connections are not penalized; only an
// abusive ping cadence (faster than MinTime) earns a GOAWAY.
func (c Config) keepaliveEnforcementPolicy() keepalive.EnforcementPolicy {
	minTime := c.MinClientPingInterval
	if minTime == 0 {
		minTime = defaultMinClientPingInterval
	}
	return keepalive.EnforcementPolicy{
		MinTime:             minTime,
		PermitWithoutStream: true,
	}
}

// maxConcurrentStreams resolves the per-connection stream cap.
func (c Config) maxConcurrentStreams() uint32 {
	if c.MaxConcurrentStreams == 0 {
		return defaultMaxConcurrentStreams
	}
	return c.MaxConcurrentStreams
}

// shutdownTimeout resolves the graceful-drain deadline, substituting the
// conservative default when left zero.
func (c Config) shutdownTimeout() time.Duration {
	if c.ShutdownTimeout == 0 {
		return defaultShutdownTimeout
	}
	return c.ShutdownTimeout
}

// Plugin implements the gRPC server as a framework plugin.
// It manages gRPC service registration, interceptor composition,
// and server lifecycle.
type Plugin struct {
	config        Config
	server        *grpc.Server
	opts          []grpc.ServerOption
	unaryInts     []grpc.UnaryServerInterceptor
	streamInts    []grpc.StreamServerInterceptor
	registrars    []ServiceRegistrar
	log           *logger.Logger
	enableReflect bool
}

// ServiceRegistrar is a function that registers gRPC services on the server.
// Each gRPC service package should provide a registrar function.
type ServiceRegistrar func(server *grpc.Server)

// NewPlugin creates a new gRPC server plugin.
func NewPlugin(config Config) *Plugin {
	return &Plugin{
		config:        config,
		log:           logger.Default().Named("grpc"),
		enableReflect: false,
	}
}

// Name returns the plugin name.
func (p *Plugin) Name() string { return "grpc" }

// Register adds a gRPC service registrar.
//
//	grpcPlugin.Register(func(s *grpc.Server) {
//	    pb.RegisterUserServiceServer(s, &userServiceImpl{})
//	})
func (p *Plugin) Register(registrar ServiceRegistrar) *Plugin {
	p.registrars = append(p.registrars, registrar)
	return p
}

// WithServerOption adds a gRPC server option.
func (p *Plugin) WithServerOption(opt grpc.ServerOption) *Plugin {
	p.opts = append(p.opts, opt)
	return p
}

// WithUnaryInterceptor adds a unary server interceptor.
func (p *Plugin) WithUnaryInterceptor(interceptor grpc.UnaryServerInterceptor) *Plugin {
	p.unaryInts = append(p.unaryInts, interceptor)
	return p
}

// WithStreamInterceptor adds a stream server interceptor.
func (p *Plugin) WithStreamInterceptor(interceptor grpc.StreamServerInterceptor) *Plugin {
	p.streamInts = append(p.streamInts, interceptor)
	return p
}

// WithReflection enables or disables gRPC server reflection.
// Reflection is disabled by default; call WithReflection(true) to opt in.
func (p *Plugin) WithReflection(enabled bool) *Plugin {
	p.enableReflect = enabled
	return p
}

// Configure implements the plugin lifecycle. The gRPC plugin holds no
// configure-time state: cross-cutting interceptors — including
// grpc.DIInterceptor / grpc.DIStreamInterceptor for per-request DI scoping —
// are wired explicitly by the caller via WithUnaryInterceptor /
// WithStreamInterceptor, mirroring how LoggingInterceptor and
// RecoveryInterceptor are registered.
func (p *Plugin) Configure(_ context.Context, _ *app.Module) error {
	return nil
}

// Start implements the plugin lifecycle — starts the gRPC server.
func (p *Plugin) Start(_ context.Context, _ *app.Module) error {
	// Build server options. Conservative connection-lifetime, keepalive,
	// and concurrency limits go first so an explicit WithServerOption from
	// the caller (applied later, last-wins) still overrides them — but a
	// server that configures nothing is hardened by default rather than
	// leaving connections immortal and streams-per-connection unbounded.
	opts := make([]grpc.ServerOption, 0, len(p.opts)+5)
	opts = append(opts,
		grpc.KeepaliveParams(p.config.keepaliveServerParameters()),
		grpc.KeepaliveEnforcementPolicy(p.config.keepaliveEnforcementPolicy()),
		grpc.MaxConcurrentStreams(p.config.maxConcurrentStreams()),
	)
	opts = append(opts, p.opts...)

	// Add interceptors
	if len(p.unaryInts) > 0 {
		opts = append(opts, grpc.ChainUnaryInterceptor(p.unaryInts...))
	}
	if len(p.streamInts) > 0 {
		opts = append(opts, grpc.ChainStreamInterceptor(p.streamInts...))
	}

	p.server = grpc.NewServer(opts...)

	// Register services
	for _, registrar := range p.registrars {
		registrar(p.server)
	}

	// Enable reflection
	if p.enableReflect {
		reflection.Register(p.server)
	}

	// Start listening
	addr := fmt.Sprintf(":%d", p.config.Port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return errors.Wrapf(err, CodeGrpcListen, "grpc listen failed", errors.String("addr", addr))
	}

	p.log.Info(fmt.Sprintf("gRPC server listening on %s", addr))

	go func() {
		if err := p.server.Serve(ln); err != nil {
			p.log.Error("gRPC server error", err)
		}
	}()

	return nil
}

// Stop implements the plugin lifecycle — gracefully shuts down the gRPC server.
// GracefulStop runs in the background; if the context deadline (clamped to
// ShutdownTimeout) elapses first, the server is force-closed via Stop so a
// stuck or long-lived stream cannot hang shutdown indefinitely. This mirrors
// the HTTP server's bounded drain.
func (p *Plugin) Stop(ctx context.Context, _ *app.Module) error {
	if p.server == nil {
		return nil
	}
	p.log.Debug("shutting down gRPC server")

	ctx, cancel := context.WithTimeout(ctx, p.config.shutdownTimeout())
	defer cancel()

	stopped := make(chan struct{})
	go func() {
		p.server.GracefulStop()
		close(stopped)
	}()

	select {
	case <-stopped:
		// Graceful drain completed within the deadline.
	case <-ctx.Done():
		// Drain deadline elapsed — force-close remaining RPCs. Stop unblocks
		// the in-flight GracefulStop, so wait for the goroutine to return.
		p.log.Warn("gRPC graceful shutdown timed out; forcing stop")
		p.server.Stop()
		<-stopped
	}
	return nil
}

// Server returns the underlying grpc.Server, or nil if not started.
func (p *Plugin) Server() *grpc.Server {
	return p.server
}

// --- Built-in interceptors ---

// LoggingInterceptor returns a unary interceptor that logs each RPC call.
func LoggingInterceptor(log *logger.Logger) grpc.UnaryServerInterceptor {
	if log == nil {
		log = logger.Default().Named("grpc")
	}
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		resp, err := handler(ctx, req)
		// Pass the method as a structured field rather than pre-formatting a
		// message with fmt.Sprintf. slog.String does no formatting, so a server
		// logging above DEBUG no longer pays a per-RPC format+allocation on the
		// success path — the field is only rendered by a sink if the entry is
		// actually emitted.
		if err != nil {
			log.Error("rpc failed", err, slog.String("method", info.FullMethod))
		} else {
			log.Debug("rpc handled", slog.String("method", info.FullMethod))
		}
		return resp, err
	}
}

// RecoveryInterceptor returns a unary interceptor that catches panics.
func RecoveryInterceptor(log *logger.Logger) grpc.UnaryServerInterceptor {
	if log == nil {
		log = logger.Default().Named("grpc")
	}
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (resp any, err error) {
		defer func() {
			if r := recover(); r != nil {
				panicErr := errors.Newf(CodeGrpcPanic, "panic: %v", r).WithCategory(errors.CategoryBug)
				log.Error(fmt.Sprintf("panic in RPC %s", info.FullMethod), panicErr)
				err = status.Error(codes.Internal, "internal server error")
			}
		}()
		return handler(ctx, req)
	}
}

// DIInterceptor returns a unary interceptor that creates a DI scope per request.
func DIInterceptor(cc *inject.ContainerContext) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (_ any, retErr error) {
		if cc == nil {
			return handler(ctx, req)
		}
		scope, err := cc.CreateScope()
		if err != nil {
			return handler(ctx, req)
		}
		defer func() {
			if cerr := scope.Close(); cerr != nil && retErr == nil {
				retErr = errors.Wrap(cerr, CodeGrpcScope)
			}
		}()
		scopedCtx := scope.Context(ctx)
		return handler(scopedCtx, req)
	}
}

// --- Built-in stream interceptors ---
//
// grpc-go interceptors are split by RPC kind, so the unary interceptors above
// do not run for streaming RPCs. These stream variants give streaming handlers
// the same recovery, logging, and DI scoping. RecoveryStreamInterceptor is the
// important one: grpc-go does not recover handler panics, so without it a panic
// in a streaming handler crashes the whole server process.

// LoggingStreamInterceptor returns a stream interceptor that logs each RPC.
func LoggingStreamInterceptor(log *logger.Logger) grpc.StreamServerInterceptor {
	if log == nil {
		log = logger.Default().Named("grpc")
	}
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		err := handler(srv, ss)
		if err != nil {
			log.Error("stream rpc failed", err, slog.String("method", info.FullMethod))
		} else {
			log.Debug("stream rpc handled", slog.String("method", info.FullMethod))
		}
		return err
	}
}

// RecoveryStreamInterceptor returns a stream interceptor that catches panics.
func RecoveryStreamInterceptor(log *logger.Logger) grpc.StreamServerInterceptor {
	if log == nil {
		log = logger.Default().Named("grpc")
	}
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
		defer func() {
			if r := recover(); r != nil {
				panicErr := errors.Newf(CodeGrpcPanic, "panic: %v", r).WithCategory(errors.CategoryBug)
				log.Error(fmt.Sprintf("panic in stream RPC %s", info.FullMethod), panicErr)
				err = status.Error(codes.Internal, "internal server error")
			}
		}()
		return handler(srv, ss)
	}
}

// scopedServerStream overrides Context so a streaming handler observes the
// DI-scoped context for the stream's lifetime.
type scopedServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *scopedServerStream) Context() context.Context { return s.ctx }

// DIStreamInterceptor returns a stream interceptor that creates a DI scope for
// the lifetime of the stream.
func DIStreamInterceptor(cc *inject.ContainerContext) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (retErr error) {
		if cc == nil {
			return handler(srv, ss)
		}
		scope, err := cc.CreateScope()
		if err != nil {
			return handler(srv, ss)
		}
		defer func() {
			if cerr := scope.Close(); cerr != nil && retErr == nil {
				retErr = errors.Wrap(cerr, CodeGrpcScope)
			}
		}()
		return handler(srv, &scopedServerStream{ServerStream: ss, ctx: scope.Context(ss.Context())})
	}
}
