package events

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	perrors "go.putnami.dev/errors"
	protoevents "go.putnami.dev/protocol/events"
	"go.putnami.dev/protocol/features/spectest"
)

const testEventServerAudience = "https://events.example.test"

func newTestEventServerTransport(t *testing.T, endpoint string, client interface {
	Do(*http.Request) (*http.Response, error)
}, source CredentialSource, mutate ...func(*EventServerTransportConfig)) *EventServerTransport {
	t.Helper()
	config := EventServerTransportConfig{
		ContractVersion:  EventServerContractVersionV1,
		Endpoint:         endpoint,
		Audience:         testEventServerAudience,
		Protocol:         protoevents.Protocol,
		CredentialSource: source,
		HTTPClient:       client,
		MaxAttempts:      1,
		BaseBackoff:      time.Microsecond,
		MaxBackoff:       time.Millisecond,
	}
	for _, apply := range mutate {
		apply(&config)
	}
	transport, err := NewEventServerTransport(config)
	if err != nil {
		t.Fatalf("NewEventServerTransport: %v", err)
	}
	return transport
}

func staticEventServerCredential() CredentialSource {
	return CredentialSourceFunc(func(context.Context, string) (string, error) {
		return "token", nil
	})
}

func writeEventServerAccepted(t *testing.T, w http.ResponseWriter, request eventServerPublishRequest) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(protoevents.PublishRef{
		Protocol:  request.Protocol,
		ID:        request.ID,
		Topic:     request.Topic,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
	}); err != nil {
		t.Errorf("encode accepted response: %v", err)
	}
}

func writeEventServerError(t *testing.T, w http.ResponseWriter, status int, retryable bool) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(protoevents.EventServerError{
		Protocol:  protoevents.Protocol,
		Code:      protoevents.ErrorUpstreamUnavailable,
		Message:   "try again",
		Retryable: retryable,
	}); err != nil {
		t.Errorf("encode error response: %v", err)
	}
}

