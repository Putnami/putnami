# Events

`@putnami/events` provides typed, transport-agnostic event messaging for building event-driven applications.

## Overview

The module has three core concepts:

- **Topics** — typed contracts (name + schema) shared between publishers and subscribers
- **Handlers** — subscribe to topics with configurable retries, distribution, and filtering
- **Publishers** — schema-validated publish functions bound to a topic

## Quick Example

```typescript
// shared/topics.ts
import { topic, Uuid, Email } from '@putnami/events';

export const UserCreated = topic('user.created', {
  id: Uuid,
  email: Email,
  name: String,
});
```

```typescript
// events/user-created.on.ts
import { handler } from '@putnami/events';
import { UserCreated } from '../shared/topics';

export default handler(UserCreated)
  .options({ distribution: 'broadcast' })
  .handle(async (msg) => {
    console.log(`Welcome ${msg.payload.name}!`);
  });
```

```typescript
// publish.ts
import { getPublisher } from '@putnami/events';
import { UserCreated } from '../shared/topics';

const publish = getPublisher(UserCreated);
await publish({ id: crypto.randomUUID(), email: 'jane@example.com', name: 'Jane' });
```

```typescript
// app.ts
import { application } from '@putnami/application';
import { events } from '@putnami/events';

const app = application()
  .use(events()); // Auto-discovers events/*.on.ts handlers

await app.start();
```

## Topics

Define typed topics using the same schema system as the rest of the framework:

```typescript
import { topic, Uuid, Int, Optional, ArrayOf } from '@putnami/events';

export const OrderPlaced = topic('order.placed', {
  orderId: Uuid,
  userId: Uuid,
  items: ArrayOf({ productId: Uuid, quantity: Int }),
  discount: Optional(Number),
  total: Number,
});
```

Topics are pure data — import them in any service for full type safety on both the publisher and subscriber side.

## Handlers

### Handler DI (`.inject()`)

Event handlers support the same typed DI pattern as HTTP handlers. Declare dependencies with `.inject()` and receive them as the first argument in `.handle()`.

```typescript
import { handler } from '@putnami/events';
import { UserCreated } from './topics';
import { UserRepository, WelcomeMailer } from './services';

export default handler(UserCreated)
  .inject({ users: UserRepository, mailer: WelcomeMailer })
  .handle(async ({ users, mailer }, msg) => {
    const user = await users.findById(msg.payload.id);
    if (user) {
      await mailer.send(user.email);
    }
  });
```

When running via `Application + events()`, each handler invocation gets its own DI scope automatically.

### Options

| Option | Default | Description |
|--------|---------|-------------|
| `group` | `undefined` | Stable subscription/group name for external transports such as Redis Streams or Google Pub/Sub |
| `distribution` | `'competing'` | `'competing'` round-robins across instances; `'broadcast'` delivers to all |
| `maxRetries` | `10` | Retry attempts before DLQ |
| `maxBackoff` | `60000` | Max retry delay in ms |
| `timeout` | `30000` | Handler execution timeout in ms |
| `concurrency` | `0` | Max concurrent invocations for this handler. `0` means unlimited. |
| `queueLimit` | `0` | Max queued deliveries waiting for a concurrency slot. `0` means unlimited. |
| `overflow` | `'throw'` | Behavior when `queueLimit` is reached: throw from publish or drop with a metric. |
| `dlq` | `true` | Send to dead-letter queue after max retries. When `false`, failed messages are logged and discarded. |
| `ack` | `'auto'` | `'auto'`: return = ack, throw = nack. `'manual'`: call `msg.ack()` |

### Attribute Filtering

```typescript
handler(OrderPlaced)
  .filter({ attributes: { region: 'eu' } })
  .handle(async (msg) => { ... });
```

### Acknowledgement

By default, returning from a handler acknowledges the message. Throwing triggers retry with exponential backoff (1s, 2s, 4s, ... capped at `maxBackoff`).

For advanced cases, use manual ack:

```typescript
handler(UserCreated)
  .options({ ack: 'manual' })
  .handle(async (msg) => {
    await longProcess(msg.payload);
    msg.ack();
  });
```

## Publishing

```typescript
const publish = getPublisher(UserCreated);

await publish(
  { id: '...', email: 'jane@example.com', name: 'Jane' },
  {
    messageId: 'message-123',
    key: 'user-123',
    dedupeKey: 'user.created:user-123',
    attributes: { region: 'eu' },
    traceId: 'custom-trace',
  },
);
```

