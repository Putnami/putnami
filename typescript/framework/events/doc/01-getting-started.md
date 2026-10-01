# Getting Started with @putnami/events

`@putnami/events` is a typed event messaging module for building event-driven applications with Putnami. It provides a transport-agnostic API with an in-memory broker for local development.

## Installation

```bash
bunx putnami deps add @putnami/events
```

## Quick Start

### 1. Define a Topic

Topics are typed contracts shared between publishers and subscribers. They carry no transport logic.

```typescript
// shared/topics/user.ts
import { topic, Uuid, Email } from '@putnami/events';

export const UserCreated = topic('user.created', {
  id: Uuid,
  email: Email,
  name: String,
});
```

### 2. Create a Handler

Handlers subscribe to topics and process messages. Place handler files with the `.on.ts` suffix in the `events/` directory for automatic discovery.

```typescript
// events/user-created.on.ts
import { handler } from '@putnami/events';
import { UserCreated } from '../shared/topics/user';

export default handler(UserCreated)
  .options({ distribution: 'broadcast' })
  .handle(async (msg) => {
    console.log(`New user: ${msg.payload.name} (${msg.payload.email})`);
    // return = ack, throw = nack → retry with backoff
  });
```

### 3. Publish Events

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

### 4. Wire It Up

```typescript
import { application } from '@putnami/application';
import { events } from '@putnami/events';

const app = application()
  .use(events()); // Auto-discovers events/*.on.ts handlers

await app.start();
// Local events server starts automatically on port 4222
```

Handlers in `events/*.on.ts` files are auto-discovered at build time. No manual imports needed.

## Key Concepts

| Concept | Description |
|---------|-------------|
| **Topic** | A typed contract (name + schema). Pure data, no transport. |
| **Handler** | Subscribes to a topic. Configured via fluent builder. |
| **Publisher** | Schema-validated publish function bound to a topic. |
| **Transport** | Pluggable backend (in-memory for dev, cloud for production). |
| **Events Plugin** | Application lifecycle plugin managing the transport. |

## Next Steps

| Need | Read |
|------|------|
| Production background handlers on Redis Streams or Google Pub/Sub | `08-production-transports.md` |
| SSE fanout with Redis Streams, Redis Pub/Sub, or Postgres LISTEN/NOTIFY | `07-redis-realtime.md` |
| React Native or browser subscriptions through an Event Server WebSocket endpoint | `09-mobile-client.md` |
| Cross-language event envelope and Event Server protocol | `10-protocol.md` |
| Handler retry, group, timeout, ack, and DLQ options | `03-handlers.md` |
