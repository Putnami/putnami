package telemetry

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Tracer returns a named tracer from the global TracerProvider.
//
//	tracer := telemetry.Tracer("my-service")
//	ctx, span := tracer.Start(ctx, "operation")
//	defer span.End()
func Tracer(name string, opts ...trace.TracerOption) trace.Tracer {
	return otel.Tracer(name, opts...)
}

// StartSpan is a convenience that creates a span from the global TracerProvider.
//
//	ctx, span := telemetry.StartSpan(ctx, "my-service", "handleRequest")
//	defer span.End()
func StartSpan(ctx context.Context, tracerName, spanName string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	return Tracer(tracerName).Start(ctx, spanName, opts...)
}

// SpanFromContext returns the current span from the context, or a no-op span.
func SpanFromContext(ctx context.Context) trace.Span {
	return trace.SpanFromContext(ctx)
}

// SetSpanError marks the current span as errored and records the error.
func SetSpanError(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
}

// SetSpanOK marks the current span as successful.
func SetSpanOK(span trace.Span) {
	span.SetStatus(codes.Ok, "")
}

// SpanAttrs is a convenience for creating span attributes.
//
//	telemetry.SpanAttrs(span, "user.id", userID, "request.method", "GET")
func SpanAttrs(span trace.Span, keyValues ...string) {
	if len(keyValues)%2 != 0 {
		return
	}
	attrs := make([]attribute.KeyValue, 0, len(keyValues)/2)
	for i := 0; i < len(keyValues); i += 2 {
		attrs = append(attrs, attribute.String(keyValues[i], keyValues[i+1]))
	}
	span.SetAttributes(attrs...)
}

// TraceIDFromContext extracts the trace ID from the context as a hex string.
// Returns an empty string if no span is active.
func TraceIDFromContext(ctx context.Context) string {
	span := trace.SpanFromContext(ctx)
	sc := span.SpanContext()
	if sc.HasTraceID() {
		return sc.TraceID().String()
	}
	return ""
}
