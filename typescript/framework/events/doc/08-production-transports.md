# Production Transports

`@putnami/events` can run background handlers on external brokers by passing an
explicit transport to the plugin. These transports are for server-side event
handlers. They are not the SSE/realtime broker APIs documented in
`07-redis-realtime.md`.

The package does not add mandatory broker dependencies. Each adapter accepts a
small compatible client interface, so applications can use the Redis, Google, or
test driver they already use.

## Choosing A Transport

| Transport | Use When | Delivery Model | Notes |
|-----------|----------|----------------|-------|
| Local / HTTP transport | Local development, tests, or a dedicated Putnami event service. | In-memory broker semantics behind HTTP. | No external broker setup. Not durable by itself. |
| Event Server publisher | A managed workload publishes through Event Server admission. | Publish-only `POST /events/publish`; subscribers use push delivery. | Provider-neutral credentials, stable retry identity, no direct-broker fallback. |
| Redis Streams transport | You want self-hosted durable queues with consumer groups. | Redis stream per topic, consumer group per handler group. | Putnami manages retries and DLQ streams. Use a stable `consumerName` in production. |
| Google Pub/Sub transport | You want managed cloud pub/sub and subscription-level retry policy. | Topic publish, subscription consume, ack/nack. | Configure retry policy and dead-letter topics in Google Cloud. |

All external transports are at-least-once. Handlers must be idempotent and
should use `msg.id`, `msg.dedupeKey`, or a domain idempotency key when they
write external state.

## Plugin Setup

Passing `transport` makes the plugin use that transport directly. It takes
precedence over `endpoint`, `EVENTS_ENDPOINT`, and the local broker.

```typescript
import { application } from '@putnami/application';
import { events, redisStreamTransport } from '@putnami/events';
import { onOrderPlaced } from './events/order-placed.on';

export const app = application()
  .use(events({
    autoScan: false,
    handlers: [onOrderPlaced],
    transport: redisStreamTransport({
      url: process.env.REDIS_URL,
      keyPrefix: 'putnami:events',
      groupPrefix: process.env.SERVICE_NAME,
      consumerName: process.env.HOSTNAME,
    }),
  }));
```

Auto-discovery and explicit handlers work the same way with an external
transport. The transport only changes how publish/subscribe is backed.

## Managed Event Server Publisher

`eventServerTransport()` implements the canonical managed-workload HTTP publish
profile without importing a cloud SDK. The hosting adapter supplies the token
source, including any provider metadata or workload-identity calls:

```typescript
import { eventServerTransport, events } from '@putnami/events';

const transport = eventServerTransport({
  contractVersion: 1,
  endpoint: 'https://events.example.com',
  audience: 'https://events.example.com',
  protocol: 'putnami.events.v1',
  tokenSource: async ({ audience, signal }) =>
    workloadIdentityToken({ audience, signal }),
});

application().use(events({ transport }));
```

Managed config can select the same transport while code contributes only the
credential seam:

```yaml
events:
  transport: eventserver
  eventServer:
    contractVersion: 1
    endpoint: https://events.example.com
    audience: https://events.example.com
    protocol: putnami.events.v1
    workspaceId: workspace-1
    environment: production
    workload: orders-api
    topologyGenerationId: generation-42
```

```typescript
application().use(events({
  eventServer: {
    tokenSource: async ({ audience, signal }) =>
      workloadIdentityToken({ audience, signal }),
  },
}));
```

Published topics must declare a non-empty `version`. The transport generates a
stable event ID through the normal publisher and uses that ID as the dedupe key
when none is supplied. It encodes the managed v1 frame once, omits caller-owned
channel and legacy `auth.*` identity, rejects tenancy/routing-reserved
attributes, and preserves the exact body across bounded jittered retries. W3C
`traceparent` and `tracestate` headers are propagated separately.

