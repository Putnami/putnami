# @putnami/events

Typed event messaging: transport-agnostic topics, fluent handlers, publishers,
and an in-memory broker that reproduces production delivery semantics locally.

```ts
import { application } from '@putnami/application';
import { Email, events, handler, topic, Uuid } from '@putnami/events';

export const UserCreated = topic('user.created', { id: Uuid, email: Email });

export default handler(UserCreated)
  .options({ maxRetries: 3, dlq: true })
  .handle(async (message) => {
    await sendWelcomeEmail(message.payload.email);
  });

export const app = () => application().use(events());
```

## Mental model

- A **topic** is a name plus a payload schema. It carries no transport.
- A **handler** subscribes to a topic and declares its own delivery policy:
  concurrency, queue limit, overflow behaviour, retries, dead-letter queue,
  timeout, and whether delivery is broadcast or competing.
- A **transport** moves envelopes. The in-memory broker is the default for local
  development; Redis Streams, Google Pub/Sub, Postgres `LISTEN/NOTIFY`, and the
  push receiver are the production options.
- An **outbox** declaration is pure data: it lets the design graph tell
  commit-time enqueue apart from relay-time publication. It owns no table,
  transaction, or relay loop.

## Delivery semantics

Delivery is **at-least-once**. Handlers must be idempotent. Set
`simulateDuplicates` on the in-memory broker to have it redeliver about 2% of
handled messages so a non-idempotent handler fails locally instead of in
production.

A failed delivery retries with exponential backoff up to the handler's maximum,
then goes to `<topic>.dlq` when the handler declared one. A message that is
genuinely lost always leaves exactly one drop record naming why — retries
exhausted, re-delivery refused, no dead-letter subscriber, dead-letter enqueue
refused, or queue full — and never both a drop and a dead-letter record. A
dead-letter with `dlq: true` and no subscriber is kept, whole, in a bounded
buffer you can drain.

## Backends are interfaces, not SDKs

This package depends on no vendor SDK. `@putnami/events/redis`,
`/google`, and `/postgres` describe the client shape they need and the
application supplies it, so the browser entry point can exist and no consumer
carries a cloud SDK it does not use.

## Entry points

| Import | Contents |
|--------|----------|
| `@putnami/events` | Topics, handlers, publishers, transports, in-memory broker, plugin |
| `@putnami/events/topic` | Topic declarations only |
| `@putnami/events/client` | Browser/mobile event client |
| `@putnami/events/protocol` | Wire protocol types |
| `@putnami/events/redis` | Redis Streams and Pub/Sub transports and realtime brokers |
| `@putnami/events/google` | Google Pub/Sub transport |
| `@putnami/events/postgres` | Postgres `LISTEN/NOTIFY` realtime broker |
| `@putnami/events/serve` | Standalone event-server entry point |

The browser condition resolves the root import to topics, client, and protocol
only — no broker, transport, or server code is reachable from a browser bundle.

## Maintained contract

This package is `stable` in the workspace
[support catalog](../../../putnami.support.json). It owns one public promise:

- **[Event messaging](specs/event-messaging.json)**, with the
  [at-least-once delivery ADR](doc/adr/0001-delivery-is-at-least-once-and-loss-is-recorded.md).
  Delivery is at-least-once and duplicates are normal; retries are bounded and
  end in a dead letter or a recorded drop; a terminal outcome emits exactly one
  record for the message, never one per refusing subscriber; and stopping the
  broker drains the work it already admitted.

Before v1.0.0, follow the workspace
[compatibility policy](../../../RELEASE.md): a breaking change is allowed in a
minor `0.x` release when the migration is documented. Do not infer strict
compatibility between every `0.x` minor.

## Documentation

- [Getting started](./doc/01-getting-started.md)
- [Topics](./doc/02-topics.md)
- [Handlers](./doc/03-handlers.md)
- [Publishing](./doc/04-publishing.md)
- [Plugin](./doc/05-plugin.md)
- [Observability](./doc/06-observability.md)
- [Redis realtime](./doc/07-redis-realtime.md)
- [Production transports](./doc/08-production-transports.md)
- [Mobile client](./doc/09-mobile-client.md)
- [Protocol](./doc/10-protocol.md)

## See also

- [`@putnami/application`](../application/README.md) — the application the plugin composes into
- [`@putnami/database`](../database/README.md) — the relational store an outbox stages rows in

## License

[FSL-1.1-MIT](../../../LICENSE.md)
