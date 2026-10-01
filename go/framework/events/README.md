# Events

The `events` package provides a typed event system with pluggable transports. Topics define event contracts, handlers subscribe to them, and publishers emit events through a configurable transport layer.

## Topics

Define typed topics with Go generics:

```go
import "go.putnami.dev/events"

type UserPayload struct {
    ID    string
    Email string
    Name  string
}

var UserCreated = events.NewTopic[UserPayload](
    "user.created",
    events.WithTopicVersion[UserPayload]("v1"),
    events.WithTopicChannel[UserPayload]("identity"),
    events.WithTopicValidator[UserPayload](func(p UserPayload) error {
        if p.ID == "" {
            return fmt.Errorf("id is required")
        }
        return nil
    }),
)
```

Topics are pure data contracts — they carry no transport logic and can be shared across services. `Channel` is logical routing metadata aligned with the canonical `putnami.events.v1` protocol; it is not a physical transport name.
Validators are optional. When present, publishers run them before wrapping payloads in envelopes.

## Handlers

Subscribe to a topic with a typed handler function:

```go
handler := events.Handle(UserCreated, func(ctx context.Context, msg *events.Message[UserPayload]) error {
    fmt.Printf("Welcome %s!\n", msg.Payload.Name)
    return nil
})
```

Handlers automatically decode JSON/map payloads into `T`. This lets Go handlers consume canonical envelopes published by TypeScript services.

### Handler Options

Configure handler behavior with functional options:

```go
handler := events.Handle(UserCreated, fn,
    events.WithDistribution(events.Broadcast),  // deliver to all handlers
    events.WithMaxRetries(5),                    // retry up to 5 times
    events.WithBaseBackoff(500*time.Millisecond), // initial backoff before first retry
    events.WithMaxBackoff(30*time.Second),       // max backoff between retries
    events.WithTimeout(10*time.Second),          // handler execution timeout
    events.WithConcurrency(4),                   // max in-flight calls for this handler
    events.WithQueueLimit(100),                  // queued deliveries waiting for slots
    events.WithOverflow(events.OverflowThrow),   // or OverflowDrop
    events.WithDLQ(true),                        // send failed messages to DLQ
    events.WithAckMode(events.AutoAck),          // auto-ack on success
)
```

| Option | Default | Description |
|--------|---------|-------------|
| `WithDistribution` | `Competing` | `Competing` round-robins; `Broadcast` delivers to all |
| `WithGroup` | `""` | Stable subscription/consumer group for external transports |
| `WithMaxRetries` | `10` | Retry attempts before DLQ |
| `WithBaseBackoff` | `1s` | Initial backoff before the first retry |
| `WithMaxBackoff` | `60s` | Max retry delay |
| `WithTimeout` | `30s` | Handler execution timeout |
| `WithConcurrency` | `0` | Max concurrent invocations. `0` means unlimited |
| `WithQueueLimit` | `0` | Max queued deliveries. `0` means unlimited |
| `WithOverflow` | `OverflowThrow` | Queue overflow behavior |
| `WithDLQ` | `true` | Send to dead-letter queue after max retries |
| `WithAckMode` | `AutoAck` | `AutoAck`: return=ack, error=nack. `ManualAck`: call `msg.Ack()` or `return msg.Nack(...)` |

### Attribute Filtering

Filter messages by attributes:

```go
handler := events.Handle(OrderPlaced, fn,
    events.WithFilter(map[string]string{"region": "eu"}),
)
```

## Publishing

Publish typed events through a transport:

```go
publisher := events.NewPublisher(UserCreated, broker)
err := publisher.Publish(ctx, UserPayload{
    ID: "123", Email: "jane@example.com", Name: "Jane",
},
    events.WithMessageID("message-123"),
    events.WithKey("user-123"),
    events.WithDedupeKey("user.created:user-123"),
    events.WithTraceID("trace-abc"),
    events.WithAttributes(map[string]string{"region": "us"}),
)
```

Or use the convenience function:

```go
events.Publish(ctx, broker, UserCreated, payload)
```

## Transport Interface

Transports are pluggable:

```go
type Transport interface {
    Publish(ctx context.Context, env Envelope) error
    Subscribe(def *HandlerDefinition) error
    Start(ctx context.Context) error
    Stop(ctx context.Context) error
}
```

`Envelope` follows the canonical events protocol fields: `protocol`, `id`, `topic`, `channel`, `payload`, `key`, `dedupeKey`, `topicVersion`, `timestamp`, `attributes`, `attempt`, and `traceId`.

## Managed Event Server Conformance

Go framework conformance consumes the managed-workload v1 request and outcome
fixtures directly from `go.putnami.dev/protocol/events`:

```go
fixtures := protoevents.ManagedPublishV1Fixtures()
request, err := fs.ReadFile(fixtures, "valid/minimal.json")
```

This is the same canonical corpus consumed by `@putnami/events`; neither
framework keeps an editable language-owned copy. Runtime Event Server transport
implementations must preserve the corpus's stable identity, exact retry bytes,
reserved-attribute, and outcome-classification rules.

## In-Memory Broker

The `MemoryBroker` implements the `Transport` interface using goroutines and channels:

```go
broker := events.NewMemoryBroker(events.MemoryBrokerConfig{
    DrainTimeout: 10 * time.Second,
    Observer:     myMetricsBridge,
})

broker.Subscribe(handler)
broker.Start(ctx)
defer broker.Stop(ctx)

publisher := events.NewPublisher(UserCreated, broker)
publisher.Publish(ctx, payload)
```

