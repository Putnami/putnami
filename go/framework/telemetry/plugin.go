// Package telemetry provides OpenTelemetry integration for the Putnami Go
// framework. It exposes metrics, tracing, and health reporting as a plugin
// that plugs into the application lifecycle.
package telemetry

import (
	"context"
	"crypto/rand"
	"fmt"
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
	"go.putnami.dev/events"
	"go.putnami.dev/inject"
	"go.putnami.dev/logger"
	otlp "go.putnami.dev/protocol/telemetry"
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

	// TraceSampling sets the trace budget: the first Budget root traces are
	// kept, then a root is kept when Ratio picks its trace ID, with at least
	// FloorPerMinute roots kept each minute. A nil pointer means "unset" and
	// takes DefaultTraceSampling. An explicit value is honored as-is, so a
	// zero TraceSampling samples no root — distinguishing a deliberate "off"
	// from the unset default.
	//
	// Each key of the telemetry.traces config section (budget, ratio,
	// floorPerMinute) overrides its field. While it runs, the plugin reloads
	// the config sources every 30 seconds and applies a changed value without
	// a restart. A source that keeps its first load for the process, such as
	// CONFIG_DATA, or a Cloud remote source without CONFIG_SERVER_REFRESH,
	// shows no change: Sampler().Update changes the settings of a running
	// process.
	// A span with a parent follows its parent's sampled flag.
	TraceSampling *TraceSampling

	// SpanBatchDelay is the longest a completed span waits before the batcher
	// exports its batch; a batch of 512 spans, or OTEL_BSP_MAX_EXPORT_BATCH_SIZE,
	// leaves at once. When zero,
	// OTEL_BSP_SCHEDULE_DELAY in milliseconds sets it, else the protocol's 30s.
	// Shutdown exports what remains.
	SpanBatchDelay time.Duration

	// ShutdownTimeout is the maximum time to wait for exporters to flush.
	// Defaults to 5 seconds.
	ShutdownTimeout time.Duration

	// samplingRefresh is how often the plugin re-reads the telemetry.traces
	// section. Zero means DefaultSamplingRefresh.
	samplingRefresh time.Duration

	// fromEnvironment makes the resource read OTEL_SERVICE_NAME and
	// OTEL_RESOURCE_ATTRIBUTES when the providers are built, not once per
	// process as the SDK default resource does. FromEnvironment sets it.
	fromEnvironment bool
}

func (c Config) withDefaults() Config {
	if c.TraceSampling == nil {
		defaults := DefaultTraceSampling()
		c.TraceSampling = &defaults
	} else {
		explicit := *c.TraceSampling
		c.TraceSampling = &explicit
	}
	if c.samplingRefresh == 0 {
		c.samplingRefresh = DefaultSamplingRefresh
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
// Built from the environment, it also carries the environment's attributes and
// a service.instance.id unique to the process, unless the environment names
// one.
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
	if len(attrs) == 0 && !c.fromEnvironment {
		return nil
	}
	base := resource.Default()
	if c.fromEnvironment {
		// The environment is read now, so it wins over the default resource,
		// which the SDK computed once, maybe before the variables were set.
		if env, err := resource.New(context.Background(), resource.WithFromEnv()); err == nil {
			if merged, err := resource.Merge(base, env); err == nil {
				base = merged
			}
		}
		// Each process names its own instance; a non-empty value the
		// environment sets is kept.
		if v, ok := base.Set().Value(otlp.AttrServiceInstanceID); !ok || v.AsString() == "" {
			attrs = append(attrs, attribute.String(otlp.AttrServiceInstanceID, processInstanceID()))
		}
	}
	merged, err := resource.Merge(base, resource.NewSchemaless(attrs...))
	if err != nil {
		// Schema URLs conflicted; fall back to the service attributes alone
		// rather than dropping them.
		return resource.NewSchemaless(attrs...)
	}
	return merged
}

// processInstanceID returns the service.instance.id of this process: a random
// UUID drawn once, the same for every provider the process builds.
var processInstanceID = sync.OnceValue(func() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // rand.Read crashes the program rather than return an error
	}
	b[6] = b[6]&0x0f | 0x40 // version 4
	b[8] = b[8]&0x3f | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
})

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
	removeEventBridge  func()
	autoTraceExporter  sdktrace.SpanExporter
	autoMetricReader   sdkmetric.Reader
	sampler            *Sampler
	stopRefresh        context.CancelFunc
	refreshDone        chan struct{}

	// removeLogRecordsBridge unregisters the log.records callback on Stop.
	removeLogRecordsBridge func() error

	mu           sync.RWMutex
	configured   bool
	configureErr error
}

