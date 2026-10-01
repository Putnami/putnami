package telemetry

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"go.putnami.dev/errors"
)

// errorBridge connects the errors package to OpenTelemetry.
// It increments an error counter on every error creation and provides
// a structured span error helper.
type errorBridge struct {
	counter metric.Int64Counter
}

// initErrorBridge registers an errors.OnError hook that increments
// the errors_total counter with code, category, and source labels.
// Called from Plugin.Provides after the MeterProvider is set.
//
// It returns the deregister function from errors.OnError so the plugin can
// remove the hook on Stop. Without this, the hook outlives the plugin: it keeps
// firing into a shut-down MeterProvider, and repeated setup (restart, tests,
// multiple instances) accumulates hooks and double-counts errors_total. The
// returned function is always non-nil and safe to call even when bridge setup
// was skipped.
func initErrorBridge(meter metric.Meter) func() {
	counter, err := meter.Int64Counter("errors_total",
		metric.WithDescription("Total structured errors created"),
	)
	if err != nil {
		// Meter creation failed — skip bridge silently.
		return func() {}
	}

	bridge := &errorBridge{counter: counter}
	return errors.OnError(bridge.onError)
}

func (b *errorBridge) onError(err *errors.Error) {
	b.counter.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("error.code", string(err.Code())),
		attribute.String("error.category", string(err.Category())),
		attribute.String("error.source", err.Source()),
	))
}

// SetSpanErrorStructured records a structured error on a span, adding
// code, category, and retryable as span attributes alongside the standard
// RecordError / SetStatus calls.
func SetSpanErrorStructured(span trace.Span, err error) {
	if err == nil {
		return
	}

	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())

	e := errors.GetError(err)
	if e == nil {
		return
	}

	attrs := []attribute.KeyValue{
		attribute.String("error.code", string(e.Code())),
		attribute.String("error.category", string(e.Category())),
		attribute.Bool("error.retryable", e.IsRetryable()),
	}
	if e.Source() != "" {
		attrs = append(attrs, attribute.String("error.source", e.Source()))
	}
	span.SetAttributes(attrs...)
}
