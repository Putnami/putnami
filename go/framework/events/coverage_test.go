package events

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

type recordingTransport struct {
	published  []Envelope
	subscribed []*HandlerDefinition
	started    int
	stopped    int

	publishErr   error
	subscribeErr error
	startErr     error
	stopErr      error
}

func (t *recordingTransport) Publish(_ context.Context, env Envelope) error {
	if t.publishErr != nil {
		return t.publishErr
	}
	t.published = append(t.published, env)
	return nil
}

func (t *recordingTransport) Subscribe(def *HandlerDefinition) error {
	if t.subscribeErr != nil {
		return t.subscribeErr
	}
	t.subscribed = append(t.subscribed, def)
	return nil
}

func (t *recordingTransport) Start(context.Context) error {
	t.started++
	return t.startErr
}

func (t *recordingTransport) Stop(context.Context) error {
	t.stopped++
	return t.stopErr
}

func TestRoutingTransportRoutesAndLifecycle(t *testing.T) {
	identity := &recordingTransport{}
	billing := &recordingTransport{}
	fallback := &recordingTransport{}
	transport := NewRoutingTransport(RoutingTransportConfig{
		Transports: map[TransportTarget]Transport{
			"identity": identity,
			"billing":  billing,
			"default":  fallback,
		},
		Routes: []TransportRoute{
			{Channel: "identity", Target: "identity"},
			{Topic: "billing.*", Target: "billing"},
		},
		DefaultTransport: "default",
	})

	handler := &HandlerDefinition{Topic: "user.created", Handler: func(context.Context, *Envelope) error { return nil }}
	if err := transport.Subscribe(handler); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if len(identity.subscribed) != 1 || len(billing.subscribed) != 1 || len(fallback.subscribed) != 1 {
		t.Fatalf("subscribe counts: identity=%d billing=%d fallback=%d", len(identity.subscribed), len(billing.subscribed), len(fallback.subscribed))
	}
	if err := transport.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if identity.started != 1 || billing.started != 1 || fallback.started != 1 {
		t.Fatalf("start counts: identity=%d billing=%d fallback=%d", identity.started, billing.started, fallback.started)
	}

	if err := transport.Publish(context.Background(), Envelope{Topic: "user.created", Channel: "identity"}); err != nil {
		t.Fatalf("publish identity: %v", err)
	}
	if err := transport.Publish(context.Background(), Envelope{Topic: "billing.invoice.created"}); err != nil {
		t.Fatalf("publish billing: %v", err)
	}
	if err := transport.Publish(context.Background(), Envelope{Topic: "audit.logged"}); err != nil {
		t.Fatalf("publish fallback: %v", err)
	}
	if len(identity.published) != 1 || len(billing.published) != 1 || len(fallback.published) != 1 {
		t.Fatalf("publish counts: identity=%d billing=%d fallback=%d", len(identity.published), len(billing.published), len(fallback.published))
	}

	billing.stopErr = stderrors.New("stop billing")
	if err := transport.Stop(context.Background()); err == nil {
		t.Fatal("expected first stop error")
	}
	if identity.stopped != 1 || billing.stopped != 1 || fallback.stopped != 1 {
		t.Fatalf("stop counts: identity=%d billing=%d fallback=%d", identity.stopped, billing.stopped, fallback.stopped)
	}
}

func TestRoutingTransportErrorsAndTopicMatching(t *testing.T) {
	single := &recordingTransport{}
	transport := NewRoutingTransport(RoutingTransportConfig{
		Transports: map[TransportTarget]Transport{"single": single},
	})
	if err := transport.Publish(context.Background(), Envelope{Topic: "anything"}); err != nil {
		t.Fatalf("single transport publish: %v", err)
	}
	if len(single.published) != 1 {
		t.Fatalf("single transport published = %d", len(single.published))
	}

	missing := NewRoutingTransport(RoutingTransportConfig{
		Transports: map[TransportTarget]Transport{"single": single},
		Routes:     []TransportRoute{{Topic: "missing.*", Target: "missing"}},
	})
	if err := missing.Publish(context.Background(), Envelope{Topic: "missing.topic"}); err == nil {
		t.Fatal("expected missing transport target error")
	}

	none := NewRoutingTransport(RoutingTransportConfig{
		Transports: map[TransportTarget]Transport{"one": single, "two": &recordingTransport{}},
	})
	if err := none.Publish(context.Background(), Envelope{Topic: "unmatched"}); err == nil {
		t.Fatal("expected unmatched route error")
	}

	if !matchTopic("literal", "literal") {
		t.Fatal("literal topic should match")
	}
	if matchTopic("literal", "other") {
		t.Fatal("different literal topic should not match")
	}
	if !matchTopic("order.*", "order.created") {
		t.Fatal("glob topic should match")
	}
	if matchTopic("[", "order.created") {
		t.Fatal("invalid glob should not match")
	}
}

