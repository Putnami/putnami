package events

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	stderrors "errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
	"golang.org/x/oauth2"
)

// writeFakeADC writes a syntactically valid service_account credentials file (a
// throwaway RSA key and an unreachable token URI) and points Application
// Default Credentials at it. Building the transport parses the file but does
// not contact the token URI, so tests need no Google credentials or network.
func writeFakeADC(t *testing.T, extra map[string]string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	fields := map[string]string{
		"type":         "service_account",
		"project_id":   "test-project",
		"private_key":  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"client_email": "test@test-project.iam.gserviceaccount.com",
		"token_uri":    "https://oauth2.example.invalid/token",
	}
	for k, v := range extra {
		fields[k] = v
	}
	body, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal credentials: %v", err)
	}
	path := filepath.Join(t.TempDir(), "sa.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write credentials: %v", err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path)
}

// pubSubRecorder is an in-memory Pub/Sub REST endpoint that records each
// publish request.
type pubSubRecorder struct {
	t        *testing.T
	server   *httptest.Server
	path     string
	rawPath  string // the path as sent on the wire
	query    string
	header   http.Header
	bodies   [][]byte
	requests []googlePubSubRESTPublishRequest
}

func newPubSubRecorder(t *testing.T, respond func(http.ResponseWriter)) *pubSubRecorder {
	t.Helper()
	rec := &pubSubRecorder{t: t}
	rec.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.path = r.URL.Path
		rec.rawPath = strings.SplitN(r.RequestURI, "?", 2)[0]
		rec.query = r.URL.RawQuery
		rec.header = r.Header.Clone()
		body, _ := io.ReadAll(r.Body)
		rec.bodies = append(rec.bodies, body)
		var req googlePubSubRESTPublishRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("decode publish request: %v", err)
		}
		rec.requests = append(rec.requests, req)
		if respond != nil {
			respond(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"messageIds":["msg-1"]}`))
	}))
	t.Cleanup(rec.server.Close)
	return rec
}

func (r *pubSubRecorder) transport(binding PubSubBinding) *DirectPubSubTransport {
	return newDirectPubSubTransport(binding, &googlePubSubRESTClient{
		client: r.server.Client(), endpoint: r.server.URL + "/", projectID: binding.ProjectID,
	})
}

func (r *pubSubRecorder) topic(name string) *googlePubSubRESTTopic {
	client := &googlePubSubRESTClient{client: r.server.Client(), endpoint: r.server.URL + "/", projectID: "my-project"}
	return client.Topic(name).(*googlePubSubRESTTopic)
}

func TestPubSubTopicNamer(t *testing.T) {
	// The template produces the id a provisioner creates with the same sanitizer.
	if got := PubSubTopicNamer("events-{topic}")("order.created"); got != "events-order.created" {
		t.Errorf("PubSubTopicNamer = %q, want events-order.created", got)
	}
	// A non-canonical logical topic is sanitized to a valid Pub/Sub id, not
	// published raw to an invalid or different topic.
	if got := PubSubTopicNamer("events-{topic}")("Order Created"); got != "events-order-created" {
		t.Errorf("PubSubTopicNamer = %q, want events-order-created", got)
	}
	// The sanitizer runs after substitution, so a non-letter start is prefixed.
	if got := PubSubTopicNamer("{topic}")("9lives"); got != "e-9lives" {
		t.Errorf("PubSubTopicNamer = %q, want e-9lives", got)
	}
	// An empty template stands for the logical name, still sanitized.
	if got := PubSubTopicNamer("")("order.created"); got != "order.created" {
		t.Errorf("empty template = %q, want order.created", got)
	}
	if got := PubSubTopicNamer("")("Order Created"); got != "order-created" {
		t.Errorf("empty template = %q, want order-created", got)
	}
}

func TestPubSubResourceID(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"passthrough valid id", "events-order.created", "events-order.created"},
		{"invalid chars become dashes", "Order Created", "order-created"},
		{"leading digit gets e- prefix", "123-topic", "e-123-topic"},
		{"all-invalid trims empty then pads to 3 chars", "@@@", "e-e"},
		{"empty pads to 3 chars", "", "e-e"},
		{"one char gets e- prefix", "a", "e-a"},
		{"two chars get e- prefix", "ab", "e-ab"},
		{"three chars stay", "abc", "abc"},
		{"goog prefix is reserved", "goog-topic", "e-goog-topic"},
		{"goog prefix in any case", "GOOGtopic", "e-googtopic"},
		{"prefixed over-long id stays at 255", "9" + strings.Repeat("a", 300), "e-9" + strings.Repeat("a", 252)},
		{"leading separators trimmed away", "--_.trim", "trim"},
		{"over-long id truncated to 255", strings.Repeat("a", 300), strings.Repeat("a", 255)},
		{"truncation trims trailing separators", strings.Repeat("a", 253) + "--" + strings.Repeat("b", 40), strings.Repeat("a", 253)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := PubSubResourceID(tc.in); got != tc.want {
				t.Errorf("PubSubResourceID(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestGooglePubSubRESTClientNames(t *testing.T) {
	c := &googlePubSubRESTClient{projectID: "my-project"}
	topic, ok := c.Topic("events-order.created").(*googlePubSubRESTTopic)
	if !ok {
		t.Fatalf("Topic returned %T, want *googlePubSubRESTTopic", c.Topic("x"))
	}
	if topic.name != "projects/my-project/topics/events-order.created" {
		t.Errorf("topic resource = %q", topic.name)
	}
	sub, ok := c.Subscription("events-order-sub").(*googlePubSubPushOnlySubscription)
	if !ok {
		t.Fatalf("Subscription returned %T, want *googlePubSubPushOnlySubscription", c.Subscription("x"))
	}
	if sub.name != "events-order-sub" {
		t.Errorf("subscription name = %q, want events-order-sub", sub.name)
	}
	// The transport is publish-only: handlers receive through push delivery.
	if err := sub.Receive(context.Background(), nil); err == nil {
		t.Fatal("Receive should report that pull is unsupported")
	}
}

func TestNewDirectPubSubTransportRequiresProject(t *testing.T) {
	// Fails before it looks for credentials, so it needs none.
	if _, err := NewDirectPubSubTransport(PubSubBinding{}); err == nil {
		t.Fatal("NewDirectPubSubTransport accepted an empty projectId, want error")
	}
}

func TestNewDirectPubSubTransportFailsWithoutCredentials(t *testing.T) {
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", filepath.Join(t.TempDir(), "missing.json"))
	if _, err := NewDirectPubSubTransport(PubSubBinding{ProjectID: "p"}); err == nil {
		t.Fatal("NewDirectPubSubTransport built a transport without credentials, want error")
	}
}

func TestNewDirectPubSubTransportWithADC(t *testing.T) {
	writeFakeADC(t, nil)
	transport, err := NewDirectPubSubTransport(PubSubBinding{ProjectID: "my-project", TopicTemplate: "events-{topic}"})
	if err != nil {
		t.Fatalf("NewDirectPubSubTransport: %v", err)
	}
	if route, generation := transport.ActivePublishRoute(); route != PublishRouteDirectPubSub || generation != "" {
		t.Fatalf("route = (%q, %q), want (%q, empty)", route, generation, PublishRouteDirectPubSub)
	}
}

func TestPubSubTransportIsBuiltIn(t *testing.T) {
	spectest.Proves(t, "go/event-delivery", "provider-publish", "pubsub-transport-is-built-in")
	resetTransportFactories(t)
	writeFakeADC(t, nil)
	t.Cleanup(func() { SetTransport(nil) })

	// events.transport: pubsub needs no provider module.
	p := Events(PluginConfig{
		TransportKind: TransportKindPubSub,
		PubSub:        PubSubBinding{ProjectID: "my-project", TopicTemplate: "events-{topic}"},
	})
	if err := p.Configure(context.Background(), nil); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if _, ok := p.transport.(*DirectPubSubTransport); !ok {
		t.Fatalf("transport = %T, want *DirectPubSubTransport", p.transport)
	}

	// A missing project fails closed instead of falling back.
	if _, err := buildRegisteredTransport(TransportKindPubSub, TransportBinding{}); err == nil {
		t.Fatal("pubsub transport built without events.pubsub.projectId, want error")
	}

	// A registered factory replaces the built-in transport.
	want := &recordingTransport{}
	RegisterBindingTransportFactory(TransportKindPubSub, func(TransportBinding) (Transport, error) { return want, nil })
	got, err := buildRegisteredTransport(TransportKindPubSub, TransportBinding{PubSub: PubSubBinding{ProjectID: "my-project"}})
	if err != nil || got != want {
		t.Fatalf("registered pubsub factory = (%T, %v), want the registered transport", got, err)
	}
}

func TestDirectPubSubTransportPublishesTheEnvelope(t *testing.T) {
	spectest.Proves(t, "go/event-delivery", "provider-publish", "pubsub-publish-carries-the-envelope")
	rec := newPubSubRecorder(t, nil)
	transport := rec.transport(PubSubBinding{ProjectID: "my-project", TopicTemplate: "events-{topic}"})

	envelope := Envelope{
		ID:         "evt-1",
		Topic:      "order.created",
		Key:        "order-1",
		Payload:    json.RawMessage(`{"id":"order-1"}`),
		Attributes: map[string]string{"tenant": "acme"},
		Timestamp:  time.Unix(1700000000, 0).UTC(),
		Attempt:    1,
	}
	if err := transport.Publish(context.Background(), envelope); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	if want := "/v1/projects/my-project/topics/events-order.created:publish"; rec.path != want {
		t.Errorf("publish path = %q, want %q", rec.path, want)
	}
	if rec.query != "alt=json&prettyPrint=false" {
		t.Errorf("publish query = %q", rec.query)
	}
	if got := rec.header.Get("Content-Type"); got != "application/json" {
		t.Errorf("content type = %q, want application/json", got)
	}
	if len(rec.requests) != 1 || len(rec.requests[0].Messages) != 1 {
		t.Fatalf("requests = %+v, want one request with one message", rec.requests)
	}
	message := rec.requests[0].Messages[0]
	data, err := base64.StdEncoding.DecodeString(message.Data)
	if err != nil {
		t.Fatalf("data is not base64: %v", err)
	}
	var got Envelope
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("data is not a JSON envelope: %v", err)
	}
	payload, _ := json.Marshal(got.Payload)
	if got.ID != envelope.ID || got.Topic != envelope.Topic || got.Key != envelope.Key || string(payload) != `{"id":"order-1"}` {
		t.Errorf("envelope = %+v, want %+v", got, envelope)
	}
	if message.OrderingKey != "order-1" {
		t.Errorf("ordering key = %q, want order-1", message.OrderingKey)
	}
	if message.Attributes["tenant"] != "acme" {
		t.Errorf("attributes = %v, want tenant=acme", message.Attributes)
	}
}

func TestGooglePubSubRESTTopicOmitsEmptyFields(t *testing.T) {
	rec := newPubSubRecorder(t, nil)
	if err := rec.topic("t").Publish(context.Background(), GooglePubSubPublishMessage{Data: []byte("x")}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if want := `{"messages":[{"data":"eA=="}]}`; len(rec.bodies) != 1 || string(rec.bodies[0]) != want {
		t.Errorf("body = %q, want %q", rec.bodies, want)
	}
}

func TestGooglePubSubRESTTopicEscapesTheResourceName(t *testing.T) {
	rec := newPubSubRecorder(t, nil)
	if err := rec.topic("a b%c").Publish(context.Background(), GooglePubSubPublishMessage{Data: []byte("x")}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if want := "/v1/projects/my-project/topics/a b%c:publish"; rec.path != want {
		t.Errorf("path = %q, want %q", rec.path, want)
	}
	if want := "/v1/projects/my-project/topics/a%20b%25c:publish"; rec.rawPath != want {
		t.Errorf("request path = %q, want %q", rec.rawPath, want)
	}
}

func TestGooglePubSubRESTTopicPublishErrors(t *testing.T) {
	const topicName = "projects/my-project/topics/t"
	tests := []struct {
		name    string
		respond func(http.ResponseWriter)
		code    int
		message string
		outcome string
	}{
		{
			name: "json error body",
			respond: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":{"code":500,"message":"boom","status":"INTERNAL"}}`))
			},
			code: 500, message: "boom", outcome: PublishOutcomeAmbiguous,
		},
		{
			name: "unavailable",
			respond: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"error":{"code":503,"message":"try later","status":"UNAVAILABLE"}}`))
			},
			code: 503, message: "try later", outcome: PublishOutcomeRetryable,
		},
		{
			name: "canceled body code",
			respond: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":{"code":499,"message":"canceled","status":"CANCELED"}}`))
			},
			code: 499, message: "canceled", outcome: PublishOutcomeAmbiguous,
		},
		{
			name: "long body is capped",
			respond: func(w http.ResponseWriter) {
				http.Error(w, strings.Repeat("x", 5000), http.StatusBadRequest)
			},
			code: 400, message: strings.Repeat("x", 1024) + "…", outcome: PublishOutcomePermanent,
		},
		{
			name: "body code wins over HTTP status",
			respond: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte(`{"error":{"code":404,"message":"topic not found","status":"NOT_FOUND"}}`))
			},
			code: 404, message: "topic not found", outcome: PublishOutcomePermanent,
		},
		{
			name: "plain text body",
			respond: func(w http.ResponseWriter) {
				http.Error(w, "forbidden", http.StatusForbidden)
			},
			code: 403, message: "forbidden", outcome: PublishOutcomePermanent,
		},
		{
			name: "throttled",
			respond: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusTooManyRequests)
			},
			code: 429, outcome: PublishOutcomeRetryable,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := newPubSubRecorder(t, tc.respond)
			err := rec.topic("t").Publish(context.Background(), GooglePubSubPublishMessage{Data: []byte("x")})
			var publishErr *GooglePubSubPublishError
			if !stderrors.As(err, &publishErr) {
				t.Fatalf("error = %v, want *GooglePubSubPublishError", err)
			}
			if publishErr.StatusCode != tc.code || publishErr.Message != tc.message || publishErr.Topic != topicName {
				t.Errorf("error = %+v, want code %d message %q", publishErr, tc.code, tc.message)
			}
			if !strings.Contains(err.Error(), "publish to "+topicName) {
				t.Errorf("error = %q, want it to name the topic", err.Error())
			}
			if got := classifyDirectPubSubPublishError(err); got != tc.outcome {
				t.Errorf("outcome = %q, want %q", got, tc.outcome)
			}
		})
	}
}

