# Events

`go.putnami.dev/events` provides a typed, transport-agnostic event system for building event-driven services.

It is a **stable** public package owned by the Go SDD surface. Reliable
transports are at-least-once, so handlers must tolerate duplicate delivery;
live-only Redis Pub/Sub does not promise replay, retry, or DLQ. See the
event-delivery specification and accepted retry and terminal-outcome decision
record next to the package source.

## Overview

The package has three core concepts:

- **Topics** — typed contracts shared between publishers and subscribers
- **Handlers** — subscribe to topics with retries, distribution modes, and filtering
- **Publishers** — optionally validated publish functions bound to a topic

## Quick example

```go
import (
    "context"
    "fmt"

    "go.putnami.dev/events"
)

// Define a typed topic
var UserCreated = events.NewTopic[UserPayload](
    "user.created",
    events.WithTopicVersion[UserPayload]("v1"),
    events.WithTopicChannel[UserPayload]("identity"),
)

type UserPayload struct {
    ID    string `json:"id"`
    Email string `json:"email"`
    Name  string `json:"name"`
}

// Subscribe
handler := events.Handle(UserCreated, func(ctx context.Context, msg *events.Message[UserPayload]) error {
    fmt.Printf("Welcome %s!\n", msg.Payload.Name)
    return nil
}, events.WithDistribution(events.Broadcast))

// Publish
broker := events.NewMemoryBroker(events.MemoryBrokerConfig{})
broker.Subscribe(handler)
broker.Start(ctx)

publisher := events.NewPublisher(UserCreated, broker)
publisher.Publish(ctx, UserPayload{ID: "123", Email: "jane@example.com", Name: "Jane"})
```

## Topics

Topics are pure data contracts that carry no transport logic:

```go
import "go.putnami.dev/events"

var OrderPlaced = events.NewTopic[OrderPayload]("order.placed")
var PaymentFailed = events.NewTopic[PaymentPayload]("payment.failed")

type OrderPayload struct {
    OrderID string  `json:"orderId"`
    UserID  string  `json:"userId"`
    Total   float64 `json:"total"`
}

type PaymentPayload struct {
    OrderID string `json:"orderId"`
    Reason  string `json:"reason"`
}
```

Topics can be imported from any package for full type safety on both publisher and subscriber sides.
Use `WithTopicValidator` when you want TypeScript-style runtime validation before publish.

## Handlers

### Basic handler

```go
handler := events.Handle(UserCreated, func(ctx context.Context, msg *events.Message[UserPayload]) error {
    user := msg.Payload
    fmt.Printf("User created: %s (%s)\n", user.Name, user.Email)
    return nil // ack
})
```

### Handler options

```go
handler := events.Handle(OrderPlaced,
    func(ctx context.Context, msg *events.Message[OrderPayload]) error {
        return processOrder(ctx, msg.Payload)
    },
    events.WithDistribution(events.Competing),
    events.WithMaxRetries(5),
    events.WithMaxBackoff(30 * time.Second),
    events.WithTimeout(10 * time.Second),
    events.WithDLQ(true),
)
```

| Option | Default | Description |
|--------|---------|-------------|
| `WithDistribution(mode)` | `Competing` | `Competing` round-robins across instances; `Broadcast` delivers to all |
| `WithMaxRetries(n)` | `10` | Retry attempts before DLQ |
| `WithMaxBackoff(d)` | `60s` | Maximum retry delay |
| `WithTimeout(d)` | `30s` | Handler execution timeout |
| `WithConcurrency(n)` | `0` | Maximum concurrent invocations for one handler |
| `WithQueueLimit(n)` | `0` | Queued deliveries waiting for concurrency slots |
| `WithOverflow(mode)` | `OverflowThrow` | Queue overflow behavior |
| `WithDLQ(bool)` | `true` | Send to dead-letter queue after max retries |
| `WithAckMode(mode)` | `AutoAck` | `AutoAck`: return = ack, error = nack. `ManualAck`: call `msg.Ack()` |
| `WithFilter(attrs)` | — | Filter by message attributes |

### Distribution modes

**Competing** — round-robins messages across instances. Each message is processed by exactly one handler. Use for work distribution:

```go
events.WithDistribution(events.Competing)
```

**Broadcast** — delivers every message to all subscribed handlers. Use for notifications, cache invalidation, or auditing:

```go
events.WithDistribution(events.Broadcast)
```

### Attribute filtering

Subscribe to a subset of messages:

```go
handler := events.Handle(OrderPlaced,
    processEUOrders,
    events.WithFilter(map[string]string{"region": "eu"}),
)
```

### Retry and acknowledgement

By default, returning `nil` acknowledges the message. Returning an error triggers retry with exponential backoff (1s, 2s, 4s, ... capped at `maxBackoff`):

```go
handler := events.Handle(OrderPlaced, func(ctx context.Context, msg *events.Message[OrderPayload]) error {
    if err := processOrder(ctx, msg.Payload); err != nil {
        return err // triggers retry
    }
    return nil // message acknowledged
})
```

For manual acknowledgement:

```go
handler := events.Handle(UserCreated,
    func(ctx context.Context, msg *events.Message[UserPayload]) error {
        go asyncProcess(msg.Payload)
        msg.Ack()
        return nil
    },
    events.WithAckMode(events.ManualAck),
)
```

## Publishing

### Basic publish

```go
publisher := events.NewPublisher(UserCreated, broker)

err := publisher.Publish(ctx, UserPayload{
    ID:    "user-123",
    Email: "jane@example.com",
    Name:  "Jane",
})
```

### Publish with options

```go
err := publisher.Publish(ctx, payload,
    events.WithAttributes(map[string]string{
        "region":  "eu",
        "source":  "api",
    }),
    events.WithTraceID("trace-abc-123"),
)
```

## Message

Handlers receive a typed `Message[T]`:

```go
type Message[T any] struct {
    ID         string
    Topic      string
    Payload    T
    Timestamp  time.Time
    Attributes map[string]string
    Attempt    int
    TraceID    string
}
```

`Handle` automatically decodes canonical JSON/map payloads into `T`, so Go handlers can consume envelopes published by TypeScript services.

## Memory broker

The in-memory broker handles local development and single-process services:

```go
broker := events.NewMemoryBroker(events.MemoryBrokerConfig{})

// Subscribe handlers
broker.Subscribe(handler1)
broker.Subscribe(handler2)

// Start processing
broker.Start(ctx)

// Publish
publisher := events.NewPublisher(UserCreated, broker)
publisher.Publish(ctx, payload)

// Graceful shutdown
broker.Stop(ctx)
```

The broker supports:
- Competing and broadcast distribution
- Exponential backoff retry
- Dead-letter queues
- Attribute-based filtering
- Per-handler concurrency and queue limits
- Manual acknowledgement

After retry exhaustion, DLQ-enabled delivery is recorded as dead-lettered only
if at least one DLQ consumer accepts it. With no accepting DLQ consumer, the
terminal outcome is dropped.

## Local event server

For multi-process local development, run a TypeScript-compatible local server:

```go
server := events.NewLocalServer(events.LocalServerConfig{Port: 4222})
server.Start(ctx)
defer server.Stop(ctx)

transport := events.NewLocalServerTransport("http://127.0.0.1:4222", server.Token())
```

Go and TypeScript services can share this local bus through `/publish`, `/subscribe`, `/pull`, `/ack`, and `/unsubscribe`.

## Routing and plugin

Use `NewRoutingTransport` to route by topic or channel across named transports. Use `events.Events(...)` as an `app.Plugin` to own transport lifecycle and handler registration.

## Provider transports

Go includes provider adapters behind small client interfaces:

- `GooglePubSubTransport` for reliable Google Pub/Sub handlers.
- `RedisStreamTransport` for reliable Redis Streams handlers with consumer groups.
- `RedisPubSubTransport` for live-only Redis fanout.

Applications provide their preferred SDK clients or thin wrappers, so the events module stays independent from provider dependency versions.

## Transport interface

The event system is transport-agnostic. Implement the `Transport` interface for custom backends:

```go
type Transport interface {
    Publish(ctx context.Context, env Envelope) error
    Subscribe(def *HandlerDefinition) error
    Start(ctx context.Context) error
    Stop(ctx context.Context) error
}
```

Switch between in-memory (development) and cloud (production) without code changes.

## Design principles

- **Idempotent handlers** — design handlers to tolerate duplicate delivery
- **Conservative defaults** — 10 retries with exponential backoff and DLQ enabled
- **Transport agnostic** — switch transports without changing application code
- **Type safety** — generics ensure compile-time type checking for payloads

## Related guides

- [Plugins & Lifecycle](/docs/frameworks/go/plugins-and-lifecycle) — event broker lifecycle
- [Telemetry](/docs/frameworks/go/telemetry) — event metrics
- [TypeScript events](/docs/frameworks/typescript/events) — TypeScript equivalent