func TestPluginLifecycleAndTransportSelection(t *testing.T) {
	SetTransport(nil)
	defer SetTransport(nil)

	topic := NewTopic[string]("plugin.topic")
	transport := &recordingTransport{}
	plugin := Events(PluginConfig{
		Transport: transport,
		Handlers: []*HandlerDefinition{
			Handle(topic, func(context.Context, *Message[string]) error { return nil }),
		},
	})
	if plugin.Name() != "events" {
		t.Fatalf("plugin name = %q", plugin.Name())
	}
	plugin.Register(Handle(NewTopic[string]("plugin.extra"), func(context.Context, *Message[string]) error { return nil }))
	if registrations := plugin.Provides(); len(registrations) != 1 {
		t.Fatalf("registrations = %d, want 1", len(registrations))
	}
	if err := plugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("configure: %v", err)
	}
	if GetTransport() != transport {
		t.Fatal("active transport was not set")
	}
	if len(transport.subscribed) != 2 {
		t.Fatalf("subscribed handlers = %d, want 2", len(transport.subscribed))
	}
	publisher := NewPublisher(topic, transport)
	if err := publisher.Publish(context.Background(), "payload"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if len(transport.published) != 1 {
		t.Fatalf("published = %d, want 1", len(transport.published))
	}
	if err := plugin.Start(context.Background(), nil); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := plugin.Stop(context.Background(), nil); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if GetTransport() != nil {
		t.Fatal("active transport should be cleared after stop")
	}
}

func TestPluginConfigureRoutingEndpointAndLocalDefaults(t *testing.T) {
	defer SetTransport(nil)

	routed := &recordingTransport{}
	routingPlugin := Events(PluginConfig{
		Transports:       map[TransportTarget]Transport{"default": routed},
		DefaultTransport: "default",
	})
	if err := routingPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("routing configure: %v", err)
	}
	if _, ok := routingPlugin.transport.(*RoutingTransport); !ok {
		t.Fatalf("transport = %T, want *RoutingTransport", routingPlugin.transport)
	}
	if err := routingPlugin.Stop(context.Background(), nil); err != nil {
		t.Fatalf("routing stop: %v", err)
	}

	endpointPlugin := Events(PluginConfig{Endpoint: "http://127.0.0.1:4222", Token: "token"})
	if err := endpointPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("endpoint configure: %v", err)
	}
	if _, ok := endpointPlugin.transport.(*LocalServerTransport); !ok {
		t.Fatalf("endpoint transport = %T, want *LocalServerTransport", endpointPlugin.transport)
	}
	if err := endpointPlugin.Start(context.Background(), nil); err != nil {
		t.Fatalf("endpoint start: %v", err)
	}
	if err := endpointPlugin.Stop(context.Background(), nil); err != nil {
		t.Fatalf("endpoint stop: %v", err)
	}

	localPlugin := Events(PluginConfig{Port: 0})
	if err := localPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("local configure: %v", err)
	}
	if localPlugin.localServer == nil || localPlugin.localServer.Broker() == nil {
		t.Fatal("local server was not configured")
	}
	if err := localPlugin.Stop(context.Background(), nil); err != nil {
		t.Fatalf("local stop: %v", err)
	}
}