func TestGooglePubSubRESTTopicUnknownDeliveryStateIsAmbiguous(t *testing.T) {
	// A 2xx answer the transport cannot read leaves the delivery state unknown.
	rec := newPubSubRecorder(t, func(w http.ResponseWriter) { _, _ = w.Write([]byte("not json")) })
	err := rec.topic("t").Publish(context.Background(), GooglePubSubPublishMessage{Data: []byte("x")})
	if err == nil || classifyDirectPubSubPublishError(err) != PublishOutcomeAmbiguous {
		t.Fatalf("unreadable answer = %v, want an ambiguous error", err)
	}

	// A 2xx answer must name exactly the one message sent.
	for _, answer := range []string{`{}`, `{"messageIds":[]}`, `{"messageIds":["a","b"]}`, `{"messageIds":[""]}`} {
		rec := newPubSubRecorder(t, func(w http.ResponseWriter) { _, _ = w.Write([]byte(answer)) })
		err := rec.topic("t").Publish(context.Background(), GooglePubSubPublishMessage{Data: []byte("x")})
		if err == nil || classifyDirectPubSubPublishError(err) != PublishOutcomeAmbiguous {
			t.Fatalf("answer %s = %v, want an ambiguous error", answer, err)
		}
	}

	// A request that reaches no server has no answer.
	closed := newPubSubRecorder(t, nil)
	topic := closed.topic("t")
	closed.server.Close()
	err = topic.Publish(context.Background(), GooglePubSubPublishMessage{Data: []byte("x")})
	if err == nil || classifyDirectPubSubPublishError(err) != PublishOutcomeAmbiguous {
		t.Fatalf("network failure = %v, want an ambiguous error", err)
	}

	// A canceled publish reports the context error.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = newPubSubRecorder(t, nil).topic("t").Publish(ctx, GooglePubSubPublishMessage{Data: []byte("x")})
	if !stderrors.Is(err, context.Canceled) || classifyDirectPubSubPublishError(err) != PublishOutcomeAmbiguous {
		t.Fatalf("canceled publish = %v, want an ambiguous context.Canceled", err)
	}

	// A malformed endpoint fails before any request.
	bad := &googlePubSubRESTClient{client: http.DefaultClient, endpoint: "://bad", projectID: "p"}
	if err := bad.Topic("t").Publish(context.Background(), GooglePubSubPublishMessage{}); err == nil {
		t.Fatal("publish to a malformed endpoint succeeded, want error")
	}
}

