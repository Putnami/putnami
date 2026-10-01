// Package telemetry provides OpenTelemetry integration for the Putnami Go
// framework. It exposes metrics, tracing, and health reporting as a plugin
// that plugs into the application lifecycle.
package telemetry

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"go.putnami.dev/app"
	"go.putnami.dev/client"
	"go.putnami.dev/errors"
	"go.putnami.dev/inject"
	"go.putnami.dev/logger"
)

// Error codes for the telemetry package.
const (
	CodeShutdown  errors.Code = "telemetry.shutdown"
	CodeConfigure errors.Code = "telemetry.configure"
	// CodeTelemetryExport marks failures on the export path to the collector —
	// including resolving the credential a push authenticates with. Exporters
	// treat any error carrying it as a collector failure: drop the batch, never
	// affect the workload.
	CodeTelemetryExport errors.Code = "telemetry.export"
)

// Config configures the telemetry plugin.
type Config struct {
	// ServiceName is the name reported in traces and metrics.
	ServiceName string

	// ServiceVersion is the version reported in traces and metrics.
	ServiceVersion string

	// TraceExporter is an optional span exporter. If nil, tracing is disabled.
	TraceExporter sdktrace.SpanExporter

	// MetricReader is an optional metric reader. If nil, metrics are disabled.
	MetricReader sdkmetric.Reader

	// OTLP, when set, wires the built-in OTLP/JSON-over-HTTP exporters: a
	// periodic metric reader pushing to <Endpoint>/v1/metrics and a span
	// exporter pushing to <Endpoint>/v1/traces. It fills MetricReader and
	// TraceExporter only when they are nil, so an explicitly supplied reader or
	// exporter still wins. Logs are opt-in via NewOTLPLogSink on the logger.
	OTLP *OTLPConfig

	// TraceSampleRate controls the sampling ratio (0.0–1.0). A nil pointer means
	// "unset" and defaults to 1.0 (sample everything). An explicit value is
	// honored as-is, so SampleRate(0) samples nothing — distinguishing a
	// deliberate "off" from the unset default, which a bare float64 zero value
	// could not. Use the SampleRate helper to set it.
	TraceSampleRate *float64

	// ShutdownTimeout is the maximum time to wait for exporters to flush.
	// Defaults to 5 seconds.
	ShutdownTimeout time.Duration
}

// SampleRate returns a pointer to rate for use as Config.TraceSampleRate.
// SampleRate(0) samples nothing; leaving TraceSampleRate nil samples everything
// (the default).
func SampleRate(rate float64) *float64 { return &rate }

func (c Config) withDefaults() Config {
	if c.TraceSampleRate == nil {
		c.TraceSampleRate = SampleRate(1.0)
	}
	if c.ShutdownTimeout == 0 {
		c.ShutdownTimeout = 5 * time.Second
	}
	return c
}

// resource builds the OpenTelemetry resource describing this service from the
// configured ServiceName/ServiceVersion. It merges them onto the SDK default
// resource so telemetry.sdk.* attributes are preserved. Returns nil when
// neither field is set, leaving the providers' default resource untouched.
//
// Without this, both fields were silently dropped and all spans/metrics were
// emitted under the default unknown_service resource — unusable for the
// per-service attribution every tracing/metrics backend relies on.
func (c Config) resource() *resource.Resource {
	var attrs []attribute.KeyValue
	if c.ServiceName != "" {
		attrs = append(attrs, attribute.String("service.name", c.ServiceName))
	}
	if c.ServiceVersion != "" {
		attrs = append(attrs, attribute.String("service.version", c.ServiceVersion))
	}
	if len(attrs) == 0 {
		return nil
	}
	merged, err := resource.Merge(resource.Default(), resource.NewSchemaless(attrs...))
	if err != nil {
		// Schema URLs conflicted; fall back to the service attributes alone
		// rather than dropping them.
		return resource.NewSchemaless(attrs...)
	}
	return merged
}