Features:
- Competing distribution (round-robin) and broadcast
- Exponential backoff retry with jitter
- Dead-letter queue (`{topic}.dlq`)
- Attribute-based filtering
- Per-handler concurrency, queue limits, and overflow handling
- Manual acknowledgement (`msg.Ack`, `msg.Nack`)
- Observer callbacks for publish, handle, retry, DLQ, and drop metrics
- Graceful shutdown with drain timeout

## Local Development Server

`LocalServer` exposes the memory broker over the same HTTP protocol used by `@putnami/events` local development:

```go
server := events.NewLocalServer(events.LocalServerConfig{Port: 4222})
server.Start(ctx)
defer server.Stop(ctx)

transport := events.NewLocalServerTransport("http://127.0.0.1:4222", server.Token())
```

This allows Go and TypeScript services to publish and subscribe through a shared local bus.

## Routing and App Plugin

Use `NewRoutingTransport` to route by topic or channel across named transports. Use `events.Events(...)` as an `app.Plugin` to own transport lifecycle and handler registration.

## Provider Transports

Go includes provider adapters behind small client interfaces so applications can use their preferred SDK versions:

```go
google := events.NewGooglePubSubTransport(events.GooglePubSubTransportConfig{
    Client: pubsubClient,
})

redis := events.NewRedisStreamTransport(events.RedisStreamTransportConfig{
    Client: redisClient,
})

fanout := events.NewRedisPubSubTransport(events.RedisPubSubTransportConfig{
    Client: redisPubSubClient,
})
```

- `GooglePubSubTransport` is a reliable handler transport. Success maps to `Ack`; failure maps to `Nack`.
- `RedisStreamTransport` is a reliable handler transport using Redis Streams consumer groups.
- `RedisPubSubTransport` is live-only fanout; it does not provide replay, competing consumers, retry, or DLQ.

## Managed Event Server Publisher

`EventServerTransport` is the provider-neutral, publish-only transport for the
managed `POST /events/publish` profile. Provider-specific credential acquisition
stays outside this package:

```go
transport, err := events.NewEventServerTransport(events.EventServerTransportConfig{
    ContractVersion: events.EventServerContractVersionV1,
    Endpoint:        "https://events.example.com",
    Audience:        "https://events.example.com",
    Protocol:        events.ProtocolVersion,
    CredentialSource: events.CredentialSourceFunc(
        func(ctx context.Context, audience string) (string, error) {
            return workloadIDToken(ctx, audience)
        },
    ),
})
```

The transport generates missing id/dedupe identity before its first attempt,
reuses identical request bytes across bounded jittered retries, validates the
matching structured acceptance, and returns `*EventServerPublishError` with a
permanent, retryable, or ambiguous outcome. Ambiguous means acceptance is
unknown; never fall back to Pub/Sub or shadow-publish that event.

Go transport tests consume the embedded
`protocols/events/fixtures/managed-publish/v1` valid, invalid, retry, and outcome
corpus. TypeScript conformance and transport tests use the same canonical profile,
so both publishers agree on required fields, server-owned authority, exact `200`
acceptance, explicit retryability, and ambiguous failures without fixture copies.

Attach W3C headers with `WithW3CTraceContext`, or provide a
`TraceContextSource`. Managed frames reject `channel` and reserved authority
attributes (`workspace_id`, `environment`, `workload`, `service`, `channel`,
`topology_generation_id`, `traceparent`, `tracestate`, `auth.*`, `putnami.*`).
Event Server stamps those values from authenticated state.

Resolved managed config uses:

```yaml
events:
  transport: eventserver
  eventServer:
    contractVersion: 1
    endpoint: https://events.example.com
    audience: https://events.example.com
    protocol: putnami.events.v1
```

Use `RegisterBindingTransportFactory` when a provider module needs to attach a
credential source to that resolved binding.

## Message Lifecycle

1. Publisher validates and wraps payload in an `Envelope`
2. Transport routes the envelope to matching subscriptions
3. Broker applies attribute filters, then distributes (competing/broadcast)
4. Handler runs with timeout context
5. On error: retry with exponential backoff (1s, 2s, 4s, ... capped at `MaxBackoff`)
6. After max retries: attempt DLQ delivery; the result is dead-lettered only
   when at least one DLQ consumer accepts it, otherwise it is dropped

## Support and contract

The SDD owner is `go`. `go.putnami.dev/events` is public, documented,
maintained, and classified **stable** in the repository
[support catalog](../../../putnami.support.json). Before v1.0, minor releases
may still require documented migrations; see the repository
[release policy](../../../RELEASE.md).

The durable delivery contract is [`go/event-delivery`](specs/event-delivery.json).
At-least-once retry and terminal outcomes are recorded in
[ADR 0001](doc/adr/0001-at-least-once-retry-and-terminal-outcomes.md) and protected
by [`events_test.go`](events_test.go), [`adapter_transports_test.go`](adapter_transports_test.go),
and [`push_receiver_test.go`](push_receiver_test.go). Under push delivery the
plugin mounts its receiver on the application's single HTTP server, as the
[route plugin ADR](../http/doc/adr/0003-a-route-plugin-mounts-itself-on-the-application-server.md)
records; [`push_mount_test.go`](push_mount_test.go) protects it.
