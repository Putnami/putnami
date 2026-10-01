# Events

`go.putnami.dev/events` is a typed event system with pluggable transports for the Putnami Go framework. It provides compile-time safe event publishing and subscribing through Go generics, with a built-in in-memory broker that supports competing and broadcast distribution, attribute-based filtering, automatic retries with exponential backoff, and dead-letter queues.

## Defining Events

Events are organized around **topics**. A topic is a typed contract that defines the payload structure for a particular event. Topics carry no transport logic and can be shared across packages.

```go
import "go.putnami.dev/events"

type OrderPlaced struct {
    OrderID string
    Amount  float64
}

var OrderPlacedTopic = events.NewTopic[OrderPlaced](
    "order.placed",
    events.WithTopicVersion[OrderPlaced]("v1"),
    events.WithTopicChannel[OrderPlaced]("orders"),
    events.WithTopicValidator[OrderPlaced](func(p OrderPlaced) error {
        if p.OrderID == "" {
            return fmt.Errorf("order id is required")
        }
        return nil
    }),
)
```

Topic names are plain strings. Use dot-separated names to organize events by domain (e.g., `user.created`, `order.placed`, `payment.failed`).
Topic channels are logical routing metadata aligned with the canonical `putnami.events.v1` protocol; they are not physical transport names.

## Publishing Events

There are two ways to publish events: through a `Publisher` instance or with the convenience `Publish` function.

### Using a Publisher

Create a `Publisher` bound to a specific topic and transport. This is useful when you publish to the same topic repeatedly. If the topic has a validator, the publisher runs it before sending the envelope.

```go
broker := events.NewMemoryBroker()
publisher := events.NewPublisher(OrderPlacedTopic, broker)

err := publisher.Publish(ctx, OrderPlaced{
    OrderID: "order-123",
    Amount:  99.99,
})
```

### Using the Convenience Function

The `Publish` function is a one-liner that creates a publisher internally. It returns an error if the transport is nil.

```go
err := events.Publish(ctx, broker, OrderPlacedTopic, OrderPlaced{
    OrderID: "order-456",
    Amount:  42.00,
})
```

### Publish Options

Attach metadata to published messages using functional options.

```go
err := publisher.Publish(ctx, payload,
    events.WithMessageID("message-123"),
    events.WithKey("order-123"),
    events.WithDedupeKey("order.placed:order-123"),
    events.WithAttributes(map[string]string{"region": "eu", "priority": "high"}),
    events.WithTraceID("trace-abc-123"),
)
```

- `WithMessageID` sets a stable message identifier.
- `WithKey` sets a routing or partition key.
- `WithDedupeKey` sets an idempotency key.
- `WithAttributes` sets key-value pairs used for routing and filtering.
- `WithTraceID` attaches a trace identifier for distributed tracing.

## Subscribing to Events

Use the `Handle` function to create a typed handler for a topic. The handler receives a `*Message[T]` with the payload already deserialized to the correct type.

```go
handler := events.Handle(OrderPlacedTopic, func(ctx context.Context, msg *events.Message[OrderPlaced]) error {
    fmt.Printf("Order %s placed for $%.2f\n", msg.Payload.OrderID, msg.Payload.Amount)
    return nil
})

err := broker.Subscribe(handler)
```

`Handle` also decodes canonical JSON/map payloads into the handler payload type. A Go handler can therefore receive events published by TypeScript as long as the payload JSON matches the Go struct tags.

The `Message[T]` struct provides access to the full message envelope:

| Field        | Type                | Description                         |
|--------------|---------------------|-------------------------------------|
| `ID`         | `string`            | Unique message identifier           |
| `Topic`      | `string`            | Topic name                          |
| `Channel`    | `string`            | Logical routing channel             |
| `Payload`    | `T`                 | Typed event payload                 |
| `Key`        | `string`            | Routing or partition key            |
| `DedupeKey`  | `string`            | Idempotency key                     |
| `TopicVersion` | `string`          | Topic contract version              |
| `Timestamp`  | `time.Time`         | When the message was published      |
| `Attributes` | `map[string]string` | Custom key-value metadata           |
| `Attempt`    | `int`               | Current delivery attempt (starts at 1) |
| `TraceID`    | `string`            | Distributed trace identifier        |

