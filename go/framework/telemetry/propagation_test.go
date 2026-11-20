package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	putnami_http "go.putnami.dev/http"
	"go.putnami.dev/protocol/features/spectest"
	otlp "go.putnami.dev/protocol/telemetry"
)

const proxySafeTraceparentFixturePath = "../../../protocols/telemetry/fixtures/conventions/proxy-safe-traceparent.json"

// proxySafeTraceparentFixture is the fixture the protocol and the TypeScript
// runtime test against.
type proxySafeTraceparentFixture struct {
	Header string `json:"header"`
	Cases  []struct {
		Name               string            `json:"name"`
		Traceparent        string            `json:"traceparent"`
		PutnamiTraceparent string            `json:"putnamiTraceparent"`
		Source             string            `json:"source"`
		Outside            bool              `json:"outside"`
		Parent             *otlp.Traceparent `json:"parent"`
	} `json:"cases"`
}

func readProxySafeTraceparentFixture(t *testing.T) proxySafeTraceparentFixture {
	t.Helper()
	data, err := os.ReadFile(proxySafeTraceparentFixturePath)
	if err != nil {
		t.Fatal(err)
	}
	var fixture proxySafeTraceparentFixture
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	if len(fixture.Cases) == 0 {
		t.Fatal("the fixture holds no case")
	}
	return fixture
}

// TestProxySafeTraceContext_TheServerSpanResolvesTheFixtureCases sends each
// fixture request through the HTTP middleware and reads the parent of the
// server span it records.
func TestProxySafeTraceContext_TheServerSpanResolvesTheFixtureCases(t *testing.T) {
	spectest.Proves(t, "telemetry/otlp-push-contract", "the-client-span-survives-a-proxy", "the-go-server-resolves-the-fixture-cases")
	saveOTelGlobals(t)
	fixture := readProxySafeTraceparentFixture(t)
	if fixture.Header != otlp.HeaderPutnamiTraceparent {
		t.Fatalf("header = %q, want %q", fixture.Header, otlp.HeaderPutnamiTraceparent)
	}
	otel.SetTextMapPropagator(ProxySafeTraceContext{})
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			exporter := tracetest.NewInMemoryExporter()
			tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter), sdktrace.WithSampler(sdktrace.AlwaysSample()))
			defer func() { _ = tp.Shutdown(context.Background()) }()
			otel.SetTracerProvider(tp)

			request := httptest.NewRequest("GET", "/", nil)
			if c.Traceparent != "" {
				request.Header.Set("traceparent", c.Traceparent)
			}
			if c.PutnamiTraceparent != "" {
				request.Header.Set(fixture.Header, c.PutnamiTraceparent)
			}
			ctx := putnami_http.NewContext(httptest.NewRecorder(), request)
			ctx.Route = "/"
			HTTPMiddleware("test-svc")(ctx, putnami_http.NoContent)

			spans := exporter.GetSpans()
			if len(spans) != 1 {
				t.Fatalf("spans = %d, want 1", len(spans))
			}
			parent := spans[0].Parent
			if c.Parent == nil {
				if parent.IsValid() {
					t.Fatalf("parent = %s/%s, want none", parent.TraceID(), parent.SpanID())
				}
				return
			}
			if !parent.IsRemote() {
				t.Error("the parent is not remote")
			}
			if parent.TraceID().String() != c.Parent.TraceID || parent.SpanID().String() != c.Parent.SpanID ||
				parent.TraceFlags().String() != c.Parent.TraceFlags {
				t.Fatalf("parent = %s-%s-%s, want %s", parent.TraceID(), parent.SpanID(), parent.TraceFlags(), c.Parent)
			}
			if spans[0].SpanContext.TraceID() != parent.TraceID() {
				t.Error("the server span left the caller's trace")
			}
		})
	}
}

// TestProxySafeTraceContext_ExtractMarksTheFixtureOutsideParents checks that
// Extract marks a remote parent as outside exactly in the fixture's outside
// cases, the parents the sampler decides under ParentDecide.
func TestProxySafeTraceContext_ExtractMarksTheFixtureOutsideParents(t *testing.T) {
	fixture := readProxySafeTraceparentFixture(t)
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			carrier := propagation.MapCarrier{}
			if c.Traceparent != "" {
				carrier.Set(otlp.HeaderTraceparent, c.Traceparent)
			}
			if c.PutnamiTraceparent != "" {
				carrier.Set(otlp.HeaderPutnamiTraceparent, c.PutnamiTraceparent)
			}
			ctx := ProxySafeTraceContext{}.Extract(context.Background(), carrier)
			if got := outsideParent(ctx); got != c.Outside {
				t.Fatalf("outside = %v, want %v", got, c.Outside)
			}
		})
	}
}

func TestProxySafeTraceContext_AMarkAppliesToItsParentOnly(t *testing.T) {
	carrier := propagation.MapCarrier{otlp.HeaderTraceparent: "00-4bf92f3577b34da6a3ce929d0e0e4736-a1a1a1a1a1a1a1a1-00"}
	ctx := ProxySafeTraceContext{}.Extract(context.Background(), carrier)
	if !outsideParent(ctx) {
		t.Fatal("a traceparent alone is an outside parent")
	}
	other := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{1}, SpanID: trace.SpanID{2}, Remote: true,
	})
	if outsideParent(trace.ContextWithRemoteSpanContext(ctx, other)) {
		t.Fatal("the mark of one parent applied to another remote parent")
	}
	local := trace.SpanContextFromContext(ctx).WithRemote(false)
	if outsideParent(trace.ContextWithSpanContext(ctx, local)) {
		t.Fatal("the mark applied to a local parent")
	}
}

func TestProxySafeTraceContext_InjectWritesTheSameValueTwice(t *testing.T) {
	traceID, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	spanID, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled,
	}))
	carrier := propagation.MapCarrier{}
	ProxySafeTraceContext{}.Inject(ctx, carrier)
	want := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	if carrier["traceparent"] != want || carrier["x-putnami-traceparent"] != want {
		t.Fatalf("carrier = %v, want both headers %q", carrier, want)
	}

	empty := propagation.MapCarrier{}
	ProxySafeTraceContext{}.Inject(context.Background(), empty)
	if len(empty) != 0 {
		t.Fatalf("a context without a span injected %v", empty)
	}
}

func TestProxySafeTraceContext_ExtractKeepsTraceState(t *testing.T) {
	carrier := propagation.MapCarrier{
		"traceparent":           "00-4bf92f3577b34da6a3ce929d0e0e4736-a1a1a1a1a1a1a1a1-01",
		"tracestate":            "vendor=value",
		"x-putnami-traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
	}
	parent := trace.SpanContextFromContext(ProxySafeTraceContext{}.Extract(context.Background(), carrier))
	if parent.SpanID().String() != "00f067aa0ba902b7" || parent.TraceState().String() != "vendor=value" || !parent.IsRemote() {
		t.Fatalf("parent = %s, state %q, remote %v", parent.SpanID(), parent.TraceState(), parent.IsRemote())
	}
}
