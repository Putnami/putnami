# Realtime Brokers

`@putnami/events` includes realtime brokers for SSE-style fanout. They are
separate from handler transports:

- realtime brokers are used by application routes that publish to connected
  browser clients
- handler transports are used by `events({ transport })` to run background
  handlers with retry and acknowledgement semantics

Realtime brokers do not provide handler retries, manual ack, DLQ, or competing
consumer behavior. They provide publish/subscribe primitives that are useful
behind Server-Sent Events or WebSocket endpoints.

## Choosing A Realtime Broker

| Broker | Use When | Replay | Backend |
|--------|----------|--------|---------|
| `redisStream()` | Clients should reconnect with `Last-Event-ID` and replay a bounded missed window. | Yes, bounded by stream retention. | Redis Streams with `XADD`, `XREAD`, `XTRIM`. |
| `redisPubSub()` | Only currently connected clients need the message. | No. | Redis Pub/Sub with `PUBLISH` / `SUBSCRIBE`. |
| `postgresRealtime()` | The app already has Postgres and live-only fanout is enough. | No. | PostgreSQL `LISTEN` / `NOTIFY`. |

For SSE, Redis Streams are the safest default because short replay protects
clients from tab refreshes, network drops, and rolling deploys. Redis Pub/Sub
and Postgres LISTEN/NOTIFY are simpler live-only options.

## Shared API

All realtime brokers implement the same shape:

```typescript
const message = await realtime.publish(topic, payload, {
  id: crypto.randomUUID(),
  attributes: { source: 'orders-api' },
  traceId: 'trace-123',
});

const subscription = await realtime.subscribe(topic, async (message) => {
  sendSse({
    id: message.id,
    event: message.topic,
    data: message.payload,
  });
});

await subscription.close();
```

`topic` can be a Putnami typed topic or a string. Typed topics validate payloads
before publish. The delivered `message` contains:

| Field | Description |
|-------|-------------|
| `id` | Public realtime message id. Use this as the SSE `id`. |
| `topic` | Topic name. Use this as the SSE event type if desired. |
| `payload` | Validated payload for typed topics, otherwise the raw payload. |
| `timestamp` | Publish timestamp. |
| `attributes` | String attributes supplied at publish time. |
| `traceId` | Optional trace correlation id. |

## Redis Streams Realtime

Use streams when clients should resume from the SSE `Last-Event-ID` header.

```typescript
import { redisStream } from '@putnami/events/redis';
import { OrderUpdated } from './topics';

const realtime = redisStream({
  url: process.env.REDIS_URL,
  keyPrefix: 'putnami:realtime',
  maxLen: 10_000,
  replayTtlMs: 60_000,
});

await realtime.publish(OrderUpdated, {
  id: crypto.randomUUID(),
  status: 'paid',
});

const sub = await realtime.subscribe(
  OrderUpdated,
  async (message) => {
    sendSse({
      id: message.id,
      event: message.topic,
      data: message.payload,
    });
  },
  { from: request.headers.get('last-event-id') ?? 'latest' },
);
```

Stream mode uses Redis stream ids as public message ids. Pass the browser's
`Last-Event-ID` header back as `from` to resume after the last delivered event.

`from` values:

| Value | Meaning |
|-------|---------|
| `latest` or omitted | Start with messages published after the subscription starts. |
| `earliest` | Read from the oldest retained stream entry. |
| Redis stream id string | Resume after that stream id. This is the normal SSE reconnect path. |

Stream mode does not use consumer groups. Each SSE connection reads
independently, so one client cannot steal messages from another client.

### SSE Route Example

```typescript
export async function GET(request: Request): Promise<Response> {
  const stream = new ReadableStream({
    async start(controller) {
      const encoder = new TextEncoder();
      const subscription = await realtime.subscribe(
        OrderUpdated,
        (message) => {
          controller.enqueue(encoder.encode(
            `id: ${message.id}\nevent: ${message.topic}\ndata: ${JSON.stringify(message.payload)}\n\n`,
          ));
        },
        { from: request.headers.get('last-event-id') ?? 'latest' },
      );

      request.signal.addEventListener('abort', () => {
        void subscription.close();
      }, { once: true });
    },
  });

  return new Response(stream, {
    headers: {
      'content-type': 'text/event-stream',
      'cache-control': 'no-cache',
      connection: 'keep-alive',
    },
  });
}
```

### Redis Streams Configuration