func TestRedisStreamFailurePaths(t *testing.T) {
	topic := NewTopic[string]("redis.failure")
	originalErr := stderrors.New("handler failed")

	t.Run("retry", func(t *testing.T) {
		client := &fakeRedisCommandClient{}
		transport := NewRedisStreamTransport(RedisStreamTransportConfig{
			Client:    client,
			KeyPrefix: "events",
			MaxLen:    100,
		})
		def := Handle(topic, func(context.Context, *Message[string]) error { return nil },
			WithMaxRetries(2),
			WithMaxBackoff(time.Nanosecond),
		)
		sub := &redisStreamSubscription{def: def, group: "workers"}
		env := Envelope{ID: "retry-1", Topic: topic.Name, Payload: "payload", Attempt: 1}

		if err := transport.handleFailure(context.Background(), originalErr, env, sub, "events:redis.failure", "1-0"); err != nil {
			t.Fatalf("handle retry: %v", err)
		}
		commands := redisCommands(client)
		if len(commands) != 2 || commands[0].name != "XADD" || commands[1].name != "XACK" {
			t.Fatalf("commands = %+v", commands)
		}
		retryEnv := redisXADDEnvelope(t, commands[0])
		if retryEnv.Topic != topic.Name || retryEnv.Attempt != 2 {
			t.Fatalf("retry envelope = %+v", retryEnv)
		}
		// The retry must be re-published to THIS group's per-group retry stream,
		// NOT the shared topic stream — otherwise every other broadcast group
		// reading the shared stream re-runs a handler that already succeeded.
		if commands[0].args[0] != "events:redis.failure:workers:retry" || commands[0].args[1] != "MAXLEN" {
			t.Fatalf("xadd args = %+v", commands[0].args)
		}
		// The original record is XACK'd on the stream it was read from.
		if commands[1].args[0] != "events:redis.failure" {
			t.Fatalf("xack args = %+v", commands[1].args)
		}
	})

	t.Run("dlq", func(t *testing.T) {
		client := &fakeRedisCommandClient{}
		transport := NewRedisStreamTransport(RedisStreamTransportConfig{Client: client, KeyPrefix: "events"})
		def := Handle(topic, func(context.Context, *Message[string]) error { return nil },
			WithMaxRetries(2),
			WithDLQ(true),
		)
		sub := &redisStreamSubscription{def: def, group: "workers"}
		env := Envelope{ID: "dlq-1", Topic: topic.Name, Payload: "payload", Attempt: 2}

		if err := transport.handleFailure(context.Background(), originalErr, env, sub, "events:redis.failure", "2-0"); err != nil {
			t.Fatalf("handle dlq: %v", err)
		}
		commands := redisCommands(client)
		if len(commands) != 2 || commands[0].name != "XADD" || commands[1].name != "XACK" {
			t.Fatalf("commands = %+v", commands)
		}
		dlqEnv := redisXADDEnvelope(t, commands[0])
		if dlqEnv.Topic != "redis.failure.dlq" || dlqEnv.Attempt != 1 {
			t.Fatalf("dlq envelope = %+v", dlqEnv)
		}
		if dlqEnv.Attributes["dlq.original_topic"] != "redis.failure" ||
			dlqEnv.Attributes["dlq.original_attempt"] != "2" ||
			dlqEnv.Attributes["dlq.error"] != originalErr.Error() {
			t.Fatalf("dlq attributes = %+v", dlqEnv.Attributes)
		}
	})

	t.Run("drop", func(t *testing.T) {
		client := &fakeRedisCommandClient{}
		transport := NewRedisStreamTransport(RedisStreamTransportConfig{Client: client})
		def := Handle(topic, func(context.Context, *Message[string]) error { return nil },
			WithMaxRetries(1),
			WithDLQ(false),
		)
		sub := &redisStreamSubscription{def: def, group: "workers"}
		env := Envelope{ID: "drop-1", Topic: topic.Name, Payload: "payload", Attempt: 1}

		if err := transport.handleFailure(context.Background(), originalErr, env, sub, "redis.failure", "3-0"); err != nil {
			t.Fatalf("handle drop: %v", err)
		}
		commands := redisCommands(client)
		if len(commands) != 1 || commands[0].name != "XACK" {
			t.Fatalf("commands = %+v", commands)
		}
	})

	t.Run("canceled", func(t *testing.T) {
		client := &fakeRedisCommandClient{}
		transport := NewRedisStreamTransport(RedisStreamTransportConfig{Client: client})
		def := Handle(topic, func(context.Context, *Message[string]) error { return nil },
			WithMaxRetries(2),
			WithMaxBackoff(time.Second),
		)
		sub := &redisStreamSubscription{def: def, group: "workers"}
		env := Envelope{ID: "cancel-1", Topic: topic.Name, Payload: "payload", Attempt: 1}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if err := transport.handleFailure(ctx, originalErr, env, sub, "redis.failure", "4-0"); !stderrors.Is(err, context.Canceled) {
			t.Fatalf("handle canceled = %v, want context.Canceled", err)
		}
		if len(redisCommands(client)) != 0 {
			t.Fatalf("commands = %+v, want none", redisCommands(client))
		}
	})
}

