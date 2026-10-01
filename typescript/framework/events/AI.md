# @putnami/events

Typed event messaging for Putnami with transport-agnostic topics, fluent handlers, publishers, and a local in-memory broker.

Use this package when you need:

- typed event contracts shared across services
- background or cross-service event delivery
- handler retry / DLQ / timeout semantics
- local development without external broker setup

Do not use this package for:

- request/response APIs: use `@putnami/application` or `@putnami/web`
- realtime browser subscriptions by itself: expose SSE/WebSocket routes through `@putnami/application`; use `redisStream()` / `redisPubSub()` from `@putnami/events/redis` only as the backend fanout/replay broker
- exactly-once guarantees; handlers must be idempotent

## Setup

```ts
import { application } from '@putnami/application';
import { events } from '@putnami/events';

export const app = () =>
  application()
    .use(events());
```

By default the plugin auto-discovers `events/*.on.ts` handler files and uses the local transport when no event endpoint is configured.

## Topics

Topics are pure typed contracts:

```ts
import { topic, DateIso, Email, Uuid } from '@putnami/events';
import type { InferTopicPayload } from '@putnami/events';

export const UserCreated = topic('user.created', {
  id: Uuid,
  email: Email,
  name: String,
  createdAt: DateIso,
});

type UserCreatedPayload = InferTopicPayload<typeof UserCreated>;
```

Important:

- topics define name + payload schema only
- they carry no transport logic
- `channel` is optional logical routing metadata, not a physical transport name
- publishers and handlers should import the same topic definition

## Handlers

Handlers use a fluent builder:

```ts
import { handler } from '@putnami/events';
import { UserCreated } from '../shared/topics/user';

export default handler(UserCreated)
  .options({
    distribution: 'broadcast',
    maxRetries: 5,
    timeout: 10_000,
  })
  .handle(async (msg) => {
    console.log(msg.payload.email);
  });
```

### Handler Builder Methods

| Method | Purpose |
|--------|---------|
| `.options(opts)` | Retry, timeout, distribution, ack mode, DLQ |
| `.filter(filter)` | Attribute-based server-side filtering |
| `.inject(tokens)` | Resolve DI dependencies for each invocation |
| `.handle(fn)` | Finalize the handler definition |

### Handler With DI

```ts
export default handler(UserCreated)
  .inject({ mailer: WelcomeMailer, users: UserRepository })
  .handle(async ({ mailer, users }, msg) => {
    const user = await users.findById(msg.payload.id);
    if (user) {
      await mailer.send(user.email);
    }
  });
```

When invoked through `events()` inside an `Application`, each handler run gets its own DI scope automatically.

## Message Model

Handlers receive a message envelope, not just raw payload:

```ts
handler(UserCreated).handle(async (msg) => {
  msg.id;
  msg.topic;
msg.payload;
  msg.key;
  msg.dedupeKey;
  msg.topicVersion;
  msg.timestamp;
  msg.attributes;
  msg.attempt;
  msg.traceId;
  msg.signal;
});
```

### Ack Modes

Auto-ack is the default:

- return normally → ack
- throw → nack / retry

Manual mode:

```ts
handler(UserCreated)
  .options({ ack: 'manual' })
  .handle(async (msg) => {
    try {
      await processUser(msg.payload);
      msg.ack();
    } catch (error) {
      msg.nack(error instanceof Error ? error.message : 'failed');
    }
  });
```

## Publishing

Use `getPublisher()` to bind a typed publisher to a topic:

```ts
import { getPublisher } from '@putnami/events';
import { UserCreated } from '../shared/topics/user';

const publishUserCreated = getPublisher(UserCreated);

await publishUserCreated({
  id: crypto.randomUUID(),
  email: 'jane@example.com',
  name: 'Jane',
  createdAt: new Date().toISOString(),
});
```

Publish options:

```ts
await publishUserCreated(
  {
    id: crypto.randomUUID(),
    email: 'jane@example.com',
    name: 'Jane',
    createdAt: new Date().toISOString(),
  },
  {
    attributes: { region: 'eu', source: 'api' },
    traceId: 'trace-123',
  },
);
```

Payloads are validated before publish.

## Transactional Outboxes

`outbox()` declares that a feature stages rows in a transaction and lets its own
relay publish them after the commit:

