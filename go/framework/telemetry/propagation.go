package telemetry

import (
	"context"
	"encoding/hex"

	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	otlp "go.putnami.dev/protocol/telemetry"
)

// ProxySafeTraceContext is the W3C Trace Context propagator, made to survive a
// proxy that rewrites the parent ID of traceparent.
//
// Inject writes traceparent and tracestate as propagation.TraceContext does,
// and copies the traceparent value into x-putnami-traceparent, a header
// proxies leave alone. Extract reads traceparent as propagation.TraceContext
// does; when x-putnami-traceparent carries the same trace ID and another
// parent ID, a proxy rewrote traceparent, and the remote parent is the one in
// x-putnami-traceparent. It takes the decision of otlp.ResolveRemoteParent,
// which the TypeScript runtime shares, over OpenTelemetry's parse of
// traceparent: that parse also accepts a later traceparent version, which
// ResolveRemoteParent and the TypeScript runtime treat as no parent. Flags are
// kept as OpenTelemetry keeps them, sampled and random.
type ProxySafeTraceContext struct {
	traceContext propagation.TraceContext
}

var _ propagation.TextMapPropagator = ProxySafeTraceContext{}

// Inject sets the span context of ctx into carrier under both headers.
func (p ProxySafeTraceContext) Inject(ctx context.Context, carrier propagation.TextMapCarrier) {
	p.traceContext.Inject(ctx, carrier)
	if value := carrier.Get(otlp.HeaderTraceparent); value != "" {
		carrier.Set(otlp.HeaderPutnamiTraceparent, value)
	}
}

// Extract reads the remote span context from carrier into a copy of ctx.
func (p ProxySafeTraceContext) Extract(ctx context.Context, carrier propagation.TextMapCarrier) context.Context {
	extracted := p.traceContext.Extract(ctx, carrier)
	remote := trace.SpanContextFromContext(extracted)
	if !remote.IsValid() || !remote.IsRemote() {
		return extracted
	}
	second, ok := otlp.ParseTraceparent(carrier.Get(otlp.HeaderPutnamiTraceparent))
	if !ok || second.TraceID != remote.TraceID().String() || second.SpanID == remote.SpanID().String() {
		return extracted
	}
	var spanID trace.SpanID
	var flags [1]byte
	if _, err := hex.Decode(spanID[:], []byte(second.SpanID)); err != nil {
		return extracted
	}
	if _, err := hex.Decode(flags[:], []byte(second.TraceFlags)); err != nil {
		return extracted
	}
	parent := remote.WithSpanID(spanID).WithTraceFlags(trace.TraceFlags(flags[0]) & (trace.FlagsSampled | trace.FlagsRandom))
	return trace.ContextWithRemoteSpanContext(extracted, parent)
}

// Fields names the headers Inject sets.
func (p ProxySafeTraceContext) Fields() []string {
	return append(p.traceContext.Fields(), otlp.HeaderPutnamiTraceparent)
}
