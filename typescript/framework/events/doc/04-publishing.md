# Publishing Events

## getPublisher

Create a typed, schema-validated publish function bound to a topic:

```typescript
import { getPublisher } from '@putnami/events';
import { UserCreated } from '../shared/topics/user';

const publish = getPublisher(UserCreated);

await publish({
  id: crypto.randomUUID(),
  email: 'jane@example.com',
  name: 'Jane',
});
```

The payload is validated against the topic's schema before publishing. Invalid payloads throw immediately:

```typescript
// Throws: "Invalid payload for topic 'user.created': user.created.email is required"
await publish({ id: crypto.randomUUID(), name: 'Jane' });
```

## Publish Options

```typescript
await publish(
  { id: '...', email: 'jane@example.com', name: 'Jane' },
  {
    messageId: 'message-123',
    key: 'user-123',
    dedupeKey: 'user.created:user-123',
    attributes: { region: 'eu', source: 'api' },
    traceId: 'custom-trace-id',
    traceparent: '00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01',
    signal: request.signal,
    deadline: Date.now() + 5_000,
  },
);
```

| Option | Description |
|--------|-------------|
| `messageId` | Stable message ID. Generated when omitted. |
| `key` | Optional routing or partition key for transports that support keyed delivery. |
| `dedupeKey` | Optional idempotency key for handlers or transports that support deduplication. |
| `attributes` | Key-value pairs attached to the message envelope. Used for server-side filtering. |
| `traceId` | Correlation ID for distributed tracing. Inherited from the current async context if omitted. Falls back to a random UUID when no context exists. |
| `traceparent` / `tracestate` | W3C trace headers used by HTTP-based transports. Inbound request headers are propagated automatically when present. |
| `signal` | Cancels publication. Cancellation after a network send has an ambiguous outcome. |
| `deadline` | Absolute deadline as epoch milliseconds or a `Date`; an earlier transport deadline still wins. |

## Context Propagation

When publishing from inside an HTTP handler (or any `runInContext` scope), the publisher **automatically captures** the current context:

- **`traceId`** — inherited from the HTTP request context so the event can be correlated back to the originating request. An explicit `traceId` in `PublishOptions` takes precedence.
- **Auth claims** — if the context contains a `user` object (set by the OAuth middleware), the following JWT claims are auto-captured as message attributes:

| Attribute | Source claim | Description |
|-----------|-------------|-------------|
| `auth.sub` | `user.sub` | Subject (user ID) |
| `auth.email` | `user.email` | User email |
| `auth.azp` | `user.azp` | Authorized party (client) |
| `auth.client_id` | `user.client_id` | Client ID |

Auto-captured `auth.*` attributes are protected. Caller-supplied attributes whose
keys start with `auth.` are ignored so user/client claims cannot be spoofed.
Non-auth attributes are preserved.

```typescript
// Inside an authenticated HTTP handler — traceId and auth claims
// are automatically attached to the envelope, no extra code needed.
app.post('/users', async (ctx) => {
  const user = await createUser(ctx);

  const publish = getPublisher(UserCreated);
  await publish({ id: user.id, email: user.email, name: user.name });
  // envelope.traceId === ctx.traceId (from the HTTP request)
  // envelope.attributes['auth.sub'] === ctx.user.sub
});
```

Handlers can then read these attributes from the message:

```typescript
handler(UserCreated).handle(async (msg) => {
  const publishedBy = msg.attributes['auth.sub']; // user who triggered the event
  const traceId = msg.traceId; // same traceId as the original HTTP request
});
```

## Transactional Outboxes

Some workloads do not publish directly. They write a row inside the business
transaction and let a relay publish it once that transaction commits, so the
event cannot be lost when the commit succeeds and the broker is unreachable.

`outbox()` declares that arrangement. It is pure data, exactly like `topic()`:
it owns no table, no transaction, no relay loop, and no retry policy. Writing
the row and relaying it stay in your code.

```typescript
import { outbox, topic, Uuid } from '@putnami/events';

export const SessionRevoked = topic('identity.session.revoked', { sessionId: Uuid });

export const IdentityOutbox = outbox('identity', {
  topics: [SessionRevoked],
  table: 'auth.event_outbox',
  datasource: 'identity',
});
```

Register the declaration on the events plugin, next to your handlers:

```typescript
module('identity')
  .feature({ id: 'identity/opaque-tokens', name: 'Opaque token revocation', outcome: '…', owner: 'identity' })
  .use(events({ outboxes: [IdentityOutbox] }));
```

Registering an outbox is runtime-inert: warmup, subscription, delivery, retry,
and shutdown behave exactly as they do without it. The declaration exists so the
design graph can tell two different facts apart:

| Relationship | Authority | Meaning |
|--------------|-----------|---------|
| `module -enqueues-> event.outbox` | exact | A row is committed with the business transaction |
| `event.outbox -publishes-> event.topic` | exact | The relay publishes that row's topic later |
| `module -publishes-> event.topic` | derived | A `getPublisher()` call site publishes directly |

Only names and bounded identifiers reach the graph — the outbox name, table, and
datasource. No payload, row, or credential value is ever projected.

## Idempotency

Delivery is at-least-once, so handlers must be idempotent: processing the same
message twice must produce the same result. To prove it locally, turn on
`events({ simulateDuplicates: true })` and the in-memory broker redelivers ~2%
of handled messages. It is off by default.

Strategies:
- Use message IDs for deduplication checks
- Design handlers as upserts rather than inserts
- Check current state before applying changes

The managed Event Server transport materializes a missing `dedupeKey` from the
stable message ID before its first send. Internal retries reuse the exact JSON
request bytes. A timeout or reset can still be ambiguous, so the transport only
retries the same Event Server route and never falls back or shadow-publishes to
another backend.