func TestRedisStreamHelpersAndDecodeFailure(t *testing.T) {
	client := &fakeRedisCommandClient{}
	transport := NewRedisStreamTransport(RedisStreamTransportConfig{
		Client:       client,
		GroupPrefix:  "groups",
		ConsumerName: "consumer",
	})
	topic := NewTopic[string]("redis.helper")
	sub := &redisStreamSubscription{
		def:   Handle(topic, func(context.Context, *Message[string]) error { return nil }, WithGroup("workers")),
		group: "groups:workers",
	}

	if err := transport.handleRecord(context.Background(), sub, "redis.helper", redisStreamRecord{id: "bad-1", message: "{"}); err == nil {
		t.Fatal("expected decode failure")
	}
	if commands := redisCommands(client); len(commands) != 1 || commands[0].name != "XACK" {
		t.Fatalf("commands = %+v", commands)
	}

	if got := transport.groupFor(sub.def, 0); got != "groups:workers" {
		t.Fatalf("group = %q", got)
	}
	broadcast := &HandlerDefinition{Topic: "redis.broadcast", Options: HandlerOptions{Distribution: Broadcast}}
	if got := transport.groupFor(broadcast, 3); got != "groups:broadcast:redis-broadcast:3" {
		t.Fatalf("broadcast group = %q", got)
	}

	records := parseRedisStreamResponse([]any{
		[]any{[]byte("stream"), []any{
			[]any{[]byte("1-0"), []string{"message", "payload"}},
		}},
	}, "stream")
	if len(records) != 1 || records[0].id != "1-0" || records[0].message != "payload" {
		t.Fatalf("records = %+v", records)
	}
	if records := parseRedisStreamResponse("not-a-stream", "stream"); len(records) != 0 {
		t.Fatalf("records = %+v, want empty", records)
	}
	if baseBackoff(4, time.Second, 3*time.Second) != 3*time.Second {
		t.Fatal("retry delay should be capped")
	}
	if baseBackoff(1, 0, 0) != time.Second {
		t.Fatal("retry delay should default to one second on first attempt")
	}
}

func TestRetryBackoffJitterStaysWithinBounds(t *testing.T) {
	spectest.Proves(t, "go/event-delivery", "retry-boundary", "retry-backoff-stays-within-bounds")
	const (
		base       = time.Second
		maxBackoff = 60 * time.Second
	)
	for attempt := 1; attempt <= 8; attempt++ {
		want := baseBackoff(attempt, base, maxBackoff)
		for i := 0; i < 100; i++ {
			delay := retryBackoff(attempt, base, maxBackoff)
			if delay < want {
				t.Fatalf("attempt %d: delay %v below base %v", attempt, delay, want)
			}
			upper := want + time.Duration(float64(want)*0.25)
			if delay > upper {
				t.Fatalf("attempt %d: delay %v above jitter ceiling %v", attempt, delay, upper)
			}
		}
	}
}