## Handler Options

Configure handler behavior with functional options passed to `Handle`.

```go
handler := events.Handle(topic, handlerFunc,
    events.WithDistribution(events.Broadcast),
    events.WithGroup("orders.email"),
    events.WithMaxRetries(5),
    events.WithBaseBackoff(500 * time.Millisecond),
    events.WithMaxBackoff(10 * time.Second),
    events.WithTimeout(5 * time.Second),
    events.WithConcurrency(4),
    events.WithQueueLimit(100),
    events.WithOverflow(events.OverflowThrow),
    events.WithDLQ(true),
    events.WithAckMode(events.AutoAck),
    events.WithFilter(map[string]string{"region": "eu"}),
)
```

### Distribution Modes

| Mode        | Behavior                                                        |
|-------------|-----------------------------------------------------------------|
| `Competing` | Round-robin across handlers -- only one handler receives each message. This is the default. |
| `Broadcast` | Every matching handler receives a copy of the message.          |

### Retry and Backoff

When a handler returns an error, the broker retries delivery with exponential backoff and jitter.

| Option          | Default      | Description                                     |
|-----------------|------------- |-------------------------------------------------|
| `WithMaxRetries`| 10           | Maximum number of retry attempts                |
| `WithBaseBackoff`| 1 second    | Initial backoff before the first retry          |
| `WithMaxBackoff`| 60 seconds   | Upper bound on backoff delay between retries    |
| `WithTimeout`   | 30 seconds   | Per-invocation timeout for the handler function |
| `WithConcurrency` | 0          | Maximum concurrent invocations for one handler. `0` means unlimited |
| `WithQueueLimit` | 0           | Queued deliveries waiting for concurrency slots. `0` means unlimited |
| `WithOverflow` | `OverflowThrow` | Queue overflow behavior: return an error or drop |

Backoff starts at roughly 1 second, doubles per attempt, caps at `maxBackoff`, and adds 0-25% random jitter to prevent thundering herds.

### Manual Acknowledgement

By default, returning `nil` acknowledges the message and returning an error triggers retry. With manual ack, the handler must call `msg.Ack()` or return `msg.Nack("reason")`.

```go
handler := events.Handle(topic, func(ctx context.Context, msg *events.Message[OrderPlaced]) error {
    if err := reserveInventory(ctx, msg.Payload); err != nil {
        return msg.Nack(err.Error())
    }
    msg.Ack()
    return nil
}, events.WithAckMode(events.ManualAck))
```

### Attribute Filtering

Handlers can declare attribute filters. Only messages whose attributes match all filter key-value pairs are delivered.

```go
// This handler only receives messages with region=us.
handler := events.Handle(topic, handlerFunc,
    events.WithFilter(map[string]string{"region": "us"}),
    events.WithDistribution(events.Broadcast),
)
```

Publish with matching attributes:

```go
events.Publish(ctx, broker, topic, payload,
    events.WithAttributes(map[string]string{"region": "us"}),
)
```

## Memory Broker

`MemoryBroker` is the built-in in-memory transport. It dispatches messages to handlers in goroutines and supports the full feature set: distribution modes, filtering, retries, and dead-letter queues.

```go
broker := events.NewMemoryBroker()

// Optional: configure drain timeout for graceful shutdown.
broker := events.NewMemoryBroker(events.MemoryBrokerConfig{
    DrainTimeout: 15 * time.Second,
    Observer:     myMetricsBridge,
})
```

### Lifecycle

The broker must be started before publishing and stopped for graceful shutdown.

```go
// Start the broker.
if err := broker.Start(ctx); err != nil {
    log.Fatal(err)
}

// ... publish and handle events ...

// Stop waits for in-flight messages to complete (up to DrainTimeout).
if err := broker.Stop(ctx); err != nil {
    log.Printf("drain timeout: %v", err)
}
```

Publishing to a stopped broker returns an `events.stopped` error.

### Dead-Letter Queue