// NewPlugin creates a new telemetry plugin.
func NewPlugin(cfg Config) *Plugin {
	cfg = cfg.withDefaults()
	return &Plugin{
		cfg:     cfg,
		log:     logger.Default().Named("telemetry"),
		sampler: newSampler(*cfg.TraceSampling),
	}
}

// Sampler returns the sampler that decides the root spans of the tracer
// provider the plugin builds. Its Update tightens or loosens the trace budget
// at runtime, until the telemetry.traces config section changes.
func (p *Plugin) Sampler() *Sampler { return p.sampler }

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
	if err := p.cfg.TraceSampling.Validate(); err != nil {
		return errors.Wrapf(err, CodeConfigure, "telemetry: invalid TraceSampling")
	}
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
		sdktrace.WithSampler(newParentPolicy(p.sampler)),
	}
	if res != nil {
		tpOpts = append(tpOpts, sdktrace.WithResource(res))
	}
	if traceExporter != nil {
		tpOpts = append(tpOpts, sdktrace.WithBatcher(traceExporter,
			sdktrace.WithBatchTimeout(p.cfg.spanBatchDelay()),
			sdktrace.WithMaxExportBatchSize(spanBatchSize())))
	}
	p.tp = sdktrace.NewTracerProvider(tpOpts...)
	otel.SetTracerProvider(p.tp)

	// Set the W3C Trace Context propagator, made proxy-safe, for distributed
	// trace correlation.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		ProxySafeTraceContext{},
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
	if err := p.sampler.bindMeter(p.mp.Meter(samplingMeterName)); err != nil {
		return errors.Wrapf(err, CodeConfigure, "configure the trace decisions counter")
	}

	// Wire error bridge — increments errors_total on each structured error.
	// Keep the deregister handle so Stop can remove the hook; otherwise it
	// outlives the plugin and double-counts across lifecycles.
	p.removeErrorBridge = initErrorBridge(p.mp.Meter("errors"))
	p.removeLogRecordsBridge = initLogRecordsBridge(p.mp.Meter("logs"))
	clientBridge, err := newGeneratedClientTelemetry(p.tp, p.mp, otel.GetTextMapPropagator())
	if err != nil {
		return errors.Wrapf(err, CodeConfigure, "configure generated client telemetry")
	}
	p.removeClientBridge = client.InstallServiceCallTelemetry(clientBridge)
	// Events carry the publisher's trace, and each delivery continues it.
	p.removeEventBridge = events.InstallDeliveryTelemetry(newEventTelemetry(p.tp))

	// Last, so a Configure error leaves no refresh running: the application
	// never calls Stop after one.
	processDirectives.register(p.sampler)
	p.startSamplingRefresh()

	return nil
}

// startSamplingRefresh reads the telemetry.traces section, then re-reads it
// every samplingRefresh until Stop. p.mu must be held by the caller.
func (p *Plugin) startSamplingRefresh() {
	refresh := &samplingRefresh{sampler: p.sampler, base: *p.cfg.TraceSampling, log: p.log}
	refresh.apply(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	p.stopRefresh = cancel
	p.refreshDone = done
	go refresh.run(ctx, p.cfg.samplingRefresh, done)
}

// Stop shuts down the tracer and meter providers, flushing pending data.
func (p *Plugin) Stop(ctx context.Context, _ *app.Module) error {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.ShutdownTimeout)
	defer cancel()

	p.mu.Lock()
	stopRefresh, refreshDone := p.stopRefresh, p.refreshDone
	p.stopRefresh, p.refreshDone = nil, nil
	p.mu.Unlock()
	if stopRefresh != nil {
		stopRefresh()
		<-refreshDone
	}
	processDirectives.unregister(p.sampler)

	p.mu.RLock()
	removeErrorBridge := p.removeErrorBridge
	removeLogRecordsBridge := p.removeLogRecordsBridge
	removeClientBridge := p.removeClientBridge
	removeEventBridge := p.removeEventBridge
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
	var logRecordsErr error
	if removeLogRecordsBridge != nil {
		logRecordsErr = removeLogRecordsBridge()
		p.mu.Lock()
		p.removeLogRecordsBridge = nil
		p.mu.Unlock()
	}
	if removeClientBridge != nil {
		removeClientBridge()
		p.mu.Lock()
		p.removeClientBridge = nil
		p.mu.Unlock()
	}
	if removeEventBridge != nil {
		removeEventBridge()
		p.mu.Lock()
		p.removeEventBridge = nil
		p.mu.Unlock()
	}

	var errs []error
	if logRecordsErr != nil {
		errs = append(errs, errors.Wrapf(logRecordsErr, CodeShutdown, "log.records unregister failed",
			errors.String("component", "logs"),
		))
	}

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
