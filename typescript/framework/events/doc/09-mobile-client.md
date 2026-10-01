# Mobile And Browser Clients

`@putnami/events` exposes client-safe entrypoints for React Native and browser
applications. Use them to share topic contracts and subscribe through an
Event Server WebSocket endpoint. Do not connect mobile apps directly to Redis,
Google Pub/Sub, Postgres, or the server-side `events()` plugin.

## Client-Safe Imports

Use subpath imports in React Native:

```typescript
import { eventClient, type ClientEventMessage } from '@putnami/events/client';
import { topic } from '@putnami/events/topic';
import { Uuid } from '@putnami/runtime';

export const UserUpdated = topic('user.updated', {
  id: Uuid,
  name: String,
}, {
  channel: 'realtime',
});
```

The package also has a browser conditional root export. Browser bundlers can
import `topic()` and `eventClient()` from `@putnami/events` without pulling the
server plugin or broker transports into the bundle.

## React Native Subscription

```typescript
import { eventClient } from '@putnami/events/client';
import { UserUpdated } from './topics';

const client = eventClient({
  url: 'wss://api.example.com/events',
  token: async () => `Bearer ${await getAccessToken()}`,
  headers: async () => ({ 'x-device-id': await getDeviceId() }),
  reconnect: true,
});

const subscription = client.subscribe(
  UserUpdated,
  (message) => {
    // message.payload is typed and runtime-validated from UserUpdated.schema
    updateUserCache(message.payload.id, message.payload.name);
  },
  {
    from: 'latest',
    onError: (error) => console.error(error),
  },
);

await subscription.ready;

// Later, for example on screen unmount:
subscription.close();
```

Each subscription opens one WebSocket. On reconnect, the client resumes from the
last delivered event id when the Event Server supports replay, for example with
`redisStream()`.

Before connecting to an external Event Server, clients can read
`GET /.well-known/putnami/events` and verify the returned capabilities with
`isCompatibleEventServer()` from `@putnami/events/protocol`.

## Event Server Protocol

The client sends JSON frames.

Authentication frame, sent first when `token` or `headers` is configured:

```json
{ "protocol": "putnami.events.v1", "type": "auth", "token": "Bearer ...", "headers": { "x-device-id": "..." } }
```

`token` and `headers` can be static values or async providers. Providers are
resolved on every open/reconnect, so React Native apps can forward the current
user access token after refresh. This is the client-side auth model; service
account and workload identity auth are server-side concerns and should be
auto-wired by Putnami Event Plane when the app runs on managed infrastructure.

Subscribe frame:

```json
{
  "protocol": "putnami.events.v1",
  "type": "subscribe",
  "topic": "user.updated",
  "channel": "realtime",
  "from": "latest"
}
```

The server sends event frames:

```json
{
  "protocol": "putnami.events.v1",
  "type": "event",
  "id": "1713980000000-0",
  "topic": "user.updated",
  "channel": "realtime",
  "payload": { "id": "...", "name": "Jane" },
  "key": "user-123",
  "dedupeKey": "user.updated:1713980000000-0",
  "topicVersion": "v1",
  "timestamp": "2026-04-24T10:00:00.000Z",
  "attributes": {},
  "traceId": "trace-123"
}
```

The server can send control frames:

```json
{ "protocol": "putnami.events.v1", "type": "error", "message": "Unauthorized" }
{ "protocol": "putnami.events.v1", "type": "complete" }
```

## Backend Shape

The backend Event Server should:

- expose `GET /.well-known/putnami/events` with `protocol: "putnami.events.v1"`
- authenticate the WebSocket before subscribing
- accept `subscribe` frames with `topic`, optional `channel`, and optional `from`
- subscribe to a server-side realtime broker such as `redisStream()`
- forward broker messages as `type: "event"` frames
- close broker subscriptions when the WebSocket closes

Use `redisStream()` when React Native clients should survive reconnects with
bounded replay. Use `redisPubSub()` only for live-only subscriptions where
missed messages are acceptable.

## Scope

The mobile client is for realtime app updates. It does not run background event
handlers and does not expose handler ack, retry, DLQ, or competing-consumer
semantics. Those remain server-side responsibilities of `events()` transports.