Failures are typed as `EventServerPermanentPublishError`,
`EventServerRetryablePublishError`, or `EventServerAmbiguousPublishError`.
Structured `400`/`401`/`403` responses are permanent; structured `429`/`503`
responses are retryable and honor `Retry-After`; reset, timeout, malformed
response, intermediary `502`/`504`, and unstructured `5xx` outcomes are
ambiguous. Error objects never retain the bearer token or server response text.
The transport never falls back to Pub/Sub or dual-publishes.

## Multiple Transports And Routing

Use named transports when a single application needs multiple backends, for
example Redis Streams for internal workers and Google Pub/Sub for analytics.

```typescript
import { events, redisStreamTransport, topic } from '@putnami/events';
import { googlePubSubTransport } from '@putnami/events/google';

export const PageViewed = topic(
  'analytics.page_view',
  { id: String, path: String },
  { channel: 'analytics' },
);

app.use(events({
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

Resolution order is intentionally strict:

| Priority | Rule |
|----------|------|
| 1 | Topic `channel` route: first `{ channel, transport }` matching `topic.channel`. |
| 2 | Topic name route: first `{ match, transport }` matching `topic.name`. String matches support `*` wildcards. |
| 3 | `defaultTransport`. |
| 4 | Fail if nothing resolves. |

`channel` is logical topic metadata, not a physical transport name. This keeps
shared topic contracts independent from deployment topology while still letting
deployments route whole domains such as `analytics`, `audit`, or `realtime`.

Routes can target multiple transports:

```typescript
routes: [
  { channel: 'audit', transport: ['stream', 'analytics'] },
]
```

Fanout is explicit because it is not atomic. If Redis accepts the publish and
Pub/Sub fails, the library does not roll Redis back. Handlers consuming the same
topic from multiple backends must also tolerate duplicate deliveries.

## Stable Handler Groups

External brokers need stable subscription identity. Set `group` on production
handlers instead of relying on generated defaults.

```typescript
import { handler } from '@putnami/events';
import { OrderPlaced } from './topics';

export const reserveCredit = handler(OrderPlaced)
  .options({
    group: 'billing.reserve-credit',
    distribution: 'competing',
  })
  .handle(async (msg) => {
    await reserveCreditForOrder(msg.payload);
  });
```

Use the same `group` across replicas of the same logical worker. Use distinct
groups for distinct logical subscribers that each need every event. For example,
`billing.reserve-credit` and `email.order-confirmation` should be different
groups even if they consume the same `order.placed` topic.

`distribution: 'broadcast'` means every logical subscriber group receives the
event. It does not mean every replica of the same group receives the event when
the broker uses consumer groups or subscriptions.

## Redis Streams Handler Transport

Use `redisStreamTransport()` for durable background handlers backed by Redis
Streams consumer groups.

```typescript
import { events, redisStreamTransport } from '@putnami/events';

