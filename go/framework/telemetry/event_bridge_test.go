package telemetry

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"testing"
	"time"

	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"go.putnami.dev/events"
	"go.putnami.dev/protocol/features/spectest"
	otlp "go.putnami.dev/protocol/telemetry"
)

const eventTraceFixturePath = "../../../protocols/telemetry/fixtures/conventions/event-trace-context.json"

type orderCreated struct {
	OrderID string `json:"orderId"`
}

// configureEventTelemetry configures a plugin that records spans in memory and
// returns the exporter and a flush that makes the ended spans visible.
func configureEventTelemetry(t *testing.T) (*tracetest.InMemoryExporter, *Plugin) {
	t.Helper()
	saveOTelGlobals(t)
	exporter := tracetest.NewInMemoryExporter()
	plugin := NewPlugin(Config{
		ServiceName:   "orders",
		TraceExporter: exporter,
		MetricReader:  sdkmetric.NewManualReader(),
	})
	if err := plugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("configure telemetry: %v", err)
	}
	t.Cleanup(func() { _ = plugin.Stop(context.Background(), nil) })
	return exporter, plugin
}

func flushSpans(t *testing.T, plugin *Plugin, exporter *tracetest.InMemoryExporter) tracetest.SpanStubs {
	t.Helper()
	if err := plugin.tp.ForceFlush(context.Background()); err != nil {
		t.Fatalf("flush spans: %v", err)
	}
	return exporter.GetSpans()
}

