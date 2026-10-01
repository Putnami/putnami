package telemetry

import (
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"

	putnami_http "go.putnami.dev/http"
	"go.putnami.dev/logger"
)

// HTTPMiddleware returns an HTTP middleware that traces each request and records
// metrics (request count, duration histogram).
//
//	server.Use(telemetry.HTTPMiddleware("my-service"))
func HTTPMiddleware(serviceName string) putnami_http.Middleware {
	tracer := Tracer(serviceName)
	meter := Meter(serviceName)
	// Use the framework logger (matching plugin.go) so these diagnostics carry
	// the "telemetry" logger name and route through the configured sink, rather
	// than going straight to log/slog and bypassing the framework's conventions.
	log := logger.Default().Named("telemetry")

	requestCount, err := meter.Int64Counter("http.server.request_count",
		metric.WithDescription("Total HTTP requests"),
	)
	if err != nil {
		log.Error("telemetry: failed to create request_count counter, metrics will be unavailable", err)
	}
	requestDuration, err := meter.Float64Histogram("http.server.duration",
		metric.WithDescription("HTTP request duration in milliseconds"),
		metric.WithUnit("ms"),
	)
	if err != nil {
		log.Error("telemetry: failed to create duration histogram, metrics will be unavailable", err)
	}

	return func(ctx *putnami_http.Context, next func() *putnami_http.Response) *putnami_http.Response {
		// Extract W3C trace context from incoming headers for distributed trace correlation
		propagator := otel.GetTextMapPropagator()
		parentCtx := propagator.Extract(ctx.Request.Context(), propagation.HeaderCarrier(ctx.Request.Header))

		spanCtx, span := tracer.Start(parentCtx, fmt.Sprintf("%s %s", ctx.Method, ctx.Route))
		// End the span unconditionally so a downstream panic can never orphan it
		// in the batch processor (the panic recovery below runs first and marks
		// the span errored before this defer ends it).
		defer span.End()
		ctx.Request = ctx.Request.WithContext(spanCtx)

		span.SetAttributes(
			attribute.String("http.method", ctx.Method),
			attribute.String("http.route", ctx.Route),
			attribute.String("http.target", ctx.Path),
		)

		start := time.Now()

		// A downstream middleware/handler may panic. Recover here so the span is
		// always marked errored and the request metrics are still recorded, then
		// re-panic so the framework's own recovery middleware keeps its behavior
		// (e.g. emitting a 500). Without this, the panic would unwind past the
		// status/metrics code below and leave the span never ended.
		defer func() {
			if r := recover(); r != nil {
				duration := durationMillis(time.Since(start))
				span.SetAttributes(
					attribute.Int("http.status_code", 500),
					attribute.Bool("http.panic", true),
				)
				span.SetStatus(codes.Error, fmt.Sprintf("panic: %v", r))
				recordMetrics(ctx, requestCount, requestDuration, 500, duration)
				panic(r)
			}
		}()

		resp := next()
		duration := durationMillis(time.Since(start))

		statusCode := 200
		if resp != nil {
			statusCode = resp.Status
		}

		span.SetAttributes(attribute.Int("http.status_code", statusCode))

		if statusCode >= 500 {
			span.SetStatus(codes.Error, fmt.Sprintf("HTTP %d", statusCode))
		} else {
			span.SetStatus(codes.Ok, "")
		}

		recordMetrics(ctx, requestCount, requestDuration, statusCode, duration)

		return resp
	}
}

// recordMetrics records the request-count and duration instruments for a single
// request. It is nil-safe — instruments may be nil if their creation failed.
func recordMetrics(
	ctx *putnami_http.Context,
	requestCount metric.Int64Counter,
	requestDuration metric.Float64Histogram,
	statusCode int,
	duration float64,
) {
	if requestCount == nil && requestDuration == nil {
		return
	}
	attrs := metric.WithAttributes(
		attribute.String("http.method", ctx.Method),
		attribute.String("http.route", ctx.Route),
		attribute.Int("http.status_code", statusCode),
	)
	if requestCount != nil {
		requestCount.Add(ctx.Request.Context(), 1, attrs)
	}
	if requestDuration != nil {
		requestDuration.Record(ctx.Request.Context(), duration, attrs)
	}
}

// durationMillis converts an elapsed duration to fractional milliseconds, the
// unit declared on the http.server.duration histogram ("ms"). Dividing the
// nanosecond-resolution duration preserves sub-millisecond precision; using
// time.Duration.Milliseconds() instead would truncate every sub-ms request to
// 0.0 (and e.g. a 2.7ms request to 2.0), collapsing low-latency endpoints into
// the zero bucket and making p50/p99 meaningless.
func durationMillis(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}