```ts
import { events, outbox, topic, Uuid } from '@putnami/events';

export const SessionRevoked = topic('identity.session.revoked', { sessionId: Uuid });

export const IdentityOutbox = outbox('identity', {
  topics: [SessionRevoked],
  table: 'auth.event_outbox',
  datasource: 'identity',
});

app.use(events({ outboxes: [IdentityOutbox] }));
```

Important:

- the declaration is pure data and runtime-inert: no table, transaction, relay loop, or retry policy comes with it
- writing and relaying the row stay in your code
- it exists so the design graph can separate the commit-time enqueue (`enqueues`) from the relay-time publication (`publishes`)
- a plugin that owns `events()` privately forwards these contributions with `designDelegates()` from `@putnami/application`

## File-Based Discovery

Default convention:

```text
src/
  events/
    user-created.on.ts
    order-placed.on.ts
    payment/
      failed.on.ts
```

Rules:

- files use the `*.on.ts` suffix
- default export or named exports can contain handlers
- folder names are for organization only; topic names come from `topic(...)`

You can disable scanning and register handlers explicitly:

```ts
application().use(events({ autoScan: false, handlers: [onUserCreated] }));
```

## Delivery Semantics

### Distribution

| Mode | Meaning |
|------|---------|
| `competing` | One consumer instance handles each message |
| `broadcast` | Every subscriber instance receives the message |

### Retry / DLQ

Important handler options:

- `group`
- `maxRetries`
- `maxBackoff`
- `timeout`
- `concurrency`
- `queueLimit`
- `overflow`
- `dlq`
- `ack`

Failed messages use exponential backoff with jitter and can be routed to `{topic}.dlq`.

## Context Propagation

When publishing from an HTTP request or another active context:

- `traceId` is propagated automatically
- authenticated user claims are copied into `auth.*` attributes
- caller-supplied `auth.*` attributes are ignored to avoid spoofing claims

Handlers can read that information from `msg.traceId` and `msg.attributes`.

## Local vs Cloud

Without an `endpoint` / `EVENTS_ENDPOINT`, the plugin uses the local broker and can start a local server on port `4222`.

With a configured endpoint, the plugin uses the HTTP event transport instead. Set `EVENTS_TOKEN` or `events({ token })` when the endpoint requires bearer authentication.

## Push Delivery (serverless)

`delivery: 'push'` lets handlers run on a scale-to-zero workload: instead of a long-lived pull/stream loop, the provider POSTs each event to a receiver route registered on the app's HTTP server.

```ts
const app = application()
  .use(http())
  .use(
    events({
      delivery: 'push',
      push: {
        issuer: 'https://accounts.google.com',
        audience: 'https://my-service.run.app/_putnami/events/orders',
        allowedServiceAccounts: ['events-server@my-project.iam.gserviceaccount.com'],
        // verify defaults to the app OAuthService.verify(); provide a custom
        // verifier when the pusher's issuer/JWKS differs from the app IdP.
      },
    }),
  );
```

In push mode the plugin registers `POST /_putnami/events/[subscription]` and does **not** start the pull/stream loop. The receiver verifies the pusher's OIDC token fail-closed (signature/issuer/audience) plus a **service-account-email allowlist**, then dispatches the decoded envelope through the shared pipeline. The HTTP status encodes the ack: `2xx` ack, `4xx` dead-letter, `5xx` retry — so handlers must be idempotent. Caller-supplied `auth.*` attributes are stripped before dispatch. `delivery` travels into the infra requirement so the deployer provisions a push subscription; pull/stream modes are unchanged. See `protocols/events/doc/11-push-delivery.md`.

For first-deploy bootstrap, `push: { enabled: false }` mounts the route but returns `503` before verification or handler dispatch. Enabled mode requires the normal issuer, audience, and service-account allowlist.

## Realtime Brokers

Realtime brokers are for SSE-style fanout, not reliable background handlers.
Redis brokers live under `@putnami/events/redis`.

```ts
import { redisStream, redisPubSub } from '@putnami/events/redis';
```

Use `redisStream()` when clients should reconnect with `Last-Event-ID` and replay a bounded window of missed messages.
Use `redisPubSub()` when live-only fanout is enough and missed messages while disconnected are acceptable.

