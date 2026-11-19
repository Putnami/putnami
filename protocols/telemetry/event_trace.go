package telemetry

import (
	"fmt"
	"strings"
)

// Event trace conventions. A runtime that publishes an event inside a span
// writes the span's W3C trace context into the event attributes, and the
// runtime that delivers the event continues that trace in a consumer span, so
// a Go publisher and a TypeScript consumer, or the reverse, share one trace.
//
// The values are also committed in fixtures/conventions/event-trace-context.json,
// which the TypeScript runtime reads in its tests; event_trace_test.go keeps
// these constants and the rule equal to that file.
const (
	// AttrEventTraceparent is the event attribute that carries the W3C
	// traceparent of the publishing span.
	AttrEventTraceparent = "traceparent"
	// AttrEventTracestate is the event attribute that carries the W3C
	// tracestate of the publishing span.
	AttrEventTracestate = "tracestate"
	// MaxEventTracestateLength is the longest tracestate a consumer continues;
	// a longer one is dropped and the trace still continues.
	MaxEventTracestateLength = 512

	// EventConsumerSpanNamePrefix starts the consumer span name, which ends
	// with the topic: "process orders.created".
	EventConsumerSpanNamePrefix = "process "
	// EventConsumerSpanKind is the OTLP kind of the consumer span: consumer.
	EventConsumerSpanKind = SpanKindConsumer
	// AttrMessagingOperation names the operation of the consumer span.
	AttrMessagingOperation = "messaging.operation.name"
	// MessagingOperationProcess is the operation a consumer span reports.
	MessagingOperationProcess = "process"
	// AttrMessagingDestination names the topic of the delivered event.
	AttrMessagingDestination = "messaging.destination.name"
	// AttrMessagingMessageID names the id of the delivered event.
	AttrMessagingMessageID = "messaging.message.id"
	// EventConsumerFailureMessage is the status message of a consumer span
	// whose handler failed.
	EventConsumerFailureMessage = "events.handler_failed"
)

// EventTraceContext is the W3C trace context an event carries in its
// attributes: the publishing span's identity, its flags and its tracestate.
type EventTraceContext struct {
	// TraceID is the 32 lowercase hex characters of the trace.
	TraceID string `json:"traceId"`
	// SpanID is the 16 lowercase hex characters of the publishing span.
	SpanID string `json:"spanId"`
	// Flags is the 2 lowercase hex characters of the trace flags.
	Flags string `json:"flags"`
	// Tracestate is the W3C tracestate, empty when the span carries none.
	Tracestate string `json:"tracestate,omitempty"`
}

// Sampled reports whether the flags carry the W3C sampled bit.
func (c EventTraceContext) Sampled() bool {
	return len(c.Flags) == 2 && hexValue(c.Flags[1])&1 == 1
}

// Traceparent renders the W3C traceparent of the context, version 00.
func (c EventTraceContext) Traceparent() string {
	return fmt.Sprintf("00-%s-%s-%s", c.TraceID, c.SpanID, c.Flags)
}

// WriteEventTraceAttributes returns a copy of attributes with the context's
// traceparent and, when it has one, its tracestate; a caller value of either
// is replaced, and a tracestate the context lacks is removed. The input map is
// never modified.
func WriteEventTraceAttributes(attributes map[string]string, context EventTraceContext) map[string]string {
	out := make(map[string]string, len(attributes)+2)
	for key, value := range attributes {
		out[key] = value
	}
	out[AttrEventTraceparent] = context.Traceparent()
	if context.Tracestate != "" {
		out[AttrEventTracestate] = context.Tracestate
	} else {
		delete(out, AttrEventTracestate)
	}
	return out
}

// ReadEventTraceAttributes reads the trace context an event's attributes
// carry. It answers false when the traceparent is missing or invalid: version
// 00, a non-zero 32-hex trace id, a non-zero 16-hex span id and 2-hex flags,
// all lowercase. A tracestate longer than MaxEventTracestateLength or holding
// a control character is dropped, and the context is still returned.
func ReadEventTraceAttributes(attributes map[string]string) (EventTraceContext, bool) {
	parent, ok := ParseTraceparent(attributes[AttrEventTraceparent])
	if !ok {
		return EventTraceContext{}, false
	}
	context := EventTraceContext{TraceID: parent.TraceID, SpanID: parent.SpanID, Flags: parent.TraceFlags}
	if state := attributes[AttrEventTracestate]; validTracestate(state) {
		context.Tracestate = state
	}
	return context, true
}

// EventConsumerSpanName is the name of the consumer span of a topic.
func EventConsumerSpanName(topic string) string {
	return EventConsumerSpanNamePrefix + topic
}

func validTracestate(value string) bool {
	if value == "" || len(value) > MaxEventTracestateLength {
		return false
	}
	return !strings.ContainsAny(value, "\x00\r\n")
}

func hexValue(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	}
	return 0
}