func TestDirectPubSubOutcomeClassification(t *testing.T) {
	spectest.Proves(t, "go/event-delivery", "provider-publish", "pubsub-publish-failures-are-classified")
	status := func(code int) error { return &GooglePubSubPublishError{Topic: "t", StatusCode: code} }
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "success", want: ""},
		{name: "invalid", err: status(http.StatusBadRequest), want: PublishOutcomePermanent},
		{name: "unauthorized", err: status(http.StatusUnauthorized), want: PublishOutcomePermanent},
		{name: "not found", err: status(http.StatusNotFound), want: PublishOutcomePermanent},
		{name: "request timeout", err: status(http.StatusRequestTimeout), want: PublishOutcomeAmbiguous},
		{name: "conflict", err: status(http.StatusConflict), want: PublishOutcomeAmbiguous},
		{name: "canceled", err: status(499), want: PublishOutcomeAmbiguous},
		{name: "throttled", err: status(http.StatusTooManyRequests), want: PublishOutcomeRetryable},
		{name: "unavailable", err: status(http.StatusServiceUnavailable), want: PublishOutcomeRetryable},
		{name: "internal", err: status(http.StatusInternalServerError), want: PublishOutcomeAmbiguous},
		{name: "bad gateway", err: status(http.StatusBadGateway), want: PublishOutcomeAmbiguous},
		{name: "gateway timeout", err: status(http.StatusGatewayTimeout), want: PublishOutcomeAmbiguous},
		{name: "no token", err: &GooglePubSubCredentialError{Topic: "t"}, want: PublishOutcomeRetryable},
		{name: "invalid token", err: &GooglePubSubCredentialError{Topic: "t", Invalid: true}, want: PublishOutcomePermanent},
		{name: "deadline", err: context.DeadlineExceeded, want: PublishOutcomeAmbiguous},
		{name: "unknown status", err: status(http.StatusContinue), want: PublishOutcomeAmbiguous},
		{name: "network unknown", err: stderrors.New("connection reset after write"), want: PublishOutcomeAmbiguous},
		{name: "stale route", err: ErrStalePublishRoute, want: PublishOutcomeAmbiguous},
	}
	transport := &DirectPubSubTransport{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := transport.ClassifyPublishError(tt.err); got != tt.want {
				t.Fatalf("outcome = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDirectPubSubDurablePublishContract(t *testing.T) {
	spectest.Proves(t, "go/event-delivery", "provider-publish", "a-stale-publish-route-is-ambiguous")
	rec := newPubSubRecorder(t, nil)
	transport := rec.transport(PubSubBinding{ProjectID: "my-project"})
	var _ DurablePublishTransport = transport

	route, generation := transport.ActivePublishRoute()
	if route != PublishRouteDirectPubSub || generation != "" {
		t.Fatalf("active route = %q/%q", route, generation)
	}
	ctx := context.Background()
	if pinned, err := transport.ContextForPublishRoute(ctx, route, generation); err != nil || pinned != ctx {
		t.Fatalf("valid route context = (%v, %v)", pinned, err)
	}
	_, err := transport.ContextForPublishRoute(ctx, PublishRouteEventServer, "generation-1")
	if !stderrors.Is(err, ErrStalePublishRoute) {
		t.Fatalf("stale route error = %v, want ErrStalePublishRoute", err)
	}
	if got := transport.ClassifyPublishError(err); got != PublishOutcomeAmbiguous {
		t.Fatalf("stale route outcome = %q", got)
	}
	if err := transport.Publish(ctx, Envelope{ID: "e", Topic: "t"}); err != nil || len(rec.requests) != 1 {
		t.Fatalf("publish = (%d requests, %v)", len(rec.requests), err)
	}
	// Pull delivery cannot work on a publish-only transport, so it fails loudly.
	err = transport.Subscribe(Handle(NewTopic[string]("t"), func(context.Context, *Message[string]) error { return nil }))
	if err == nil || !strings.Contains(err.Error(), "pubsub transport is publish-only; set events.delivery: push") {
		t.Fatalf("subscribe = %v, want the publish-only configuration error", err)
	}
	if err := transport.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := transport.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

func TestQuotaProjectOf(t *testing.T) {
	if got := quotaProjectOf(nil); got != "" {
		t.Errorf("no credentials file = %q, want empty", got)
	}
	if got := quotaProjectOf([]byte(`not json`)); got != "" {
		t.Errorf("unreadable credentials file = %q, want empty", got)
	}
	if got := quotaProjectOf([]byte(`{"quota_project_id":"billing-project"}`)); got != "billing-project" {
		t.Errorf("quota project = %q, want billing-project", got)
	}
}

// TestNewDirectPubSubTransportAuthorizesEachPublish runs the production client:
// Application Default Credentials from a service account file whose token URI
// is an in-memory token server.
func TestNewDirectPubSubTransportAuthorizesEachPublish(t *testing.T) {
	tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"access-1","token_type":"Bearer","expires_in":3600}`))
	}))
	t.Cleanup(tokens.Close)
	writeFakeADC(t, map[string]string{"token_uri": tokens.URL + "/token", "quota_project_id": "billing-project"})
	rec := newPubSubRecorder(t, nil)
	previous := googlePubSubEndpoint
	googlePubSubEndpoint = rec.server.URL + "/"
	t.Cleanup(func() { googlePubSubEndpoint = previous })

	transport, err := NewDirectPubSubTransport(PubSubBinding{ProjectID: "my-project", TopicTemplate: "events-{topic}"})
	if err != nil {
		t.Fatalf("NewDirectPubSubTransport: %v", err)
	}
	if err := transport.Publish(context.Background(), Envelope{ID: "e", Topic: "order.created"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got := rec.header.Get("Authorization"); got != "Bearer access-1" {
		t.Errorf("Authorization = %q, want Bearer access-1", got)
	}
	if got := rec.header.Get("X-Goog-User-Project"); got != "billing-project" {
		t.Errorf("X-Goog-User-Project = %q, want billing-project", got)
	}
	if want := "/v1/projects/my-project/topics/events-order.created:publish"; rec.path != want {
		t.Errorf("publish path = %q, want %q", rec.path, want)
	}
}

type fakeTokenSource struct {
	token *oauth2.Token
	err   error
	block chan struct{}
}

func (s *fakeTokenSource) Token() (*oauth2.Token, error) {
	if s.block != nil {
		<-s.block
	}
	return s.token, s.err
}

func TestGooglePubSubRESTTopicCredentialFailuresSendNothing(t *testing.T) {
	const secret = "refresh-token-secret"
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name    string
		ctx     context.Context
		tokens  oauth2.TokenSource
		outcome string
	}{
		{"token source fails", context.Background(), &fakeTokenSource{err: stderrors.New("oauth2: " + secret)}, PublishOutcomeRetryable},
		{"no token", context.Background(), &fakeTokenSource{}, PublishOutcomeRetryable},
		{"token lookup outlives the publish", expired, &fakeTokenSource{block: block}, PublishOutcomeRetryable},
		{"empty token", context.Background(), &fakeTokenSource{token: &oauth2.Token{}}, PublishOutcomePermanent},
		{"token with a line break", context.Background(), &fakeTokenSource{token: &oauth2.Token{AccessToken: "a\r\nX-Injected: 1"}}, PublishOutcomePermanent},
		{"token with spaces", context.Background(), &fakeTokenSource{token: &oauth2.Token{AccessToken: " a "}}, PublishOutcomePermanent},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := newPubSubRecorder(t, nil)
			client := &googlePubSubRESTClient{client: rec.server.Client(), endpoint: rec.server.URL + "/", projectID: "my-project", tokens: tc.tokens}
			err := client.Topic("t").Publish(tc.ctx, GooglePubSubPublishMessage{Data: []byte("x")})
			var credentialErr *GooglePubSubCredentialError
			if !stderrors.As(err, &credentialErr) {
				t.Fatalf("error = %v, want *GooglePubSubCredentialError", err)
			}
			if len(rec.requests) != 0 {
				t.Errorf("sent %d requests, want none", len(rec.requests))
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("error %q carries the credential provider's text", err.Error())
			}
			if got := classifyDirectPubSubPublishError(err); got != tc.outcome {
				t.Errorf("outcome = %q, want %q", got, tc.outcome)
			}
		})
	}
}

type deadlineDoer struct{ deadline time.Duration }

func (d *deadlineDoer) Do(req *http.Request) (*http.Response, error) {
	if deadline, ok := req.Context().Deadline(); ok {
		d.deadline = time.Until(deadline)
	}
	return nil, stderrors.New("no network in this test")
}

func TestGooglePubSubRESTTopicBoundsEachPublish(t *testing.T) {
	doer := &deadlineDoer{}
	client := &googlePubSubRESTClient{client: doer, endpoint: "http://pubsub.example.invalid/", projectID: "my-project"}
	_ = client.Topic("t").Publish(context.Background(), GooglePubSubPublishMessage{Data: []byte("x")})
	if doer.deadline <= 0 || doer.deadline > googlePubSubPublishTimeout {
		t.Fatalf("publish deadline = %v, want at most %v", doer.deadline, googlePubSubPublishTimeout)
	}
	if googlePubSubPublishTimeout != 10*time.Second {
		t.Fatalf("publish timeout = %v, want 10s", googlePubSubPublishTimeout)
	}
}

func TestGooglePubSubCredentialErrorMessage(t *testing.T) {
	missing := &GooglePubSubCredentialError{Topic: "projects/p/topics/t"}
	if want := "events.google_pubsub: publish to projects/p/topics/t: no access token from the credentials"; missing.Error() != want {
		t.Errorf("Error() = %q, want %q", missing.Error(), want)
	}
	invalid := &GooglePubSubCredentialError{Topic: "projects/p/topics/t", Invalid: true}
	if !strings.Contains(invalid.Error(), "invalid access token") {
		t.Errorf("Error() = %q, want it to name the invalid token", invalid.Error())
	}
}

func TestTruncateErrorTextKeepsRunes(t *testing.T) {
	// A cut inside a multi-byte rune moves back to the rune start.
	in := strings.Repeat("a", 1023) + "é" + "tail"
	if got := truncateErrorText(in); got != strings.Repeat("a", 1023)+"…" {
		t.Errorf("truncateErrorText cut %q", got[len(got)-8:])
	}
}

func TestGooglePubSubPublishErrorMessage(t *testing.T) {
	err := &GooglePubSubPublishError{Topic: "projects/p/topics/t", StatusCode: 404, Status: "NOT_FOUND", Message: "gone"}
	if want := "events.google_pubsub: publish to projects/p/topics/t: status 404 NOT_FOUND: gone"; err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}
}

func TestCheckPublishRoute(t *testing.T) {
	ctx := context.Background()
	if got, err := CheckPublishRoute(ctx, PublishRouteEventServer, "g1", PublishRouteEventServer, "g1"); err != nil || got != ctx {
		t.Fatalf("matching route = (%v, %v)", got, err)
	}
	if _, err := CheckPublishRoute(ctx, PublishRouteEventServer, "g1", PublishRouteEventServer, "g2"); !stderrors.Is(err, ErrStalePublishRoute) {
		t.Fatalf("other generation = %v, want ErrStalePublishRoute", err)
	}
}