Payloads are validated against the topic schema before publishing. Invalid payloads throw immediately.

### Context Propagation

When publishing from inside an HTTP handler or any `runInContext` scope, the publisher **automatically captures**:

- **`traceId`** — inherited from the current async context, preserving end-to-end trace continuity across the HTTP request → event boundary. An explicit `traceId` in options takes precedence.
- **Auth claims** — if the context contains a `user` (set by the OAuth middleware), the claims `sub`, `email`, `azp`, and `client_id` are auto-captured as `auth.*` message attributes.

```typescript
// Inside an authenticated HTTP handler — no extra options needed
app.post('/users', async (ctx) => {
  const user = await createUser(ctx);
  const publish = getPublisher(UserCreated);
  await publish({ id: user.id, email: user.email, name: user.name });
  // envelope.traceId === request traceId
  // envelope.attributes['auth.sub'] === ctx.user.sub
});
```

Handlers can read these on the other side:

```typescript
handler(UserCreated).handle(async (msg) => {
  const who = msg.attributes['auth.sub'];
  const traceId = msg.traceId; // same as HTTP request
});
```

Auto-captured `auth.*` attributes are protected. Caller-supplied attributes whose
keys start with `auth.` are ignored so user/client claims cannot be spoofed.
Non-auth attributes are preserved.

## Handler Auto-Discovery

The plugin automatically discovers handler files with the `.on.ts` suffix in a sibling `events/` directory:

```
src/
├── main.ts                        ← events() called here
└── events/
    ├── user-created.on.ts         → export default handler(UserCreated).handle(...)
    ├── order-placed.on.ts         → export default handler(OrderPlaced).handle(...)
    └── payment/
        └── failed.on.ts           → export default handler(PaymentFailed).handle(...)
```

- **Suffix**: `*.on.ts` identifies a file as an event handler
- **Exports**: default and named exports are scanned for `HandlerDefinition` objects
- **Nesting**: subdirectories are supported for organization (path has no semantic meaning)
- **Build-time**: discovery happens during the `generate` phase, producing a static loader

Multiple handlers can be exported from a single file:

```typescript
// events/order.on.ts
export const onPlaced = handler(OrderPlaced).handle(async (msg) => { ... });
export const onShipped = handler(OrderShipped).handle(async (msg) => { ... });
```

## Plugin Setup

```typescript
import { events } from '@putnami/events';

// Auto-discovery (default) — scans events/*.on.ts
const app = application()
  .use(events());

// Explicit handlers (no scanning)
const app = application()
  .use(events({ autoScan: false, handlers: [handler1, handler2] }));

// Custom scan directory
const app = application()
  .use(events({ scanFolder: 'subscribers' }));
```

In local development (no `EVENTS_ENDPOINT` env var), the plugin starts an in-memory broker with a local HTTP server on port 4222. Multiple services can share the same local server for cross-service event exchange. The `endpoint` config option takes precedence over the env var.

Production deployments can pass an explicit transport:

```typescript
import { events, redisStreamTransport } from '@putnami/events';

const app = application()
  .use(events({
    transport: redisStreamTransport({
      url: process.env.REDIS_URL,
      keyPrefix: 'putnami:events',
    }),
  }));
```

External transports need stable handler groups:

```typescript
handler(OrderPlaced)
  .options({ group: 'billing.reserve-credit' })
  .handle(async (msg) => {
    await reserveCredit(msg.payload);
  });
```

Use `redisStreamTransport()` for Redis Streams consumer groups, or
`googlePubSubTransport()` from `@putnami/events/google` for Google Pub/Sub.
Redis retries and DLQ streams are managed by Putnami. Google Pub/Sub retry,
ack deadline, retention, and dead-letter timing should be configured on the
subscription; Putnami maps successful handlers to `ack()` and failed handlers to
`nack()`.

Managed workloads can select `events.transport: eventserver` and provide an
`events.eventServer` contract-version/origin/audience binding. Application code
supplies the provider-owned `tokenSource`; `@putnami/events` performs canonical
`POST /events/publish`, stable byte-identical retries, W3C trace propagation,
and typed permanent/retryable/ambiguous outcomes without importing a cloud SDK
or falling back to a direct broker. Managed topics must have a non-empty topic
version. See `typescript/framework/events/doc/08-production-transports.md`.