Both brokers accept `url` for `Bun.RedisClient`, or explicit compatible Redis clients/adapters.

Postgres realtime lives under `@putnami/events/postgres` and uses LISTEN/NOTIFY for live-only fanout:

```ts
import { postgresRealtime } from '@putnami/events/postgres';
```

## Mobile And Browser Clients

React Native and browser apps should use client-safe subpaths:

```ts
import { eventClient } from '@putnami/events/client';
import { topic } from '@putnami/events/topic';
```

The client API subscribes through an Event Server WebSocket endpoint. It does not
connect directly to Redis, Pub/Sub, Postgres, or the server-side `events()`
plugin. Browser conditional root exports expose only topic/client-safe APIs.
Event Server frames use the canonical `putnami.events.v1` protocol from
`@putnami/events/protocol`.

## Production Transports

Use these for reliable background event handlers:

- `redisStreamTransport()` from `@putnami/events/redis`
- `googlePubSubTransport()` from `@putnami/events/google`
- `eventServerTransport()` for managed, publish-only `POST /events/publish`
- `routingTransport()` / `events({ transports, routes, defaultTransport })` for multiple named transports

Configure `events({ transport })` to use an explicit transport instead of the local server:

```ts
application().use(events({
  transport: redisStreamTransport({ url: process.env.REDIS_URL }),
}));
```

Set stable handler `group` values for external transports. Use the same group across replicas for competing consumers, and a distinct group per logical subscriber for broadcast handlers.

Multiple named transports are resolved by topic channel first, then topic name
matches, then `defaultTransport`:

```ts
topic('analytics.page_view', schema, { channel: 'analytics' });

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

Fanout is explicit with `transport: ['stream', 'analytics']` and is not atomic.

Managed config may select `events.transport: eventserver` and supply `events.eventServer` with `contractVersion: 1`, endpoint, audience, and `putnami.events.v1`. Code must provide the provider-owned `tokenSource`; this package does not acquire Google credentials. The transport requires versioned topics, materializes stable id/dedupe identity, preserves request bytes across bounded retries, propagates W3C trace headers, and exposes typed permanent, retryable, and ambiguous errors. It never falls back or dual-publishes.

## Common Pitfalls

- Do not treat handlers as exactly-once; write them to be idempotent
- Do not put transport details into topics; topics are schema contracts only
- Do not forget that return/throw controls ack/nack in auto mode
- Do not use event folder names as if they changed topic names
- Do not assume `.inject()` works outside the plugin-managed application lifecycle
- Do not use events when a synchronous request/response API is the right shape

## Detailed Documentation

See `doc/`:

- `01-getting-started.md`
- `02-topics.md`
- `03-handlers.md`
- `04-publishing.md`
- `05-plugin.md`
- `06-observability.md`
- `07-redis-realtime.md` (realtime brokers: Redis Streams, Redis Pub/Sub, Postgres)
- `08-production-transports.md`
- `09-mobile-client.md`
- `10-protocol.md`

## Maintained contract

This public package is `stable` in the workspace [support
catalog](../../../putnami.support.json). It owns the
[event-messaging specification](specs/event-messaging.json) with its
[at-least-once delivery ADR](doc/adr/0001-delivery-is-at-least-once-and-loss-is-recorded.md).

Facts to rely on when generating code:

- Delivery is at-least-once. Handlers must be idempotent; never write an example
  that assumes a message arrives once.
- Handler policy lives in `.options({ ... })` on the builder — `maxRetries`,
  `maxBackoff`, `timeout`, `concurrency`, `queueLimit`, `overflow`, `dlq`,
  `ack`, `distribution`, `group`. There are no per-option builder methods.
  `maxRetries` counts total attempts, not extra retries.
- A terminal loss emits exactly one record for the message, never one per
  refusing subscriber, and a dead-letter record is emitted only once a `.dlq`
  subscriber has accepted it.
- `outbox()` is a declaration for the design graph. It creates no table, no
  transaction, and no relay loop.
- Redis, Google Pub/Sub, and Postgres transports take a client object the
  application supplies. This package has no vendor SDK dependency, and the
  browser entry exposes only topics, client, and protocol.

Before v1.0, follow the workspace
[migration-based compatibility policy](../../../RELEASE.md); do not infer strict
compatibility between every `0.x` minor.
