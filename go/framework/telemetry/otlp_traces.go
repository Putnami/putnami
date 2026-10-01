package telemetry

import (
	"context"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"go.putnami.dev/logger"
	otlp "go.putnami.dev/protocol/telemetry"
)

// newOTLPSpanExporter returns a SpanExporter that renders finished spans as
// OTLP/JSON and POSTs them to the collector at /v1/traces; the plugin batches
// it and flushes on shutdown.
func newOTLPSpanExporter(cfg OTLPConfig) sdktrace.SpanExporter {
	return &otlpSpanExporter{
		client: newOTLPClient(cfg),
		log:    logger.Default().Named("telemetry.otlp.traces"),
	}
}

type otlpSpanExporter struct {
	client *otlpClient
	log    *logger.Logger
}

// ExportSpans renders a batch of spans and POSTs them. Per the protocol's
// drop-on-collector-error rule, transport failures are logged and swallowed.
func (e *otlpSpanExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	if len(spans) == 0 {
		return nil
	}
	req := tracesRequest(spans)
	body, err := otlp.MarshalCanonical(req)
	if err != nil {
		return err
	}
	if err := e.client.post(ctx, otlp.PathTraces, body); err != nil {
		e.log.Warn("dropped span batch: " + err.Error())
	}
	return nil
}

func (e *otlpSpanExporter) Shutdown(context.Context) error { return nil }

// ---------------------------------------------------------------------------
// ReadOnlySpan → OTLP translation
// ---------------------------------------------------------------------------

// tracesRequest groups a span batch by instrumentation scope under one resource
// (every span in a provider shares its resource).
func tracesRequest(spans []sdktrace.ReadOnlySpan) otlp.TracesRequest {
	type scopeKey struct{ name, version string }
	order := make([]scopeKey, 0)
	byScope := make(map[scopeKey][]otlp.Span)
	var res *resource.Resource

	for _, s := range spans {
		res = s.Resource()
		sc := s.InstrumentationScope()
		k := scopeKey{sc.Name, sc.Version}
		if _, ok := byScope[k]; !ok {
			order = append(order, k)
		}
		byScope[k] = append(byScope[k], convSpan(s))
	}

	ss := make([]otlp.ScopeSpans, 0, len(order))
	for _, k := range order {
		ss = append(ss, otlp.ScopeSpans{
			Scope: otlp.Scope{Name: k.name, Version: k.version},
			Spans: byScope[k],
		})
	}
	return otlp.TracesRequest{ResourceSpans: []otlp.ResourceSpans{{
		Resource:   otlpResourceFromSDK(res),
		ScopeSpans: ss,
	}}}
}

func convSpan(s sdktrace.ReadOnlySpan) otlp.Span {
	sc := s.SpanContext()
	span := otlp.Span{
		TraceID:           sc.TraceID().String(),
		SpanID:            sc.SpanID().String(),
		Name:              s.Name(),
		Kind:              otlpSpanKind(s.SpanKind()),
		StartTimeUnixNano: nanos(s.StartTime()),
		EndTimeUnixNano:   nanos(s.EndTime()),
		Attributes:        otlpAttrs(s.Attributes()),
	}
	if p := s.Parent(); p.HasSpanID() {
		span.ParentSpanID = p.SpanID().String()
	}
	st := s.Status()
	if code := otlpStatusCode(st.Code); code != otlp.StatusUnset || st.Description != "" {
		span.Status = &otlp.SpanStatus{Code: code, Message: st.Description}
	}
	return span
}

// otlpSpanKind maps the OTel span kind to the OTLP wire enum. The numeric
// values already match, but we map explicitly so a future divergence is caught.
func otlpSpanKind(k trace.SpanKind) otlp.SpanKind {
	switch k {
	case trace.SpanKindInternal:
		return otlp.SpanKindInternal
	case trace.SpanKindServer:
		return otlp.SpanKindServer
	case trace.SpanKindClient:
		return otlp.SpanKindClient
	case trace.SpanKindProducer:
		return otlp.SpanKindProducer
	case trace.SpanKindConsumer:
		return otlp.SpanKindConsumer
	default:
		return otlp.SpanKindUnspecified
	}
}

// otlpStatusCode maps the OTel status code to the OTLP wire enum. They differ:
// OTel numbers Unset=0/Error=1/Ok=2, OTLP numbers Unset=0/Ok=1/Error=2.
func otlpStatusCode(c codes.Code) otlp.StatusCode {
	switch c {
	case codes.Ok:
		return otlp.StatusOK
	case codes.Error:
		return otlp.StatusError
	default:
		return otlp.StatusUnset
	}
}