// waitForSpans flushes until a span of each name is exported: a delivery ends
// its span just after its handler returns.
func waitForSpans(t *testing.T, plugin *Plugin, exporter *tracetest.InMemoryExporter, names ...string) tracetest.SpanStubs {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		spans := flushSpans(t, plugin, exporter)
		missing := false
		for _, name := range names {
			found := false
			for _, span := range spans {
				found = found || span.Name == name
			}
			missing = missing || !found
		}
		if !missing || time.Now().After(deadline) {
			return spans
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func spanNamed(t *testing.T, spans tracetest.SpanStubs, name string) tracetest.SpanStub {
	t.Helper()
	for _, span := range spans {
		if span.Name == name {
			return span
		}
	}
	t.Fatalf("no span named %q among %d", name, len(spans))
	return tracetest.SpanStub{}
}

// deliverOne publishes one event inside ctx through a memory broker and
// returns the message and the context its handler received.
func deliverOne(t *testing.T, ctx context.Context) (*events.Message[orderCreated], context.Context) {
	t.Helper()
	topic := events.NewTopic[orderCreated]("orders.created")
	broker := events.NewMemoryBroker()
	type delivery struct {
		msg *events.Message[orderCreated]
		ctx context.Context
	}
	delivered := make(chan delivery, 1)
	handler := events.Handle(topic, func(ctx context.Context, msg *events.Message[orderCreated]) error {
		select {
		case delivered <- delivery{msg: msg, ctx: ctx}:
		default:
		}
		return nil
	})
	if err := broker.Subscribe(handler); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := broker.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = broker.Stop(context.Background()) })
	if err := events.Publish(ctx, broker, topic, orderCreated{OrderID: "order-1"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	select {
	case got := <-delivered:
		return got.msg, got.ctx
	case <-time.After(5 * time.Second):
		t.Fatal("the event was not delivered")
	}
	return nil, nil
}

func TestEventTrace_APublishInsideASpanWritesTheTraceAttributes(t *testing.T) {
	spectest.Proves(t, "go/event-delivery", "event-trace-context", "a-publish-inside-a-span-writes-the-trace-attributes")
	exporter, plugin := configureEventTelemetry(t)
	ctx, publisher := Tracer("orders").Start(context.Background(), "POST /orders")
	msg, _ := deliverOne(t, ctx)
	publisher.End()

	want := publisher.SpanContext()
	parent, ok := otlp.ReadEventTraceAttributes(msg.Attributes)
	if !ok {
		t.Fatalf("the event carries no valid traceparent: %v", msg.Attributes)
	}
	if parent.TraceID != want.TraceID().String() || parent.SpanID != want.SpanID().String() || !parent.Sampled() {
		t.Errorf("traceparent = %q, want the publishing span %s/%s", msg.Attributes[otlp.AttrEventTraceparent], want.TraceID(), want.SpanID())
	}
	waitForSpans(t, plugin, exporter, "process orders.created")
}

func TestEventTrace_APublishOutsideASpanWritesNoTraceAttribute(t *testing.T) {
	spectest.Proves(t, "go/event-delivery", "event-trace-context", "a-publish-outside-a-span-writes-no-trace-attribute")
	configureEventTelemetry(t)
	msg, _ := deliverOne(t, context.Background())
	if _, ok := msg.Attributes[otlp.AttrEventTraceparent]; ok {
		t.Errorf("the event carries a traceparent outside a span: %v", msg.Attributes)
	}
	if _, ok := msg.Attributes[otlp.AttrEventTracestate]; ok {
		t.Errorf("the event carries a tracestate outside a span: %v", msg.Attributes)
	}
}

func TestEventTrace_TheConsumerSpanContinuesThePublishersTrace(t *testing.T) {
	spectest.Proves(t, "go/event-delivery", "event-trace-context", "the-consumer-span-continues-the-publishers-trace")
	exporter, plugin := configureEventTelemetry(t)
	ctx, publisher := Tracer("orders").Start(context.Background(), "POST /orders")
	msg, handlerCtx := deliverOne(t, ctx)
	publisher.End()

	spans := waitForSpans(t, plugin, exporter, "process orders.created")
	consumer := spanNamed(t, spans, "process orders.created")
	if consumer.SpanKind != trace.SpanKindConsumer {
		t.Errorf("kind = %v, want consumer", consumer.SpanKind)
	}
	if consumer.Parent.SpanID() != publisher.SpanContext().SpanID() || consumer.SpanContext.TraceID() != publisher.SpanContext().TraceID() {
		t.Errorf("the consumer span is not the publishing span's child: parent %v, trace %v", consumer.Parent.SpanID(), consumer.SpanContext.TraceID())
	}
	if consumer.Status.Code != codes.Ok {
		t.Errorf("status = %v, want ok", consumer.Status)
	}
	attributes := map[string]string{}
	for _, attribute := range consumer.Attributes {
		attributes[string(attribute.Key)] = attribute.Value.AsString()
	}
	want := map[string]string{
		otlp.AttrMessagingOperation:   otlp.MessagingOperationProcess,
		otlp.AttrMessagingDestination: "orders.created",
		otlp.AttrMessagingMessageID:   msg.ID,
	}
	if !maps.Equal(attributes, want) {
		t.Errorf("attributes = %v, want %v", attributes, want)
	}
	if got := trace.SpanContextFromContext(handlerCtx).SpanID(); got != consumer.SpanContext.SpanID() {
		t.Errorf("the handler does not run in the consumer span: %v", got)
	}
}

func TestEventTrace_AnEventWithoutTraceAttributesStartsARoot(t *testing.T) {
	spectest.Proves(t, "go/event-delivery", "event-trace-context", "an-event-without-trace-attributes-starts-a-root")
	exporter, plugin := configureEventTelemetry(t)
	deliverOne(t, context.Background())

	// The span delivering the event is not the consumer's parent either.
	delivering, deliverer := Tracer("orders").Start(context.Background(), "POST /push")
	bridge := newEventTelemetry(plugin.tp)
	_, end := bridge.StartDelivery(delivering, events.Envelope{ID: "evt-2", Topic: "orders.pushed"})
	end(nil)
	deliverer.End()

	spans := waitForSpans(t, plugin, exporter, "process orders.created", "process orders.pushed")
	for _, name := range []string{"process orders.created", "process orders.pushed"} {
		span := spanNamed(t, spans, name)
		if span.Parent.IsValid() {
			t.Errorf("%s has parent %v, want a root", name, span.Parent.SpanID())
		}
	}
	if spanNamed(t, spans, "process orders.pushed").SpanContext.TraceID() == deliverer.SpanContext().TraceID() {
		t.Error("the consumer span joined the delivering span's trace")
	}
}

func TestEventTrace_AFailedHandlerEndsItsSpanWithError(t *testing.T) {
	exporter, plugin := configureEventTelemetry(t)
	bridge := newEventTelemetry(plugin.tp)
	for name, err := range map[string]error{
		"failed":      context.DeadlineExceeded,
		"redelivered": events.Redeliver("later"),
	} {
		_, end := bridge.StartDelivery(context.Background(), events.Envelope{ID: name, Topic: name})
		end(err)
	}
	spans := flushSpans(t, plugin, exporter)
	if status := spanNamed(t, spans, "process failed").Status; status.Code != codes.Error || status.Description != otlp.EventConsumerFailureMessage {
		t.Errorf("failed status = %+v", status)
	}
	if status := spanNamed(t, spans, "process redelivered").Status; status.Code != codes.Ok {
		t.Errorf("redelivered status = %+v", status)
	}
}

func TestEventTrace_StopRemovesTheBridge(t *testing.T) {
	_, plugin := configureEventTelemetry(t)
	if err := plugin.Stop(context.Background(), nil); err != nil {
		t.Fatalf("stop: %v", err)
	}
	ctx, span := Tracer("orders").Start(context.Background(), "after stop")
	defer span.End()
	msg, _ := deliverOne(t, ctx)
	if _, ok := msg.Attributes[otlp.AttrEventTraceparent]; ok {
		t.Errorf("a stopped plugin still writes trace attributes: %v", msg.Attributes)
	}
}

// eventTraceFixture is the part of the protocol fixture the Go runtime runs.
type eventTraceFixture struct {
	Publish []struct {
		Name             string                  `json:"name"`
		Span             *otlp.EventTraceContext `json:"span"`
		CallerAttributes map[string]string       `json:"callerAttributes"`
		Attributes       map[string]string       `json:"attributes"`
	} `json:"publish"`
	Consume []struct {
		Name       string                  `json:"name"`
		Attributes map[string]string       `json:"attributes"`
		Continues  bool                    `json:"continues"`
		Parent     *otlp.EventTraceContext `json:"parent"`
	} `json:"consume"`
}

// TestEventTrace_TheGoRuntimeFollowsTheFixture runs every publish and consume
// case of the shared fixture through the bridge the plugin installs.
func TestEventTrace_TheGoRuntimeFollowsTheFixture(t *testing.T) {
	spectest.Proves(t, "telemetry/otlp-push-contract", "event-trace-context-is-shared", "the-go-runtime-follows-the-event-trace-fixture")
	data, err := os.ReadFile(eventTraceFixturePath)
	if err != nil {
		t.Fatal(err)
	}
	var fixture eventTraceFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	exporter, plugin := configureEventTelemetry(t)
	bridge := newEventTelemetry(plugin.tp)

	for _, c := range fixture.Publish {
		t.Run("publish/"+c.Name, func(t *testing.T) {
			ctx := context.Background()
			if c.Span != nil {
				parent, ok := remoteParent(otlp.WriteEventTraceAttributes(nil, *c.Span))
				if !ok {
					t.Fatalf("the fixture span %+v is not valid", *c.Span)
				}
				ctx = trace.ContextWithSpanContext(ctx, parent)
			}
			got := bridge.PublishAttributes(ctx, maps.Clone(c.CallerAttributes))
			if !maps.Equal(got, c.Attributes) {
				t.Errorf("attributes = %v, want %v", got, c.Attributes)
			}
		})
	}
	for i, c := range fixture.Consume {
		t.Run("consume/"+c.Name, func(t *testing.T) {
			name := "fixture-" + string(rune('a'+i))
			ctx, end := bridge.StartDelivery(context.Background(), events.Envelope{ID: name, Topic: name, Attributes: c.Attributes})
			end(nil)
			consumer := trace.SpanContextFromContext(ctx)
			if !c.Continues {
				// A new root: sampled by the budget, recorded with no parent.
				if span := spanNamed(t, flushSpans(t, plugin, exporter), "process "+name); span.Parent.IsValid() {
					t.Errorf("parent = %v, want a root", span.Parent.SpanID())
				}
				return
			}
			if consumer.TraceID().String() != c.Parent.TraceID {
				t.Errorf("trace = %v, want %s", consumer.TraceID(), c.Parent.TraceID)
			}
			if consumer.IsSampled() != c.Parent.Sampled() {
				t.Errorf("sampled = %v, want %v", consumer.IsSampled(), c.Parent.Sampled())
			}
			if got := consumer.TraceState().String(); got != c.Parent.Tracestate {
				t.Errorf("tracestate = %q, want %q", got, c.Parent.Tracestate)
			}
			if !c.Parent.Sampled() {
				return
			}
			span := spanNamed(t, flushSpans(t, plugin, exporter), "process "+name)
			if span.Parent.SpanID().String() != c.Parent.SpanID {
				t.Errorf("parent = %v, want %s", span.Parent.SpanID(), c.Parent.SpanID)
			}
		})
	}
}