| Option | Default | Description |
|--------|---------|-------------|
| `url` | `undefined` | Redis URL. Used to create a `Bun.RedisClient` when `client` is not supplied. |
| `client` | `undefined` | Redis command client or adapter. Must support `command()` for `XADD`, `XREAD`, and `XTRIM`. |
| `keyPrefix` | `undefined` | Prefix for Redis stream keys. |
| `maxLen` | `undefined` | Approximate maximum entries retained per stream using `MAXLEN ~`. |
| `replayTtlMs` | `undefined` | Approximate time window retained for replay using `XTRIM MINID ~`. |
| `blockMs` | `5000` | `XREAD BLOCK` timeout. |
| `count` | `100` | Maximum entries read per `XREAD` call. |
| `codec` | JSON | Realtime message encoder/decoder. |
| `closeClient` | `true` when `url` is used | Close the client created by the broker during `close()`. |

Use both `maxLen` and `replayTtlMs` for bounded retention. Redis trimming is
approximate because the broker uses Redis approximate trim commands.

## Redis Pub/Sub Realtime

Use Pub/Sub when live-only fanout is enough and missing messages while a client
is disconnected is acceptable.

```typescript
import { redisPubSub } from '@putnami/events/redis';

const realtime = redisPubSub({
  url: process.env.REDIS_URL,
  keyPrefix: 'putnami:realtime',
});

await realtime.subscribe('orders.live', async (message) => {
  sendSse({
    id: message.id,
    event: message.topic,
    data: message.payload,
  });
});

await realtime.publish('orders.live', {
  orderId: 'order_123',
  status: 'paid',
});
```

The `from` subscribe option is ignored in Pub/Sub mode because Redis does not
retain messages for disconnected subscribers.

### Redis Pub/Sub Configuration

| Option | Default | Description |
|--------|---------|-------------|
| `url` | `undefined` | Redis URL. Used to create a `Bun.RedisClient` when `publisher` is not supplied. |
| `keyPrefix` | `undefined` | Prefix for Redis channels. |
| `publisher` | `undefined` | Redis Pub/Sub publisher client. |
| `subscriber` | `publisher.duplicate()` when available, else `publisher` | Redis Pub/Sub subscriber client. |
| `codec` | JSON | Realtime message encoder/decoder. |
| `closeClients` | `true` when `url` is used | Close clients created by the broker during `close()`. |

## Custom Redis Clients

Passing `url` uses `Bun.RedisClient`. Passing explicit clients lets applications
adapt `ioredis`, `redis`, or test doubles without adding a hard dependency to
`@putnami/events`.

Command clients must expose:

```typescript
interface RedisCommandClient {
  command(command: string, args: string[]): Promise<unknown> | unknown;
  close?(): Promise<void> | void;
}
```

Pub/Sub clients must expose:

```typescript
interface RedisPubSubClient {
  publish(channel: string, message: string): Promise<unknown> | unknown;
  subscribe(channel: string, handler: (message: string) => void): Promise<unknown> | unknown;
  unsubscribe?(channel: string, handler?: (message: string) => void): Promise<unknown> | unknown;
  duplicate?(): RedisPubSubClient;
  close?(): Promise<void> | void;
}
```

## PostgreSQL LISTEN/NOTIFY Realtime

Use Postgres realtime when the app already has a PostgreSQL connection and
live-only fanout for small payloads is enough.

```typescript
import { postgresRealtime } from '@putnami/events/postgres';

const realtime = postgresRealtime({
  client: pgClient,
  channelPrefix: 'putnami_realtime',
});

await realtime.subscribe('orders.live', async (message) => {
  sendSse({
    id: message.id,
    event: message.topic,
    data: message.payload,
  });
});

await realtime.publish('orders.live', {
  orderId: 'order_123',
  status: 'paid',
});
```

Postgres `NOTIFY` payloads are limited to roughly 8000 bytes. The broker checks
the encoded payload size before publishing and throws if it exceeds
`maxPayloadBytes`.

The broker sanitizes channel names to PostgreSQL-safe identifiers and applies
`channelPrefix` before sanitizing. `from` must be omitted or set to `latest`;
Postgres cannot replay missed notifications.

### Postgres Configuration

| Option | Default | Description |
|--------|---------|-------------|
| `client` | required | Compatible client with `query()` and notification listener methods. |
| `channelPrefix` | `undefined` | Prefix for PostgreSQL notification channels. |
| `maxPayloadBytes` | `7900` | Guard for encoded `NOTIFY` payload size. |
| `codec` | JSON | Realtime message encoder/decoder. |

Compatible clients must expose:

```typescript
interface PostgresNotifyClient {
  query(sql: string, params?: unknown[]): Promise<unknown> | unknown;
  on?(event: 'notification', handler: (notification: PostgresNotification) => void): unknown;
  off?(event: 'notification', handler: (notification: PostgresNotification) => void): unknown;
  removeListener?(event: 'notification', handler: (notification: PostgresNotification) => void): unknown;
}
```
