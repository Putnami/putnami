package telemetry

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"go.putnami.dev/events"
	otlp "go.putnami.dev/protocol/telemetry"
)

const eventInstrumentation = "go.putnami.dev/events"

// eventTelemetry carries a trace across an event by the telemetry protocol's
// event trace conventions: a publish writes the active span's traceparent and
// tracestate into the event attributes, and a delivery runs its handler in a
// consumer span that continues that trace, or starts a new root without one.
type eventTelemetry struct {
	tracer trace.Tracer
}

var _ events.DeliveryTelemetry = (*eventTelemetry)(nil)

func newEventTelemetry(provider trace.TracerProvider) *eventTelemetry {
	return &eventTelemetry{tracer: provider.Tracer(eventInstrumentation)}
}

// PublishAttributes returns attributes with the trace context of the span
// active on ctx, or attributes unchanged outside a span.
func (telemetry *eventTelemetry) PublishAttributes(ctx context.Context, attributes map[string]string) map[string]string {
	span := trace.SpanContextFromContext(ctx)
	if !span.IsValid() {
		return attributes
	}
	return otlp.WriteEventTraceAttributes(attributes, otlp.EventTraceContext{
		TraceID:    span.TraceID().String(),
		SpanID:     span.SpanID().String(),
		Flags:      fmt.Sprintf("%02x", byte(span.TraceFlags())),
		Tracestate: span.TraceState().String(),
	})
}

// StartDelivery starts the consumer span of one delivery: the child of the
// span the event's traceparent names, or a new root whatever span delivers it.
func (telemetry *eventTelemetry) StartDelivery(ctx context.Context, env events.Envelope) (context.Context, func(error)) {
	options := []trace.SpanStartOption{
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			attribute.String(otlp.AttrMessagingOperation, otlp.MessagingOperationProcess),
			attribute.String(otlp.AttrMessagingDestination, env.Topic),
			attribute.String(otlp.AttrMessagingMessageID, env.ID),
		),
	}
	if parent, ok := remoteParent(env.Attributes); ok {
		ctx = trace.ContextWithRemoteSpanContext(ctx, parent)
	} else {
		options = append(options, trace.WithNewRoot())
	}
	ctx, span := telemetry.tracer.Start(ctx, otlp.EventConsumerSpanName(env.Topic), options...)
	return ctx, func(err error) {
		if err != nil && !events.IsRedeliver(err) {
			span.SetStatus(codes.Error, otlp.EventConsumerFailureMessage)
		} else {
			span.SetStatus(codes.Ok, "")
		}
		span.End()
	}
}

// remoteParent reads the publisher's span from the event attributes. A
// tracestate the W3C list syntax refuses is dropped; the trace continues.
func remoteParent(attributes map[string]string) (trace.SpanContext, bool) {
	context, ok := otlp.ReadEventTraceAttributes(attributes)
	if !ok {
		return trace.SpanContext{}, false
	}
	traceID, err := trace.TraceIDFromHex(context.TraceID)
	if err != nil {
		return trace.SpanContext{}, false
	}
	spanID, err := trace.SpanIDFromHex(context.SpanID)
	if err != nil {
		return trace.SpanContext{}, false
	}
	config := trace.SpanContextConfig{TraceID: traceID, SpanID: spanID, Remote: true}
	if context.Sampled() {
		config.TraceFlags = trace.FlagsSampled
	}
	if state, err := trace.ParseTraceState(context.Tracestate); err == nil {
		config.TraceState = state
	}
	return trace.NewSpanContext(config), true
}
