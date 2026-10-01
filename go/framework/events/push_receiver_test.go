package events

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"go.putnami.dev/app"
	phttp "go.putnami.dev/http"
	protoevents "go.putnami.dev/protocol/events"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/protocol/infra"
)

// --- helpers ---

func pushBody(t *testing.T, env Envelope) []byte {
	t.Helper()
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	wrapper := protoevents.PushEnvelope{
		Message:      protoevents.PushMessage{Data: base64.StdEncoding.EncodeToString(raw)},
		Subscription: "projects/p/subscriptions/s",
	}
	body, err := json.Marshal(wrapper)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func pushContext(body []byte) *phttp.Context {
	req := httptest.NewRequest("POST", "/_putnami/events/s", bytes.NewReader(body))
	ctx := phttp.NewContext(httptest.NewRecorder(), req)
	ctx.Params = map[string]string{"subscription": "s"}
	return ctx
}

func recordingHandler(topic string, gotTopic *string, ret error) *HandlerDefinition {
	return &HandlerDefinition{
		Topic: topic,
		Handler: func(_ context.Context, env *Envelope) error {
			*gotTopic = env.Topic
			return ret
		},
	}
}

// --- allowlist guard ---

func TestAllowPusher(t *testing.T) {
	p := Events(PluginConfig{Push: PushConfig{AllowedServiceAccounts: []string{"pusher@sa.example"}}})

	cases := []struct {
		name  string
		extra map[string]any
		want  bool
	}{
		{"allowlisted", map[string]any{"email": "pusher@sa.example"}, true},
		{"allowlisted verified", map[string]any{"email": "pusher@sa.example", "email_verified": true}, true},
		{"not allowlisted", map[string]any{"email": "intruder@evil.example"}, false},
		{"empty email", map[string]any{}, false},
		{"unverified email", map[string]any{"email": "pusher@sa.example", "email_verified": false}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := p.allowPusher(&phttp.Claims{Extra: tc.extra}, nil); got != tc.want {
				t.Errorf("allowPusher = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAllowPusherEmptyAllowlistRejects(t *testing.T) {
	spectest.Proves(t, "go/event-delivery", "ingress-authority", "empty-allowlist-rejects-every-pusher")
	p := Events(PluginConfig{})
	if p.allowPusher(&phttp.Claims{Extra: map[string]any{"email": "anyone@sa.example"}}, nil) {
		t.Error("empty allowlist must reject (fail-closed)")
	}
}

// --- envelope decode ---

func TestDecodePushEnvelopeDefaultsAndStrip(t *testing.T) {
	spectest.Proves(t, "go/event-delivery", "ingress-authority", "push-ingress-strips-auth-attributes")
	env := Envelope{Topic: "order.created", Payload: map[string]any{"id": "o-1"}}
	raw, _ := json.Marshal(env)
	wrapper := protoevents.PushEnvelope{
		Message: protoevents.PushMessage{
			Data:       base64.StdEncoding.EncodeToString(raw),
			MessageID:  "msg-1",
			Attributes: map[string]string{"region": "eu", "auth.sub": "spoofed"},
		},
		Subscription: "s",
	}
	got, err := decodePushEnvelope(wrapper)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "msg-1" {
		t.Errorf("ID = %q, want msg-1 (defaulted from messageId)", got.ID)
	}
	if got.Attempt != 1 {
		t.Errorf("Attempt = %d, want 1", got.Attempt)
	}
	if got.Timestamp.IsZero() {
		t.Error("Timestamp should be defaulted to now")
	}
	if _, ok := got.Attributes["auth.sub"]; ok {
		t.Error("auth.* attributes must be stripped (anti-spoofing)")
	}
	if got.Attributes["region"] != "eu" {
		t.Error("non-auth attributes must be preserved")
	}
}

func TestDecodePushEnvelopeErrors(t *testing.T) {
	validData := base64.StdEncoding.EncodeToString([]byte(`{"id":"x","topic":"t","payload":{"a":1},"timestamp":"2026-05-01T12:00:00Z","attempt":1}`))

	// Missing subscription is a wrapper-contract violation, even with a valid
	// encoded envelope.
	if _, err := decodePushEnvelope(protoevents.PushEnvelope{Message: protoevents.PushMessage{Data: validData}}); err == nil {
		t.Error("expected error for push wrapper missing subscription")
	}
	if _, err := decodePushEnvelope(protoevents.PushEnvelope{Subscription: "s", Message: protoevents.PushMessage{Data: "%%%"}}); err == nil {
		t.Error("expected error for bad base64")
	}
	noTopic := base64.StdEncoding.EncodeToString([]byte(`{"id":"x"}`))
	if _, err := decodePushEnvelope(protoevents.PushEnvelope{Subscription: "s", Message: protoevents.PushMessage{Data: noTopic}}); err == nil {
		t.Error("expected error for envelope missing topic")
	}
}

func TestHandlePushMissingSubscriptionDLQs(t *testing.T) {
	p := Events(PluginConfig{Delivery: DeliveryPush})
	// A valid encoded envelope but no subscription on the wrapper must be
	// rejected (400/DLQ), not dispatched.
	raw, err := json.Marshal(Envelope{Topic: "order.created", Payload: map[string]any{"x": 1}, Attempt: 1})
	if err != nil {
		t.Fatal(err)
	}
	noSub, err := json.Marshal(protoevents.PushEnvelope{Message: protoevents.PushMessage{Data: base64.StdEncoding.EncodeToString(raw)}})
	if err != nil {
		t.Fatal(err)
	}
	resp := p.handlePush(pushContext(noSub))
	if resp.Status != 400 {
		t.Fatalf("status = %d, want 400 (DLQ) for missing subscription", resp.Status)
	}
}

func TestPushCompetingDistributionRoundRobins(t *testing.T) {
	spectest.Proves(t, "go/event-delivery", "delivery-semantics", "push-competing-delivery-round-robins")
	var a, b int
	p := Events(PluginConfig{Delivery: DeliveryPush})
	p.Register(&HandlerDefinition{Topic: "order.created", Handler: func(context.Context, *Envelope) error { a++; return nil }})
	p.Register(&HandlerDefinition{Topic: "order.created", Handler: func(context.Context, *Envelope) error { b++; return nil }})

	for i := 0; i < 2; i++ {
		resp := p.handlePush(pushContext(pushBody(t, Envelope{Topic: "order.created", Payload: map[string]any{}})))
		if resp.Status != 204 {
			t.Fatalf("status = %d, want 204", resp.Status)
		}
	}
	// Competing handlers share deliveries round-robin: two pushes hit each once,
	// not both twice.
	if a != 1 || b != 1 {
		t.Errorf("competing handlers got a=%d b=%d, want 1 and 1 (round-robin)", a, b)
	}
}

func TestPushBroadcastDistributionFansOut(t *testing.T) {
	spectest.Proves(t, "go/event-delivery", "delivery-semantics", "push-broadcast-delivery-fans-out")
	var a, b int
	p := Events(PluginConfig{Delivery: DeliveryPush})
	p.Register(&HandlerDefinition{Topic: "order.created", Options: HandlerOptions{Distribution: Broadcast}, Handler: func(context.Context, *Envelope) error { a++; return nil }})
	p.Register(&HandlerDefinition{Topic: "order.created", Options: HandlerOptions{Distribution: Broadcast}, Handler: func(context.Context, *Envelope) error { b++; return nil }})

	resp := p.handlePush(pushContext(pushBody(t, Envelope{Topic: "order.created", Payload: map[string]any{}})))
	if resp.Status != 204 {
		t.Fatalf("status = %d, want 204", resp.Status)
	}
	if a != 1 || b != 1 {
		t.Errorf("broadcast handlers got a=%d b=%d, want both 1", a, b)
	}
}

// --- handlePush status mapping ---

func TestHandlePushAcks(t *testing.T) {
	var got string
	p := Events(PluginConfig{Delivery: DeliveryPush})
	p.Register(recordingHandler("order.created", &got, nil))

	resp := p.handlePush(pushContext(pushBody(t, Envelope{Topic: "order.created", Payload: map[string]any{"id": "o-1"}})))
	if resp.Status != 204 {
		t.Fatalf("status = %d, want 204 (ack)", resp.Status)
	}
	if got != "order.created" {
		t.Errorf("handler not invoked with the delivered topic, got %q", got)
	}
}

func TestHandlePushHandlerErrorRetries(t *testing.T) {
	var got string
	p := Events(PluginConfig{Delivery: DeliveryPush})
	p.Register(recordingHandler("order.created", &got, context.DeadlineExceeded))

	resp := p.handlePush(pushContext(pushBody(t, Envelope{Topic: "order.created", Payload: map[string]any{"id": "o-1"}})))
	if resp.Status != 500 {
		t.Fatalf("status = %d, want 500 (retry)", resp.Status)
	}
}

func TestHandlePushBadBodyDLQs(t *testing.T) {
	p := Events(PluginConfig{Delivery: DeliveryPush})
	resp := p.handlePush(pushContext([]byte("not json")))
	if resp.Status != 400 {
		t.Fatalf("status = %d, want 400 (DLQ)", resp.Status)
	}
}

func TestHandlePushNoMatchingHandlerAcks(t *testing.T) {
	var got string
	p := Events(PluginConfig{Delivery: DeliveryPush})
	// A handler registered for a different topic must not run for this delivery.
	p.Register(recordingHandler("billing.charged", &got, nil))

	resp := p.handlePush(pushContext(pushBody(t, Envelope{Topic: "unhandled.topic", Payload: map[string]any{}})))
	if resp.Status != 204 {
		t.Fatalf("status = %d, want 204 (no handler: drop/ack)", resp.Status)
	}
	if got != "" {
		t.Error("a handler for a different topic must not be invoked")
	}
}

// --- secured chain (OIDC + allowlist), exercised without minting JWTs ---

func TestSecuredPushHandlerRejectsMissingToken(t *testing.T) {
	spectest.Proves(t, "go/event-delivery", "ingress-authority", "push-rejects-a-missing-token")
	p := Events(PluginConfig{
		Delivery: DeliveryPush,
		Push:     PushConfig{Issuer: "https://accounts.google.com", AllowedServiceAccounts: []string{"pusher@sa.example"}},
	})
	// No Authorization header: JWKSJWT resolves no identity, the guard rejects.
	resp := p.securedPushHandler()(pushContext(pushBody(t, Envelope{Topic: "order.created", Payload: map[string]any{}})))
	if resp.Status != 401 {
		t.Fatalf("status = %d, want 401 (fail-closed, no token)", resp.Status)
	}
}

func TestSecuredPushHandlerRejectsNonAllowlisted(t *testing.T) {
	spectest.Proves(t, "go/event-delivery", "ingress-authority", "push-rejects-a-non-allowlisted-pusher")
	var got string
	p := Events(PluginConfig{
		Delivery: DeliveryPush,
		Push:     PushConfig{Issuer: "https://accounts.google.com", AllowedServiceAccounts: []string{"pusher@sa.example"}},
	})
	p.Register(recordingHandler("order.created", &got, nil))

	ctx := pushContext(pushBody(t, Envelope{Topic: "order.created", Payload: map[string]any{}}))
	// A verified identity already on the context (JWKSJWT is skipped) whose
	// email is not on the allowlist must be rejected before dispatch.
	ctx.User = &phttp.Claims{Extra: map[string]any{"email": "intruder@evil.example"}}
	resp := p.securedPushHandler()(ctx)
	if resp.Status != 403 {
		t.Fatalf("status = %d, want 403 (non-allowlisted)", resp.Status)
	}
	if got != "" {
		t.Error("handler must not run for a non-allowlisted pusher")
	}
}

func TestSecuredPushHandlerAllowsAllowlisted(t *testing.T) {
	spectest.Proves(t, "go/event-delivery", "ingress-authority", "push-allows-an-allowlisted-pusher")
	var got string
	p := Events(PluginConfig{
		Delivery: DeliveryPush,
		Push:     PushConfig{Issuer: "https://accounts.google.com", AllowedServiceAccounts: []string{"pusher@sa.example"}},
	})
	p.Register(recordingHandler("order.created", &got, nil))

	ctx := pushContext(pushBody(t, Envelope{Topic: "order.created", Payload: map[string]any{}}))
	ctx.User = &phttp.Claims{Extra: map[string]any{"email": "pusher@sa.example", "email_verified": true}}
	resp := p.securedPushHandler()(ctx)
	if resp.Status != 204 {
		t.Fatalf("status = %d, want 204 (allowlisted, dispatched)", resp.Status)
	}
	if got != "order.created" {
		t.Error("allowlisted pusher should reach the handler")
	}
}

func TestDisabledPushBootstrapMountsAndFailsClosed(t *testing.T) {
	enabled := false
	var calls int
	p := Events(PluginConfig{
		Delivery: DeliveryPush,
		Push:     PushConfig{Enabled: &enabled},
	})
	p.Register(&HandlerDefinition{Topic: "order.created", Handler: func(context.Context, *Envelope) error {
		calls++
		return nil
	}})
	server := phttp.NewServerPlugin(phttp.ServerConfig{Port: 0})
	p.RegisterOn(server)
	testServer := server.TestServer()
	defer testServer.Close()

	response, err := http.Post(testServer.URL+"/_putnami/events/orders", "application/json", bytes.NewReader(pushBody(t, Envelope{
		Topic:   "order.created",
		Payload: map[string]any{"id": "o-1"},
	})))
	if err != nil {
		t.Fatalf("POST disabled receiver: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.StatusCode)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	var protocolError protoevents.EventServerError
	if err := json.Unmarshal(body, &protocolError); err != nil {
		t.Fatalf("decode response %q: %v", body, err)
	}
	if !protocolError.Retryable || protocolError.Code != protoevents.ErrorUpstreamUnavailable {
		t.Fatalf("disabled response = %+v", protocolError)
	}
	if calls != 0 {
		t.Fatalf("disabled receiver invoked handler %d times", calls)
	}
}

func TestUnsetPushEnabledPreservesSecuredReceiver(t *testing.T) {
	p := Events(PluginConfig{
		Delivery: DeliveryPush,
		Push:     PushConfig{Issuer: "https://accounts.google.com"},
	})
	server := phttp.NewServerPlugin(phttp.ServerConfig{Port: 0})
	p.RegisterOn(server)
	testServer := server.TestServer()
	defer testServer.Close()

	response, err := http.Post(testServer.URL+"/_putnami/events/orders", "application/json", bytes.NewReader(pushBody(t, Envelope{
		Topic:   "order.created",
		Payload: map[string]any{"id": "o-1"},
	})))
	if err != nil {
		t.Fatalf("POST enabled receiver: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want legacy secured 401", response.StatusCode)
	}
}

// --- describe emits delivery ---

func TestDescribeEmitsPushDelivery(t *testing.T) {
	type Order struct{ ID string }
	consumed := NewTopic[Order]("order.created")
	p := Events(PluginConfig{Delivery: DeliveryPush})
	p.Register(Handle(consumed, func(context.Context, *Message[Order]) error { return nil }))

	out := t.TempDir()
	if err := p.Describe(&app.DescribeContext{OutputDir: out}); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	data, err := os.ReadFile(infra.SidecarPathIn(out, "events"))
	if err != nil {
		t.Fatal(err)
	}
	var m infra.PerProjectManifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	want := []infra.Subscription{{Topic: "order.created", Delivery: infra.DeliveryPush}}
	if m.Events == nil || len(m.Events.Subscribes) != 1 || m.Events.Subscribes[0] != want[0] {
		t.Errorf("subscribes = %+v, want %+v", m.Events.Subscribes, want)
	}
}

// --- push mode does not start the pull loop ---
// (recordingTransport is defined in coverage_test.go)

func TestPushModeSkipsSubscribeAndStart(t *testing.T) {
	var got string
	tr := &recordingTransport{}
	p := Events(PluginConfig{Delivery: DeliveryPush, Transport: tr})
	p.Register(recordingHandler("order.created", &got, nil))

	if err := p.Configure(context.Background(), nil); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if err := p.Start(context.Background(), nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(tr.subscribed) != 0 {
		t.Errorf("push mode must not Subscribe handlers to the transport, got %d", len(tr.subscribed))
	}
	if tr.started != 0 {
		t.Errorf("push mode must not Start the transport pull loop, got %d", tr.started)
	}
}

func TestPullModeSubscribesAndStarts(t *testing.T) {
	var got string
	tr := &recordingTransport{}
	p := Events(PluginConfig{Transport: tr}) // default pull
	p.Register(recordingHandler("order.created", &got, nil))

	if err := p.Configure(context.Background(), nil); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if err := p.Start(context.Background(), nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(tr.subscribed) != 1 {
		t.Errorf("pull mode must Subscribe the handler, got %d", len(tr.subscribed))
	}
	if tr.started != 1 {
		t.Errorf("pull mode must Start the transport, got %d", tr.started)
	}
}