// Plugin integrates OpenTelemetry into the application lifecycle.
// It registers a TracerProvider and MeterProvider as DI singletons.
//
//	app.New("my-service").
//	    Use(telemetry.NewPlugin(telemetry.Config{
//	        ServiceName: "my-service",
//	        TraceExporter: myExporter,
//	        MetricReader:  myReader,
//	    }))
type Plugin struct {
	cfg                Config
	tp                 *sdktrace.TracerProvider
	mp                 *sdkmetric.MeterProvider
	log                *logger.Logger
	removeErrorBridge  func()
	removeClientBridge func()
	autoTraceExporter  sdktrace.SpanExporter
	autoMetricReader   sdkmetric.Reader

	mu           sync.RWMutex
	configured   bool
	configureErr error
}

// NewPlugin creates a new telemetry plugin.
func NewPlugin(cfg Config) *Plugin {
	return &Plugin{
		cfg: cfg.withDefaults(),
		log: logger.Default().Named("telemetry"),
	}
}

// Name returns the plugin name.
func (p *Plugin) Name() string { return "telemetry" }

// Provides implements app.Provider. It declares the DI registrations for the
// tracer and meter providers and nothing else — building the providers,
// installing the OTel globals, wiring the OTLP exporters, and registering the
// error bridge are side effects that belong to the configure phase, so they
// live in Configure.
//
// The registrations are lazy factories on purpose. The app auto-wire phase may
// resolve them before Configure runs, and OTel's provider interfaces are sealed
// so they cannot be proxied; configure first so DI never caches a typed nil
// p.tp/p.mp value.
func (p *Plugin) Provides() []inject.Registration {
	return []inject.Registration{
		inject.Provide(
			inject.TokenOf[trace.TracerProvider](),
			func(_ inject.Resolver) (any, error) {
				if err := p.Configure(context.Background(), nil); err != nil {
					return nil, err
				}
				p.mu.RLock()
				defer p.mu.RUnlock()
				return trace.TracerProvider(p.tp), nil
			},
			inject.WithLazy(),
		),
		inject.Provide(
			inject.TokenOf[metric.MeterProvider](),
			func(_ inject.Resolver) (any, error) {
				if err := p.Configure(context.Background(), nil); err != nil {
					return nil, err
				}
				p.mu.RLock()
				defer p.mu.RUnlock()
				return metric.MeterProvider(p.mp), nil
			},
			inject.WithLazy(),
		),
	}
}

// Configure implements app.Configurer. It performs the one-time global
// initialization: it builds the tracer and meter providers, installs them as
// the OTel globals along with the W3C propagator, auto-wires the built-in
// OTLP/JSON exporters when Config.OTLP is set, and registers the error bridge.
//
// Configure is idempotent while the plugin is active, so repeated configure
// passes do not leak providers or double-register the error bridge. Stop marks
// the plugin inactive again, allowing a later app Start to build fresh providers
// and re-register the bridge.
func (p *Plugin) Configure(_ context.Context, _ *app.Module) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.configured || p.configureErr != nil {
		return p.configureErr
	}
	if err := p.configureLocked(); err != nil {
		p.configureErr = err
		return err
	}
	p.configured = true
	return nil
}