Applications can also use multiple named transports with routing:

```typescript
import { events, redisStreamTransport, topic, Uuid } from '@putnami/events';
import { googlePubSubTransport } from '@putnami/events/google';

export const PageViewed = topic(
  'analytics.page_view',
  { id: Uuid, path: String },
  { channel: 'analytics' },
);

application().use(events({
  transports: {
    stream: redisStreamTransport({ url: process.env.REDIS_URL }),
    analytics: googlePubSubTransport({ client: pubsub }),
  },
  routes: [
    { channel: 'analytics', transport: 'analytics' },
    { match: 'stream.*', transport: 'stream' },
  ],
  defaultTransport: 'stream',
}));
```

Routing resolves topic `channel` routes first, then topic-name `match` routes,
then `defaultTransport`. The channel is logical metadata; it is not
automatically a physical transport name.

See `typescript/framework/events/doc/08-production-transports.md` for transport
option references and production caveats.

## Realtime Brokers

`@putnami/events` also provides Redis and Postgres realtime brokers for
SSE-style fanout. These APIs are separate from reliable handler transports: they
do not provide handler retries, manual ack, DLQ, or competing consumers.

Use Redis Streams when clients should reconnect with `Last-Event-ID` and replay
a short window of missed messages:

```typescript
import { redisStream } from '@putnami/events/redis';
import { OrderUpdated } from './topics';

const realtime = redisStream({
  url: process.env.REDIS_URL,
  keyPrefix: 'putnami:realtime',
  maxLen: 10_000,
  replayTtlMs: 60_000,
});

const sub = await realtime.subscribe(
  OrderUpdated,
  async (message) => {
    // message.id is the Redis stream id; use it as the SSE event id.
    sendSse({ id: message.id, event: message.topic, data: message.payload });
  },
  { from: request.headers.get('last-event-id') ?? 'latest' },
);
```

Use Redis Pub/Sub when live-only fanout is enough and missing messages while a
client is disconnected is acceptable:

```typescript
import { redisPubSub } from '@putnami/events/redis';

const realtime = redisPubSub({
  url: process.env.REDIS_URL,
  keyPrefix: 'putnami:realtime',
});

await realtime.publish('orders.live', { orderId: '...', status: 'paid' });
```

Use Postgres LISTEN/NOTIFY when the app already has a PostgreSQL connection and
live-only fanout is enough:

```typescript
import { postgresRealtime } from '@putnami/events/postgres';

const realtime = postgresRealtime({
  client: pgClient,
  channelPrefix: 'putnami_realtime',
});
```

Passing `url` uses `Bun.RedisClient`. Applications can also pass compatible
Redis clients/adapters to avoid taking a dependency on a specific Redis package.

See `typescript/framework/events/doc/07-redis-realtime.md` for SSE reconnect
examples, `Last-Event-ID` behavior, and broker option references.

## Mobile / Browser Clients

React Native and browser applications should not import server transports or
connect directly to Redis/Pub/Sub/Postgres. Share topic contracts through
client-safe subpaths and subscribe through an Event Server WebSocket endpoint:

```typescript
import { eventClient } from '@putnami/events/client';
import { topic } from '@putnami/events/topic';
import { Uuid } from '@putnami/runtime';

export const UserUpdated = topic('user.updated', {
  id: Uuid,
  name: String,
}, {
  channel: 'realtime',
});

const client = eventClient({
  url: 'wss://api.example.com/events',
  token: async () => `Bearer ${await getAccessToken()}`,
  reconnect: true,
});

const sub = client.subscribe(UserUpdated, (message) => {
  updateUserCache(message.payload);
});
```

See `typescript/framework/events/doc/09-mobile-client.md` for the Event Server
WebSocket protocol and React Native guidance.

## Protocol

The cross-language event envelope and Event Server frame contract is defined in
`protocols/events`, where the JSON schemas and fixtures are canonical. It
also carries a non-normative gRPC profile illustration at
`protocols/events/proto/putnami/events/v1/event_server.proto`
and the public conformance manifest at
`protocols/events/conformance/manifest.json`. TypeScript exposes the
matching constants and types from `@putnami/events/protocol`:

```typescript
import { PUTNAMI_EVENTS_PROTOCOL, type EventServerFrame } from '@putnami/events/protocol';
```