// TestBaseBackoffHonorsConfiguredBase asserts the exponential curve scales from
// the configured BaseBackoff, so a sub-second base yields a sub-second first
// retry — mirroring client.RetryConfig.BaseDelay.
func TestBaseBackoffHonorsConfiguredBase(t *testing.T) {
	spectest.Proves(t, "go/event-delivery", "retry-boundary", "retry-backoff-honors-the-configured-base")
	const maxBackoff = 60 * time.Second

	if got := baseBackoff(1, 100*time.Millisecond, maxBackoff); got != 100*time.Millisecond {
		t.Errorf("attempt 1 with 100ms base = %v, want 100ms", got)
	}
	if got := baseBackoff(2, 100*time.Millisecond, maxBackoff); got != 200*time.Millisecond {
		t.Errorf("attempt 2 with 100ms base = %v, want 200ms", got)
	}
	if got := baseBackoff(3, 100*time.Millisecond, maxBackoff); got != 400*time.Millisecond {
		t.Errorf("attempt 3 with 100ms base = %v, want 400ms", got)
	}

	// normalizeHandlerOptions preserves an explicit sub-second base instead of
	// forcing the 1s default.
	opts := normalizeHandlerOptions(HandlerOptions{BaseBackoff: 250 * time.Millisecond})
	if opts.BaseBackoff != 250*time.Millisecond {
		t.Errorf("normalized BaseBackoff = %v, want 250ms", opts.BaseBackoff)
	}
	// An unset base defaults to one second, preserving prior behavior.
	if got := normalizeHandlerOptions(HandlerOptions{}).BaseBackoff; got != time.Second {
		t.Errorf("default BaseBackoff = %v, want 1s", got)
	}
}

func TestLocalServerHelpersAndHTTPBranches(t *testing.T) {
	server := NewLocalServer()
	if server.Broker() == nil {
		t.Fatal("broker is nil")
	}
	if server.Port() != DefaultLocalServerPort {
		t.Fatalf("default port = %d", server.Port())
	}

	if !strings.Contains(localTokenFilePath(0), fmt.Sprintf("%d", DefaultLocalServerPort)) {
		t.Fatalf("default token path = %q", localTokenFilePath(0))
	}

	req := httptest.NewRequest(http.MethodPost, "/health", nil)
	res := httptest.NewRecorder()
	server.auth(server.handleHealth)(res, req)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", res.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/publish", strings.NewReader("{"))
	req.Header.Set("Authorization", "Bearer "+server.Token())
	res = httptest.NewRecorder()
	server.auth(server.handlePublish)(res, req)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("bad publish status = %d", res.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/unsubscribe", strings.NewReader("{"))
	req.Header.Set("Authorization", "Bearer "+server.Token())
	res = httptest.NewRecorder()
	server.auth(server.handleUnsubscribe)(res, req)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("bad unsubscribe status = %d", res.Code)
	}
}