app.use(events({
  handlers: [reserveCredit],
  transport: redisStreamTransport({
    url: process.env.REDIS_URL,
    keyPrefix: 'putnami:events',
    groupPrefix: 'orders-api',
    consumerName: process.env.HOSTNAME,
    maxLen: 100_000,
    blockMs: 5_000,
    count: 100,
  }),
}));
```

### Redis Semantics

| Operation | Redis Command |
|-----------|---------------|
| Publish event | `XADD {stream}` |
| Create handler group | `XGROUP CREATE {stream} {group} $ MKSTREAM` |
| Consume events | `XREADGROUP GROUP {group} {consumer}` |
| Acknowledge success | `XACK {stream} {group} {id}` |
| Retain bounded stream | `XADD MAXLEN ~ {maxLen}` |

The stream key is the topic name, optionally prefixed by `keyPrefix`. A handler
group is `handler.options.group`, optionally prefixed by `groupPrefix`.

On success, Putnami acknowledges the Redis stream entry with `XACK`. On failure,
timeout, or manual `nack()`, Putnami schedules a retry by writing a new stream
entry with `attempt + 1`. The original entry is acknowledged only after the
retry entry has been written. When attempts are exhausted and `dlq` is enabled,
Putnami writes the message to `{topic}.dlq` and then acknowledges the original
entry.

Redis pending entries are read for the same `consumerName` when the transport
starts. For production workers, set a stable `consumerName` per process or pod
if you want restart recovery for entries that were pending during shutdown.
Automatic stealing of pending entries from dead consumers with `XAUTOCLAIM` is
not implemented yet.

### Redis Configuration

| Option | Default | Description |
|--------|---------|-------------|
| `url` | `undefined` | Redis URL. Used to create a `Bun.RedisClient` when `client` is not supplied. |
| `client` | `undefined` | Redis command client or adapter. Must support `command()` for `XADD`, `XGROUP`, `XREADGROUP`, and `XACK`. |
| `keyPrefix` | `undefined` | Prefix for topic stream keys. |
| `groupPrefix` | `undefined` | Prefix for consumer group names. Useful to isolate environments or services. |
| `consumerName` | random process-local id | Redis consumer name. Use a stable value in production. |
| `maxLen` | `undefined` | Approximate max retained entries per stream via `MAXLEN ~`. |
| `blockMs` | `5000` | `XREADGROUP BLOCK` timeout. |
| `count` | `100` | Maximum entries read per `XREADGROUP` call. |
| `closeClient` | `true` when `url` is used | Close the client created by the transport during `stop()`. |

## Google Pub/Sub Handler Transport

Use `googlePubSubTransport()` when Google Pub/Sub should own the durable queue,
redelivery, and dead-letter policy.

```typescript
import { PubSub } from '@google-cloud/pubsub';
import { events } from '@putnami/events';
import { googlePubSubTransport } from '@putnami/events/google';

const pubsub = new PubSub();

app.use(events({
  handlers: [reserveCredit],
  transport: googlePubSubTransport({
    client: pubsub,
    topicName: (topic) => `events-${topic.replaceAll('.', '-')}`,
    subscriptionName: (definition) =>
      definition.options.group ?? definition.topic.name.replaceAll('.', '-'),
  }),
}));
```

### Google Pub/Sub Semantics

Putnami publishes the internal event envelope as JSON message data. Envelope
attributes are forwarded to Pub/Sub attributes and `msg.key` is forwarded as
the Pub/Sub `orderingKey`.

When a handler succeeds, Putnami calls `ack()`. When a handler throws, times
out, or fails manual acknowledgement checks, Putnami calls `nack()`. Pub/Sub then
applies the subscription retry, ack deadline, retention, and dead-letter policy
configured in Google Cloud.

Putnami handler options such as `timeout`, `ack`, `concurrency`, `queueLimit`,
and filters still run locally. `maxRetries`, `maxBackoff`, and `dlq` do not
configure Google Pub/Sub infrastructure; model those policies on the
subscription itself.

Transport-level streaming-pull errors are reported through `onError`. Register
it in production so auth, quota, and connection failures are visible to
observability instead of only being logged by the runtime:

```typescript
googlePubSubTransport({
  client: pubsub,
  onError: (error, { subscriptionName }) => {
    logger.error({ error, subscriptionName }, 'Pub/Sub event subscription failed');
  },
});
```

### Google Pub/Sub Configuration

| Option | Default | Description |
|--------|---------|-------------|
| `client` | required | Compatible Pub/Sub client with `topic(name)` and `subscription(name)`. |
| `topicName` | identity | Maps Putnami topic names to Pub/Sub topic names. |
| `subscriptionName` | handler `group`, else topic with dots replaced by dashes | Maps each handler definition to a Pub/Sub subscription name. |
| `onError` | logs only | Called when the underlying Pub/Sub subscription emits an `error` event. |
| `closeSubscriptions` | `true` | Close subscription handles during transport `stop()`. |

## Testing With Adapters

Each transport depends on narrow interfaces. Unit tests can pass fakes instead
of starting Redis or Pub/Sub:

```typescript
const transport = redisStreamTransport({
  client: fakeRedisCommandClient,
  blockMs: 5,
  consumerName: 'test-worker',
});
```

For integration tests, prefer real broker containers and stable groups per test
suite so pending messages from one test do not affect another.