func TestEventServerTransportPublishesCanonicalManagedFrame(t *testing.T) {
	var gotAudience string
	credentials := CredentialSourceFunc(func(_ context.Context, audience string) (string, error) {
		gotAudience = audience
		return "workload-id-token", nil
	})
	traceparent := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	tracestate := "vendor=value"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != protoevents.DefaultEventServerPublishPath {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer workload-id-token" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		if got := r.Header.Get("traceparent"); got != traceparent {
			t.Errorf("traceparent = %q", got)
		}
		if got := r.Header.Get("tracestate"); got != tracestate {
			t.Errorf("tracestate = %q", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			return
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(body, &raw); err != nil {
			t.Errorf("decode raw body: %v", err)
			return
		}
		for _, forbidden := range []string{"channel", "workspaceId", "environment", "workload", "service", "topologyGenerationId", "attempt"} {
			if _, ok := raw[forbidden]; ok {
				t.Errorf("managed request contains authority/provider field %q: %s", forbidden, body)
			}
		}
		var request eventServerPublishRequest
		if err := json.Unmarshal(body, &request); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		if request.Protocol != protoevents.Protocol || request.Type != protoevents.FramePublish {
			t.Errorf("protocol/type = %q/%q", request.Protocol, request.Type)
		}
		if request.ID != "event-1" || request.DedupeKey != "orders:o1" || request.TopicVersion != "v1" {
			t.Errorf("identity/version = %+v", request)
		}
		if request.Topic != "orders.created" || request.Key != "o1" || request.Attributes["region"] != "eu" {
			t.Errorf("routing fields = %+v", request)
		}
		writeEventServerAccepted(t, w, request)
	}))
	defer server.Close()

	transport := newTestEventServerTransport(t, server.URL, server.Client(), credentials)
	ctx := WithW3CTraceContext(t.Context(), traceparent, tracestate)
	err := transport.Publish(ctx, Envelope{
		Protocol:     ProtocolVersion,
		ID:           "event-1",
		Topic:        "orders.created",
		Payload:      map[string]string{"orderId": "o1"},
		Key:          "o1",
		DedupeKey:    "orders:o1",
		TopicVersion: "v1",
		Attributes:   map[string]string{"region": "eu"},
		TraceID:      "4bf92f3577b34da6a3ce929d0e0e4736",
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if gotAudience != testEventServerAudience {
		t.Errorf("credential audience = %q", gotAudience)
	}
}

func TestEventServerTransportGeneratesStableIdentityAndBytesAcrossRetries(t *testing.T) {
	spectest.Proves(t, "go/event-delivery", "retry-boundary", "retry-preserves-stable-event-identity")
	var mu sync.Mutex
	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			return
		}
		mu.Lock()
		bodies = append(bodies, append([]byte(nil), body...))
		attempt := len(bodies)
		mu.Unlock()
		if attempt == 1 {
			writeEventServerError(t, w, http.StatusServiceUnavailable, true)
			return
		}
		if attempt == 2 {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("intermediary failure"))
			return
		}
		var request eventServerPublishRequest
		if err := json.Unmarshal(body, &request); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		writeEventServerAccepted(t, w, request)
	}))
	defer server.Close()

	transport := newTestEventServerTransport(t, server.URL, server.Client(), staticEventServerCredential(), func(config *EventServerTransportConfig) {
		config.MaxAttempts = 3
	})
	err := transport.Publish(t.Context(), Envelope{
		Protocol:     ProtocolVersion,
		Topic:        "orders.created",
		TopicVersion: "v1",
		Payload:      map[string]string{"orderId": "o1"},
		Key:          "o1",
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 3 {
		t.Fatalf("attempts = %d, want 3", len(bodies))
	}
	for i := 1; i < len(bodies); i++ {
		if string(bodies[i]) != string(bodies[0]) {
			t.Fatalf("retry %d bytes changed:\nfirst: %s\nretry: %s", i+1, bodies[0], bodies[i])
		}
	}
	var request eventServerPublishRequest
	if err := json.Unmarshal(bodies[0], &request); err != nil {
		t.Fatal(err)
	}
	if request.ID == "" || request.DedupeKey != request.ID {
		t.Fatalf("generated identity = id %q dedupe %q", request.ID, request.DedupeKey)
	}
}

func TestEventServerTransportClassifiesOutcomes(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		body      string
		outcome   EventServerPublishOutcome
		code      perrors.Code
		retryable bool
	}{
		{name: "bad request", status: 400, body: `{"protocol":"putnami.events.v1","code":"invalid_frame","message":"bad","retryable":false}`, outcome: EventServerPublishPermanent, code: CodeEventsEventServerPermanent},
		{name: "unauthorized", status: 401, body: `{"protocol":"putnami.events.v1","code":"unauthorized","message":"bad","retryable":false}`, outcome: EventServerPublishPermanent, code: CodeEventsEventServerPermanent},
		{name: "forbidden", status: 403, body: `{"protocol":"putnami.events.v1","code":"forbidden","message":"bad","retryable":false}`, outcome: EventServerPublishPermanent, code: CodeEventsEventServerPermanent},
		{name: "rate limited", status: 429, body: `{"protocol":"putnami.events.v1","code":"rate_limited","message":"later","retryable":true}`, outcome: EventServerPublishRetryable, code: CodeEventsEventServerRetryable, retryable: true},
		{name: "unavailable structured", status: 503, body: `{"protocol":"putnami.events.v1","code":"upstream_unavailable","message":"later","retryable":true}`, outcome: EventServerPublishRetryable, code: CodeEventsEventServerRetryable, retryable: true},
		{name: "bad gateway", status: 502, body: `gateway unavailable`, outcome: EventServerPublishAmbiguous, code: CodeEventsEventServerAmbiguous},
		{name: "gateway timeout", status: 504, body: `gateway timeout`, outcome: EventServerPublishAmbiguous, code: CodeEventsEventServerAmbiguous},
		{name: "unstructured unavailable", status: 503, body: `unavailable`, outcome: EventServerPublishAmbiguous, code: CodeEventsEventServerAmbiguous},
		{name: "unstructured rate limited", status: 429, body: `rate limited`, outcome: EventServerPublishAmbiguous, code: CodeEventsEventServerAmbiguous},
		{name: "structured permanent without retryability", status: 400, body: `{"protocol":"putnami.events.v1","code":"invalid_frame","message":"bad"}`, outcome: EventServerPublishAmbiguous, code: CodeEventsEventServerAmbiguous},
		{name: "structured retry without retryability", status: 503, body: `{"protocol":"putnami.events.v1","code":"upstream_unavailable","message":"later"}`, outcome: EventServerPublishAmbiguous, code: CodeEventsEventServerAmbiguous},
		{name: "unstructured internal", status: 500, body: `boom`, outcome: EventServerPublishAmbiguous, code: CodeEventsEventServerAmbiguous},
		{name: "malformed acceptance", status: 200, body: `{"protocol":"putnami.events.v1","id":"wrong"}`, outcome: EventServerPublishAmbiguous, code: CodeEventsEventServerAmbiguous},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()
			transport := newTestEventServerTransport(t, server.URL, server.Client(), staticEventServerCredential())
			err := transport.Publish(t.Context(), validEventServerEnvelope())
			if err == nil {
				t.Fatal("Publish returned nil")
			}
			outcome, ok := EventServerPublishOutcomeOf(err)
			if !ok || outcome != tt.outcome {
				t.Fatalf("outcome = %q, %v; want %q", outcome, ok, tt.outcome)
			}
			if !perrors.Is(err, tt.code) {
				t.Errorf("error %v does not carry code %q", err, tt.code)
			}
			if got := perrors.IsRetryable(err); got != tt.retryable {
				t.Errorf("retryable = %v, want %v", got, tt.retryable)
			}
		})
	}
}