// TestRedisStreamRetryStaysInFailingGroup pins the cross-group amplification
// invariant: with two broadcast handlers on the same topic (⇒ two consumer
// groups reading the shared topic stream), a retry from the group whose handler
// FAILED must be re-consumed only by that group — the other group, which
// already succeeded on first delivery, must NOT be re-invoked by the retry.
func TestRedisStreamRetryStaysInFailingGroup(t *testing.T) {
	spectest.Proves(t, "go/event-delivery", "retry-boundary", "redis-retry-stays-in-its-consumer-group")
	topic := NewTopic[string]("orders.created")
	redis := newStreamRedis()
	transport := NewRedisStreamTransport(RedisStreamTransportConfig{
		Client:       redis,
		KeyPrefix:    "events",
		ConsumerName: "consumer",
	})

	var invokedA, invokedB int
	failFirst := true
	// Handler A fails on its first delivery, then succeeds on the retry.
	defA := Handle(topic, func(_ context.Context, _ *Message[string]) error {
		invokedA++
		if failFirst {
			failFirst = false
			return stderrors.New("A transient failure")
		}
		return nil
	}, WithDistribution(Broadcast), WithMaxRetries(3), WithMaxBackoff(time.Nanosecond))
	// Handler B always succeeds — it must be invoked exactly once (first delivery)
	// and never re-invoked by A's retry.
	defB := Handle(topic, func(_ context.Context, _ *Message[string]) error {
		invokedB++
		return nil
	}, WithDistribution(Broadcast), WithMaxRetries(3), WithMaxBackoff(time.Nanosecond))

	subA := &redisStreamSubscription{def: defA, group: transport.groupFor(defA, 0), id: "0", retryID: "0"}
	subB := &redisStreamSubscription{def: defB, group: transport.groupFor(defB, 1), id: "0", retryID: "0"}
	if subA.group == subB.group {
		t.Fatalf("broadcast subs must have distinct groups, both = %q", subA.group)
	}
	for _, sub := range []*redisStreamSubscription{subA, subB} {
		if err := transport.ensureGroup(context.Background(), sub); err != nil {
			t.Fatalf("ensureGroup(%s): %v", sub.group, err)
		}
	}

	// Publish one event to the shared topic stream.
	if err := transport.Publish(context.Background(), Envelope{
		ID: "o1", Topic: topic.Name, Payload: "payload", Attempt: 1,
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	ctx := context.Background()
	// Drive each group's read loop once against BOTH the shared topic stream and
	// its own retry stream, using the same helper the production consume loop
	// calls. Group A: reads shared (fails → retry re-published to A's retry
	// stream + original XACK'd), then reads A's retry stream (succeeds).
	drainGroup := func(sub *redisStreamSubscription) {
		main := transport.streamFor(sub.def.Topic)
		retry := transport.retryStreamFor(sub.def.Topic, sub.group)
		// Drain any pending, then live, then pending on the retry stream. Two
		// passes cover: pending("0") → live(">") on each stream and the retry
		// entry produced during the main-stream pass.
		for range 3 {
			transport.consumeStream(ctx, sub, main, &sub.id, transport.blockTimeout)
			transport.consumeStream(ctx, sub, retry, &sub.retryID, 0)
		}
	}
	drainGroup(subA)
	drainGroup(subB)

	if invokedA != 2 {
		t.Fatalf("handler A invoked %d times, want 2 (fail then retry-success)", invokedA)
	}
	// The core assertion: B saw the event exactly once and was NOT redelivered
	// A's retry.
	if invokedB != 1 {
		t.Fatalf("handler B invoked %d times, want 1 — A's retry leaked into B's group", invokedB)
	}
	// The retry landed on A's per-group retry stream, never on B's.
	if got := redis.streamLen(transport.retryStreamFor(topic.Name, subA.group)); got == 0 {
		t.Fatal("expected A's retry stream to have received the retry entry")
	}
	if got := redis.streamLen(transport.retryStreamFor(topic.Name, subB.group)); got != 0 {
		t.Fatalf("B's retry stream received %d entries, want 0 — cross-group amplification", got)
	}
}

// TestRedisStreamConsumeStreamBlockScoping pins the read contract: the primary
// topic read carries a BLOCK wait, and the secondary retry read is non-blocking
// (no BLOCK), so an idle retry stream never throttles topic throughput.
func TestRedisStreamConsumeStreamBlockScoping(t *testing.T) {
	client := &fakeRedisCommandClient{response: make(chan any, 2)}
	client.response <- []any{} // topic read → empty batch
	client.response <- []any{} // retry read → empty batch
	transport := NewRedisStreamTransport(RedisStreamTransportConfig{
		Client:       client,
		KeyPrefix:    "events",
		ConsumerName: "consumer",
		BlockTimeout: 5 * time.Second,
		Count:        100,
	})
	sub := &redisStreamSubscription{group: "workers", id: "0", retryID: "0"}
	ctx := context.Background()
	transport.consumeStream(ctx, sub, transport.streamFor("orders"), &sub.id, transport.blockTimeout)
	transport.consumeStream(ctx, sub, transport.retryStreamFor("orders", sub.group), &sub.retryID, 0)

	var reads []redisCommand
	for _, c := range redisCommands(client) {
		if c.name == "XREADGROUP" {
			reads = append(reads, c)
		}
	}
	if len(reads) != 2 {
		t.Fatalf("expected 2 XREADGROUP reads, got %d: %+v", len(reads), reads)
	}
	if !slices.Contains(reads[0].args, "BLOCK") {
		t.Errorf("topic read must block: args = %v", reads[0].args)
	}
	if slices.Contains(reads[1].args, "BLOCK") {
		t.Errorf("retry read must be non-blocking (no BLOCK): args = %v", reads[1].args)
	}
}

func redisCommands(client *fakeRedisCommandClient) []redisCommand {
	client.mu.Lock()
	defer client.mu.Unlock()
	return append([]redisCommand(nil), client.commands...)
}

// streamRedis is a stateful in-memory Redis Streams fake covering the exact
// command surface RedisStreamTransport uses (XADD / XGROUP CREATE / XREADGROUP /
// XACK) with per-group consumer cursors and a PEL, so tests can exercise the
// real read+retry path deterministically without a live Redis or goroutines.
type streamRedis struct {
	mu      sync.Mutex
	seq     int
	streams map[string][]streamEntry
	groups  map[string]*streamGroup // key: stream + "|" + group
}

type streamEntry struct {
	id      string
	message string
}

type streamGroup struct {
	lastDelivered int             // count of entries already delivered to ">"
	pending       map[string]bool // entry IDs delivered but not yet XACK'd
	order         []string        // PEL insertion order for stable "0" reads
}

func newStreamRedis() *streamRedis {
	return &streamRedis{
		streams: map[string][]streamEntry{},
		groups:  map[string]*streamGroup{},
	}
}

func (r *streamRedis) streamLen(stream string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.streams[stream])
}

func (r *streamRedis) Do(_ context.Context, command string, args ...string) (any, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch command {
	case "XADD":
		return r.xadd(args), nil
	case "XGROUP":
		// XGROUP CREATE <stream> <group> <id> [MKSTREAM]
		if len(args) >= 4 && args[0] == "CREATE" {
			key := args[1] + "|" + args[2]
			if _, ok := r.groups[key]; !ok {
				r.groups[key] = &streamGroup{pending: map[string]bool{}}
			}
			if _, ok := r.streams[args[1]]; !ok {
				r.streams[args[1]] = []streamEntry{}
			}
		}
		return "OK", nil
	case "XREADGROUP":
		return r.xreadgroup(args), nil
	case "XACK":
		// XACK <stream> <group> <id>...
		if len(args) >= 3 {
			g := r.groups[args[0]+"|"+args[1]]
			if g != nil {
				for _, id := range args[2:] {
					delete(g.pending, id)
				}
			}
		}
		return "OK", nil
	default:
		return "OK", nil
	}
}

func (r *streamRedis) xadd(args []string) any {
	stream := args[0]
	var message string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "message" {
			message = args[i+1]
			break
		}
	}
	r.seq++
	id := fmt.Sprintf("%d-0", r.seq)
	r.streams[stream] = append(r.streams[stream], streamEntry{id: id, message: message})
	return id
}