When a handler exhausts all retry attempts and DLQ is enabled (the default), the message is forwarded to a dead-letter topic named `<original-topic>.dlq`. Subscribe to the DLQ topic to inspect or reprocess failed messages.

```go
// Subscribe to the DLQ for order.placed events.
dlqHandler := &events.HandlerDefinition{
    Topic:   "order.placed.dlq",
    Options: events.DefaultHandlerOptions(),
    Handler: func(ctx context.Context, env *events.Envelope) error {
        originalTopic := env.Attributes["dlq.original_topic"]
        errorMsg := env.Attributes["dlq.error"]
        log.Printf("DLQ: topic=%s error=%s", originalTopic, errorMsg)
        return nil
    },
}
broker.Subscribe(dlqHandler)
```

DLQ envelopes carry two additional attributes:

| Attribute             | Description                     |
|-----------------------|---------------------------------|
| `dlq.original_topic`  | The topic where the message originated |
| `dlq.original_attempt` | The attempt that exhausted retries |
| `dlq.error`           | The error string from the last failed attempt |

The broker reports exactly **one** message-level outcome per dead-letter, and only
after the enqueue: an ERROR `message dead-lettered` record (with the original
topic, its true final `attempt`, and the `dlqTopic`) once at least one `.dlq`
subscriber accepted the message, or a single ERROR `message dropped` record
(`event.reason: dlq_enqueue_failed`, or `dlq_no_subscriber` when nothing is
listening) when none did. The two are mutually exclusive, so an alert on
`message dead-lettered` never points at a DLQ the message never reached. The
record shapes are the cross-runtime contract in
`protocols/logging/conformance`.

## Local Development Server

Use `LocalServer` when multiple local processes should share one event bus. The protocol matches the TypeScript local events server.

```go
server := events.NewLocalServer(events.LocalServerConfig{Port: 4222})
if err := server.Start(ctx); err != nil {
    log.Fatal(err)
}
defer server.Stop(ctx)

transport := events.NewLocalServerTransport("http://127.0.0.1:4222", server.Token())
```

The server exposes `/publish`, `/subscribe`, `/pull`, `/ack`, `/unsubscribe`, and `/health`.

## Routing Transport

Route publishes by logical channel or topic pattern:

```go
transport := events.NewRoutingTransport(events.RoutingTransportConfig{
    Transports: map[events.TransportTarget]events.Transport{
        "local": broker,
        "cloud": cloudTransport,
    },
    Routes: []events.TransportRoute{
        {Channel: "orders", Target: "cloud"},
        {Topic: "audit.*", Target: "local"},
    },
    DefaultTransport: "local",
})
```

## Provider Transports

Go ships provider adapters behind minimal client interfaces. This keeps the events module independent from a specific cloud or Redis SDK version.

```go
google := events.NewGooglePubSubTransport(events.GooglePubSubTransportConfig{
    Client: pubsubClient,
})

redis := events.NewRedisStreamTransport(events.RedisStreamTransportConfig{
    Client: redisClient,
    KeyPrefix: "events",
})

fanout := events.NewRedisPubSubTransport(events.RedisPubSubTransportConfig{
    Client: redisPubSubClient,
})
```

`GooglePubSubTransport` and `RedisStreamTransport` are reliable handler transports. `RedisPubSubTransport` is live-only fanout and intentionally does not provide replay, competing consumers, retry, or DLQ.

## Managed Event Server Publishing

Use `EventServerTransport` for managed workload publishing through the canonical
`POST /events/publish` boundary. It is publish-only; push delivery continues to
use the receiver route the plugin mounts, and existing pull/stream transports
are unchanged.

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

The credential hook receives the exact configured audience. Token acquisition
failures occur before any publish request and token material is never included
in the returned error. `WithW3CTraceContext` carries `traceparent` and
`tracestate` as HTTP headers.

Retries remain pinned to the same endpoint with a stable id, dedupe key, and
byte-identical body. Inspect `*events.EventServerPublishError` (or call
`EventServerPublishOutcomeOf`) to distinguish permanent, retryable, and
ambiguous exhaustion. An ambiguous result may already have been accepted, so a
caller must never switch it to another provider transport.