func TestEventServerTransportConsumesSharedValidFixtures(t *testing.T) {
	fixtures := protoevents.ManagedPublishV1Fixtures()
	entries, err := fs.ReadDir(fixtures, "valid")
	if err != nil {
		t.Fatal(err)
	}
	transport := newTestEventServerTransport(
		t,
		"https://events.example.test",
		eventServerDoerFunc(func(*http.Request) (*http.Response, error) {
			return nil, stderrors.New("unexpected network request")
		}),
		staticEventServerCredential(),
	)

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		t.Run(entry.Name(), func(t *testing.T) {
			fixtureBody, err := fs.ReadFile(fixtures, "valid/"+entry.Name())
			if err != nil {
				t.Fatal(err)
			}
			frame, diags := protoevents.ParseAndValidateManagedPublishFrame(fixtureBody)
			if len(diags) != 0 {
				t.Fatalf("shared valid fixture diagnostics: %v", diags)
			}
			body, _, err := transport.buildRequest(Envelope{
				Protocol:     frame.Protocol,
				ID:           frame.ID,
				Topic:        frame.Topic,
				Payload:      frame.Payload,
				Key:          frame.Key,
				DedupeKey:    frame.DedupeKey,
				TopicVersion: frame.TopicVersion,
				Attributes:   frame.Attributes,
				TraceID:      frame.TraceID,
			})
			if err != nil {
				t.Fatalf("buildRequest: %v", err)
			}
			var want, got any
			if err := json.Unmarshal(fixtureBody, &want); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("Go transport semantics differ from shared fixture\nwant: %s\ngot:  %s", fixtureBody, body)
			}
		})
	}
}

func TestEventServerTransportConsumesSharedInvalidFixtures(t *testing.T) {
	fixtures := protoevents.ManagedPublishV1Fixtures()
	entries, err := fs.ReadDir(fixtures, "invalid")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		t.Run(entry.Name(), func(t *testing.T) {
			data, err := fs.ReadFile(fixtures, "invalid/"+entry.Name())
			if err != nil {
				t.Fatal(err)
			}
			if _, diags := protoevents.ParseAndValidateManagedPublishFrame(data); len(diags) == 0 {
				t.Fatal("shared invalid fixture was accepted")
			}
		})
	}
}