func (r *streamRedis) xreadgroup(args []string) any {
	// XREADGROUP GROUP <group> <consumer> BLOCK <ms> COUNT <n> STREAMS <stream> <cursor>
	var group, stream, cursor string
	for i := range args {
		switch args[i] {
		case "GROUP":
			if i+1 < len(args) {
				group = args[i+1]
			}
		case "STREAMS":
			if i+2 < len(args) {
				stream = args[i+1]
				cursor = args[i+2]
			}
		}
	}
	g := r.groups[stream+"|"+group]
	if g == nil {
		return []any{}
	}
	entries := r.streams[stream]
	var selected []streamEntry
	if cursor == ">" {
		// New messages: everything past lastDelivered → mark pending.
		for ; g.lastDelivered < len(entries); g.lastDelivered++ {
			e := entries[g.lastDelivered]
			if !g.pending[e.id] {
				g.pending[e.id] = true
				g.order = append(g.order, e.id)
			}
			selected = append(selected, e)
		}
	} else {
		// Pending read (cursor "0" or a concrete ID): replay this group's PEL in
		// insertion order, honoring the ">= cursor" lower bound Redis applies.
		byID := map[string]streamEntry{}
		for _, e := range entries {
			byID[e.id] = e
		}
		for _, id := range g.order {
			if g.pending[id] && id >= cursor {
				selected = append(selected, byID[id])
			}
		}
	}
	if len(selected) == 0 {
		return []any{}
	}
	inner := make([]any, 0, len(selected))
	for _, e := range selected {
		inner = append(inner, []any{e.id, []any{"message", e.message}})
	}
	return []any{[]any{stream, inner}}
}

func redisXADDEnvelope(t *testing.T, command redisCommand) Envelope {
	t.Helper()
	for i := 0; i+1 < len(command.args); i++ {
		if command.args[i] == "message" {
			var env Envelope
			if err := json.Unmarshal([]byte(command.args[i+1]), &env); err != nil {
				t.Fatalf("unmarshal XADD envelope: %v", err)
			}
			return env
		}
	}
	t.Fatalf("message field not found in XADD args: %+v", command.args)
	return Envelope{}
}
