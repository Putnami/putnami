package events

import (
	"context"
	"sync/atomic"

	"go.putnami.dev/errors"
)

// CodeEventsHandlerPanicked identifies a handler that panicked: its consumer
// span ends with this error before the panic goes on.
const CodeEventsHandlerPanicked errors.Code = "events.handler_panicked"

// The event attributes that carry a trace: the telemetry protocol's
// AttrEventTraceparent and AttrEventTracestate, the W3C header names.
const (
	traceparentAttribute = "traceparent"
	tracestateAttribute  = "tracestate"
)

var errDeliveryPanicked = errors.New(CodeEventsHandlerPanicked, "the handler panicked")

// DeliveryTelemetry carries a trace across an event. The telemetry plugin
// installs one: a publish then writes the trace context of the span active on
// its context into the event attributes, and every delivery runs its handler
// inside a consumer span that continues that trace.
//
// The attribute names, the span name and kind, and the parent rule are the
// telemetry protocol's event trace conventions (go.putnami.dev/protocol/telemetry),
// which the TypeScript runtime follows too.
type DeliveryTelemetry interface {
	// PublishAttributes returns the attributes a publish on ctx sends: a copy
	// of attributes with the active span's traceparent and tracestate, or
	// attributes unchanged when no span is active.
	PublishAttributes(ctx context.Context, attributes map[string]string) map[string]string
	// StartDelivery starts the consumer span of one delivery of env and
	// returns the context the handler runs in and the function that ends the
	// span with the handler's error.
	StartDelivery(ctx context.Context, env Envelope) (context.Context, func(error))
}

type deliveryTelemetryHolder struct {
	telemetry DeliveryTelemetry
}

var installedDeliveryTelemetry atomic.Pointer[deliveryTelemetryHolder]

// InstallDeliveryTelemetry installs telemetry for every publisher and
// transport of the process and returns the function that removes it. The
// removal restores the previous installation, unless another one replaced it
// since.
func InstallDeliveryTelemetry(telemetry DeliveryTelemetry) func() {
	previous := installedDeliveryTelemetry.Load()
	installed := &deliveryTelemetryHolder{telemetry: telemetry}
	installedDeliveryTelemetry.Store(installed)
	return func() {
		installedDeliveryTelemetry.CompareAndSwap(installed, previous)
	}
}

func currentDeliveryTelemetry() DeliveryTelemetry {
	holder := installedDeliveryTelemetry.Load()
	if holder == nil {
		return nil
	}
	return holder.telemetry
}

// publishAttributes returns the attributes a publish on ctx sends.
func publishAttributes(ctx context.Context, attributes map[string]string) map[string]string {
	telemetry := currentDeliveryTelemetry()
	if telemetry == nil || ctx == nil {
		return attributes
	}
	return telemetry.PublishAttributes(ctx, attributes)
}

// startDeliverySpan starts the consumer span of one delivery, or does nothing
// when no telemetry is installed.
func startDeliverySpan(ctx context.Context, env Envelope) (context.Context, func(error)) {
	telemetry := currentDeliveryTelemetry()
	if telemetry == nil {
		return ctx, func(error) {}
	}
	return telemetry.StartDelivery(ctx, env)
}

// adoptMessageTraceContext takes the trace context a provider carries in its
// message attributes, such as the one the managed Event Server stamps, when the
// envelope carries none of its own: the consumer span then continues that trace.
// The push receiver and the Pub/Sub pull transport both apply it.
func adoptMessageTraceContext(attributes, pushed map[string]string) {
	traceparent, ok := pushed[traceparentAttribute]
	if !ok {
		return
	}
	if _, own := attributes[traceparentAttribute]; own {
		return
	}
	attributes[traceparentAttribute] = traceparent
	if tracestate, ok := pushed[tracestateAttribute]; ok {
		attributes[tracestateAttribute] = tracestate
	} else {
		delete(attributes, tracestateAttribute)
	}
}
