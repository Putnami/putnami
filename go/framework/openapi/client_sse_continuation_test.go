package openapi

import (
	"reflect"
	"strings"
	"testing"

	"go.putnami.dev/api"
	phttp "go.putnami.dev/http"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

type sseContinuationFrame struct {
	Cursor string `json:"cursor" validate:"required"`
	Line   string `json:"line" validate:"required"`
	Count  int    `json:"count" validate:"required"`
	Note   string `json:"note,omitempty"`
}

type sseContinuationQuery struct {
	Cursor string `json:"cursor"`
	Limit  int    `json:"limit"`
}

func sseContinuationEndpoint(options api.ClientOperationOptions) api.EndpointDefinition {
	return api.Endpoint("GET", "/events").
		Query(reflect.TypeFor[sseContinuationQuery]()).
		Returns(api.StreamOf[sseContinuationFrame]()).
		Client(options).
		Handle(api.ServerStream(func(_ *api.ServerStreamContext[sseContinuationFrame]) error { return nil }))
}

// A declared continuation is published once, on the SSE transport, and the
// published contract passes the strict reader.
func TestOpenAPI_PublishesADeclaredSSEContinuationOnTheSSETransport(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "declared-sse-continuation",
		"a-declared-sse-continuation-is-published-on-the-sse-transport")

	for name, declared := range map[string]*clientcontract.SSEContinuation{
		"cursor":      api.SSECursorContinuation("cursor", "cursor"),
		"best-effort": api.SSEBestEffortContinuation(),
	} {
		t.Run(name, func(t *testing.T) {
			reconnect := true
			operation, err := declaredOrderContract(t, sseContinuationEndpoint(api.ClientOperationOptions{
				SSEContinuation: declared,
				Resilience:      &clientcontract.ResiliencePolicy{Stream: &clientcontract.StreamPolicy{Reconnect: &reconnect}},
			}))
			if err != nil {
				t.Fatal(err)
			}
			published := 0
			for _, transport := range operation.Transports {
				continuation := transport.Continuation()
				if transport.Protocol != clientcontract.TransportSSE {
					if transport.SSE != nil {
						t.Fatalf("%s transport carries sse metadata", transport.Protocol)
					}
					continue
				}
				if !reflect.DeepEqual(continuation, declared) {
					t.Fatalf("published continuation = %+v, want %+v", continuation, declared)
				}
				published++
			}
			if published != 1 {
				t.Fatalf("published %d sse continuations, want 1", published)
			}
		})
	}
}

// An SSE-only stream without a continuation keeps its exact transport bytes.
func TestOpenAPI_AStreamWithoutAContinuationPublishesNoSSEMetadata(t *testing.T) {
	operation, err := declaredOrderContract(t, sseContinuationEndpoint(api.ClientOperationOptions{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, transport := range operation.Transports {
		if transport.SSE != nil {
			t.Fatalf("%s transport carries undeclared sse metadata", transport.Protocol)
		}
	}
}

// Every shape a continuation cannot be carried on is refused at projection
// with the route named.
func TestOpenAPI_RefusesAnSSEContinuationOnAShapeThatCannotCarryIt(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "declared-sse-continuation",
		"an-sse-continuation-is-refused-on-every-shape-that-cannot-carry-it")

	cases := []struct {
		name     string
		endpoint api.EndpointDefinition
		want     string
	}{
		{
			name: "a unary operation",
			endpoint: api.Endpoint("GET", "/events").
				Returns(reflect.TypeFor[sseContinuationFrame]()).
				Client(api.ClientOperationOptions{SSEContinuation: api.SSEBestEffortContinuation()}).
				Handle(func(*phttp.EndpointContext) *phttp.Response { return nil }),
			want: "only a server stream can be continued",
		},
		{
			name: "a stream the provider did not declare safe",
			endpoint: sseContinuationEndpoint(api.ClientOperationOptions{
				SSEContinuation: api.SSEBestEffortContinuation(),
				Idempotency:     &clientcontract.Idempotency{Kind: clientcontract.IdempotencyNonIdempotent},
			}),
			want: "only a safe stream can be continued",
		},
		{
			name: "an order that drops the sse transport",
			endpoint: sseContinuationEndpoint(api.ClientOperationOptions{
				SSEContinuation: api.SSEBestEffortContinuation(),
				Transports:      []clientcontract.TransportProtocol{clientcontract.TransportWebSocket},
			}),
			want: "publishes no sse transport to carry it",
		},
		{
			name: "an unknown mode",
			endpoint: sseContinuationEndpoint(api.ClientOperationOptions{
				SSEContinuation: &clientcontract.SSEContinuation{Mode: "replay"},
			}),
			want: `declares sse continuation mode "replay"`,
		},
		{
			name:     "a cursor that names no query parameter",
			endpoint: sseContinuationEndpoint(api.ClientOperationOptions{SSEContinuation: api.SSECursorContinuation("cursor", " ")}),
			want:     "does not name both its output field and its query parameter",
		},
		{
			name: "a best-effort continuation with a cursor",
			endpoint: sseContinuationEndpoint(api.ClientOperationOptions{SSEContinuation: &clientcontract.SSEContinuation{
				Mode: clientcontract.SSEContinuationBestEffort, Cursor: &clientcontract.SSECursor{OutputField: "cursor", QueryParameter: "cursor"},
			}}),
			want: "best-effort reopens the original selector",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := declaredOrderContract(t, testCase.endpoint)
			if err == nil || !strings.Contains(err.Error(), testCase.want) || !strings.Contains(err.Error(), "GET /events") {
				t.Fatalf("refusal = %v, want %q with the route named", err, testCase.want)
			}
		})
	}
}

// A cursor names a required plain-string output field and a declared
// plain-string query parameter; anything else is refused at projection.
func TestOpenAPI_RefusesACursorItsSchemasCannotCarry(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "declared-sse-continuation",
		"a-cursor-names-a-required-plain-string-output-field-and-a-declared-query-parameter")

	cases := map[string]struct {
		output, query, want string
	}{
		"an undeclared output field": {output: "position", query: "cursor", want: "names no property of the declared output message"},
		"an optional output field":   {output: "note", query: "cursor", want: "must be required"},
		"a non-string output field":  {output: "count", query: "cursor", want: "must be a plain string"},
		"an undeclared query":        {output: "cursor", query: "after", want: `query parameter "after" is not declared`},
		"a non-string query":         {output: "cursor", query: "limit", want: `query parameter "limit" must be a plain string`},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := declaredOrderContract(t, sseContinuationEndpoint(api.ClientOperationOptions{
				SSEContinuation: api.SSECursorContinuation(testCase.output, testCase.query),
			}))
			if err == nil || !strings.Contains(err.Error(), testCase.want) || !strings.Contains(err.Error(), "GET /events") {
				t.Fatalf("refusal = %v, want %q with the route named", err, testCase.want)
			}
		})
	}
}
