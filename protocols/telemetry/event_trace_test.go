package telemetry

import (
	"bytes"
	"encoding/json"
	"maps"
	"os"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// eventTraceFixture is fixtures/conventions/event-trace-context.json, which
// the TypeScript runtime reads too.
type eventTraceFixture struct {
	Attributes struct {
		Traceparent string `json:"traceparent"`
		Tracestate  string `json:"tracestate"`
	} `json:"attributes"`
	MaxTracestateLength int `json:"maxTracestateLength"`
	ConsumerSpan        struct {
		NamePrefix string `json:"namePrefix"`
		Kind       int    `json:"kind"`
		Attributes struct {
			Operation   string `json:"operation"`
			Destination string `json:"destination"`
			MessageID   string `json:"messageId"`
		} `json:"attributes"`
		Operation      string `json:"operation"`
		FailureMessage string `json:"failureMessage"`
	} `json:"consumerSpan"`
	Publish []EventTracePublishCase `json:"publish"`
	Consume []EventTraceConsumeCase `json:"consume"`
}

// EventTracePublishCase is one publish: the span it runs in, if any, the
// attributes the caller set, and the attributes the event carries.
type EventTracePublishCase struct {
	Name             string             `json:"name"`
	Span             *EventTraceContext `json:"span"`
	CallerAttributes map[string]string  `json:"callerAttributes"`
	Attributes       map[string]string  `json:"attributes"`
}

// EventTraceConsumeCase is one delivery: the event attributes and the parent
// its consumer span continues, if any.
type EventTraceConsumeCase struct {
	Name       string             `json:"name"`
	Attributes map[string]string  `json:"attributes"`
	Continues  bool               `json:"continues"`
	Parent     *EventTraceContext `json:"parent"`
}

func readEventTraceFixture(t *testing.T) eventTraceFixture {
	t.Helper()
	data, err := os.ReadFile("fixtures/conventions/event-trace-context.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture eventTraceFixture
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return fixture
}

// TestEventTrace_TheConventionMatchesItsFixture keeps the attribute names, the
// consumer span and the read and write rules equal to the fixture the
// TypeScript runtime reads.
func TestEventTrace_TheConventionMatchesItsFixture(t *testing.T) {
	spectest.Proves(t, "telemetry/otlp-push-contract", "event-trace-context-is-shared", "the-event-trace-convention-matches-its-fixture")
	fixture := readEventTraceFixture(t)
	if fixture.Attributes.Traceparent != AttrEventTraceparent || fixture.Attributes.Tracestate != AttrEventTracestate {
		t.Errorf("attributes = %+v", fixture.Attributes)
	}
	if fixture.MaxTracestateLength != MaxEventTracestateLength {
		t.Errorf("maxTracestateLength = %d, want %d", fixture.MaxTracestateLength, MaxEventTracestateLength)
	}
	span := fixture.ConsumerSpan
	if span.NamePrefix != EventConsumerSpanNamePrefix || span.Kind != EventConsumerSpanKind ||
		span.Attributes.Operation != AttrMessagingOperation || span.Attributes.Destination != AttrMessagingDestination ||
		span.Attributes.MessageID != AttrMessagingMessageID || span.Operation != MessagingOperationProcess ||
		span.FailureMessage != EventConsumerFailureMessage {
		t.Errorf("consumerSpan = %+v", span)
	}
	if got := EventConsumerSpanName("orders.created"); got != "process orders.created" {
		t.Errorf("EventConsumerSpanName = %q", got)
	}
	if len(fixture.Publish) == 0 || len(fixture.Consume) == 0 {
		t.Fatal("the fixture holds no publish or no consume case")
	}
	for _, c := range fixture.Publish {
		t.Run("publish/"+c.Name, func(t *testing.T) {
			caller := maps.Clone(c.CallerAttributes)
			got := caller
			if c.Span != nil {
				got = WriteEventTraceAttributes(caller, *c.Span)
				if !maps.Equal(caller, c.CallerAttributes) {
					t.Errorf("the caller attributes changed: %v", caller)
				}
			}
			if !maps.Equal(got, c.Attributes) {
				t.Errorf("attributes = %v, want %v", got, c.Attributes)
			}
		})
	}
	for _, c := range fixture.Consume {
		t.Run("consume/"+c.Name, func(t *testing.T) {
			got, ok := ReadEventTraceAttributes(c.Attributes)
			if ok != c.Continues {
				t.Fatalf("continues = %v, want %v", ok, c.Continues)
			}
			if ok && got != *c.Parent {
				t.Errorf("parent = %+v, want %+v", got, *c.Parent)
			}
		})
	}
}

func TestEventTrace_SampledReadsTheSampledBit(t *testing.T) {
	for flags, want := range map[string]bool{"01": true, "00": false, "03": true, "02": false, "ff": true, "": false} {
		if got := (EventTraceContext{Flags: flags}).Sampled(); got != want {
			t.Errorf("Sampled(%q) = %v, want %v", flags, got, want)
		}
	}
}

func TestEventTrace_AControlCharacterDropsTheTracestate(t *testing.T) {
	got, ok := ReadEventTraceAttributes(map[string]string{
		AttrEventTraceparent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		AttrEventTracestate:  "vendor=a\nb",
	})
	if !ok || got.Tracestate != "" {
		t.Errorf("got %+v, %v", got, ok)
	}
}