The Go and TypeScript transports share the versioned managed-publish corpus from
`go.putnami.dev/protocol/events`. Go tests load its embedded bytes directly;
TypeScript tests read the same canonical files. This locks request semantics and
accepted/permanent/retryable/ambiguous outcomes across both framework languages.

Managed config may set `events.push.enabled: false` while the receiver identity
and URL are being bootstrapped. The route remains mounted but returns retryable
`503` without dispatching. Omitting the field preserves enabled push delivery.

## App Plugin

Use the plugin to own transport lifecycle and register handlers through the Go application lifecycle:

```go
app.New("orders").Use(events.Events(events.PluginConfig{
    Handlers: []*events.HandlerDefinition{handler},
}))
```

Use `MemoryBrokerConfig.Observer` to bridge publish, handle, retry, DLQ, and drop callbacks into `go.putnami.dev/telemetry` or your logging stack. `DeadLettered` and
`Dropped` follow the same rule as the records above — `DeadLettered` fires once a
`.dlq` subscriber accepted the message, `Dropped` once when none did — so the
metrics bridge and the log stream never disagree about a message's fate.

## Declaring Infrastructure Requirements

At build time the plugin emits a per-project infra-requirements scratch fragment at `.gen/infra/events.json` listing the topics the project consumes and produces. The Go generator syncs that fragment into committed `infra/requirements.json`, and `putnami build` merges committed requirements into the workload's ephemeral `.gen/requirements.json` so deployers can provision the necessary event infrastructure. Subscribed topics are inferred from the handlers you register, but **published topics must be declared** — publishing is otherwise an ad-hoc call the build cannot see.

Declare the topics you publish to with `PluginConfig.Publishes` (topic names) or the type-safe `RegisterPublisher` helper, alongside the handlers that determine your subscriptions:

```go
plugin := events.Events(events.PluginConfig{
    Handlers:  []*events.HandlerDefinition{handler}, // subscribes to OrderPlacedTopic
    Publishes: []string{OrderShippedTopic.Name},
})

// Or declare publishers from their typed topics:
events.RegisterPublisher(plugin, OrderShippedTopic)

app.New("orders").Use(plugin)
```

Topic names are validated against the canonical resource-name pattern and deduplicated. A project with no handlers or declared publishers emits no manifest.

## Transport Interface

The `Transport` interface allows plugging in alternative backends (e.g., Pub/Sub, Kafka, NATS). Any implementation that satisfies the interface works with the publisher and handler system.

```go
type Transport interface {
    Publish(ctx context.Context, env Envelope) error
    Subscribe(def *HandlerDefinition) error
    Start(ctx context.Context) error
    Stop(ctx context.Context) error
}
```

`Envelope` follows the canonical events protocol fields: `protocol`, `id`, `topic`, `channel`, `payload`, `key`, `dedupeKey`, `topicVersion`, `timestamp`, `attributes`, `attempt`, and `traceId`.

## Concurrency

The `MemoryBroker` is goroutine-safe. Key concurrency properties:

- **Handler dispatch**: Each handler invocation runs in its own goroutine. Handler functions must be safe for concurrent execution.
- **Graceful shutdown**: `Stop` waits for all in-flight handlers (including retries) to complete, up to the configured `DrainTimeout`.
- **Thread-safe publishing**: Multiple goroutines can publish concurrently. The broker uses `sync.RWMutex` to protect internal state.
- **Round-robin state**: Competing distribution maintains a per-topic counter protected by a mutex.

## Best Practices

- **Define topics as package-level variables** so they can be imported by both publishers and subscribers.
- **Use dot-separated topic names** for clear domain organization (e.g., `user.created`, `order.shipped`).
- **Prefer `Competing` distribution** for work queues where each message should be processed once. Use `Broadcast` for notifications that multiple consumers need to see.
- **Set reasonable timeouts** on handlers to avoid goroutine leaks. The default is 30 seconds.
- **Monitor DLQ topics** in production to catch persistent failures early.
- **Keep handler functions idempotent** since retries may deliver the same message more than once.
- **Use attributes for routing** instead of encoding routing information in the payload. This keeps payloads clean and enables transport-level filtering.