func TestEventServerTransportConsumesSharedOutcomeFixtures(t *testing.T) {
	fixtures := protoevents.ManagedPublishV1Fixtures()
	entries, err := fs.ReadDir(fixtures, "outcomes")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		t.Run(entry.Name(), func(t *testing.T) {
			data, err := fs.ReadFile(fixtures, "outcomes/"+entry.Name())
			if err != nil {
				t.Fatal(err)
			}
			fixture, diags := protoevents.ParseAndValidateManagedPublishOutcomeFixture(data)
			if len(diags) != 0 {
				t.Fatalf("shared outcome fixture diagnostics: %v", diags)
			}

			client := eventServerDoerFunc(func(request *http.Request) (*http.Response, error) {
				if fixture.Input.TransportError != "" {
					return nil, stderrors.New(string(fixture.Input.TransportError))
				}
				headers := make(http.Header, len(fixture.Input.HTTP.Headers))
				for name, value := range fixture.Input.HTTP.Headers {
					headers.Set(name, value)
				}
				return &http.Response{
					StatusCode: fixture.Input.HTTP.Status,
					Header:     headers,
					Body:       io.NopCloser(strings.NewReader(eventServerFixtureWireBody(fixture.Input.HTTP.Body))),
					Request:    request,
				}, nil
			})
			transport := newTestEventServerTransport(t, "https://events.example.test", client, staticEventServerCredential())
			err = transport.Publish(t.Context(), Envelope{
				Protocol:     ProtocolVersion,
				ID:           fixture.Request.ID,
				Topic:        fixture.Request.Topic,
				Payload:      map[string]string{"fixture": fixture.Name},
				DedupeKey:    fixture.Request.ID,
				TopicVersion: "v1",
			})

			gotClass := protoevents.ManagedPublishAccepted
			if err != nil {
				outcome, ok := EventServerPublishOutcomeOf(err)
				if !ok {
					t.Fatalf("fixture returned untyped error: %v", err)
				}
				gotClass = protoevents.ManagedPublishOutcomeClass(outcome)
			}
			if gotClass != fixture.Expected.Class {
				t.Fatalf("outcome = %q, want shared fixture class %q; err = %v", gotClass, fixture.Expected.Class, err)
			}
			if fixture.Expected.HonorRetryAfter {
				delay := transport.retryDelay(1, fixture.Input.HTTP.Headers["Retry-After"])
				if delay < 3*time.Second {
					t.Fatalf("Retry-After delay = %s, want at least 3s", delay)
				}
			}
		})
	}
}

func eventServerFixtureWireBody(body json.RawMessage) string {
	var text string
	if err := json.Unmarshal(body, &text); err == nil {
		return text
	}
	return string(body)
}

func TestEventServerTransportNetworkFailureIsAmbiguous(t *testing.T) {
	client := eventServerDoerFunc(func(*http.Request) (*http.Response, error) {
		return nil, stderrors.New("connection reset")
	})
	transport := newTestEventServerTransport(t, "https://events.example.test", client, staticEventServerCredential())
	err := transport.Publish(t.Context(), validEventServerEnvelope())
	if outcome, ok := EventServerPublishOutcomeOf(err); !ok || outcome != EventServerPublishAmbiguous {
		t.Fatalf("outcome = %q, %v; err = %v", outcome, ok, err)
	}
}