Use the canonical wire field `channel` for logical routing metadata. Avoid
implementation-specific aliases so TypeScript, Go, Putnami Event Plane, and
self-hosted Event Servers stay aligned.

Event Servers should also expose `GET /.well-known/putnami/events`. The
returned capabilities document declares supported endpoint profiles (`sse`,
`websocket`, `http`, `grpc`), feature flags, concrete endpoints, and limits.
Clients can use `isCompatibleEventServer()` before opening a subscription.

Putnami Event Plane is the managed Event Server option: not the only compatible
solution, but the simplest hosted path when you want all Event Server features
without managing the underlying providers.

Event Server implementations should run the public conformance runner against a
deployed endpoint. The runner is black-box by design, so private Putnami Cloud
implementation details are not required to validate compatibility.

Server-to-server auth for Putnami Event Plane should be auto-wired from managed
deployment identity by default. Browser and mobile clients should continue to
use explicit user token/header providers so they can forward refreshed user
credentials safely.

## Observability

### Async Context

Every handler runs inside `runInContext()`. The event context is available via `useContext()` anywhere in the call chain:

```typescript
import { useContext } from '@putnami/runtime';
import type { EventContext } from '@putnami/events';

handler(UserCreated).handle(async (msg) => {
  const ctx = useContext<EventContext>();
  // ctx.traceId, ctx.eventTopic, ctx.eventMessageId, ctx.eventAttempt
});
```

`useLogger()` automatically prefixes log lines with the trace ID, and DI services resolved inside the handler (including `.inject()` dependencies) share the same async context.

### Telemetry Metrics

When the telemetry plugin is active, these metrics are emitted automatically:

| Metric | Type | Description |
|--------|------|-------------|
| `events.handle.{topic}.success` | Counter | Successful handler invocations |
| `events.handle.{topic}.failure` | Counter | Failed handler invocations |
| `events.handle.{topic}.duration` | Histogram | Handler execution time (ms) |
| `events.handle.{topic}.retry` | Counter | Retry attempts |
| `events.handle.{topic}.dlq` | Counter | Messages sent to dead-letter queue |
| `events.publish.{topic}` | Counter | Successfully published messages |
| `events.publish.{topic}.error` | Counter | Publish validation errors |

### Structured Logging

The broker emits one completion entry per delivery. Topic, message ID, attempt,
outcome, and duration are accumulated in the entry's `event` context:

```text
[trace-id] message handled {
  event: { topic: "user.created", messageId: "abc-123", attempt: 1, outcome: "success", duration: 12 }
}
```

## Design Principles

- **Idempotent handlers** — Delivery is at-least-once, so handlers must tolerate duplicates. Opt in with `events({ simulateDuplicates: true })` and the in-memory broker redelivers ~2% of handled messages so a non-idempotent handler fails locally; it is off by default.
- **Conservative defaults** — 10 retries with exponential backoff and DLQ enabled by default.
- **Transport agnostic** — The API is decoupled from the transport. Switch between in-memory (dev) and cloud (production) without code changes.
- **Convention over configuration** — Place `*.on.ts` files in `events/` and they are discovered automatically. Same pattern as `api()` route scanning.
- **Context per message** — Each handler invocation gets its own `runInContext` with trace ID, topic, and message metadata.

See the [`@putnami/events` package documentation](https://github.com/putnami/putnami/tree/main/typescript/framework/events) for the full API reference.

## Support and compatibility

`@putnami/events` is a public, documented, maintained package classified
`stable`. Its event-messaging specification and its accepted at-least-once
delivery decision record live next to the package source.

The promise is that delivery is at-least-once and every loss is accounted for:
handlers must be idempotent, a failed delivery retries with bounded exponential
backoff and then dead-letters, a dead letter with no `<topic>.dlq` subscriber is
kept in a bounded buffer rather than reduced to a log line, and a terminal
outcome emits exactly one record for the message — never one per refusing
subscriber, and never both a drop and a dead-letter record. Stopping the broker
drains the work it already admitted.

The Redis, Google Pub/Sub, and Postgres integrations are structural interfaces
the application supplies; no vendor SDK is a dependency of this package.

Before v1.0.0, minor `0.x` releases may still contain documented breaking
changes; strict compatibility across every pre-1.0 minor is not promised.
