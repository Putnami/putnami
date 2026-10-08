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
	return newDirectPubSubTransport(binding, r.server.Client(), r.server.URL+"/")
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
	// An empty template passes the logical name through.
	if got := PubSubTopicNamer("")("order.created"); got != "order.created" {
		t.Errorf("empty template = %q, want passthrough", got)
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
		{"all-invalid trims empty then e- prefix", "@@@", "e-"},
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
	transport, err := NewDirectPubSubTransport(PubSubBinding{ProjectID: "control-project", TopicTemplate: "events-{topic}"})
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
			code: 500, message: "boom", outcome: PublishOutcomeRetryable,
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
		{name: "timeout", err: status(http.StatusRequestTimeout), want: PublishOutcomeRetryable},
		{name: "conflict", err: status(http.StatusConflict), want: PublishOutcomeRetryable},
		{name: "throttled", err: status(http.StatusTooManyRequests), want: PublishOutcomeRetryable},
		{name: "server", err: status(http.StatusBadGateway), want: PublishOutcomeRetryable},
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
	if err := transport.Subscribe(Handle(NewTopic[string]("t"), func(context.Context, *Message[string]) error { return nil })); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := transport.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := transport.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

func TestQuotaProjectTransport(t *testing.T) {
	if quotaProjectTransport(nil) != http.DefaultTransport {
		t.Error("credentials without a quota project should use the default transport")
	}
	if quotaProjectTransport([]byte(`{"type":"service_account"}`)) != http.DefaultTransport {
		t.Error("credentials without quota_project_id should use the default transport")
	}

	var got string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Goog-User-Project")
	}))
	defer server.Close()
	client := &http.Client{Transport: quotaProjectTransport([]byte(`{"quota_project_id":"billing-project"}`))}
	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	_ = resp.Body.Close()
	if got != "billing-project" {
		t.Errorf("X-Goog-User-Project = %q, want billing-project", got)
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