func TestEventServerTransportCredentialFailureSendsNothingAndRedactsCause(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()
	credentials := CredentialSourceFunc(func(context.Context, string) (string, error) {
		return "", stderrors.New("provider echoed secret-token-value")
	})
	transport := newTestEventServerTransport(t, server.URL, server.Client(), credentials)
	err := transport.Publish(t.Context(), validEventServerEnvelope())
	if err == nil || !perrors.Is(err, CodeEventsEventServerCredential) {
		t.Fatalf("credential error = %v", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("requests = %d, want 0", requests.Load())
	}
	if strings.Contains(err.Error(), "secret-token-value") {
		t.Fatalf("credential leaked through error: %v", err)
	}
	if outcome, ok := EventServerPublishOutcomeOf(err); !ok || outcome != EventServerPublishRetryable || !perrors.IsRetryable(err) {
		t.Fatalf("credential outcome = %q, %v; retryable = %v", outcome, ok, perrors.IsRetryable(err))
	}
}

func TestEventServerTransportPreflightRejectsAuthorityAndInvalidFrames(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()
	transport := newTestEventServerTransport(t, server.URL, server.Client(), staticEventServerCredential())

	type preflightTest struct {
		name   string
		mutate func(*Envelope)
	}
	tests := make([]preflightTest, 0, 20)
	tests = append(tests,
		preflightTest{name: "channel", mutate: func(env *Envelope) { env.Channel = "caller-workspace" }},
		preflightTest{name: "missing topic", mutate: func(env *Envelope) { env.Topic = "" }},
		preflightTest{name: "missing version", mutate: func(env *Envelope) { env.TopicVersion = "" }},
		preflightTest{name: "protocol mismatch", mutate: func(env *Envelope) { env.Protocol = "putnami.events.v2" }},
		preflightTest{name: "invalid payload", mutate: func(env *Envelope) { env.Payload = json.RawMessage("{") }},
		preflightTest{name: "blank id", mutate: func(env *Envelope) { env.ID = " " }},
		preflightTest{name: "blank dedupe key", mutate: func(env *Envelope) { env.DedupeKey = " " }},
		preflightTest{name: "blank topic", mutate: func(env *Envelope) { env.Topic = " " }},
		preflightTest{name: "blank version", mutate: func(env *Envelope) { env.TopicVersion = " " }},
		preflightTest{name: "physical topic", mutate: func(env *Envelope) { env.Topic = "projects/p/topics/orders" }},
	)
	for _, key := range []string{"workspace_id", "environment", "workload", "service", "channel", "topology_generation_id", "traceparent", "tracestate", "auth.subject", "putnami.route"} {
		key := key
		tests = append(tests, preflightTest{
			name: "reserved " + key,
			mutate: func(env *Envelope) {
				env.Attributes = map[string]string{key: "spoof"}
			},
		})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := validEventServerEnvelope()
			tt.mutate(&env)
			err := transport.Publish(t.Context(), env)
			if outcome, ok := EventServerPublishOutcomeOf(err); !ok || outcome != EventServerPublishPermanent {
				t.Fatalf("outcome = %q, %v; err = %v", outcome, ok, err)
			}
		})
	}
	if requests.Load() != 0 {
		t.Fatalf("preflight sent %d requests", requests.Load())
	}
}

func TestEventServerTransportEnforcesRequestSizeAndDeadline(t *testing.T) {
	t.Run("request size", func(t *testing.T) {
		var requests atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			requests.Add(1)
		}))
		defer server.Close()
		transport := newTestEventServerTransport(t, server.URL, server.Client(), staticEventServerCredential(), func(config *EventServerTransportConfig) {
			config.MaxRequestBytes = 32
		})
		err := transport.Publish(t.Context(), validEventServerEnvelope())
		var publishErr *EventServerPublishError
		if !stderrors.As(err, &publishErr) || publishErr.ProtocolCode != protoevents.ErrorPayloadTooLarge {
			t.Fatalf("size error = %v", err)
		}
		if requests.Load() != 0 {
			t.Fatalf("requests = %d", requests.Load())
		}
	})

	t.Run("deadline", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			// Complete eventually so httptest.Server.Close never waits on a
			// deliberately context-blocked handler after the client times out.
			time.Sleep(100 * time.Millisecond)
		}))
		defer server.Close()
		transport := newTestEventServerTransport(t, server.URL, server.Client(), staticEventServerCredential(), func(config *EventServerTransportConfig) {
			config.Timeout = 20 * time.Millisecond
		})
		err := transport.Publish(t.Context(), validEventServerEnvelope())
		if outcome, ok := EventServerPublishOutcomeOf(err); !ok || outcome != EventServerPublishAmbiguous {
			t.Fatalf("deadline outcome = %q, %v; err = %v", outcome, ok, err)
		}
	})
}

