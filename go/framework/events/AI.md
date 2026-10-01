# go.putnami.dev/events

Typed event system aligned with `@putnami/events`: topics, handler builders, canonical `putnami.events.v1` envelopes, memory broker, competing/broadcast distribution, retry, DLQ, manual ack, local cross-process server, routing transport, and app plugin integration.

## Quick Start

```go
import "go.putnami.dev/events"

type OrderCreated struct {
    OrderID string
    UserID  string
}

var OrderTopic = events.NewTopic[OrderCreated]("order.created")
```

Use `WithTopicVersion`, `WithTopicChannel`, `WithTopicMetadata`, and optional `WithTopicValidator` to match TypeScript topic metadata and runtime publish validation.

## Publishing

```go
broker := events.NewMemoryBroker()

// Option 1: Create a reusable publisher
publisher := events.NewPublisher(OrderTopic, broker)
err := publisher.Publish(ctx, OrderCreated{
    OrderID: "123",
    UserID:  "456",
})

// Option 2: One-shot convenience function
err = events.Publish(ctx, broker, OrderTopic, OrderCreated{
    OrderID: "123",
    UserID:  "456",
})
```

## Subscribing

```go
// Competing — only one handler in the group processes each event (default)
broker.Subscribe(events.Handle(OrderTopic,
    func(ctx context.Context, msg *events.Message[OrderCreated]) error {
        return sendNotification(msg.Payload.UserID, msg.Payload.OrderID)
    },
))

// Broadcast — every subscriber gets every event
broker.Subscribe(events.Handle(OrderTopic,
    func(ctx context.Context, msg *events.Message[OrderCreated]) error {
        return updateAnalytics(msg.Payload)
    },
    events.WithDistribution(events.Broadcast),
))
```

Handlers automatically decode JSON/map payloads into `T`, so envelopes published by TypeScript transports can be consumed by Go typed handlers.

## Handler Options

```go
broker.Subscribe(events.Handle(OrderTopic, processPayment,
    events.WithDistribution(events.Competing),  // default: round-robin
    events.WithMaxRetries(5),                    // default: 10
    events.WithBaseBackoff(500*time.Millisecond), // default: 1s (initial retry delay)
    events.WithMaxBackoff(10*time.Second),       // default: 60s
    events.WithTimeout(5*time.Second),           // default: 30s
    events.WithConcurrency(4),                    // default: unlimited
    events.WithQueueLimit(100),                   // default: unlimited
    events.WithOverflow(events.OverflowThrow),    // or OverflowDrop
    events.WithDLQ(true),                        // default: true
    events.WithAckMode(events.ManualAck),         // msg.Ack() / msg.Nack(...)
    events.WithFilter(map[string]string{"region": "us-east"}),
))
```

`MemoryBrokerConfig.Observer` can bridge publish, handle, retry, DLQ, and drop callbacks to telemetry/logging.

## Publish Options

```go
publisher.Publish(ctx, payload,
    events.WithAttributes(map[string]string{"region": "us-east"}),
    events.WithTraceID("trace-123"),
)
```

## Transports and Plugin

```go
plugin := events.Events(events.PluginConfig{
    Handlers:  []*events.HandlerDefinition{handler}, // subscribed topics
    Publishes: []string{OrderShippedTopic.Name},     // declared published topics
})
// Or type-safe: events.RegisterPublisher(plugin, OrderShippedTopic)

// Local dev cross-process bus compatible with TypeScript:
server := events.NewLocalServer()
transport := events.NewLocalServerTransport("http://127.0.0.1:4222", token)

// Reliable provider adapters:
google := events.NewGooglePubSubTransport(events.GooglePubSubTransportConfig{Client: pubsubClient})
redis := events.NewRedisStreamTransport(events.RedisStreamTransportConfig{Client: redisClient})

// Provider-neutral managed publisher. A cloud integration supplies the token
// source; Google metadata/ID-token acquisition is intentionally not in events.
managed, err := events.NewEventServerTransport(events.EventServerTransportConfig{
    ContractVersion: events.EventServerContractVersionV1,
    Endpoint:        "https://events.example.com",
    Audience:        "https://events.example.com",
    Protocol:        events.ProtocolVersion,
    CredentialSource: events.CredentialSourceFunc(func(ctx context.Context, audience string) (string, error) {
        return workloadIDToken(ctx, audience)
    }),
})

// Live-only Redis fanout:
fanout := events.NewRedisPubSubTransport(events.RedisPubSubTransportConfig{Client: redisPubSubClient})

// Route by channel/topic across named transports:
router := events.NewRoutingTransport(events.RoutingTransportConfig{...})
```

