package telemetry

import (
	"context"
	"net/http"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"go.putnami.dev/client"
)

const generatedClientInstrumentation = "go.putnami.dev/client"

type generatedClientTelemetry struct {
	tracer          trace.Tracer
	propagator      propagation.TextMapPropagator
	calls           metric.Int64Counter
	attempts        metric.Int64Counter
	callDuration    metric.Float64Histogram
	attemptDuration metric.Float64Histogram
	authDuration    metric.Float64Histogram
	staleServed     metric.Int64Counter
}

var _ client.ServiceCacheTelemetry = (*generatedClientTelemetry)(nil)

func newGeneratedClientTelemetry(
	tracerProvider trace.TracerProvider,
	meterProvider metric.MeterProvider,
	propagator propagation.TextMapPropagator,
) (*generatedClientTelemetry, error) {
	meter := meterProvider.Meter(generatedClientInstrumentation)
	calls, err := meter.Int64Counter("rpc.client.calls", metric.WithDescription("Completed generated service calls"))
	if err != nil {
		return nil, err
	}
	attempts, err := meter.Int64Counter("rpc.client.attempts", metric.WithDescription("Generated service call attempts"))
	if err != nil {
		return nil, err
	}
	callDuration, err := meter.Float64Histogram("rpc.client.duration", metric.WithDescription("Generated service call duration"), metric.WithUnit("ms"))
	if err != nil {
		return nil, err
	}
	attemptDuration, err := meter.Float64Histogram("rpc.client.attempt.duration", metric.WithDescription("Generated service attempt duration"), metric.WithUnit("ms"))
	if err != nil {
		return nil, err
	}
	authDuration, err := meter.Float64Histogram("rpc.client.auth.duration", metric.WithDescription("Generated service credential acquisition duration"), metric.WithUnit("ms"))
	if err != nil {
		return nil, err
	}
	staleServed, err := meter.Int64Counter("rpc.client.cache.stale_served",
		metric.WithDescription("Stored generated service answers returned after the provider call failed"))
	if err != nil {
		return nil, err
	}
	return &generatedClientTelemetry{
		tracer:          tracerProvider.Tracer(generatedClientInstrumentation),
		propagator:      propagator,
		calls:           calls,
		attempts:        attempts,
		callDuration:    callDuration,
		attemptDuration: attemptDuration,
		authDuration:    authDuration,
		staleServed:     staleServed,
	}, nil
}

// ServiceCacheStaleServed counts one stored answer a generated client returned
// because the provider call failed inside the operation's declared stale
// window. The series carries the call identity only: the age is in the log
// line the client runtime writes, not in a metric attribute.
func (telemetry *generatedClientTelemetry) ServiceCacheStaleServed(ctx context.Context, info client.ServiceCallInfo, _ time.Duration) {
	telemetry.staleServed.Add(ctx, 1, metric.WithAttributes(serviceCallAttributes(info)...))
}

func (telemetry *generatedClientTelemetry) StartServiceCall(ctx context.Context, info client.ServiceCallInfo) (context.Context, func(client.ServiceCallResult)) {
	started := time.Now()
	ctx, span := telemetry.tracer.Start(ctx, info.OperationID,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(serviceCallAttributes(info)...),
	)
	var once sync.Once
	return ctx, func(result client.ServiceCallResult) {
		once.Do(func() {
			attrs := append(serviceCallAttributes(info), serviceResultAttributes(result.StatusCode, result.Code)...)
			span.SetAttributes(attrs...)
			if result.Code != "" {
				span.SetStatus(codes.Error, result.Code)
			} else {
				span.SetStatus(codes.Ok, "")
			}
			span.End()
			options := metric.WithAttributes(attrs...)
			telemetry.calls.Add(ctx, 1, options)
			telemetry.callDuration.Record(ctx, durationMillis(time.Since(started)), options)
		})
	}
}

func (telemetry *generatedClientTelemetry) StartServiceAttempt(ctx context.Context, info client.ServiceAttemptInfo) (context.Context, func(client.ServiceAttemptResult)) {
	started := time.Now()
	attrs := append(serviceCallAttributes(info.ServiceCallInfo), attribute.Int("rpc.client.attempt", info.Attempt))
	ctx, span := telemetry.tracer.Start(ctx, info.OperationID+" attempt",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attrs...),
	)
	var once sync.Once
	return ctx, func(result client.ServiceAttemptResult) {
		once.Do(func() {
			metricAttrs := append([]attribute.KeyValue(nil), attrs...)
			metricAttrs = append(metricAttrs, serviceResultAttributes(result.StatusCode, result.Code)...)
			spanAttrs := append(append([]attribute.KeyValue(nil), metricAttrs...),
				attribute.Float64("rpc.client.auth.duration_ms", durationMillis(result.AuthDuration)))
			span.SetAttributes(spanAttrs...)
			if result.Code != "" {
				span.SetStatus(codes.Error, result.Code)
			} else {
				span.SetStatus(codes.Ok, "")
			}
			span.End()
			options := metric.WithAttributes(metricAttrs...)
			telemetry.attempts.Add(ctx, 1, options)
			telemetry.attemptDuration.Record(ctx, durationMillis(time.Since(started)), options)
			telemetry.authDuration.Record(ctx, durationMillis(result.AuthDuration), options)
		})
	}
}

func (telemetry *generatedClientTelemetry) InjectServiceContext(ctx context.Context, headers http.Header) {
	telemetry.propagator.Inject(ctx, propagation.HeaderCarrier(headers))
}

func serviceCallAttributes(info client.ServiceCallInfo) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("rpc.system", "putnami"),
		attribute.String("rpc.service", info.ServiceID),
		attribute.String("rpc.method", info.OperationID),
		attribute.String("network.protocol.name", info.Protocol),
	}
}

func serviceResultAttributes(status int, code string) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, 2)
	if status != 0 {
		attrs = append(attrs, attribute.Int("http.response.status_code", status))
	}
	if code != "" {
		attrs = append(attrs, attribute.String("error.type", code))
	}
	return attrs
}