// configureLocked does the real wiring. p.mu must be held by the caller.
func (p *Plugin) configureLocked() error {
	traceExporter := p.cfg.TraceExporter
	metricReader := p.cfg.MetricReader

	// Wire the built-in OTLP/JSON exporters when configured, defaulting their
	// service identity from the plugin config. Explicit reader/exporter values
	// still win — we only fill the nil slots.
	if p.cfg.OTLP != nil {
		oc := *p.cfg.OTLP
		if oc.Endpoint == "" {
			return errors.Newf(CodeConfigure, "telemetry: OTLP.Endpoint is required when OTLP is set")
		}
		if oc.ServiceName == "" {
			oc.ServiceName = p.cfg.ServiceName
		}
		if oc.ServiceVersion == "" {
			oc.ServiceVersion = p.cfg.ServiceVersion
		}
		if metricReader == nil {
			metricReader = NewOTLPMetricReader(oc)
			p.cfg.MetricReader = metricReader
			p.autoMetricReader = metricReader
		}
		if traceExporter == nil {
			traceExporter = newOTLPSpanExporter(oc)
			p.cfg.TraceExporter = traceExporter
			p.autoTraceExporter = traceExporter
		}
	}

	// Build the service resource once and share it across both providers so
	// spans and metrics carry the same service.name/service.version.
	res := p.cfg.resource()

	// Build TracerProvider
	tpOpts := []sdktrace.TracerProviderOption{
		sdktrace.WithSampler(sdktrace.TraceIDRatioBased(*p.cfg.TraceSampleRate)),
	}
	if res != nil {
		tpOpts = append(tpOpts, sdktrace.WithResource(res))
	}
	if traceExporter != nil {
		tpOpts = append(tpOpts, sdktrace.WithBatcher(traceExporter))
	}
	p.tp = sdktrace.NewTracerProvider(tpOpts...)
	otel.SetTracerProvider(p.tp)

	// Set W3C Trace Context propagator for distributed trace correlation
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	// Build MeterProvider
	mpOpts := []sdkmetric.Option{}
	if res != nil {
		mpOpts = append(mpOpts, sdkmetric.WithResource(res))
	}
	if metricReader != nil {
		mpOpts = append(mpOpts, sdkmetric.WithReader(metricReader))
	}
	p.mp = sdkmetric.NewMeterProvider(mpOpts...)
	otel.SetMeterProvider(p.mp)

	// Wire error bridge — increments errors_total on each structured error.
	// Keep the deregister handle so Stop can remove the hook; otherwise it
	// outlives the plugin and double-counts across lifecycles.
	p.removeErrorBridge = initErrorBridge(p.mp.Meter("errors"))
	clientBridge, err := newGeneratedClientTelemetry(p.tp, p.mp, otel.GetTextMapPropagator())
	if err != nil {
		return errors.Wrapf(err, CodeConfigure, "configure generated client telemetry")
	}
	p.removeClientBridge = client.InstallServiceCallTelemetry(clientBridge)

	return nil
}

// Stop shuts down the tracer and meter providers, flushing pending data.
func (p *Plugin) Stop(ctx context.Context, _ *app.Module) error {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.ShutdownTimeout)
	defer cancel()

	p.mu.RLock()
	removeErrorBridge := p.removeErrorBridge
	removeClientBridge := p.removeClientBridge
	tp := p.tp
	mp := p.mp
	p.mu.RUnlock()

	// Remove the error-bridge hook so it stops firing into the about-to-be
	// shut-down MeterProvider and does not accumulate across lifecycles.
	if removeErrorBridge != nil {
		removeErrorBridge()
		p.mu.Lock()
		p.removeErrorBridge = nil
		p.mu.Unlock()
	}
	if removeClientBridge != nil {
		removeClientBridge()
		p.mu.Lock()
		p.removeClientBridge = nil
		p.mu.Unlock()
	}

	var errs []error

	if tp != nil {
		if err := tp.Shutdown(ctx); err != nil {
			errs = append(errs, errors.Wrapf(err, CodeShutdown, "tracer shutdown failed",
				errors.String("component", "tracer"),
			))
		}
	}

	if mp != nil {
		if err := mp.Shutdown(ctx); err != nil {
			errs = append(errs, errors.Wrapf(err, CodeShutdown, "meter shutdown failed",
				errors.String("component", "meter"),
			))
		}
	}

	p.mu.Lock()
	if p.tp == tp && p.mp == mp {
		p.configured = false
		if p.autoTraceExporter != nil {
			p.cfg.TraceExporter = nil
			p.autoTraceExporter = nil
		}
		if p.autoMetricReader != nil {
			p.cfg.MetricReader = nil
			p.autoMetricReader = nil
		}
	}
	p.mu.Unlock()

	if len(errs) > 0 {
		return errors.NewAggregate("telemetry shutdown", errs)
	}

	p.log.Debug("telemetry shut down")
	return nil
}

var (
	_ app.Plugin     = (*Plugin)(nil)
	_ app.Provider   = (*Plugin)(nil)
	_ app.Configurer = (*Plugin)(nil)
	_ app.Stopper    = (*Plugin)(nil)
)