`events.transport: eventserver` reads `events.eventServer` (`contractVersion`,
`endpoint`, `audience`, `protocol`, and local topology hints). Provider modules
can use `RegisterBindingTransportFactory` to attach credentials. Managed publishes use
stable id/dedupe identity, byte-stable bounded retries, W3C trace headers, and
typed permanent/retryable/ambiguous errors. They never fall back or dual-publish.

The plugin emits a build-time infra-requirements scratch fragment at `.gen/infra/events.json` from subscribed handlers and declared `Publishes`/`RegisterPublisher` topics. The Go generator syncs that fragment into committed `infra/requirements.json`, and a project with neither emits no fragment.

## Managed Event Server Conformance

The Go framework test suite consumes the managed-workload v1 request and outcome corpus directly from `go.putnami.dev/protocol/events`:

```go
fixtures := protoevents.ManagedPublishV1Fixtures()
request, err := fs.ReadFile(fixtures, "valid/minimal.json")
```

The same canonical bytes are consumed by `@putnami/events`; do not maintain a Go-owned fixture copy. Managed Event Server transports must preserve stable identity and exact request bytes across retries, reject caller-owned routing attributes, and follow the shared outcome classification.

## Push Delivery (serverless)

`Delivery: events.DeliveryPush` lets handlers run on a scale-to-zero workload: instead of a long-lived pull/stream loop, the provider POSTs each event to a receiver route. The plugin mounts that route on the application's single HTTP server when the application configures (the same rule as `platform.Plugin`):

```go
ev := events.Events(events.PluginConfig{
    Handlers: []*events.HandlerDefinition{handler},
    Delivery: events.DeliveryPush,
    Push: events.PushConfig{
        Issuer:                 "https://accounts.google.com",
        Audience:               "https://my-service.run.app/_putnami/events/orders",
        AllowedServiceAccounts: []string{"events-server@my-project.iam.gserviceaccount.com"},
    },
})
server := http.NewServerPlugin(http.ServerConfig{Port: 8080})

a := app.New("my-service")
a.Use(server)
a.Use(ev) // push delivery: registers POST /_putnami/events/{subscription} on server
```

The plugin mounts the receiver after it resolves the `events` config, so a deployment that sets `events.delivery: push` mounts it too, with the resolved issuer, key set, and audience. A push application that holds no server, or several, fails configure with an error that names `RegisterOn`. Call `ev.RegisterOn(server)` before the application configures to choose the server: when the delivery set in code is push it registers the route at once from the push configuration set in code; otherwise it records the server and the plugin mounts the receiver on it at configure. Pull and stream delivery need no server.

The receiver verifies the pusher's OIDC token fail-closed (JWKS signature, issuer, audience) and a **service-account-email allowlist**, then dispatches the decoded envelope. The HTTP status encodes the ack: `2xx` ack, `4xx` dead-letter, `5xx` retry — so handlers must be idempotent. Caller-supplied `auth.*` attributes are stripped before dispatch; the pusher identity never becomes a carried end-user identity. `delivery` travels into the infra requirement so the deployer provisions a push subscription. Pull/stream modes are unchanged.

During first-deploy bootstrap, resolved config may set `events.push.enabled:
false`. The plugin still mounts the receiver, but it returns a retryable `503`
and never invokes a handler. Omitting `enabled` preserves the existing enabled
push behavior.

See `doc/getting-started.md` and `protocols/events/doc/11-push-delivery.md` for full reference.

## Contract invariants

- Reliable handlers are at-least-once: retries preserve delivery identity and
  consumers must tolerate duplicates.
- Retry exhaustion has an explicit terminal outcome; live-only Redis Pub/Sub
  never claims replay, retry, or DLQ semantics.
- Push ingress authenticates before dispatch, strips reserved authority, and
  maps handler outcomes to ack, retry, or dead-letter responses.

Support is **stable** in `../../../putnami.support.json`; the normative feature
contract is `specs/event-delivery.json`, with the decision in
`doc/adr/0001-at-least-once-retry-and-terminal-outcomes.md`.