func TestEventServerTransportConfigAndPublishOnlyLifecycle(t *testing.T) {
	valid := EventServerTransportConfig{
		ContractVersion:  EventServerContractVersionV1,
		Endpoint:         "https://events.example.test",
		Audience:         testEventServerAudience,
		Protocol:         protoevents.Protocol,
		CredentialSource: staticEventServerCredential(),
	}
	tests := []struct {
		name   string
		mutate func(*EventServerTransportConfig)
	}{
		{name: "contract", mutate: func(config *EventServerTransportConfig) { config.ContractVersion = 2 }},
		{name: "protocol", mutate: func(config *EventServerTransportConfig) { config.Protocol = "putnami.events.v2" }},
		{name: "audience", mutate: func(config *EventServerTransportConfig) { config.Audience = "" }},
		{name: "credentials", mutate: func(config *EventServerTransportConfig) { config.CredentialSource = nil }},
		{name: "endpoint path", mutate: func(config *EventServerTransportConfig) { config.Endpoint += "/custom" }},
		{name: "endpoint credentials", mutate: func(config *EventServerTransportConfig) { config.Endpoint = "https://user:pass@events.example.test" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := valid
			tt.mutate(&config)
			if _, err := NewEventServerTransport(config); err == nil {
				t.Fatal("NewEventServerTransport accepted invalid config")
			}
		})
	}

	transport, err := NewEventServerTransport(valid)
	if err != nil {
		t.Fatal(err)
	}
	if err := transport.Subscribe(&HandlerDefinition{}); !perrors.Is(err, CodeEventsEventServerUnsupported) {
		t.Fatalf("Subscribe error = %v", err)
	}
	if err := transport.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := transport.Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestEventServerTransportRetryAfterParsing(t *testing.T) {
	if got, ok := parseEventServerRetryAfter("2"); !ok || got != 2*time.Second {
		t.Fatalf("Retry-After seconds = %s, %v", got, ok)
	}
	if _, ok := parseEventServerRetryAfter("invalid"); ok {
		t.Fatal("invalid Retry-After accepted")
	}
}

func TestEventServerTransportRetryAfterIsNotCappedByBackoff(t *testing.T) {
	transport := newTestEventServerTransport(t, "https://events.example.test", nil, staticEventServerCredential(), func(config *EventServerTransportConfig) {
		config.BaseBackoff = 10 * time.Millisecond
		config.MaxBackoff = 50 * time.Millisecond
	})
	if got := transport.retryDelay(1, "3"); got != 3*time.Second {
		t.Fatalf("Retry-After delay = %s, want 3s", got)
	}
}

func validEventServerEnvelope() Envelope {
	return Envelope{
		Protocol:     ProtocolVersion,
		ID:           "event-1",
		Topic:        "orders.created",
		Payload:      map[string]string{"orderId": "o1"},
		DedupeKey:    "orders:o1",
		TopicVersion: "v1",
	}
}

type eventServerDoerFunc func(*http.Request) (*http.Response, error)

func (f eventServerDoerFunc) Do(request *http.Request) (*http.Response, error) {
	return f(request)
}

func ExampleEventServerTransport() {
	transport, err := NewEventServerTransport(EventServerTransportConfig{
		ContractVersion: EventServerContractVersionV1,
		Endpoint:        "https://events.example.com",
		Audience:        "https://events.example.com",
		Protocol:        ProtocolVersion,
		CredentialSource: CredentialSourceFunc(func(context.Context, string) (string, error) {
			return "provider-token", nil
		}),
	})
	fmt.Println(transport != nil, err)
	// Output: true <nil>
}
